-- Report jobs (docs/DATABASE.md §4, T-402, ADR-011). The PDF lives in the "reports" bucket under
-- object_key; inspection_id, property_id and requested_by are ids of core-service without foreign
-- keys (separate databases, ADR-005).
CREATE TABLE reports (
    id              uuid PRIMARY KEY,
    inspection_id   uuid        NOT NULL,
    property_id     uuid        NOT NULL,
    requested_by    uuid        NOT NULL,
    locale          text        NOT NULL CHECK (locale IN ('ru', 'en')),
    status          text        NOT NULL CHECK (status IN ('QUEUED', 'RUNNING', 'DONE', 'FAILED')),
    -- Attempts started so far; a job is retried at most three times (ADR-011).
    attempts        int         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL,
    error_code      text        NULL,
    object_key      text        NULL,
    page_count      int         NULL CHECK (page_count > 0),
    size_bytes      bigint      NULL CHECK (size_bytes > 0),
    idempotency_key text        NULL,
    created_at      timestamptz NOT NULL,
    started_at      timestamptz NULL,
    finished_at     timestamptz NULL,
    CHECK ((status = 'DONE') = (object_key IS NOT NULL)),
    CHECK ((status = 'FAILED') = (error_code IS NOT NULL))
);

CREATE INDEX reports_inspection_created_idx ON reports (inspection_id, created_at DESC);
CREATE INDEX reports_property_idx ON reports (property_id);
CREATE INDEX reports_requested_by_idx ON reports (requested_by);
-- Workers pick the oldest due job (SELECT … FOR UPDATE SKIP LOCKED).
CREATE INDEX reports_due_idx ON reports (next_attempt_at) WHERE status = 'QUEUED';
CREATE UNIQUE INDEX reports_requester_idempotency_uq ON reports (requested_by, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
