package core

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/config"
	"github.com/inspecto-system/report-service/internal/testsupport"
)

const reportDataJSON = `{
  "property": {"id": "0193f6a0-0000-7000-8000-000000000001", "name": "Квартира", "type": "APARTMENT",
    "address": "ул. Мира, 5", "city": "Казань", "version": 0},
  "inspection": {"id": "0193f6a0-0000-7000-8000-000000000002", "inspectedAt": "2026-11-01T09:00:00Z",
    "status": "COMPLETED", "completedAt": "2026-11-01T11:00:00Z"},
  "inspector": {"fullName": "Иван Петров"},
  "rooms": [{"room": {"id": "0193f6a0-0000-7000-8000-000000000003", "name": "Кухня", "roomType": "KITCHEN"},
    "checklist": [{"title": "Стены", "elementGroup": "WALLS", "result": "DEFECT"}],
    "defects": [{"defect": {"id": "0193f6a0-0000-7000-8000-000000000004", "title": "Трещина", "elementGroup": "WALLS"},
      "categoryName": "Трещины", "observation": {"status": "OPEN", "severity": "MAJOR",
      "photos": [{"photoId": "0193f6a0-0000-7000-8000-000000000005", "sortOrder": 0}]}}]}],
  "summary": {"total": 1, "bySeverity": {"MAJOR": 1, "MINOR": 0}},
  "comparison": null,
  "generatedAt": "2026-11-07T10:00:00Z",
  "unknownField": true
}`

func TestReportDataIsDecodedWithTheServiceToken(t *testing.T) {
	inspection := uuid.MustParse("0193f6a0-0000-7000-8000-000000000002")
	fake := &fakeCore{expires: 900, access: func(r *http.Request) int {
		if r.URL.Path != "/internal/v1/inspections/"+inspection.String()+"/report-data" ||
			r.URL.Query().Get("locale") != "en" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer token-") {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}}
	fake.body = reportDataJSON
	c, _ := newClient(t, fake)

	data, err := c.ReportData(context.Background(), inspection, "en")
	testsupport.Require(t, err == nil, "ReportData: %v", err)
	testsupport.Expect(t, data.Inspector.FullName == "Иван Петров" && data.Property.City == "Казань", "data = %+v", data)
	testsupport.Expect(t, data.Comparison == nil && !data.Inspection.CompletedAt.IsZero(), "inspection = %+v", data.Inspection)
	photo := data.Rooms[0].Defects[0].Observation.Photos[0]
	testsupport.Expect(t, photo.PhotoID.String() == "0193f6a0-0000-7000-8000-000000000005", "photo = %+v", photo)
	testsupport.Expect(t, data.Summary.BySeverity["MAJOR"] == 1, "summary = %+v", data.Summary)
}

func TestReportDataErrors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   error
	}{
		"unknown inspection": {http.StatusNotFound, "", ErrNotFound},
		"server error":       {http.StatusInternalServerError, "", ErrUnavailable},
		"unreadable body":    {http.StatusOK, "{", ErrUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeCore{expires: 900, access: func(*http.Request) int { return tc.status }, body: tc.body}
			c, _ := newClient(t, fake)
			if _, err := c.ReportData(context.Background(), uuid.New(), "ru"); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReportDataRenewsARejectedToken(t *testing.T) {
	calls := 0
	fake := &fakeCore{expires: 900, body: reportDataJSON, access: func(*http.Request) int {
		calls++
		if calls == 1 {
			return http.StatusUnauthorized
		}
		return http.StatusOK
	}}
	c, _ := newClient(t, fake)

	if _, err := c.ReportData(context.Background(), uuid.New(), "ru"); err != nil {
		t.Fatalf("ReportData: %v", err)
	}
	if fake.tokens.Load() != 2 {
		t.Errorf("tokens = %d, want a new one after 401", fake.tokens.Load())
	}
	unreachable := New(config.Core{URL: "http://127.0.0.1:1", ClientID: "report-service", ClientSecret: "secret"})
	if _, err := unreachable.ReportData(context.Background(), uuid.New(), "ru"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unreachable: %v", err)
	}
}
