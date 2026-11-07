package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/inspecto-system/report-service/internal/testsupport"
)

var now = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

func reports(t *testing.T) *Reports {
	t.Helper()
	db := testsupport.StartPostgres(t)
	testsupport.Require(t, Migrate(db.URL(t, db.Config.Migrator)) == nil, "Migrate failed")
	pool, err := Connect(context.Background(), db.URL(t, db.Config.App))
	testsupport.Require(t, err == nil, "Connect: %v", err)
	t.Cleanup(pool.Close)
	return NewReports(pool)
}

func queued(property, requester uuid.UUID, at time.Time) Report {
	return Report{
		ID: uuid.Must(uuid.NewV7()), InspectionID: uuid.New(), PropertyID: property, RequestedBy: requester,
		Locale: "ru", Status: StatusQueued, NextAttemptAt: at, CreatedAt: at,
	}
}

func insert(t *testing.T, repo *Reports, rows ...Report) {
	t.Helper()
	for _, rep := range rows {
		testsupport.Require(t, repo.Insert(context.Background(), rep) == nil, "Insert %s", rep.ID)
	}
}

func TestIdempotencyKeyIsUniquePerRequester(t *testing.T) {
	repo := reports(t)
	ctx := context.Background()
	key := "request-key-1"
	first := queued(uuid.New(), uuid.New(), now)
	first.IdempotencyKey = &key
	insert(t, repo, first)
	second := queued(first.PropertyID, first.RequestedBy, now)
	second.IdempotencyKey = &key
	other := queued(first.PropertyID, uuid.New(), now)
	other.IdempotencyKey = &key

	testsupport.Expect(t, errors.Is(repo.Insert(ctx, second), ErrDuplicateIdempotencyKey), "duplicate key accepted")
	testsupport.Expect(t, repo.Insert(ctx, other) == nil, "another requester may use the same key")
	found, err := repo.FindByIdempotencyKey(ctx, first.RequestedBy, key)
	testsupport.Expect(t, err == nil && found.ID == first.ID, "FindByIdempotencyKey = %v, %v", found.ID, err)
	_, err = repo.Get(ctx, uuid.New())
	testsupport.Expect(t, errors.Is(err, ErrNotFound), "unknown report: %v", err)
}

func TestClaimRetryAndComplete(t *testing.T) {
	repo := reports(t)
	ctx := context.Background()
	first := queued(uuid.New(), uuid.New(), now)
	insert(t, repo, first)

	claimed, ok, err := repo.ClaimNext(ctx, now)
	testsupport.Require(t, err == nil && ok && claimed.ID == first.ID, "ClaimNext = %v %v %v", claimed.ID, ok, err)
	testsupport.Expect(t, claimed.Status == StatusRunning && claimed.Attempts == 1, "claimed = %+v", claimed)
	_, ok, _ = repo.ClaimNext(ctx, now)
	testsupport.Expect(t, !ok, "a running job must not be claimed twice")

	testsupport.Require(t, repo.Retry(ctx, first.ID, now.Add(time.Minute)) == nil, "Retry")
	_, ok, _ = repo.ClaimNext(ctx, now)
	testsupport.Expect(t, !ok, "a job must wait until its next attempt")
	claimed, ok, _ = repo.ClaimNext(ctx, now.Add(time.Minute))
	testsupport.Expect(t, ok && claimed.Attempts == 2, "second attempt = %+v", claimed)

	result := Result{ObjectKey: first.PropertyID.String() + "/" + first.ID.String() + ".pdf", PageCount: 3, SizeBytes: 1234}
	testsupport.Require(t, repo.Complete(ctx, first.ID, result, now) == nil, "Complete")
	done, err := repo.Get(ctx, first.ID)
	testsupport.Require(t, err == nil, "Get: %v", err)
	testsupport.Expect(t, done.Status == StatusDone && *done.ObjectKey == result.ObjectKey && *done.PageCount == 3 &&
		done.FinishedAt != nil, "done = %+v", done)
}

func TestFailAndRequeueStale(t *testing.T) {
	repo := reports(t)
	ctx := context.Background()
	stale := queued(uuid.New(), uuid.New(), now)
	failing := queued(uuid.New(), uuid.New(), now.Add(time.Second))
	insert(t, repo, stale, failing)
	_, _, _ = repo.ClaimNext(ctx, now)
	claimed, _, _ := repo.ClaimNext(ctx, now.Add(time.Second))
	testsupport.Require(t, repo.Fail(ctx, claimed.ID, "RENDER_FAILED", now) == nil, "Fail")

	n, err := repo.RequeueStale(ctx, now.Add(time.Hour), now.Add(time.Hour))
	testsupport.Require(t, err == nil && n == 1, "RequeueStale = %d %v", n, err)

	again, ok, _ := repo.ClaimNext(ctx, now.Add(time.Hour))
	testsupport.Expect(t, ok && again.ID == stale.ID && again.Attempts == 2, "requeued job = %+v %v", again, ok)
	failed, _ := repo.Get(ctx, failing.ID)
	testsupport.Expect(t, failed.Status == StatusFailed && *failed.ErrorCode == "RENDER_FAILED", "failed job = %+v", failed)
}

func TestListAndDeleteByOwner(t *testing.T) {
	repo := reports(t)
	ctx := context.Background()
	property, user := uuid.New(), uuid.New()
	older := queued(property, user, now)
	newer := queued(property, uuid.New(), now.Add(time.Minute))
	newer.InspectionID = older.InspectionID
	other := queued(uuid.New(), user, now)
	insert(t, repo, older, newer, other)
	_ = repo.Complete(ctx, older.ID, Result{ObjectKey: "k1", PageCount: 1, SizeBytes: 1}, now)

	list, err := repo.ListByInspection(ctx, older.InspectionID)
	testsupport.Expect(t, err == nil && len(list) == 2 && list[0].ID == newer.ID, "ListByInspection = %v %v", list, err)
	keys, err := repo.ObjectKeys(ctx, ByProperty(property))
	testsupport.Expect(t, err == nil && len(keys) == 1 && keys[0] == "k1", "ObjectKeys = %v %v", keys, err)

	testsupport.Require(t, repo.Delete(ctx, ByRequester(user)) == nil, "Delete by requester")
	_, err = repo.Get(ctx, other.ID)
	testsupport.Expect(t, errors.Is(err, ErrNotFound), "reports of the requester must be deleted")
	_, err = repo.Get(ctx, newer.ID)
	testsupport.Expect(t, err == nil, "reports of other requesters must stay")

	testsupport.Require(t, repo.Delete(ctx, ByProperty(property)) == nil, "Delete by property")
	list, _ = repo.ListByInspection(ctx, older.InspectionID)
	testsupport.Expect(t, len(list) == 0, "reports of the property must be deleted: %v", list)
}
