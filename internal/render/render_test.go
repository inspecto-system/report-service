package render

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/config"
	"github.com/inspecto-system/report-service/internal/core"
)

var moscow = func() *time.Location {
	zone, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		panic(err)
	}
	return zone
}()

// sample is an inspection with a repeat comparison, two rooms and a defect with photos; rooms
// repeat to make the report span several pages.
func sample(photoIDs ...uuid.UUID) core.ReportData {
	photos := make([]core.Photo, 0, len(photoIDs))
	for _, id := range photoIDs {
		photos = append(photos, core.Photo{PhotoID: id})
	}
	inspected := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)
	room := func(name string) core.Room {
		checklist := []core.ChecklistItem{
			{Title: "Стены: ровность, трещины, отделка", ElementGroup: "WALLS", Result: "DEFECT"},
			{Title: "Потолок: ровность, следы протечек", ElementGroup: "CEILING", Result: "OK"},
			{Title: "Окна: открывание, уплотнители", ElementGroup: "WINDOWS", Result: "NOT_APPLICABLE"},
			{Title: "Пол: <script>alert(1)</script>", ElementGroup: "FLOOR", Result: "NOT_CHECKED", Comment: "Скол у двери"},
		}
		return core.Room{
			Room:      core.RoomInfo{ID: uuid.New(), Name: name, RoomType: "KITCHEN"},
			Checklist: checklist,
			Defects: []core.RoomDefect{{
				Defect:       core.Defect{ID: uuid.New(), Title: "Трещина у окна", Description: "Длина 30 см", ElementGroup: "WALLS"},
				CategoryName: "Трещины",
				Observation:  core.Observation{Status: "OPEN", Severity: "MAJOR", Comment: "Свежая", Photos: photos},
			}},
		}
	}
	rooms := []core.Room{room("Кухня")}
	for range 6 {
		rooms = append(rooms, room("Спальня"))
	}
	return core.ReportData{
		Property:   core.Property{ID: uuid.New(), Name: "Квартира на Мира", Type: "APARTMENT", Address: "ул. Мира, 5", City: "Казань"},
		Inspection: core.Inspection{ID: uuid.New(), InspectedAt: inspected, Status: "COMPLETED", CompletedAt: inspected.Add(2 * time.Hour)},
		Inspector:  core.Inspector{FullName: "Иван Петров"},
		Rooms:      rooms,
		Summary:    core.Summary{Total: 7, BySeverity: map[string]int{"MAJOR": 7, "CRITICAL": 0, "MINOR": 0, "COSMETIC": 0}},
		Comparison: &core.Comparison{
			Base:    core.ComparedInspection{ID: uuid.New(), InspectedAt: inspected.AddDate(0, -1, 0)},
			Summary: core.StateCounts{New: 1, Remaining: 1},
			Defects: []core.ComparedDefect{
				{Title: "Трещина у окна", State: "REMAINING", Severity: "MAJOR", RoomName: "Кухня"},
				{Title: "Скол плитки", State: "NEW", Reopened: true, Severity: "MINOR", RoomName: "Кухня"},
			},
		},
		GeneratedAt: inspected.Add(3 * time.Hour),
	}
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for x := range 320 {
		for y := range 240 {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHTMLEscapesTextAndEmbedsEverything(t *testing.T) {
	photo := uuid.New()
	html, err := HTML(sample(photo, uuid.New()), map[uuid.UUID][]byte{photo: jpegBytes(t)}, "ru", moscow)
	if err != nil {
		t.Fatalf("HTML: %v", err)
	}
	page := string(html)
	for _, want := range []string{
		"Акт осмотра", "Квартира на Мира", "Иван Петров", "01.11.2026 09:30", "Значительный", "Остался",
		"Новый ↺", "&lt;script&gt;", "data:image/jpeg;base64,", "data:font/ttf;base64,", `font-family: "PT Sans"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	if strings.Contains(page, "<script>") {
		t.Error("user text must be escaped")
	}
	// Only the photo that media-service gave is embedded; the missing one leaves no broken image.
	if n := strings.Count(page, "<img"); n != 7 {
		t.Errorf("images = %d, want one per room", n)
	}
	// Nothing outside the document is referenced (THREAT_MODEL T-12).
	if external := regexp.MustCompile(`(?i)(src|href)\s*=\s*"(?:https?:|//|file:)|url\((?:["']?)(?:https?:|//|file:)`).FindString(page); external != "" {
		t.Errorf("external reference %q", external)
	}
}

func TestHTMLInEnglish(t *testing.T) {
	data := sample()
	data.Comparison = nil
	html, err := HTML(data, nil, "en", time.UTC)
	if err != nil {
		t.Fatalf("HTML: %v", err)
	}
	page := string(html)
	for _, want := range []string{"Inspection report", "2026-11-01 06:30", "Major", "Not applicable"} {
		if !strings.Contains(page, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	if strings.Contains(page, "Comparison with the previous inspection") {
		t.Error("no comparison for a first inspection")
	}
	if LabelsFor("de").Title != "Акт осмотра" {
		t.Error("unknown locales fall back to Russian")
	}
}

func TestGotenbergErrorsAreClassified(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		limit  int64
		want   error
	}{
		"server error":   {http.StatusServiceUnavailable, "busy", 1 << 20, ErrUnavailable},
		"rejected input": {http.StatusBadRequest, "bad html", 1 << 20, ErrRenderFailed},
		"too large":      {http.StatusOK, "%PDF-" + strings.Repeat("x", 100), 50, ErrTooLarge},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseMultipartForm(1 << 20); err != nil || r.MultipartForm.File["files"] == nil {
					t.Errorf("not a multipart conversion request: %v", err)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := NewGotenberg(config.Gotenberg{URL: server.URL}).PDF(context.Background(), []byte("<html></html>"), tc.limit)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	unreachable := NewGotenberg(config.Gotenberg{URL: "http://127.0.0.1:1"})
	if _, err := unreachable.PDF(context.Background(), nil, 1); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unreachable: %v", err)
	}
	if err := unreachable.Ping(context.Background()); err == nil {
		t.Error("Ping must fail")
	}
}

func TestPageCountIgnoresPageTree(t *testing.T) {
	pdf := []byte("1 0 obj << /Type /Pages /Kids [2 0 R 3 0 R] >> 2 0 obj << /Type /Page >> 3 0 obj <</Type/Page/Parent 1 0 R>>")
	if n := PageCount(pdf); n != 2 {
		t.Errorf("PageCount = %d, want 2", n)
	}
}
