-- +goose Up
ALTER TABLE processing_tasks
    ADD COLUMN locked_by TEXT,
    ADD COLUMN lease_token TEXT,
    ADD COLUMN locked_until TIMESTAMPTZ;

CREATE INDEX idx_tasks_expired_leases
    ON processing_tasks(locked_until)
    WHERE status IN ('processing', 'transcribed', 'summarized');

-- +goose Down
DROP INDEX IF EXISTS idx_tasks_expired_leases;

ALTER TABLE processing_tasks
    DROP COLUMN IF EXISTS locked_until,
    DROP COLUMN IF EXISTS lease_token,
    DROP COLUMN IF EXISTS locked_by;
