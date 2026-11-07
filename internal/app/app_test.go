package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/config"
	"github.com/inspecto-system/report-service/internal/testsupport"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return listener
}

func send(t *testing.T, req *http.Request) (int, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

func newRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRunGeneratesReportAndStopsGracefully(t *testing.T) {
	owner, property, inspection, photo := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	issuer := testsupport.NewTokenIssuer(t)
	cfg := config.Config{
		Database:  testsupport.StartPostgres(t).Config,
		Auth:      config.Auth{JWKSURL: issuer.JWKSURL, Issuer: "inspecto-core", Audience: "inspecto-api", InternalAudience: "inspecto-internal"},
		Core:      config.Core{URL: fakeCore(t, owner, property, inspection, photo), ClientID: "report-service", ClientSecret: "secret"},
		Media:     config.Media{URL: fakeMedia(t, photo)},
		Storage:   testsupport.StartSeaweedFS(t),
		Gotenberg: testsupport.StartGotenberg(t),
		Reports:   config.Reports{MaxSizeBytes: 20 << 20, Workers: 1, MaxAttempts: 3, RenderTimeout: time.Minute, TimeZone: "Europe/Moscow"},
	}
	listener := listen(t)
	base := "http://" + listener.Addr().String()
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- Run(ctx, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), listener) }()

	waitReady(t, base, &logs)

	if status, _ := send(t, newRequest(t, http.MethodGet, base+"/health/live", nil)); status != http.StatusOK {
		t.Errorf("live: %d", status)
	}
	token := "Bearer " + issuer.Token(t, owner, "INSPECTOR")
	reportID := requestReport(t, base, token, inspection)
	waitDone(t, base, token, reportID, &logs)
	download := newRequest(t, http.MethodGet, base+"/api/v1/reports/"+reportID+"/file", nil)
	download.Header.Set("Authorization", token)
	if status, pdf := send(t, download); status != http.StatusOK || !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Errorf("download: %d %.20q", status, pdf)
	}
	stranger := newRequest(t, http.MethodGet, base+"/api/v1/reports/"+reportID, nil)
	stranger.Header.Set("Authorization", "Bearer "+issuer.Token(t, uuid.New(), "INSPECTOR"))
	if status, _ := send(t, stranger); status != http.StatusNotFound {
		t.Errorf("stranger got %d, want 404", status)
	}

	cancel()
	waitStopped(t, done, &logs)
}

func waitReady(t *testing.T, base string, logs *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if status, _ := send(t, newRequest(t, http.MethodGet, base+"/health/ready", nil)); status == http.StatusOK {
			return
		}
	}
	t.Fatalf("not ready; logs: %s", logs.String())
}

func waitStopped(t *testing.T, done <-chan error, logs *bytes.Buffer) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if !strings.Contains(logs.String(), "report-service stopped") {
		t.Errorf("no stop record in logs: %s", logs.String())
	}
}

func TestRunFailsWithoutDatabaseAndReleasesListener(t *testing.T) {
	listener := listen(t)
	cfg := config.Config{Database: config.Database{
		URL:      "postgres://127.0.0.1:1/inspecto_report?sslmode=disable&connect_timeout=1",
		App:      config.Credentials{User: "report_app", Password: "x"},
		Migrator: config.Credentials{User: "report_migrator", Password: "x"},
	}}

	err := Run(context.Background(), cfg, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), listener)

	if err == nil || !strings.Contains(err.Error(), "migrations") {
		t.Fatalf("err = %v", err)
	}
	if _, acceptErr := listener.Accept(); !errors.Is(acceptErr, net.ErrClosed) {
		t.Errorf("listener still open: %v", acceptErr)
	}
}

// fakeCore answers service token requests, allows the owner on the property and gives the data of
// a completed inspection with one photo.
func fakeCore(t *testing.T, owner, property, inspection, photo uuid.UUID) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/internal/v1/auth/service-token":
			_, _ = io.WriteString(w, `{"access_token":"service","token_type":"Bearer","expires_in":900}`)
		case r.URL.Path == "/internal/v1/access/properties/"+property.String() && r.URL.Query().Get("userId") == owner.String():
			_, _ = io.WriteString(w, `{"allowed":true}`)
		case r.URL.Path == "/internal/v1/inspections/"+inspection.String()+"/report-data":
			_, _ = fmt.Fprintf(w, `{"property":{"id":%q,"name":"Квартира","type":"APARTMENT","address":"ул. Мира, 5"},
				"inspection":{"id":%q,"inspectedAt":"2026-11-01T09:00:00Z","status":"COMPLETED"},
				"inspector":{"fullName":"Иван Петров"},
				"rooms":[{"room":{"name":"Кухня"},"checklist":[{"title":"Стены","result":"DEFECT"}],
				  "defects":[{"defect":{"title":"Трещина"},"categoryName":"Трещины",
				    "observation":{"status":"OPEN","severity":"MAJOR","photos":[{"photoId":%q}]}}]}],
				"summary":{"total":1,"bySeverity":{"MAJOR":1}},"generatedAt":"2026-11-07T10:00:00Z"}`,
				property, inspection, photo)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// fakeMedia serves the preview of the photo.
func fakeMedia(t *testing.T, photo uuid.UUID) string {
	t.Helper()
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/photos/"+photo.String()+"/content" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(jpegData.Bytes())
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func requestReport(t *testing.T, base, token string, inspection uuid.UUID) string {
	t.Helper()
	req := newRequest(t, http.MethodPost, base+"/api/v1/reports", strings.NewReader(`{"inspectionId":"`+inspection.String()+`"}`))
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	status, body := send(t, req)
	var job struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &job); status != http.StatusAccepted || err != nil {
		t.Fatalf("request report: %d %s", status, body)
	}
	return job.ID
}

func waitDone(t *testing.T, base, token, reportID string, logs *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		req := newRequest(t, http.MethodGet, base+"/api/v1/reports/"+reportID, nil)
		req.Header.Set("Authorization", token)
		_, body := send(t, req)
		var job struct {
			Status    string `json:"status"`
			PageCount int    `json:"pageCount"`
		}
		_ = json.Unmarshal(body, &job)
		if job.Status == "DONE" && job.PageCount > 0 {
			return
		}
		if job.Status == "FAILED" {
			t.Fatalf("report failed: %s; logs: %s", body, logs.String())
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("report not done; logs: %s", logs.String())
}
