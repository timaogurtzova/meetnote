-- +goose Up
ALTER TABLE meetings
    ADD COLUMN file_size BIGINT NOT NULL DEFAULT 20971520;

ALTER TABLE meetings
    ADD CONSTRAINT meetings_file_size_nonnegative
    CHECK (file_size >= 0) NOT VALID;
ALTER TABLE meetings VALIDATE CONSTRAINT meetings_file_size_nonnegative;

CREATE TABLE processing_user_leases (
    user_id      BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    task_id      BIGINT NOT NULL UNIQUE REFERENCES processing_tasks(id) ON DELETE CASCADE,
    lease_token  TEXT NOT NULL,
    locked_until TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_processing_user_leases_expiry
    ON processing_user_leases(locked_until);

-- +goose Down
DROP TABLE IF EXISTS processing_user_leases;
ALTER TABLE meetings DROP COLUMN IF EXISTS file_size;
