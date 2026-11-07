package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/core"
	"github.com/inspecto-system/report-service/internal/storage"
	"github.com/inspecto-system/report-service/internal/testsupport"
)

type fakeCore struct {
	data    core.ReportData
	dataErr error
	allowed map[uuid.UUID]bool
	err     error
}

func (f *fakeCore) ReportData(context.Context, uuid.UUID, string) (core.ReportData, error) {
	return f.data, f.dataErr
}

func (f *fakeCore) CanAccess(_ context.Context, _, user uuid.UUID, action core.Action) (bool, error) {
	if action != core.Read {
		return false, errors.New("reports need read access only")
	}
	return f.allowed[user], f.err
}

type fakeRepo struct {
	rows      map[uuid.UUID]storage.Report
	deleted   []storage.Owner
	keys      []string
	insertErr error
}

func (f *fakeRepo) Insert(_ context.Context, rep storage.Report) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.rows[rep.ID] = rep
	return nil
}

func (f *fakeRepo) Get(_ context.Context, id uuid.UUID) (storage.Report, error) {
	if rep, ok := f.rows[id]; ok {
		return rep, nil
	}
	return storage.Report{}, storage.ErrNotFound
}

func (f *fakeRepo) FindByIdempotencyKey(_ context.Context, user uuid.UUID, key string) (storage.Report, error) {
	for _, rep := range f.rows {
		if rep.RequestedBy == user && rep.IdempotencyKey != nil && *rep.IdempotencyKey == key {
			return rep, nil
		}
	}
	return storage.Report{}, storage.ErrNotFound
}

func (f *fakeRepo) ListByInspection(_ context.Context, inspectionID uuid.UUID) ([]storage.Report, error) {
	var list []storage.Report
	for _, rep := range f.rows {
		if rep.InspectionID == inspectionID {
			list = append(list, rep)
		}
	}
	return list, nil
}

func (f *fakeRepo) ObjectKeys(context.Context, storage.Owner) ([]string, error) { return f.keys, nil }

func (f *fakeRepo) Delete(_ context.Context, owner storage.Owner) error {
	f.deleted = append(f.deleted, owner)
	return nil
}

type fakeFiles struct {
	deleted []string
	err     error
}

func (f *fakeFiles) Get(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader([]byte("%PDF-"))), 5, f.err
}

func (f *fakeFiles) Delete(_ context.Context, keys ...string) error {
	f.deleted = append(f.deleted, keys...)
	return f.err
}

type fixture struct {
	reports *Reports
	core    *fakeCore
	repo    *fakeRepo
	files   *fakeFiles
	owner   uuid.UUID
}

func setup() fixture {
	owner := uuid.New()
	coreAPI := &fakeCore{
		data: core.ReportData{
			Property:   core.Property{ID: uuid.New()},
			Inspection: core.Inspection{ID: uuid.New(), Status: "COMPLETED"},
		},
		allowed: map[uuid.UUID]bool{owner: true},
	}
	repo := &fakeRepo{rows: map[uuid.UUID]storage.Report{}}
	files := &fakeFiles{}
	return fixture{reports: NewReports(repo, files, coreAPI), core: coreAPI, repo: repo, files: files, owner: owner}
}

func TestRequestQueuesJobOnceForAKey(t *testing.T) {
	f := setup()
	req := Request{UserID: f.owner, InspectionID: f.core.data.Inspection.ID, Locale: "en", IdempotencyKey: "key-12345"}

	first, err := f.reports.Request(context.Background(), req)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	again, err := f.reports.Request(context.Background(), req)
	if err != nil || again.ID != first.ID {
		t.Fatalf("repeated request = %v, %v", again.ID, err)
	}
	if first.Status != storage.StatusQueued || first.PropertyID != f.core.data.Property.ID || first.Locale != "en" {
		t.Errorf("job = %+v", first)
	}
	if len(f.repo.rows) != 1 {
		t.Errorf("jobs = %d, want 1", len(f.repo.rows))
	}
}

func TestRequestChecksAccessAndStatus(t *testing.T) {
	cases := map[string]struct {
		change func(*fixture)
		want   error
	}{
		"stranger":          {func(f *fixture) { f.owner = uuid.New() }, ErrNotFound},
		"unknown":           {func(f *fixture) { f.core.dataErr = core.ErrNotFound }, ErrNotFound},
		"core down":         {func(f *fixture) { f.core.dataErr = core.ErrUnavailable }, ErrUnavailable},
		"access check down": {func(f *fixture) { f.core.err = core.ErrUnavailable }, ErrUnavailable},
		"not completed":     {func(f *fixture) { f.core.data.Inspection.Status = "IN_PROGRESS" }, ErrNotCompleted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup()
			tc.change(&f)
			_, err := f.reports.Request(context.Background(), Request{UserID: f.owner, InspectionID: uuid.New(), Locale: "ru"})
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestConcurrentRepetitionReturnsTheWinner(t *testing.T) {
	f := setup()
	key := "key-12345"
	winner := storage.Report{ID: uuid.New(), RequestedBy: f.owner, IdempotencyKey: &key}
	f.repo.insertErr = storage.ErrDuplicateIdempotencyKey
	calls := 0
	f.reports.repo = &raceRepo{fakeRepo: f.repo, winner: winner, calls: &calls}

	got, err := f.reports.Request(context.Background(), Request{UserID: f.owner, InspectionID: uuid.New(), Locale: "ru", IdempotencyKey: key})
	if err != nil || got.ID != winner.ID {
		t.Fatalf("Request = %v, %v", got.ID, err)
	}
}

// raceRepo lets another request insert the same key between the lookup and the insert.
type raceRepo struct {
	*fakeRepo
	winner storage.Report
	calls  *int
}

func (r *raceRepo) FindByIdempotencyKey(context.Context, uuid.UUID, string) (storage.Report, error) {
	*r.calls++
	if *r.calls == 1 {
		return storage.Report{}, storage.ErrNotFound
	}
	return r.winner, nil
}

func readFixture() (fixture, storage.Report, storage.Report) {
	f := setup()
	key := "p/r.pdf"
	done := storage.Report{ID: uuid.New(), InspectionID: uuid.New(), PropertyID: uuid.New(), Status: storage.StatusDone, ObjectKey: &key}
	queued := storage.Report{ID: uuid.New(), InspectionID: done.InspectionID, PropertyID: done.PropertyID, Status: storage.StatusQueued}
	f.repo.rows[done.ID], f.repo.rows[queued.ID] = done, queued
	return f, done, queued
}

func TestReadingFiles(t *testing.T) {
	f, done, queued := readFixture()
	ctx := context.Background()

	_, body, size, err := f.reports.File(ctx, f.owner, done.ID)
	testsupport.Require(t, err == nil && size == 5, "File = %d %v", size, err)
	_ = body.Close()
	_, _, _, err = f.reports.File(ctx, f.owner, queued.ID)
	testsupport.Expect(t, errors.Is(err, ErrNotReady), "queued file: %v", err)
	f.files.err = errors.New("storage down")
	_, _, _, err = f.reports.File(ctx, f.owner, done.ID)
	testsupport.Expect(t, errors.Is(err, ErrUnavailable), "storage down: %v", err)
}

func TestReadingJobs(t *testing.T) {
	f, done, _ := readFixture()
	ctx := context.Background()

	_, err := f.reports.Get(ctx, uuid.New(), done.ID)
	testsupport.Expect(t, errors.Is(err, ErrNotFound), "stranger: %v", err)
	_, err = f.reports.Get(ctx, f.owner, uuid.New())
	testsupport.Expect(t, errors.Is(err, ErrNotFound), "unknown: %v", err)
	list, err := f.reports.List(ctx, f.owner, done.InspectionID)
	testsupport.Expect(t, err == nil && len(list) == 2, "List = %d %v", len(list), err)
	list, err = f.reports.List(ctx, uuid.New(), done.InspectionID)
	testsupport.Expect(t, errors.Is(err, ErrNotFound) && list == nil, "stranger list = %v %v", list, err)
	list, err = f.reports.List(ctx, uuid.New(), uuid.New())
	testsupport.Expect(t, err == nil && len(list) == 0, "empty list = %v %v", list, err)
}

func TestDeleteAllRemovesFilesBeforeRows(t *testing.T) {
	f := setup()
	f.repo.keys = []string{"a.pdf", "b.pdf"}
	owner := storage.ByRequester(f.owner)

	if err := f.reports.DeleteAll(context.Background(), owner); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if len(f.files.deleted) != 2 || len(f.repo.deleted) != 1 || f.repo.deleted[0] != owner {
		t.Errorf("files %v rows %v", f.files.deleted, f.repo.deleted)
	}

	f.files.err = errors.New("storage down")
	f.repo.deleted = nil
	if err := f.reports.DeleteAll(context.Background(), owner); !errors.Is(err, ErrUnavailable) || f.repo.deleted != nil {
		t.Errorf("rows must stay while files remain: %v %v", err, f.repo.deleted)
	}
}
