-- +goose Up
ALTER TABLE meetings
    ADD COLUMN source_key TEXT;

CREATE UNIQUE INDEX idx_meetings_user_source_key
    ON meetings(user_id, source_key)
    WHERE source_key IS NOT NULL;

ALTER TABLE chat_history
    ADD COLUMN request_key TEXT;

CREATE UNIQUE INDEX idx_chat_user_request_key
    ON chat_history(user_id, request_key)
    WHERE request_key IS NOT NULL;

ALTER TABLE processing_tasks
    ADD COLUMN last_retry_key TEXT,
    ADD COLUMN error_code TEXT;

ALTER TABLE task_status_history
    ADD COLUMN error_code TEXT;

UPDATE processing_tasks
SET error_code = 'legacy_processing_failed',
    error_text = 'Обработка встречи завершилась ошибкой. Используйте /retry ID, чтобы повторить попытку.'
WHERE status = 'failed' AND error_text IS NOT NULL;

UPDATE task_status_history
SET error_code = 'legacy_processing_failed',
    error_text = 'Обработка встречи завершилась ошибкой.'
WHERE to_status = 'failed' AND error_text IS NOT NULL;

ALTER TABLE transcriptions
    ADD CONSTRAINT transcriptions_content_not_blank
    CHECK (length(btrim(content)) > 0) NOT VALID;
ALTER TABLE transcriptions VALIDATE CONSTRAINT transcriptions_content_not_blank;

ALTER TABLE summaries
    ADD CONSTRAINT summaries_content_not_blank
    CHECK (length(btrim(content)) > 0) NOT VALID;
ALTER TABLE summaries VALIDATE CONSTRAINT summaries_content_not_blank;

ALTER TABLE chat_history
    ADD CONSTRAINT chat_history_question_not_blank
    CHECK (length(btrim(question)) > 0) NOT VALID,
    ADD CONSTRAINT chat_history_answer_not_blank
    CHECK (length(btrim(answer)) > 0) NOT VALID;
ALTER TABLE chat_history VALIDATE CONSTRAINT chat_history_question_not_blank;
ALTER TABLE chat_history VALIDATE CONSTRAINT chat_history_answer_not_blank;

CREATE TABLE telegram_updates (
    update_id    BIGINT PRIMARY KEY,
    user_key     TEXT NOT NULL,
    payload      JSONB NOT NULL,
    status       TEXT NOT NULL DEFAULT 'created'
                 CHECK (status IN ('created', 'processing', 'completed', 'failed')),
    attempts     INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_by    TEXT,
    lease_token  TEXT,
    locked_until TIMESTAMPTZ,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX idx_telegram_updates_claim
    ON telegram_updates(available_at, update_id, user_key)
    WHERE status = 'created';

CREATE INDEX idx_telegram_updates_expired_lease
    ON telegram_updates(locked_until)
    WHERE status = 'processing';

CREATE INDEX idx_telegram_updates_cleanup
    ON telegram_updates(updated_at)
    WHERE status IN ('completed', 'failed');

CREATE TABLE telegram_user_leases (
    user_key     TEXT PRIMARY KEY,
    update_id    BIGINT NOT NULL UNIQUE REFERENCES telegram_updates(update_id) ON DELETE CASCADE,
    lease_token  TEXT NOT NULL,
    locked_until TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_telegram_user_leases_expiry ON telegram_user_leases(locked_until);

-- +goose Down
DROP TABLE IF EXISTS telegram_user_leases;
DROP TABLE IF EXISTS telegram_updates;

ALTER TABLE chat_history
    DROP CONSTRAINT IF EXISTS chat_history_answer_not_blank,
    DROP CONSTRAINT IF EXISTS chat_history_question_not_blank;
ALTER TABLE summaries DROP CONSTRAINT IF EXISTS summaries_content_not_blank;
ALTER TABLE transcriptions DROP CONSTRAINT IF EXISTS transcriptions_content_not_blank;

DROP INDEX IF EXISTS idx_chat_user_request_key;
DROP INDEX IF EXISTS idx_meetings_user_source_key;

ALTER TABLE task_status_history DROP COLUMN IF EXISTS error_code;
ALTER TABLE processing_tasks
    DROP COLUMN IF EXISTS error_code,
    DROP COLUMN IF EXISTS last_retry_key;
ALTER TABLE chat_history DROP COLUMN IF EXISTS request_key;
ALTER TABLE meetings DROP COLUMN IF EXISTS source_key;
