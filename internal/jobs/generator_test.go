package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/core"
	"github.com/inspecto-system/report-service/internal/media"
	"github.com/inspecto-system/report-service/internal/render"
	"github.com/inspecto-system/report-service/internal/storage"
)

type outcome struct {
	result storage.Result
	retry  time.Time
	code   string
}

type fakeQueue struct {
	jobs     []storage.Report
	outcomes map[uuid.UUID]outcome
	stale    int
}

func (q *fakeQueue) ClaimNext(context.Context, time.Time) (storage.Report, bool, error) {
	if len(q.jobs) == 0 {
		return storage.Report{}, false, nil
	}
	job := q.jobs[0]
	q.jobs = q.jobs[1:]
	return job, true, nil
}

func (q *fakeQueue) Complete(_ context.Context, id uuid.UUID, result storage.Result, _ time.Time) error {
	q.outcomes[id] = outcome{result: result}
	return nil
}

func (q *fakeQueue) Retry(_ context.Context, id uuid.UUID, at time.Time) error {
	q.outcomes[id] = outcome{retry: at}
	return nil
}

func (q *fakeQueue) Fail(_ context.Context, id uuid.UUID, code string, _ time.Time) error {
	q.outcomes[id] = outcome{code: code}
	return nil
}

func (q *fakeQueue) RequeueStale(context.Context, time.Time, time.Time) (int64, error) {
	q.stale++
	return 0, nil
}

var now = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func data(photos ...uuid.UUID) core.ReportData {
	refs := make([]core.Photo, 0, len(photos))
	for _, id := range photos {
		refs = append(refs, core.Photo{PhotoID: id})
	}
	return core.ReportData{
		Property:   core.Property{ID: uuid.New(), Name: "Дом"},
		Inspection: core.Inspection{ID: uuid.New(), Status: "COMPLETED"},
		Rooms: []core.Room{{
			Room:    core.RoomInfo{Name: "Кухня"},
			Defects: []core.RoomDefect{{Defect: core.Defect{Title: "Трещина"}, Observation: core.Observation{Photos: refs}}},
		}},
	}
}

type harness struct {
	gen    *Generator
	queue  *fakeQueue
	stored map[string][]byte
	html   []byte
}

func newHarness(sources Sources) *harness {
	h := &harness{queue: &fakeQueue{outcomes: map[uuid.UUID]outcome{}}, stored: map[string][]byte{}}
	if sources.Store == nil {
		sources.Store = func(_ context.Context, key string, pdf []byte) error {
			h.stored[key] = pdf
			return nil
		}
	}
	if sources.PDF == nil {
		sources.PDF = func(_ context.Context, html []byte, _ int64) ([]byte, error) {
			h.html = html
			return []byte("%PDF-1.7 << /Type /Page >> << /Type /Page >>"), nil
		}
	}
	h.gen = NewGenerator(h.queue, sources, Settings{
		Workers: 1, MaxAttempts: 3, MaxSizeBytes: 1 << 20, RenderTimeout: time.Minute, Zone: time.UTC,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.gen.now = func() time.Time { return now }
	return h
}

func job(attempts int) storage.Report {
	return storage.Report{ID: uuid.New(), InspectionID: uuid.New(), PropertyID: uuid.New(), Locale: "ru", Attempts: attempts}
}

func TestReportIsRenderedWithAvailablePhotos(t *testing.T) {
	kept, gone := uuid.New(), uuid.New()
	h := newHarness(Sources{
		ReportData: func(context.Context, uuid.UUID, string) (core.ReportData, error) { return data(kept, gone), nil },
		Photo: func(_ context.Context, id uuid.UUID) ([]byte, error) {
			if id == gone {
				return nil, media.ErrNotFound
			}
			return []byte{0xFF, 0xD8, 0xFF}, nil
		},
	})
	rep := job(1)
	h.queue.jobs = []storage.Report{rep}

	if !h.gen.ProcessNext(context.Background()) {
		t.Fatal("a job was due")
	}

	got := h.queue.outcomes[rep.ID].result
	wantKey := rep.PropertyID.String() + "/" + rep.ID.String() + ".pdf"
	if got.ObjectKey != wantKey || got.PageCount != 2 || got.SizeBytes == 0 || h.stored[wantKey] == nil {
		t.Errorf("result = %+v", got)
	}
	if strings.Count(string(h.html), "data:image/jpeg") != 1 {
		t.Error("the available photo must be embedded, the deleted one left out")
	}
	if h.gen.ProcessNext(context.Background()) {
		t.Error("the queue is empty now")
	}
}

func TestFailuresAreRetriedOrEnded(t *testing.T) {
	unavailable := errors.Join(core.ErrUnavailable, errors.New("connection refused"))
	cases := map[string]struct {
		attempts int
		pdfErr   error
		dataErr  error
		storeErr error
		wantCode string
		wantWait time.Duration
	}{
		"first failure is retried in 10 s": {attempts: 1, dataErr: unavailable, wantWait: 10 * time.Second},
		"second failure waits longer":      {attempts: 2, pdfErr: render.ErrUnavailable, wantWait: 20 * time.Second},
		"last attempt fails the job":       {attempts: 3, dataErr: unavailable, wantCode: CodeUnavailable},
		"storage down is temporary":        {attempts: 1, storeErr: errors.New("s3 down"), wantWait: 10 * time.Second},
		"too large is final":               {attempts: 1, pdfErr: render.ErrTooLarge, wantCode: CodeTooLarge},
		"rejected HTML is final":           {attempts: 1, pdfErr: render.ErrRenderFailed, wantCode: CodeRender},
		"deleted inspection is final":      {attempts: 1, dataErr: core.ErrNotFound, wantCode: CodeNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sources := Sources{
				ReportData: func(context.Context, uuid.UUID, string) (core.ReportData, error) { return data(), tc.dataErr },
				Photo:      func(context.Context, uuid.UUID) ([]byte, error) { return nil, nil },
				PDF:        func(context.Context, []byte, int64) ([]byte, error) { return []byte("%PDF-"), tc.pdfErr },
			}
			if tc.storeErr != nil {
				sources.Store = func(context.Context, string, []byte) error { return tc.storeErr }
			}
			h := newHarness(sources)
			rep := job(tc.attempts)
			h.queue.jobs = []storage.Report{rep}

			h.gen.ProcessNext(context.Background())

			got := h.queue.outcomes[rep.ID]
			if got.code != tc.wantCode || (tc.wantWait > 0 && !got.retry.Equal(now.Add(tc.wantWait))) {
				t.Errorf("outcome = %+v", got)
			}
		})
	}
}

func TestPhotoOutageIsRetried(t *testing.T) {
	h := newHarness(Sources{
		ReportData: func(context.Context, uuid.UUID, string) (core.ReportData, error) { return data(uuid.New()), nil },
		Photo:      func(context.Context, uuid.UUID) ([]byte, error) { return nil, media.ErrUnavailable },
	})
	rep := job(1)
	h.queue.jobs = []storage.Report{rep}

	h.gen.ProcessNext(context.Background())

	if h.queue.outcomes[rep.ID].retry.IsZero() {
		t.Errorf("outcome = %+v", h.queue.outcomes[rep.ID])
	}
}

func TestRunStopsWithContext(t *testing.T) {
	var logs bytes.Buffer
	h := newHarness(Sources{})
	h.gen.logger = slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.gen.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if h.queue.stale == 0 {
		t.Error("stale jobs must be requeued at start")
	}
}
