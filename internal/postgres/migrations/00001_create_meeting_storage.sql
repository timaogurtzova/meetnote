-- +goose Up
CREATE TABLE users (
    id          BIGSERIAL PRIMARY KEY,
    external_id TEXT NOT NULL UNIQUE CHECK (char_length(external_id) BETWEEN 1 AND 128),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE meetings (
    id                BIGSERIAL PRIMARY KEY,
    user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    original_filename TEXT NOT NULL,
    stored_path       TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE processing_tasks (
    id          BIGSERIAL PRIMARY KEY,
    meeting_id  BIGINT NOT NULL UNIQUE REFERENCES meetings(id) ON DELETE CASCADE,
    status      TEXT NOT NULL CHECK (status IN ('created', 'processing', 'transcribed', 'summarized', 'completed', 'failed')),
    attempts    INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    error_text  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE transcriptions (
    meeting_id BIGINT PRIMARY KEY REFERENCES meetings(id) ON DELETE CASCADE,
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE summaries (
    meeting_id BIGINT PRIMARY KEY REFERENCES meetings(id) ON DELETE CASCADE,
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE task_status_history (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT NOT NULL REFERENCES processing_tasks(id) ON DELETE CASCADE,
    from_status TEXT,
    to_status   TEXT NOT NULL,
    error_text  TEXT,
    changed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE chat_history (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    question   TEXT NOT NULL,
    answer     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_meetings_user_created ON meetings(user_id, created_at DESC);
CREATE INDEX idx_tasks_status_created ON processing_tasks(status, created_at);
CREATE INDEX idx_task_history_task_changed ON task_status_history(task_id, changed_at DESC);
CREATE INDEX idx_chat_user_created ON chat_history(user_id, created_at DESC);
CREATE INDEX idx_transcriptions_fts ON transcriptions USING GIN (to_tsvector('simple', content));
CREATE INDEX idx_summaries_fts ON summaries USING GIN (to_tsvector('simple', content));

-- +goose Down
DROP TABLE IF EXISTS chat_history;
DROP TABLE IF EXISTS task_status_history;
DROP TABLE IF EXISTS summaries;
DROP TABLE IF EXISTS transcriptions;
DROP TABLE IF EXISTS processing_tasks;
DROP TABLE IF EXISTS meetings;
DROP TABLE IF EXISTS users;
