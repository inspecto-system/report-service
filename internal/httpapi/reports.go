package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/auth"
	"github.com/inspecto-system/report-service/internal/service"
	"github.com/inspecto-system/report-service/internal/storage"
)

const (
	maxRequestBytes = 4 << 10
	minKeyLength    = 8
	maxKeyLength    = 64
)

// ScopeReportsManage is the scope of DELETE /internal/v1/reports (core-service, ADR-018).
const ScopeReportsManage = "reports.manage"

// ReportService is the business logic behind /api/v1/reports.
type ReportService interface {
	Request(ctx context.Context, req service.Request) (storage.Report, error)
	Get(ctx context.Context, userID, reportID uuid.UUID) (storage.Report, error)
	List(ctx context.Context, userID, inspectionID uuid.UUID) ([]storage.Report, error)
	File(ctx context.Context, userID, reportID uuid.UUID) (storage.Report, io.ReadCloser, int64, error)
	DeleteAll(ctx context.Context, owner storage.Owner) error
}

// ServiceVerifier checks service tokens of /internal/**.
type ServiceVerifier interface {
	VerifyService(token string) (auth.Service, error)
}

// reportJob is ReportJob of report.yaml.
type reportJob struct {
	ID           uuid.UUID  `json:"id"`
	InspectionID uuid.UUID  `json:"inspectionId"`
	Status       string     `json:"status"`
	Locale       string     `json:"locale"`
	PageCount    *int       `json:"pageCount"`
	SizeBytes    *int64     `json:"sizeBytes"`
	ErrorCode    *string    `json:"errorCode"`
	CreatedAt    time.Time  `json:"createdAt"`
	FinishedAt   *time.Time `json:"finishedAt"`
}

func jobOf(rep storage.Report) reportJob {
	return reportJob{
		ID: rep.ID, InspectionID: rep.InspectionID, Status: rep.Status, Locale: rep.Locale,
		PageCount: rep.PageCount, SizeBytes: rep.SizeBytes, ErrorCode: rep.ErrorCode,
		CreatedAt: rep.CreatedAt, FinishedAt: rep.FinishedAt,
	}
}

type reportHandlers struct {
	reports ReportService
	logger  *slog.Logger
}

// request handles POST /api/v1/reports: 202 with the job and its Location.
func (h reportHandlers) request(w http.ResponseWriter, r *http.Request) {
	var body struct {
		InspectionID *uuid.UUID `json:"inspectionId"`
		Locale       string     `json:"locale"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeInvalid(w, r, FieldViolation{Field: "body", Code: "INVALID_FORMAT"})
		return
	}
	if body.Locale == "" {
		body.Locale = "ru"
	}
	key := r.Header.Get("Idempotency-Key")
	if violations := invalidFields(body.InspectionID, body.Locale, key); len(violations) > 0 {
		writeInvalid(w, r, violations...)
		return
	}
	rep, err := h.reports.Request(r.Context(), service.Request{
		UserID: userFrom(r.Context()).ID, InspectionID: *body.InspectionID, Locale: body.Locale, IdempotencyKey: key,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/reports/"+rep.ID.String())
	writeJSON(w, http.StatusAccepted, jobOf(rep))
}

// invalidFields checks a report request; the locale already has its default.
func invalidFields(inspectionID *uuid.UUID, locale, key string) []FieldViolation {
	var violations []FieldViolation
	if inspectionID == nil {
		violations = append(violations, FieldViolation{Field: "inspectionId", Code: "NOT_BLANK"})
	}
	if locale != "ru" && locale != "en" {
		violations = append(violations, FieldViolation{Field: "locale", Code: "INVALID_FORMAT"})
	}
	if key != "" && (len(key) < minKeyLength || len(key) > maxKeyLength) {
		violations = append(violations, FieldViolation{Field: "Idempotency-Key", Code: "INVALID_LENGTH"})
	}
	return violations
}

// list handles GET /api/v1/reports?inspectionId=…
func (h reportHandlers) list(w http.ResponseWriter, r *http.Request) {
	inspectionID, err := uuid.Parse(r.URL.Query().Get("inspectionId"))
	if err != nil {
		writeInvalid(w, r, FieldViolation{Field: "inspectionId", Code: "INVALID_FORMAT"})
		return
	}
	reports, err := h.reports.List(r.Context(), userFrom(r.Context()).ID, inspectionID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	jobs := make([]reportJob, 0, len(reports))
	for _, rep := range reports {
		jobs = append(jobs, jobOf(rep))
	}
	writeJSON(w, http.StatusOK, jobs)
}

// get handles GET /api/v1/reports/{reportId}.
func (h reportHandlers) get(w http.ResponseWriter, r *http.Request) {
	id, ok := reportID(w, r)
	if !ok {
		return
	}
	rep, err := h.reports.Get(r.Context(), userFrom(r.Context()).ID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, jobOf(rep))
}

// file handles GET /api/v1/reports/{reportId}/file: the PDF as an attachment (SECURITY.md §7).
func (h reportHandlers) file(w http.ResponseWriter, r *http.Request) {
	id, ok := reportID(w, r)
	if !ok {
		return
	}
	rep, body, size, err := h.reports.File(r.Context(), userFrom(r.Context()).ID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="inspection-report-`+rep.InspectionID.String()+`.pdf"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		h.logger.WarnContext(r.Context(), "report download interrupted", slog.Any("error", err))
	}
}

// deleteInBulk handles DELETE /internal/v1/reports?propertyId=…|requestedBy=… (exactly one).
func (h reportHandlers) deleteInBulk(w http.ResponseWriter, r *http.Request) {
	property, requester := r.URL.Query().Get("propertyId"), r.URL.Query().Get("requestedBy")
	field, raw, selector := "propertyId", property, storage.ByProperty
	if requester != "" {
		field, raw, selector = "requestedBy", requester, storage.ByRequester
	}
	id, err := uuid.Parse(raw)
	if (property == "") == (requester == "") || err != nil {
		writeInvalid(w, r, FieldViolation{Field: field, Code: "INVALID_FORMAT"})
		return
	}
	if err := h.reports.DeleteAll(r.Context(), selector(id)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h reportHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		WriteProblem(w, r, http.StatusNotFound, CodeNotFound, "Resource not found")
	case errors.Is(err, service.ErrNotCompleted):
		WriteProblem(w, r, http.StatusConflict, CodeInvalidState, "Only a completed inspection can be reported")
	case errors.Is(err, service.ErrNotReady):
		WriteProblem(w, r, http.StatusConflict, CodeReportNotReady, "The report is not ready yet")
	case errors.Is(err, service.ErrUnavailable):
		h.logger.WarnContext(r.Context(), "dependency unavailable", slog.Any("error", err))
		WriteProblem(w, r, http.StatusServiceUnavailable, CodeDependencyDown, "A dependency is unavailable, retry later")
	default:
		h.logger.ErrorContext(r.Context(), "report request failed", slog.Any("error", err))
		WriteProblem(w, r, http.StatusInternalServerError, CodeInternal, "Unexpected error")
	}
}

func reportID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "reportId")))
	if err != nil {
		WriteProblem(w, r, http.StatusNotFound, CodeNotFound, "Resource not found")
		return uuid.Nil, false
	}
	return id, true
}

// serviceAuthenticated admits only service tokens with the given scope (SECURITY.md §8).
func serviceAuthenticated(verifier ServiceVerifier, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				unauthenticated(w, r, CodeUnauthenticated)
				return
			}
			caller, err := verifier.VerifyService(token)
			if errors.Is(err, auth.ErrExpiredToken) {
				unauthenticated(w, r, CodeTokenExpired)
				return
			}
			if err != nil {
				unauthenticated(w, r, CodeUnauthenticated)
				return
			}
			if !caller.Has(scope) {
				WriteProblem(w, r, http.StatusForbidden, CodeForbidden, "The service token lacks the scope "+scope)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
