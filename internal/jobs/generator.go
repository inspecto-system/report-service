// Package jobs runs the report workers (ADR-011): they claim queued jobs, render the PDF and store
// it, retrying temporary failures with growing delays.
package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/core"
	"github.com/inspecto-system/report-service/internal/media"
	"github.com/inspecto-system/report-service/internal/render"
	"github.com/inspecto-system/report-service/internal/storage"
)

// Error codes of failed jobs (report.yaml ReportJob.errorCode).
const (
	CodeUnavailable = "DEPENDENCY_UNAVAILABLE"
	CodeRender      = "RENDER_FAILED"
	CodeTooLarge    = "REPORT_TOO_LARGE"
	CodeNotFound    = "NOT_FOUND"
)

const (
	pollInterval = 2 * time.Second
	firstRetry   = 10 * time.Second
	// staleCheckInterval is how often jobs left RUNNING by a stopped instance are requeued.
	staleCheckInterval = time.Minute
)

// Queue is the storage of jobs used by the workers.
type Queue interface {
	ClaimNext(ctx context.Context, now time.Time) (storage.Report, bool, error)
	Complete(ctx context.Context, id uuid.UUID, result storage.Result, now time.Time) error
	Retry(ctx context.Context, id uuid.UUID, at time.Time) error
	Fail(ctx context.Context, id uuid.UUID, code string, now time.Time) error
	RequeueStale(ctx context.Context, startedBefore, now time.Time) (int64, error)
}

// Sources give the data, photos and the PDF of a report.
type Sources struct {
	ReportData func(ctx context.Context, inspectionID uuid.UUID, locale string) (core.ReportData, error)
	Photo      func(ctx context.Context, photoID uuid.UUID) ([]byte, error)
	PDF        func(ctx context.Context, html []byte, maxBytes int64) ([]byte, error)
	Store      func(ctx context.Context, key string, pdf []byte) error
}

// Settings bound the work (config.Reports).
type Settings struct {
	Workers       int
	MaxAttempts   int
	MaxSizeBytes  int64
	RenderTimeout time.Duration
	Zone          *time.Location
}

// Generator renders queued reports.
type Generator struct {
	queue    Queue
	sources  Sources
	settings Settings
	logger   *slog.Logger
	now      func() time.Time
}

// NewGenerator returns the workers; Run starts them.
func NewGenerator(queue Queue, sources Sources, settings Settings, logger *slog.Logger) *Generator {
	return &Generator{queue: queue, sources: sources, settings: settings, logger: logger, now: time.Now}
}

// Run starts the workers and the requeueing of stale jobs; it returns when ctx is cancelled.
func (g *Generator) Run(ctx context.Context) {
	done := make(chan struct{})
	for range g.settings.Workers {
		go func() {
			g.work(ctx)
			done <- struct{}{}
		}()
	}
	g.requeueStale(ctx)
	for range g.settings.Workers {
		<-done
	}
}

func (g *Generator) work(ctx context.Context) {
	for ctx.Err() == nil {
		if worked := g.ProcessNext(ctx); !worked {
			sleep(ctx, pollInterval)
		}
	}
}

// ProcessNext handles one due job; false when the queue had none (or could not be read).
func (g *Generator) ProcessNext(ctx context.Context) bool {
	rep, ok, err := g.queue.ClaimNext(ctx, g.now())
	if err != nil {
		g.logger.WarnContext(ctx, "cannot read report queue", slog.Any("error", err))
		return false
	}
	if !ok {
		return false
	}
	attemptCtx, cancel := context.WithTimeout(ctx, g.settings.RenderTimeout)
	defer cancel()
	result, err := g.generate(attemptCtx, rep)
	g.settle(ctx, rep, result, err)
	return true
}

func (g *Generator) generate(ctx context.Context, rep storage.Report) (storage.Result, error) {
	data, err := g.sources.ReportData(ctx, rep.InspectionID, rep.Locale)
	if err != nil {
		return storage.Result{}, err
	}
	photos, err := g.photos(ctx, data)
	if err != nil {
		return storage.Result{}, err
	}
	html, err := render.HTML(data, photos, rep.Locale, g.settings.Zone)
	if err != nil {
		return storage.Result{}, errors.Join(render.ErrRenderFailed, err)
	}
	pdf, err := g.sources.PDF(ctx, html, g.settings.MaxSizeBytes)
	if err != nil {
		return storage.Result{}, err
	}
	key := rep.PropertyID.String() + "/" + rep.ID.String() + ".pdf"
	if err := g.sources.Store(ctx, key, pdf); err != nil {
		return storage.Result{}, errors.Join(render.ErrUnavailable, err)
	}
	return storage.Result{ObjectKey: key, PageCount: max(render.PageCount(pdf), 1), SizeBytes: int64(len(pdf))}, nil
}

// photos loads the previews of every observed defect; a photo removed meanwhile is left out.
func (g *Generator) photos(ctx context.Context, data core.ReportData) (map[uuid.UUID][]byte, error) {
	photos := make(map[uuid.UUID][]byte)
	for _, room := range data.Rooms {
		for _, defect := range room.Defects {
			for _, photo := range defect.Observation.Photos {
				bytes, err := g.sources.Photo(ctx, photo.PhotoID)
				if errors.Is(err, media.ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, err
				}
				photos[photo.PhotoID] = bytes
			}
		}
	}
	return photos, nil
}

// settle stores the outcome: done, retried later, or failed with a code.
func (g *Generator) settle(ctx context.Context, rep storage.Report, result storage.Result, err error) {
	now := g.now()
	var settleErr error
	code := failureCode(err)
	switch {
	case err == nil:
		settleErr = g.queue.Complete(ctx, rep.ID, result, now)
	case code == "" && rep.Attempts < g.settings.MaxAttempts:
		delay := firstRetry << (rep.Attempts - 1)
		g.logger.InfoContext(ctx, "report will be retried", slog.String("report.id", rep.ID.String()),
			slog.Int("attempt", rep.Attempts), slog.Any("error", err))
		settleErr = g.queue.Retry(ctx, rep.ID, now.Add(delay))
	default:
		if code == "" {
			code = CodeUnavailable
		}
		g.logger.WarnContext(ctx, "report failed", slog.String("report.id", rep.ID.String()),
			slog.String("code", code), slog.Any("error", err))
		settleErr = g.queue.Fail(ctx, rep.ID, code, now)
	}
	if settleErr != nil {
		// The job stays RUNNING and is requeued by requeueStale.
		g.logger.ErrorContext(ctx, "cannot store report outcome", slog.Any("error", settleErr))
	}
}

// failureCode is the code of an error that retrying cannot fix, or "" for a temporary one.
func failureCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, render.ErrTooLarge):
		return CodeTooLarge
	case errors.Is(err, render.ErrRenderFailed):
		return CodeRender
	case errors.Is(err, core.ErrNotFound):
		return CodeNotFound
	default:
		return ""
	}
}

func (g *Generator) requeueStale(ctx context.Context) {
	ticker := time.NewTicker(staleCheckInterval)
	defer ticker.Stop()
	for {
		now := g.now()
		if n, err := g.queue.RequeueStale(ctx, now.Add(-2*g.settings.RenderTimeout), now); err != nil {
			g.logger.WarnContext(ctx, "cannot requeue stale reports", slog.Any("error", err))
		} else if n > 0 {
			g.logger.InfoContext(ctx, "stale reports requeued", slog.Int64("count", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
