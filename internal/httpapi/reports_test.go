package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/auth"
	"github.com/inspecto-system/report-service/internal/config"
	"github.com/inspecto-system/report-service/internal/service"
	"github.com/inspecto-system/report-service/internal/storage"
	"github.com/inspecto-system/report-service/internal/testsupport"
)

type fakeReports struct {
	requests []service.Request
	reqErr   error
	job      storage.Report
	err      error
	deleted  []storage.Owner
}

func (f *fakeReports) Request(_ context.Context, req service.Request) (storage.Report, error) {
	f.requests = append(f.requests, req)
	return f.job, f.reqErr
}

func (f *fakeReports) Get(context.Context, uuid.UUID, uuid.UUID) (storage.Report, error) {
	return f.job, f.err
}

func (f *fakeReports) List(context.Context, uuid.UUID, uuid.UUID) ([]storage.Report, error) {
	return []storage.Report{f.job}, f.err
}

func (f *fakeReports) File(context.Context, uuid.UUID, uuid.UUID) (storage.Report, io.ReadCloser, int64, error) {
	return f.job, io.NopCloser(strings.NewReader("%PDF-1.7")), 8, f.err
}

func (f *fakeReports) DeleteAll(_ context.Context, owner storage.Owner) error {
	f.deleted = append(f.deleted, owner)
	return f.err
}

type reportAPI struct {
	router  http.Handler
	reports *fakeReports
	issuer  *testsupport.TokenIssuer
	user    uuid.UUID
}

func newReportAPI(t *testing.T) reportAPI {
	t.Helper()
	issuer := testsupport.NewTokenIssuer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := auth.NewVerifier(ctx, config.Auth{
		JWKSURL: issuer.JWKSURL, Issuer: "inspecto-core", Audience: "inspecto-api", InternalAudience: "inspecto-internal",
	})
	if err != nil {
		t.Fatal(err)
	}
	pages, size := 3, int64(1234)
	reports := &fakeReports{job: storage.Report{
		ID: uuid.New(), InspectionID: uuid.New(), Status: storage.StatusDone, Locale: "ru",
		PageCount: &pages, SizeBytes: &size, CreatedAt: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC),
	}}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Verifier: verifier, Services: verifier, Reports: reports,
	})
	return reportAPI{router: router, reports: reports, issuer: issuer, user: uuid.New()}
}

func (a reportAPI) send(t *testing.T, method, path, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.issuer.Token(t, a.user, "INSPECTOR"))
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return rec
}

func TestRequestReportReturnsAcceptedJob(t *testing.T) {
	api := newReportAPI(t)
	inspection := uuid.New()

	rec := api.send(t, http.MethodPost, "/api/v1/reports", `{"inspectionId":"`+inspection.String()+`","locale":"en"}`,
		http.Header{"Idempotency-Key": {"request-1234"}})

	if rec.Code != http.StatusAccepted || rec.Header().Get("Location") != "/api/v1/reports/"+api.reports.job.ID.String() {
		t.Fatalf("status %d location %q body %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	var job map[string]any
	decode(t, rec, &job)
	if job["status"] != "DONE" || job["pageCount"] != float64(3) {
		t.Errorf("job = %v", job)
	}
	want := service.Request{UserID: api.user, InspectionID: inspection, Locale: "en", IdempotencyKey: "request-1234"}
	if len(api.reports.requests) != 1 || api.reports.requests[0] != want {
		t.Errorf("requests = %+v", api.reports.requests)
	}
}

func TestRequestReportValidatesInput(t *testing.T) {
	cases := map[string]struct{ body, key, field string }{
		"no inspection":   {`{}`, "", "inspectionId"},
		"bad locale":      {`{"inspectionId":"` + uuid.NewString() + `","locale":"de"}`, "", "locale"},
		"unknown field":   {`{"inspectionId":"` + uuid.NewString() + `","x":1}`, "", "body"},
		"short key":       {`{"inspectionId":"` + uuid.NewString() + `"}`, "short", "Idempotency-Key"},
		"not json at all": {`nope`, "", "body"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newReportAPI(t)
			header := http.Header{}
			if tc.key != "" {
				header.Set("Idempotency-Key", tc.key)
			}
			rec := api.send(t, http.MethodPost, "/api/v1/reports", tc.body, header)
			var problem Problem
			decode(t, rec, &problem)
			if rec.Code != http.StatusBadRequest || len(problem.Errors) == 0 || problem.Errors[0].Field != tc.field {
				t.Errorf("status %d problem %+v", rec.Code, problem)
			}
			if len(api.reports.requests) != 0 {
				t.Error("an invalid request must not reach the service")
			}
		})
	}
}

func TestServiceErrorsBecomeProblems(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		code   Code
	}{
		"not found":     {service.ErrNotFound, http.StatusNotFound, CodeNotFound},
		"not completed": {service.ErrNotCompleted, http.StatusConflict, CodeInvalidState},
		"not ready":     {service.ErrNotReady, http.StatusConflict, CodeReportNotReady},
		"dependency":    {service.ErrUnavailable, http.StatusServiceUnavailable, CodeDependencyDown},
		"unexpected":    {io.ErrUnexpectedEOF, http.StatusInternalServerError, CodeInternal},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newReportAPI(t)
			api.reports.err = tc.err
			rec := api.send(t, http.MethodGet, "/api/v1/reports/"+uuid.NewString()+"/file", "", nil)
			var problem Problem
			decode(t, rec, &problem)
			if rec.Code != tc.status || problem.Code != tc.code {
				t.Errorf("status %d code %s", rec.Code, problem.Code)
			}
		})
	}
}

func TestReadingReports(t *testing.T) {
	api := newReportAPI(t)

	file := api.send(t, http.MethodGet, "/api/v1/reports/"+api.reports.job.ID.String()+"/file", "", nil)
	testsupport.Expect(t, file.Code == http.StatusOK && file.Body.String() == "%PDF-1.7", "file: %d %s", file.Code, file.Body)
	testsupport.Expect(t, file.Header().Get("Content-Type") == "application/pdf" &&
		strings.HasPrefix(file.Header().Get("Content-Disposition"), "attachment;") &&
		file.Header().Get("Cache-Control") == "private, no-store", "file headers: %v", file.Header())

	rec := api.send(t, http.MethodGet, "/api/v1/reports/"+api.reports.job.ID.String(), "", nil)
	testsupport.Expect(t, rec.Code == http.StatusOK, "get: %d", rec.Code)
	rec = api.send(t, http.MethodGet, "/api/v1/reports?inspectionId="+uuid.NewString(), "", nil)
	testsupport.Expect(t, rec.Code == http.StatusOK && strings.HasPrefix(rec.Body.String(), "["), "list: %d %s", rec.Code, rec.Body)
	rec = api.send(t, http.MethodGet, "/api/v1/reports?inspectionId=nope", "", nil)
	testsupport.Expect(t, rec.Code == http.StatusBadRequest, "bad list query: %d", rec.Code)
	rec = api.send(t, http.MethodGet, "/api/v1/reports/nope", "", nil)
	testsupport.Expect(t, rec.Code == http.StatusNotFound, "bad id: %d", rec.Code)
}

func TestReportsNeedAUserToken(t *testing.T) {
	api := newReportAPI(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/reports/"+uuid.NewString(), nil)
	rec := httptest.NewRecorder()
	api.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", rec.Code)
	}
}

func TestDeleteInBulkNeedsTheScope(t *testing.T) {
	api := newReportAPI(t)
	property, user := uuid.New(), uuid.New()
	internal := func(token, query string) int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/internal/v1/reports?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		api.router.ServeHTTP(rec, req)
		return rec.Code
	}
	manage := api.issuer.ServiceToken(t, "core-service", ScopeReportsManage)

	if code := internal(manage, "propertyId="+property.String()); code != http.StatusNoContent {
		t.Errorf("by property: %d", code)
	}
	if code := internal(manage, "requestedBy="+user.String()); code != http.StatusNoContent {
		t.Errorf("by requester: %d", code)
	}
	want := []storage.Owner{storage.ByProperty(property), storage.ByRequester(user)}
	if len(api.reports.deleted) != 2 || api.reports.deleted[0] != want[0] || api.reports.deleted[1] != want[1] {
		t.Errorf("deleted = %+v", api.reports.deleted)
	}
	for query, want := range map[string]int{"": http.StatusBadRequest, "propertyId=x": http.StatusBadRequest,
		"propertyId=" + property.String() + "&requestedBy=" + user.String(): http.StatusBadRequest} {
		if code := internal(manage, query); code != want {
			t.Errorf("query %q: %d", query, code)
		}
	}
	if code := internal(api.issuer.ServiceToken(t, "media-service", "photos.read"), "propertyId="+property.String()); code != http.StatusForbidden {
		t.Errorf("wrong scope: %d", code)
	}
	if code := internal(api.issuer.Token(t, user, "INSPECTOR"), "propertyId="+property.String()); code != http.StatusUnauthorized {
		t.Errorf("user token: %d", code)
	}
}
