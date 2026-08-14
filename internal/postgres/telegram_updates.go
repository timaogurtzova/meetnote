package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/timaogurtzova/meetnote/internal/domain"
	"github.com/timaogurtzova/meetnote/internal/inbox"
)

func (r *Repository) NextOffset(ctx context.Context) (int64, error) {
	var offset int64
	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(update_id) + 1, 0) FROM telegram_updates
	`).Scan(&offset); err != nil {
		return 0, fmt.Errorf("load Telegram offset: %w", err)
	}
	return offset, nil
}

func (r *Repository) Enqueue(ctx context.Context, updates []inbox.Item) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin Telegram update enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, update := range updates {
		if update.ID < 1 || update.OwnerKey == "" || len(update.Payload) == 0 {
			return fmt.Errorf("%w: Telegram update id, user key, and payload are required", domain.ErrInvalidInput)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO telegram_updates(update_id, user_key, payload)
			VALUES($1, $2, $3::jsonb)
			ON CONFLICT (update_id) DO NOTHING
		`, update.ID, update.OwnerKey, update.Payload); err != nil {
			return fmt.Errorf("insert Telegram update %d: %w", update.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Telegram updates: %w", err)
	}
	return nil
}

func (r *Repository) RecoverExpired(ctx context.Context) (int64, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin Telegram update recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Сначала блокируются обновления, затем связанные пользовательские блокировки.
	// Один порядок захвата во всех операциях не дает транзакциям заблокировать друг друга.
	rows, err := tx.Query(ctx, `
		SELECT update_id, user_key
		FROM telegram_updates
		WHERE status = 'processing' AND (locked_until IS NULL OR locked_until <= now())
		FOR UPDATE SKIP LOCKED
	`)
	if err != nil {
		return 0, fmt.Errorf("lock expired Telegram updates: %w", err)
	}
	type expiredUpdate struct {
		id       int64
		ownerKey string
	}
	var expired []expiredUpdate
	for rows.Next() {
		var update expiredUpdate
		if err := rows.Scan(&update.id, &update.ownerKey); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired Telegram update: %w", err)
		}
		expired = append(expired, update)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate expired Telegram updates: %w", err)
	}
	rows.Close()

	for _, update := range expired {
		if _, err := tx.Exec(ctx, `
			UPDATE telegram_updates
			SET status = 'created', locked_by = NULL, lease_token = NULL,
			    locked_until = NULL, available_at = now(), updated_at = now()
			WHERE update_id = $1
		`, update.id); err != nil {
			return 0, fmt.Errorf("recover Telegram update %d: %w", update.id, err)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM telegram_user_leases
			WHERE user_key = $1 AND update_id = $2
		`, update.ownerKey, update.id); err != nil {
			return 0, fmt.Errorf("release recovered Telegram update %d: %w", update.id, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM telegram_user_leases lease
		WHERE lease.locked_until <= now()
		   OR NOT EXISTS (
			   SELECT 1
			   FROM telegram_updates update_row
			   WHERE update_row.update_id = lease.update_id
			     AND update_row.user_key = lease.user_key
			     AND update_row.status = 'processing'
			     AND update_row.lease_token = lease.lease_token
			     AND update_row.locked_until > now()
		   )
	`); err != nil {
		return 0, fmt.Errorf("remove stale Telegram user leases: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit Telegram update recovery: %w", err)
	}
	return int64(len(expired)), nil
}

func (r *Repository) Prune(ctx context.Context, completedBefore time.Time) (int64, error) {
	if completedBefore.IsZero() {
		return 0, fmt.Errorf("%w: Telegram inbox retention cutoff is required", domain.ErrInvalidInput)
	}
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM telegram_updates
		WHERE status IN ('completed', 'failed') AND updated_at < $1
	`, completedBefore)
	if err != nil {
		return 0, fmt.Errorf("prune Telegram inbox: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) Claim(
	ctx context.Context,
	workerID string,
	lease time.Duration,
) (inbox.Item, bool, error) {
	if workerID == "" || lease <= 0 {
		return inbox.Item{}, false, fmt.Errorf("%w: update worker and lease are required", domain.ErrInvalidInput)
	}
	leaseToken, err := randomLeaseToken()
	if err != nil {
		return inbox.Item{}, false, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return inbox.Item{}, false, fmt.Errorf("begin Telegram update claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var update inbox.Item
	err = tx.QueryRow(ctx, `
		SELECT candidate.update_id, candidate.user_key, candidate.payload, candidate.attempts + 1
		FROM telegram_updates candidate
		WHERE candidate.status = 'created' AND candidate.available_at <= now()
		  AND NOT EXISTS (
		      SELECT 1 FROM telegram_user_leases active
		      WHERE active.user_key = candidate.user_key
		        AND active.locked_until > now()
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM telegram_updates active_update
		      WHERE active_update.user_key = candidate.user_key
		        AND active_update.status = 'processing'
		        AND active_update.locked_until > now()
		  )
		ORDER BY candidate.update_id
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&update.ID, &update.OwnerKey, &update.Payload, &update.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return inbox.Item{}, false, nil
	}
	if err != nil {
		return inbox.Item{}, false, fmt.Errorf("select Telegram update: %w", err)
	}
	leaseTag, err := tx.Exec(ctx, `
		INSERT INTO telegram_user_leases(user_key, update_id, lease_token, locked_until)
		VALUES($1, $2, $3, now() + ($4 * interval '1 millisecond'))
		ON CONFLICT (user_key) DO UPDATE
		SET update_id = EXCLUDED.update_id,
		    lease_token = EXCLUDED.lease_token,
		    locked_until = EXCLUDED.locked_until
		WHERE telegram_user_leases.locked_until <= now()
	`, update.OwnerKey, update.ID, leaseToken, lease.Milliseconds())
	if err != nil {
		return inbox.Item{}, false, fmt.Errorf("acquire Telegram user lease: %w", err)
	}
	if leaseTag.RowsAffected() != 1 {
		return inbox.Item{}, false, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE telegram_updates
		SET status = 'processing', attempts = attempts + 1, locked_by = $2,
		    lease_token = $3, locked_until = now() + ($4 * interval '1 millisecond'),
		    last_error = NULL, updated_at = now()
		WHERE update_id = $1
	`, update.ID, workerID, leaseToken, lease.Milliseconds()); err != nil {
		return inbox.Item{}, false, fmt.Errorf("lease Telegram update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return inbox.Item{}, false, fmt.Errorf("commit Telegram update claim: %w", err)
	}
	update.LeaseToken = leaseToken
	return update, true, nil
}

func (r *Repository) Complete(ctx context.Context, update inbox.Item) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin Telegram update completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE telegram_updates
		SET status = 'completed', completed_at = now(), updated_at = now(),
		    locked_by = NULL, lease_token = NULL, locked_until = NULL, last_error = NULL
		WHERE update_id = $1 AND status = 'processing' AND lease_token = $2 AND locked_until > now()
	`, update.ID, update.LeaseToken)
	if err != nil {
		return fmt.Errorf("complete Telegram update: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	leaseTag, err := tx.Exec(ctx, `
		DELETE FROM telegram_user_leases
		WHERE user_key = $1 AND update_id = $2 AND lease_token = $3
	`, update.OwnerKey, update.ID, update.LeaseToken)
	if err != nil {
		return fmt.Errorf("release Telegram user lease: %w", err)
	}
	if leaseTag.RowsAffected() != 1 {
		return domain.ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Telegram update completion: %w", err)
	}
	return nil
}

func (r *Repository) Retry(
	ctx context.Context,
	update inbox.Item,
	cause string,
	delay time.Duration,
	maxAttempts int,
) (bool, error) {
	if delay < 0 || maxAttempts < 1 {
		return false, fmt.Errorf("%w: retry delay and attempts are invalid", domain.ErrInvalidInput)
	}
	terminal := update.Attempt >= maxAttempts
	status := "created"
	if terminal {
		status = "failed"
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin Telegram update retry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE telegram_updates
		SET status = $3, available_at = now() + ($4 * interval '1 millisecond'),
		    last_error = $5, locked_by = NULL, lease_token = NULL,
		    locked_until = NULL, updated_at = now()
		WHERE update_id = $1 AND status = 'processing' AND lease_token = $2 AND locked_until > now()
	`, update.ID, update.LeaseToken, status, delay.Milliseconds(), truncateError(cause))
	if err != nil {
		return false, fmt.Errorf("retry Telegram update: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, domain.ErrLeaseLost
	}
	leaseTag, err := tx.Exec(ctx, `
		DELETE FROM telegram_user_leases
		WHERE user_key = $1 AND update_id = $2 AND lease_token = $3
	`, update.OwnerKey, update.ID, update.LeaseToken)
	if err != nil {
		return false, fmt.Errorf("release Telegram user lease after retry: %w", err)
	}
	if leaseTag.RowsAffected() != 1 {
		return false, domain.ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit Telegram update retry: %w", err)
	}
	return terminal, nil
}
