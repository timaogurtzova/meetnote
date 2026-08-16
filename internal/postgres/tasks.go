package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/timaogurtzova/meetnote/internal/domain"
)

func (r *Repository) RecoverInterrupted(ctx context.Context) (int64, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin task recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, status
		FROM processing_tasks
		WHERE status IN ('processing', 'transcribed', 'summarized')
		  AND (locked_until IS NULL OR locked_until <= now())
		FOR UPDATE SKIP LOCKED
	`)
	if err != nil {
		return 0, fmt.Errorf("lock interrupted tasks: %w", err)
	}
	type interruptedTask struct {
		id     int64
		status string
	}
	var tasks []interruptedTask
	for rows.Next() {
		var task interruptedTask
		if err := rows.Scan(&task.id, &task.status); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan interrupted task: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate interrupted tasks: %w", err)
	}
	rows.Close()

	for _, task := range tasks {
		if _, err := tx.Exec(ctx, `
			UPDATE processing_tasks
			SET status = 'created', error_text = NULL, error_code = NULL, started_at = NULL, finished_at = NULL,
			    locked_by = NULL, lease_token = NULL, locked_until = NULL, updated_at = now()
			WHERE id = $1
		`, task.id); err != nil {
			return 0, fmt.Errorf("recover task %d: %w", task.id, err)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM processing_user_leases WHERE task_id = $1
		`, task.id); err != nil {
			return 0, fmt.Errorf("release recovered task owner %d: %w", task.id, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO task_status_history(task_id, from_status, to_status)
			VALUES($1, $2, 'created')
		`, task.id, task.status); err != nil {
			return 0, fmt.Errorf("record recovery for task %d: %w", task.id, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM processing_user_leases l
		USING processing_tasks p
		WHERE p.id = l.task_id
		  AND (
		      p.status NOT IN ('processing', 'transcribed', 'summarized')
		      OR p.lease_token IS DISTINCT FROM l.lease_token
		      OR p.locked_until IS NULL
		  )
	`); err != nil {
		return 0, fmt.Errorf("remove stale processing user leases: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit task recovery: %w", err)
	}
	return int64(len(tasks)), nil
}

func (r *Repository) ClaimNextTask(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (domain.Task, bool, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || leaseDuration <= 0 {
		return domain.Task{}, false, fmt.Errorf("%w: worker id and lease duration are required", domain.ErrInvalidInput)
	}
	leaseToken, err := randomLeaseToken()
	if err != nil {
		return domain.Task{}, false, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Task{}, false, fmt.Errorf("begin task claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var task domain.Task
	var ownerID int64
	// Обе проверки нужны для безопасной работы нескольких реплик.
	// Отдельная блокировка исключает пользователя, а проверка задачи защищает при рассинхронизации данных.
	err = tx.QueryRow(ctx, `
		SELECT p.id, m.id, m.user_id, m.stored_path, p.attempts + 1
		FROM processing_tasks p
		JOIN meetings m ON m.id = p.meeting_id
		WHERE p.status = 'created'
		  AND NOT EXISTS (
		      SELECT 1
		      FROM processing_user_leases l
		      WHERE l.user_id = m.user_id AND l.locked_until > now()
		  )
		  AND NOT EXISTS (
		      SELECT 1
		      FROM processing_tasks active
		      JOIN meetings active_meeting ON active_meeting.id = active.meeting_id
		      WHERE active_meeting.user_id = m.user_id
		        AND active.status IN ('processing', 'transcribed', 'summarized')
		        AND active.locked_until > now()
		  )
		ORDER BY p.created_at
		FOR UPDATE OF p SKIP LOCKED
		LIMIT 1
	`).Scan(
		&task.ID,
		&task.MeetingID,
		&ownerID,
		&task.StoredPath,
		&task.Attempt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Task{}, false, nil
	}
	if err != nil {
		return domain.Task{}, false, fmt.Errorf("select task for claim: %w", err)
	}
	leaseTag, err := tx.Exec(ctx, `
		INSERT INTO processing_user_leases(user_id, task_id, lease_token, locked_until)
		VALUES($1, $2, $3, now() + ($4 * interval '1 millisecond'))
		ON CONFLICT (user_id) DO UPDATE
		SET task_id = EXCLUDED.task_id,
		    lease_token = EXCLUDED.lease_token,
		    locked_until = EXCLUDED.locked_until
		WHERE processing_user_leases.locked_until <= now()
	`, ownerID, task.ID, leaseToken, leaseDuration.Milliseconds())
	if err != nil {
		return domain.Task{}, false, fmt.Errorf("acquire processing user lease: %w", err)
	}
	if leaseTag.RowsAffected() != 1 {
		return domain.Task{}, false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE processing_tasks
		SET status = 'processing', attempts = attempts + 1, error_text = NULL, error_code = NULL,
		    started_at = now(), finished_at = NULL, updated_at = now(),
		    locked_by = $2, lease_token = $3,
		    locked_until = now() + ($4 * interval '1 millisecond')
		WHERE id = $1
	`, task.ID, workerID, leaseToken, leaseDuration.Milliseconds()); err != nil {
		return domain.Task{}, false, fmt.Errorf("mark task processing: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_status_history(task_id, from_status, to_status)
		VALUES($1, 'created', 'processing')
	`, task.ID); err != nil {
		return domain.Task{}, false, fmt.Errorf("record processing status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Task{}, false, fmt.Errorf("commit task claim: %w", err)
	}
	task.LeaseToken = leaseToken
	return task, true, nil
}

func (r *Repository) RefreshLease(ctx context.Context, task domain.Task, leaseDuration time.Duration) error {
	if task.LeaseToken == "" || leaseDuration <= 0 {
		return fmt.Errorf("%w: lease token and duration are required", domain.ErrInvalidInput)
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin task lease refresh: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE processing_tasks
		SET locked_until = now() + ($4 * interval '1 millisecond')
		WHERE id = $1
		  AND meeting_id = $2
		  AND lease_token = $3
		  AND locked_until > now()
		  AND status IN ('processing', 'transcribed', 'summarized')
	`, task.ID, task.MeetingID, task.LeaseToken, leaseDuration.Milliseconds())
	if err != nil {
		return fmt.Errorf("refresh task lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	userTag, err := tx.Exec(ctx, `
		UPDATE processing_user_leases
		SET locked_until = now() + ($3 * interval '1 millisecond')
		WHERE task_id = $1
		  AND lease_token = $2
		  AND locked_until > now()
	`, task.ID, task.LeaseToken, leaseDuration.Milliseconds())
	if err != nil {
		return fmt.Errorf("refresh processing user lease: %w", err)
	}
	if userTag.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit task lease refresh: %w", err)
	}
	return nil
}

func (r *Repository) SaveTranscription(ctx context.Context, task domain.Task, transcript string) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transcription save: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var meetingID int64
	err = tx.QueryRow(ctx, `
		UPDATE processing_tasks
		SET status = 'transcribed', error_text = NULL, updated_at = now()
		WHERE id = $1 AND meeting_id = $2 AND status = 'processing'
		  AND lease_token = $3 AND locked_until > now()
		RETURNING meeting_id
	`, task.ID, task.MeetingID, task.LeaseToken).Scan(&meetingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.taskTransitionError(ctx, tx, task)
	}
	if err != nil {
		return fmt.Errorf("mark task transcribed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO transcriptions(meeting_id, content)
		VALUES($1, $2)
		ON CONFLICT (meeting_id)
		DO UPDATE SET content = EXCLUDED.content, updated_at = now()
	`, meetingID, transcript); err != nil {
		return fmt.Errorf("upsert transcription: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_status_history(task_id, from_status, to_status)
		VALUES($1, 'processing', 'transcribed')
	`, task.ID); err != nil {
		return fmt.Errorf("record transcribed status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transcription and status: %w", err)
	}
	return nil
}

func (r *Repository) SaveSummary(ctx context.Context, task domain.Task, summary string) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin summary save: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var meetingID int64
	err = tx.QueryRow(ctx, `
		UPDATE processing_tasks
		SET status = 'summarized', error_text = NULL, updated_at = now()
		WHERE id = $1 AND meeting_id = $2 AND status = 'transcribed'
		  AND lease_token = $3 AND locked_until > now()
		RETURNING meeting_id
	`, task.ID, task.MeetingID, task.LeaseToken).Scan(&meetingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.taskTransitionError(ctx, tx, task)
	}
	if err != nil {
		return fmt.Errorf("mark task summarized: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO summaries(meeting_id, content)
		VALUES($1, $2)
		ON CONFLICT (meeting_id)
		DO UPDATE SET content = EXCLUDED.content, updated_at = now()
	`, meetingID, summary); err != nil {
		return fmt.Errorf("upsert summary: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_status_history(task_id, from_status, to_status)
		VALUES($1, 'transcribed', 'summarized')
	`, task.ID); err != nil {
		return fmt.Errorf("record summarized status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit summary and status: %w", err)
	}
	return nil
}

func (r *Repository) CompleteTask(ctx context.Context, task domain.Task) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin task completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE processing_tasks
		SET status = 'completed', error_text = NULL, finished_at = now(), updated_at = now(),
		    locked_by = NULL, lease_token = NULL, locked_until = NULL
		WHERE id = $1 AND meeting_id = $2 AND status = 'summarized'
		  AND lease_token = $3 AND locked_until > now()
	`, task.ID, task.MeetingID, task.LeaseToken)
	if err != nil {
		return fmt.Errorf("mark task completed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return r.taskTransitionError(ctx, tx, task)
	}
	leaseTag, err := tx.Exec(ctx, `
		DELETE FROM processing_user_leases
		WHERE task_id = $1 AND lease_token = $2
	`, task.ID, task.LeaseToken)
	if err != nil {
		return fmt.Errorf("release completed task owner: %w", err)
	}
	if leaseTag.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_status_history(task_id, from_status, to_status)
		VALUES($1, 'summarized', 'completed')
	`, task.ID); err != nil {
		return fmt.Errorf("record completed status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit task completion: %w", err)
	}
	return nil
}

func (r *Repository) FailTask(ctx context.Context, task domain.Task, failure domain.ProcessingFailure) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin task failure: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var currentStatus string
	err = tx.QueryRow(ctx, `
		SELECT status FROM processing_tasks
		WHERE id = $1 AND meeting_id = $2
		  AND lease_token = $3 AND locked_until > now()
		FOR UPDATE
	`, task.ID, task.MeetingID, task.LeaseToken).Scan(&currentStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.taskTransitionError(ctx, tx, task)
	}
	if err != nil {
		return fmt.Errorf("lock task for failure: %w", err)
	}
	if domain.Status(currentStatus) == domain.StatusCompleted {
		return domain.ErrInvalidState
	}
	failure.Code = truncateError(failure.Code)
	failure.Message = truncateError(failure.Message)
	if _, err := tx.Exec(ctx, `
		UPDATE processing_tasks
		SET status = 'failed', error_code = $2, error_text = $3, finished_at = now(), updated_at = now(),
		    locked_by = NULL, lease_token = NULL, locked_until = NULL
		WHERE id = $1
	`, task.ID, failure.Code, failure.Message); err != nil {
		return fmt.Errorf("mark task failed: %w", err)
	}
	leaseTag, err := tx.Exec(ctx, `
		DELETE FROM processing_user_leases
		WHERE task_id = $1 AND lease_token = $2
	`, task.ID, task.LeaseToken)
	if err != nil {
		return fmt.Errorf("release failed task owner: %w", err)
	}
	if leaseTag.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_status_history(task_id, from_status, to_status, error_code, error_text)
		VALUES($1, $2, 'failed', $3, $4)
	`, task.ID, currentStatus, failure.Code, failure.Message); err != nil {
		return fmt.Errorf("record failed status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit task failure: %w", err)
	}
	return nil
}

func (r *Repository) taskTransitionError(ctx context.Context, tx pgx.Tx, task domain.Task) error {
	var status string
	var leaseValid bool
	err := tx.QueryRow(ctx, `
		SELECT status, COALESCE(lease_token = $3 AND locked_until > now(), FALSE)
		FROM processing_tasks
		WHERE id = $1 AND meeting_id = $2
	`, task.ID, task.MeetingID, task.LeaseToken).Scan(&status, &leaseValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("inspect task transition: %w", err)
	}
	if !leaseValid {
		return domain.ErrLeaseLost
	}
	return fmt.Errorf("%w: current status is %s", domain.ErrInvalidState, status)
}
