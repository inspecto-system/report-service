// Package service holds the rules of report jobs: who may request and read them (access checks
// through core-service, SECURITY.md §3), idempotent requests and deletion (ADR-018).
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/core"
	"github.com/inspecto-system/report-service/internal/storage"
)

var (
	// ErrNotFound covers a missing report or inspection and one the user may not see (404).
	ErrNotFound = errors.New("not found")
	// ErrNotCompleted means only a completed inspection can be reported (409 INVALID_STATE_TRANSITION).
	ErrNotCompleted = errors.New("inspection is not completed")
	// ErrNotReady means the PDF is not generated yet (409 REPORT_NOT_READY).
	ErrNotReady = errors.New("report not ready")
	// ErrUnavailable means a dependency could not answer (503 DEPENDENCY_UNAVAILABLE).
	ErrUnavailable = errors.New("dependency unavailable")
)

// Core is the part of core-service the rules need.
type Core interface {
	ReportData(ctx context.Context, inspectionID uuid.UUID, locale string) (core.ReportData, error)
	CanAccess(ctx context.Context, propertyID, userID uuid.UUID, action core.Action) (bool, error)
}

// Files is the storage of the PDFs.
type Files interface {
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Delete(ctx context.Context, keys ...string) error
}

// Repository is the storage of report jobs.
type Repository interface {
	Insert(ctx context.Context, rep storage.Report) error
	Get(ctx context.Context, id uuid.UUID) (storage.Report, error)
	FindByIdempotencyKey(ctx context.Context, requestedBy uuid.UUID, key string) (storage.Report, error)
	ListByInspection(ctx context.Context, inspectionID uuid.UUID) ([]storage.Report, error)
	ObjectKeys(ctx context.Context, owner storage.Owner) ([]string, error)
	Delete(ctx context.Context, owner storage.Owner) error
}

// Request is a user's request of a report.
type Request struct {
	UserID         uuid.UUID
	InspectionID   uuid.UUID
	Locale         string
	IdempotencyKey string
}

// Reports applies the rules of report jobs.
type Reports struct {
	repo  Repository
	files Files
	core  Core
	now   func() time.Time
}

// NewReports returns the service.
func NewReports(repo Repository, files Files, coreAPI Core) *Reports {
	return &Reports{repo: repo, files: files, core: coreAPI, now: time.Now}
}

// Request creates a QUEUED job for a completed inspection the user may read; a repeated request
// with the same idempotency key returns the first job.
func (s *Reports) Request(ctx context.Context, req Request) (storage.Report, error) {
	if req.IdempotencyKey != "" {
		if earlier, err := s.repo.FindByIdempotencyKey(ctx, req.UserID, req.IdempotencyKey); err == nil {
			return earlier, nil
		} else if !errors.Is(err, storage.ErrNotFound) {
			return storage.Report{}, err
		}
	}
	data, err := s.core.ReportData(ctx, req.InspectionID, req.Locale)
	if err != nil {
		return storage.Report{}, dependency(err)
	}
	if err := s.requireAccess(ctx, data.Property.ID, req.UserID); err != nil {
		return storage.Report{}, err
	}
	if data.Inspection.Status != "COMPLETED" {
		return storage.Report{}, ErrNotCompleted
	}
	now := s.now()
	rep := storage.Report{
		ID: uuid.Must(uuid.NewV7()), InspectionID: req.InspectionID, PropertyID: data.Property.ID,
		RequestedBy: req.UserID, Locale: req.Locale, Status: storage.StatusQueued,
		NextAttemptAt: now, CreatedAt: now,
	}
	if req.IdempotencyKey != "" {
		rep.IdempotencyKey = &req.IdempotencyKey
	}
	err = s.repo.Insert(ctx, rep)
	if errors.Is(err, storage.ErrDuplicateIdempotencyKey) {
		// A concurrent repetition won; return its job.
		return s.repo.FindByIdempotencyKey(ctx, req.UserID, req.IdempotencyKey)
	}
	if err != nil {
		return storage.Report{}, err
	}
	return rep, nil
}

// Get returns a job of a property the user may read.
func (s *Reports) Get(ctx context.Context, userID, reportID uuid.UUID) (storage.Report, error) {
	rep, err := s.repo.Get(ctx, reportID)
	if errors.Is(err, storage.ErrNotFound) {
		return storage.Report{}, ErrNotFound
	}
	if err != nil {
		return storage.Report{}, err
	}
	if err := s.requireAccess(ctx, rep.PropertyID, userID); err != nil {
		return storage.Report{}, err
	}
	return rep, nil
}

// List returns the jobs of an inspection, newest first. Without jobs there is nothing to protect,
// so the list is empty without asking core-service.
func (s *Reports) List(ctx context.Context, userID, inspectionID uuid.UUID) ([]storage.Report, error) {
	reports, err := s.repo.ListByInspection(ctx, inspectionID)
	if err != nil || len(reports) == 0 {
		return reports, err
	}
	if err := s.requireAccess(ctx, reports[0].PropertyID, userID); err != nil {
		return nil, err
	}
	return reports, nil
}

// File opens the PDF of a finished job; the caller closes it.
func (s *Reports) File(ctx context.Context, userID, reportID uuid.UUID) (storage.Report, io.ReadCloser, int64, error) {
	rep, err := s.Get(ctx, userID, reportID)
	if err != nil {
		return storage.Report{}, nil, 0, err
	}
	if rep.Status != storage.StatusDone || rep.ObjectKey == nil {
		return storage.Report{}, nil, 0, ErrNotReady
	}
	body, size, err := s.files.Get(ctx, *rep.ObjectKey)
	if err != nil {
		return storage.Report{}, nil, 0, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return rep, body, size, nil
}

// DeleteAll removes the PDFs and then the jobs of a property or of a user; a retry after a partial
// failure finds the remaining rows and deletes them (ADR-018).
func (s *Reports) DeleteAll(ctx context.Context, owner storage.Owner) error {
	keys, err := s.repo.ObjectKeys(ctx, owner)
	if err != nil {
		return err
	}
	if err := s.files.Delete(ctx, keys...); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return s.repo.Delete(ctx, owner)
}

func (s *Reports) requireAccess(ctx context.Context, propertyID, userID uuid.UUID) error {
	allowed, err := s.core.CanAccess(ctx, propertyID, userID, core.Read)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if !allowed {
		return ErrNotFound
	}
	return nil
}

func dependency(err error) error {
	if errors.Is(err, core.ErrNotFound) {
		return ErrNotFound
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
