// Пакет inbox описывает надежную очередь входящих событий, не зависящую от транспорта.
package inbox

import (
	"context"
	"time"
)

// Item хранит данные одного события и признак права на его обработку.
type Item struct {
	ID         int64
	OwnerKey   string
	Payload    []byte
	Attempt    int
	LeaseToken string
}

// Repository сохраняет события до их подтверждения и выдает их ограниченному пулу обработчиков.
type Repository interface {
	NextOffset(ctx context.Context) (int64, error)
	Enqueue(ctx context.Context, items []Item) error
	RecoverExpired(ctx context.Context) (int64, error)
	Prune(ctx context.Context, completedBefore time.Time) (int64, error)
	Claim(ctx context.Context, workerID string, lease time.Duration) (Item, bool, error)
	Complete(ctx context.Context, item Item) error
	Retry(ctx context.Context, item Item, cause string, delay time.Duration, maxAttempts int) (bool, error)
}
