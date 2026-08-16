package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/timaogurtzova/meetnote/internal/domain"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) (*Repository, error) {
	if pool == nil {
		return nil, errors.New("database pool must not be nil")
	}
	return &Repository{pool: pool}, nil
}

func (r *Repository) RegisterUser(ctx context.Context, userID string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO users(external_id) VALUES($1)
		ON CONFLICT (external_id) DO NOTHING
	`, userID)
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

func (r *Repository) CreateMeeting(
	ctx context.Context,
	userID, requestKey string,
	file domain.StoredFile,
	quota domain.MeetingQuota,
) (domain.Meeting, bool, error) {
	if file.Size < 0 || quota.MaxMeetings < 1 || quota.MaxPending < 1 ||
		quota.MaxPending > quota.MaxMeetings || quota.MaxStorageBytes < 1 {
		return domain.Meeting{}, false, fmt.Errorf("%w: invalid stored file or meeting quota", domain.ErrInvalidInput)
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Meeting{}, false, fmt.Errorf("begin create meeting: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ownerID int64
	// Обновление без изменения данных блокирует строку пользователя до конца транзакции.
	// Параллельные загрузки одного пользователя последовательно проверяют общую квоту.
	err = tx.QueryRow(ctx, `
		INSERT INTO users(external_id) VALUES($1)
		ON CONFLICT (external_id) DO UPDATE SET external_id = EXCLUDED.external_id
		RETURNING id
	`, userID).Scan(&ownerID)
	if err != nil {
		return domain.Meeting{}, false, fmt.Errorf("upsert meeting owner: %w", err)
	}

	var meeting domain.Meeting
	err = tx.QueryRow(ctx, `
		SELECT m.id, m.original_filename, p.status,
		       COALESCE(s.content, ''), COALESCE(p.error_text, ''),
		       m.created_at, p.updated_at
		FROM meetings m
		JOIN processing_tasks p ON p.meeting_id = m.id
		LEFT JOIN summaries s ON s.meeting_id = m.id
		WHERE m.user_id = $1 AND m.source_key = $2
	`, ownerID, requestKey).Scan(
		&meeting.ID,
		&meeting.OriginalFilename,
		&meeting.Status,
		&meeting.Summary,
		&meeting.Error,
		&meeting.CreatedAt,
		&meeting.UpdatedAt,
	)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return domain.Meeting{}, false, fmt.Errorf("commit idempotent meeting lookup: %w", err)
		}
		return meeting, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.Meeting{}, false, fmt.Errorf("load idempotent meeting: %w", err)
	}

	var meetingCount, pendingCount, storedBytes int64
	err = tx.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE p.status IN ('created', 'processing', 'transcribed', 'summarized')),
		       COALESCE(sum(m.file_size), 0)
		FROM meetings m
		JOIN processing_tasks p ON p.meeting_id = m.id
		WHERE m.user_id = $1
	`, ownerID).Scan(&meetingCount, &pendingCount, &storedBytes)
	if err != nil {
		return domain.Meeting{}, false, fmt.Errorf("load meeting quota usage: %w", err)
	}
	if meetingCount >= int64(quota.MaxMeetings) {
		return domain.Meeting{}, false, fmt.Errorf("%w: meeting count limit reached", domain.ErrQuotaExceeded)
	}
	if pendingCount >= int64(quota.MaxPending) {
		return domain.Meeting{}, false, fmt.Errorf("%w: pending meeting limit reached", domain.ErrQuotaExceeded)
	}
	if file.Size > quota.MaxStorageBytes || storedBytes > quota.MaxStorageBytes-file.Size {
		return domain.Meeting{}, false, fmt.Errorf("%w: storage limit reached", domain.ErrQuotaExceeded)
	}

	meeting.OriginalFilename = file.OriginalFilename
	meeting.Status = domain.StatusCreated
	err = tx.QueryRow(ctx, `
		INSERT INTO meetings(user_id, original_filename, stored_path, source_key, file_size)
		VALUES($1, $2, $3, $4, $5)
		RETURNING id, created_at
	`, ownerID, file.OriginalFilename, file.Path, requestKey, file.Size).Scan(&meeting.ID, &meeting.CreatedAt)
	if err != nil {
		return domain.Meeting{}, false, fmt.Errorf("insert meeting: %w", err)
	}

	var taskID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO processing_tasks(meeting_id, status)
		VALUES($1, $2)
		RETURNING id, updated_at
	`, meeting.ID, domain.StatusCreated).Scan(&taskID, &meeting.UpdatedAt)
	if err != nil {
		return domain.Meeting{}, false, fmt.Errorf("insert processing task: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_status_history(task_id, to_status) VALUES($1, $2)
	`, taskID, domain.StatusCreated); err != nil {
		return domain.Meeting{}, false, fmt.Errorf("record created status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Meeting{}, false, fmt.Errorf("commit meeting and task: %w", err)
	}
	return meeting, true, nil
}

func (r *Repository) ListMeetings(ctx context.Context, userID string, limit int) ([]domain.Meeting, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT m.id, m.original_filename,
		       p.status, COALESCE(s.content, ''), COALESCE(p.error_text, ''),
		       m.created_at, p.updated_at
		FROM meetings m
		JOIN users u ON u.id = m.user_id
		JOIN processing_tasks p ON p.meeting_id = m.id
		LEFT JOIN summaries s ON s.meeting_id = m.id
		WHERE u.external_id = $1
		ORDER BY m.created_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("query user meetings: %w", err)
	}
	defer rows.Close()

	meetings := make([]domain.Meeting, 0)
	for rows.Next() {
		meeting, err := scanMeeting(rows)
		if err != nil {
			return nil, err
		}
		meetings = append(meetings, meeting)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user meetings: %w", err)
	}
	return meetings, nil
}

func (r *Repository) GetMeeting(ctx context.Context, userID string, meetingID int64) (domain.Meeting, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT m.id, m.original_filename,
		       p.status, COALESCE(s.content, ''), COALESCE(p.error_text, ''),
		       m.created_at, p.updated_at
		FROM meetings m
		JOIN users u ON u.id = m.user_id
		JOIN processing_tasks p ON p.meeting_id = m.id
		LEFT JOIN summaries s ON s.meeting_id = m.id
		WHERE u.external_id = $1 AND m.id = $2
	`, userID, meetingID)
	meeting, err := scanMeeting(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Meeting{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Meeting{}, err
	}
	return meeting, nil
}

func (r *Repository) GetTranscript(ctx context.Context, userID string, meetingID int64) (string, error) {
	var transcript sql.NullString
	err := r.pool.QueryRow(ctx, `
		SELECT t.content
		FROM meetings m
		JOIN users u ON u.id = m.user_id
		LEFT JOIN transcriptions t ON t.meeting_id = m.id
		WHERE u.external_id = $1 AND m.id = $2
	`, userID, meetingID).Scan(&transcript)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("query transcript: %w", err)
	}
	if !transcript.Valid {
		return "", domain.ErrNotReady
	}
	return transcript.String, nil
}

func (r *Repository) FindMeetings(ctx context.Context, userID, query string, limit int) ([]domain.SearchResult, error) {
	likeQuery := "%" + strings.NewReplacer(
		`\`, `\\`,
		`%`, `\%`,
		`_`, `\_`,
	).Replace(query) + "%"
	rows, err := r.pool.Query(ctx, `
		SELECT m.id, p.status, m.created_at,
		       COALESCE(
		           NULLIF(ts_headline('simple',
		               COALESCE(t.content, s.content, ''),
		               websearch_to_tsquery('simple', $2),
		               'MaxWords=30, MinWords=10, StartSel=[, StopSel=]'
		           ), ''),
		           left(COALESCE(s.content, t.content, ''), 240)
		       ) AS snippet
		FROM meetings m
		JOIN users u ON u.id = m.user_id
		JOIN processing_tasks p ON p.meeting_id = m.id
		LEFT JOIN transcriptions t ON t.meeting_id = m.id
		LEFT JOIN summaries s ON s.meeting_id = m.id
		WHERE u.external_id = $1
		  AND (
		      to_tsvector('simple', COALESCE(t.content, '')) @@ websearch_to_tsquery('simple', $2)
		      OR to_tsvector('simple', COALESCE(s.content, '')) @@ websearch_to_tsquery('simple', $2)
		      OR t.content ILIKE $4 ESCAPE '\'
		      OR s.content ILIKE $4 ESCAPE '\'
		  )
		ORDER BY m.created_at DESC
		LIMIT $3
	`, userID, query, limit, likeQuery)
	if err != nil {
		return nil, fmt.Errorf("search meetings: %w", err)
	}
	defer rows.Close()

	result := make([]domain.SearchResult, 0)
	for rows.Next() {
		var item domain.SearchResult
		var status string
		if err := rows.Scan(&item.MeetingID, &status, &item.CreatedAt, &item.Snippet); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		item.Status = domain.Status(status)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search results: %w", err)
	}
	return result, nil
}

func (r *Repository) ChatDocuments(ctx context.Context, userID string, limit int) ([]domain.ChatDocument, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT m.id, left(t.content, 12000), COALESCE(left(s.content, 4000), '')
		FROM meetings m
		JOIN users u ON u.id = m.user_id
		JOIN processing_tasks p ON p.meeting_id = m.id
		JOIN transcriptions t ON t.meeting_id = m.id
		LEFT JOIN summaries s ON s.meeting_id = m.id
		WHERE u.external_id = $1 AND p.status = 'completed'
		ORDER BY m.created_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("query chat documents: %w", err)
	}
	defer rows.Close()

	documents := make([]domain.ChatDocument, 0)
	for rows.Next() {
		var document domain.ChatDocument
		if err := rows.Scan(&document.MeetingID, &document.Transcript, &document.Summary); err != nil {
			return nil, fmt.Errorf("scan chat document: %w", err)
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat documents: %w", err)
	}
	return documents, nil
}

func (r *Repository) GetChatAnswer(ctx context.Context, userID, requestKey string) (string, bool, error) {
	var answer string
	err := r.pool.QueryRow(ctx, `
		SELECT c.answer
		FROM chat_history c
		JOIN users u ON u.id = c.user_id
		WHERE u.external_id = $1 AND c.request_key = $2
	`, userID, requestKey).Scan(&answer)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query chat history by request key: %w", err)
	}
	return answer, true, nil
}

func (r *Repository) SaveChat(
	ctx context.Context,
	userID, requestKey, question, answer string,
	maxHistory int,
) (string, error) {
	if maxHistory < 1 {
		return "", fmt.Errorf("%w: chat history limit must be positive", domain.ErrInvalidInput)
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin chat history save: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ownerID int64
	// Блокировка пользователя сериализует вставку и обрезку истории для одного владельца.
	err = tx.QueryRow(ctx, `
		SELECT id FROM users WHERE external_id = $1 FOR UPDATE
	`, userID).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock chat owner: %w", err)
	}

	var storedAnswer string
	err = tx.QueryRow(ctx, `
		INSERT INTO chat_history(user_id, request_key, question, answer)
		VALUES($1, $2, $3, $4)
		ON CONFLICT (user_id, request_key) WHERE request_key IS NOT NULL
		DO UPDATE SET request_key = EXCLUDED.request_key
		RETURNING answer
	`, ownerID, requestKey, question, answer).Scan(&storedAnswer)
	if err != nil {
		return "", fmt.Errorf("insert chat history: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM chat_history
		WHERE id IN (
			SELECT id
			FROM chat_history
			WHERE user_id = $1
			ORDER BY created_at DESC, id DESC
			OFFSET $2
		)
	`, ownerID, maxHistory); err != nil {
		return "", fmt.Errorf("trim chat history: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit chat history: %w", err)
	}
	return storedAnswer, nil
}

func (r *Repository) RetryMeeting(ctx context.Context, userID, requestKey string, meetingID int64, maxPending int) error {
	if maxPending < 1 {
		return fmt.Errorf("%w: pending meeting limit must be positive", domain.ErrInvalidInput)
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ownerID int64
	err = tx.QueryRow(ctx, `
		SELECT id FROM users WHERE external_id = $1 FOR UPDATE
	`, userID).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock retry owner: %w", err)
	}

	var taskID int64
	var status string
	var lastRetryKey sql.NullString
	err = tx.QueryRow(ctx, `
		SELECT p.id, p.status, p.last_retry_key
		FROM processing_tasks p
		JOIN meetings m ON m.id = p.meeting_id
		WHERE m.user_id = $1 AND m.id = $2
		FOR UPDATE OF p
	`, ownerID, meetingID).Scan(&taskID, &status, &lastRetryKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock task for retry: %w", err)
	}
	if lastRetryKey.Valid && lastRetryKey.String == requestKey {
		return tx.Commit(ctx)
	}
	if domain.Status(status) != domain.StatusFailed {
		return domain.ErrInvalidState
	}
	var pendingCount int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM processing_tasks p
		JOIN meetings m ON m.id = p.meeting_id
		WHERE m.user_id = $1
		  AND p.status IN ('created', 'processing', 'transcribed', 'summarized')
	`, ownerID).Scan(&pendingCount); err != nil {
		return fmt.Errorf("load retry quota usage: %w", err)
	}
	if pendingCount >= int64(maxPending) {
		return fmt.Errorf("%w: pending meeting limit reached", domain.ErrQuotaExceeded)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM processing_user_leases WHERE task_id = $1`, taskID); err != nil {
		return fmt.Errorf("remove stale retry lease: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE processing_tasks
		SET status = 'created', error_text = NULL, started_at = NULL, finished_at = NULL,
		    locked_by = NULL, lease_token = NULL, locked_until = NULL,
		    last_retry_key = $2, error_code = NULL, updated_at = now()
		WHERE id = $1
	`, taskID, requestKey); err != nil {
		return fmt.Errorf("reset failed task: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_status_history(task_id, from_status, to_status)
		VALUES($1, $2, 'created')
	`, taskID, status); err != nil {
		return fmt.Errorf("record retry status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit retry: %w", err)
	}
	return nil
}

func (r *Repository) ListStoredPaths(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT stored_path FROM meetings`)
	if err != nil {
		return nil, fmt.Errorf("query stored upload paths: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan stored upload path: %w", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stored upload paths: %w", err)
	}
	return paths, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMeeting(row rowScanner) (domain.Meeting, error) {
	var meeting domain.Meeting
	var status string
	err := row.Scan(
		&meeting.ID,
		&meeting.OriginalFilename,
		&status,
		&meeting.Summary,
		&meeting.Error,
		&meeting.CreatedAt,
		&meeting.UpdatedAt,
	)
	if err != nil {
		return domain.Meeting{}, fmt.Errorf("scan meeting: %w", err)
	}
	meeting.Status = domain.Status(status)
	return meeting, nil
}
