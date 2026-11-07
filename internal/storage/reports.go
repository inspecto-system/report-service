package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Report statuses (common.yaml ReportStatus).
const (
	StatusQueued  = "QUEUED"
	StatusRunning = "RUNNING"
	StatusDone    = "DONE"
	StatusFailed  = "FAILED"
)

var (
	// ErrNotFound means there is no such report.
	ErrNotFound = errors.New("report not found")
	// ErrDuplicateIdempotencyKey means the user already requested a report with this key.
	ErrDuplicateIdempotencyKey = errors.New("idempotency key already used")
)

// Report is a row of the reports table.
type Report struct {
	ID             uuid.UUID
	InspectionID   uuid.UUID
	PropertyID     uuid.UUID
	RequestedBy    uuid.UUID
	Locale         string
	Status         string
	Attempts       int
	NextAttemptAt  time.Time
	ErrorCode      *string
	ObjectKey      *string
	PageCount      *int
	SizeBytes      *int64
	IdempotencyKey *string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

// Result is what a finished job stores.
type Result struct {
	ObjectKey string
	PageCount int
	SizeBytes int64
}

const reportColumns = `id, inspection_id, property_id, requested_by, locale, status, attempts,
	next_attempt_at, error_code, object_key, page_count, size_bytes, idempotency_key, created_at,
	started_at, finished_at`

// uniqueViolation is the PostgreSQL error code of a unique constraint violation.
const uniqueViolation = "23505"

// Reports is the repository of report jobs. All queries use placeholders (SECURITY.md §4).
type Reports struct {
	pool *pgxpool.Pool
}

// NewReports returns the repository.
func NewReports(pool *pgxpool.Pool) *Reports {
	return &Reports{pool: pool}
}

// Insert stores a new job.
func (r *Reports) Insert(ctx context.Context, rep Report) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO reports (`+reportColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		rep.ID, rep.InspectionID, rep.PropertyID, rep.RequestedBy, rep.Locale, rep.Status, rep.Attempts,
		rep.NextAttemptAt, rep.ErrorCode, rep.ObjectKey, rep.PageCount, rep.SizeBytes, rep.IdempotencyKey,
		rep.CreatedAt, rep.StartedAt, rep.FinishedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "reports_requester_idempotency_uq" {
		return ErrDuplicateIdempotencyKey
	}
	if err != nil {
		return fmt.Errorf("insert report: %w", err)
	}
	return nil
}

// Get returns a job by id.
func (r *Reports) Get(ctx context.Context, id uuid.UUID) (Report, error) {
	return r.one(ctx, `SELECT `+reportColumns+` FROM reports WHERE id = $1`, id)
}

// FindByIdempotencyKey returns the job the user requested with the key.
func (r *Reports) FindByIdempotencyKey(ctx context.Context, requestedBy uuid.UUID, key string) (Report, error) {
	return r.one(ctx, `SELECT `+reportColumns+` FROM reports WHERE requested_by = $1 AND idempotency_key = $2`,
		requestedBy, key)
}

// ListByInspection returns the jobs of an inspection, newest first.
func (r *Reports) ListByInspection(ctx context.Context, inspectionID uuid.UUID) ([]Report, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+reportColumns+` FROM reports
		WHERE inspection_id = $1 ORDER BY created_at DESC, id DESC`, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	reports, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Report, error) { return scanReport(row) })
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	return reports, nil
}

// ClaimNext marks the oldest due job RUNNING and returns it; ok is false when nothing is due.
// SKIP LOCKED lets several workers and instances claim different jobs (ADR-011).
func (r *Reports) ClaimNext(ctx context.Context, now time.Time) (rep Report, ok bool, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Report{}, false, fmt.Errorf("claim report: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row := tx.QueryRow(ctx, `SELECT `+reportColumns+` FROM reports
		WHERE status = 'QUEUED' AND next_attempt_at <= $1
		ORDER BY next_attempt_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now)
	rep, err = scanReport(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, fmt.Errorf("claim report: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE reports SET status = 'RUNNING', attempts = attempts + 1, started_at = $2
		WHERE id = $1`, rep.ID, now); err != nil {
		return Report{}, false, fmt.Errorf("claim report: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Report{}, false, fmt.Errorf("claim report: %w", err)
	}
	rep.Status, rep.Attempts, rep.StartedAt = StatusRunning, rep.Attempts+1, &now
	return rep, true, nil
}

// Complete stores the result of a finished job.
func (r *Reports) Complete(ctx context.Context, id uuid.UUID, result Result, now time.Time) error {
	return r.exec(ctx, "complete report", `UPDATE reports SET status = 'DONE', object_key = $2, page_count = $3,
		size_bytes = $4, finished_at = $5 WHERE id = $1`, id, result.ObjectKey, result.PageCount, result.SizeBytes, now)
}

// Retry puts a job back into the queue until the given time.
func (r *Reports) Retry(ctx context.Context, id uuid.UUID, at time.Time) error {
	return r.exec(ctx, "retry report", `UPDATE reports SET status = 'QUEUED', next_attempt_at = $2
		WHERE id = $1`, id, at)
}

// Fail ends a job with an error code.
func (r *Reports) Fail(ctx context.Context, id uuid.UUID, code string, now time.Time) error {
	return r.exec(ctx, "fail report", `UPDATE reports SET status = 'FAILED', error_code = $2, finished_at = $3
		WHERE id = $1`, id, code, now)
}

// RequeueStale returns jobs left RUNNING by a stopped instance to the queue; they count as an
// attempt already, so a job that keeps crashing still ends after the last attempt.
func (r *Reports) RequeueStale(ctx context.Context, startedBefore, now time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE reports SET status = 'QUEUED', next_attempt_at = $2
		WHERE status = 'RUNNING' AND started_at < $1`, startedBefore, now)
	if err != nil {
		return 0, fmt.Errorf("requeue stale reports: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Owner selects the reports deleted together: those of a property or of a requesting user.
type Owner struct {
	Column string
	ID     uuid.UUID
}

// ByProperty selects the reports of a property.
func ByProperty(id uuid.UUID) Owner { return Owner{Column: "property_id", ID: id} }

// ByRequester selects the reports requested by a user.
func ByRequester(id uuid.UUID) Owner { return Owner{Column: "requested_by", ID: id} }

// ObjectKeys returns the stored files of the selected reports.
func (r *Reports) ObjectKeys(ctx context.Context, owner Owner) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT object_key FROM reports WHERE `+owner.column()+` = $1
		AND object_key IS NOT NULL`, owner.ID)
	if err != nil {
		return nil, fmt.Errorf("list report files: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list report files: %w", err)
	}
	return keys, nil
}

// Delete removes the selected reports.
func (r *Reports) Delete(ctx context.Context, owner Owner) error {
	return r.exec(ctx, "delete reports", `DELETE FROM reports WHERE `+owner.column()+` = $1`, owner.ID)
}

// column is one of two fixed names, never user input.
func (o Owner) column() string {
	if o.Column == "requested_by" {
		return "requested_by"
	}
	return "property_id"
}

func (r *Reports) exec(ctx context.Context, action, sql string, args ...any) error {
	if _, err := r.pool.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}

func (r *Reports) one(ctx context.Context, sql string, args ...any) (Report, error) {
	rep, err := scanReport(r.pool.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Report{}, ErrNotFound
	}
	if err != nil {
		return Report{}, fmt.Errorf("read report: %w", err)
	}
	return rep, nil
}

func scanReport(row pgx.Row) (Report, error) {
	var rep Report
	err := row.Scan(&rep.ID, &rep.InspectionID, &rep.PropertyID, &rep.RequestedBy, &rep.Locale, &rep.Status,
		&rep.Attempts, &rep.NextAttemptAt, &rep.ErrorCode, &rep.ObjectKey, &rep.PageCount, &rep.SizeBytes,
		&rep.IdempotencyKey, &rep.CreatedAt, &rep.StartedAt, &rep.FinishedAt)
	return rep, err
}
