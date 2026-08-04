// Пакет app реализует сценарии приложения и объявляет интерфейсы внешних зависимостей.
package app

import (
	"context"
	"io"
	"time"

	"github.com/timaogurtzova/meetnote/internal/domain"
)

// Repository описывает хранилище для пользовательских сценариев.
type Repository interface {
	RegisterUser(ctx context.Context, userID string) error
	CreateMeeting(ctx context.Context, userID, requestKey string, file domain.StoredFile, quota domain.MeetingQuota) (domain.Meeting, bool, error)
	ListMeetings(ctx context.Context, userID string, limit int) ([]domain.Meeting, error)
	GetMeeting(ctx context.Context, userID string, meetingID int64) (domain.Meeting, error)
	GetTranscript(ctx context.Context, userID string, meetingID int64) (string, error)
	FindMeetings(ctx context.Context, userID, query string, limit int) ([]domain.SearchResult, error)
	ChatDocuments(ctx context.Context, userID string, limit int) ([]domain.ChatDocument, error)
	GetChatAnswer(ctx context.Context, userID, requestKey string) (string, bool, error)
	SaveChat(ctx context.Context, userID, requestKey, question, answer string, maxHistory int) (string, error)
	RetryMeeting(ctx context.Context, userID, requestKey string, meetingID int64, maxPending int) error
	ListStoredPaths(ctx context.Context) ([]string, error)
}

// TaskRepository описывает хранилище задач фоновой обработки.
type TaskRepository interface {
	RecoverInterrupted(ctx context.Context) (int64, error)
	ClaimNextTask(ctx context.Context, workerID string, leaseDuration time.Duration) (domain.Task, bool, error)
	RefreshLease(ctx context.Context, task domain.Task, leaseDuration time.Duration) error
	SaveTranscription(ctx context.Context, task domain.Task, transcript string) error
	SaveSummary(ctx context.Context, task domain.Task, summary string) error
	CompleteTask(ctx context.Context, task domain.Task) error
	FailTask(ctx context.Context, task domain.Task, failure domain.ProcessingFailure) error
}

// FileStore сохраняет загрузки в управляемом приложением хранилище.
type FileStore interface {
	Save(ctx context.Context, userID, filename string, source io.Reader, size int64) (domain.StoredFile, error)
	Remove(ctx context.Context, path string) error
	RemoveOrphans(ctx context.Context, keep map[string]struct{}, olderThan time.Time) (int, error)
}

// SpeechClient описывает клиент распознавания речи.
type SpeechClient interface {
	Transcribe(ctx context.Context, filePath string) (string, error)
}

// LLMClient описывает клиент для выжимок и ответов по материалам встреч.
type LLMClient interface {
	Summarize(ctx context.Context, transcript string) (string, error)
	Answer(ctx context.Context, question string, documents []domain.ChatDocument) (string, error)
}
