// Пакет domain содержит общие бизнес-сущности приложения.
package domain

import "time"

// Status описывает этап обработки встречи.
type Status string

const (
	StatusCreated     Status = "created"
	StatusProcessing  Status = "processing"
	StatusTranscribed Status = "transcribed"
	StatusSummarized  Status = "summarized"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
)

// Meeting содержит данные встречи, которые можно показать пользователю.
type Meeting struct {
	ID               int64
	OriginalFilename string
	Status           Status
	Summary          string
	Error            string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// SearchResult описывает одну найденную встречу.
type SearchResult struct {
	MeetingID int64
	Status    Status
	Snippet   string
	CreatedAt time.Time
}

// Task содержит данные задачи, захваченной фоновым обработчиком.
type Task struct {
	ID         int64
	MeetingID  int64
	StoredPath string
	Attempt    int
	LeaseToken string
}

// ChatDocument хранит ограниченный фрагмент контекста для LLM-клиента.
type ChatDocument struct {
	MeetingID  int64
	Transcript string
	Summary    string
}

// StoredFile описывает файл в управляемом приложением хранилище.
type StoredFile struct {
	OriginalFilename string
	Path             string
	Size             int64
}

// MeetingQuota ограничивает число встреч, длину очереди и объем файлов одного пользователя.
type MeetingQuota struct {
	MaxMeetings     int
	MaxPending      int
	MaxStorageBytes int64
}

// ProcessingFailure содержит безопасное для базы данных и ответа пользователю описание ошибки.
type ProcessingFailure struct {
	Code    string
	Message string
}
