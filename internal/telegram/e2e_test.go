package telegram_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/app"
	"github.com/timaogurtzova/meetnote/internal/clients/mock"
	"github.com/timaogurtzova/meetnote/internal/domain"
	"github.com/timaogurtzova/meetnote/internal/postgres"
	"github.com/timaogurtzova/meetnote/internal/storage"
	"github.com/timaogurtzova/meetnote/internal/telegram"
	"github.com/timaogurtzova/meetnote/internal/worker"
)

func TestTelegramFullBusinessScenario(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := telegramTestPool(t, ctx, databaseURL)
	require.NoError(t, postgres.Migrate(ctx, pool))
	repository, err := postgres.NewRepository(pool)
	require.NoError(t, err)
	fileStore, err := storage.NewLocal(t.TempDir(), 20*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	llm := mock.NewLLM(time.Millisecond)
	service, err := app.NewService(
		repository,
		fileStore,
		llm,
		app.Limits{
			MaxQuestionRunes: 2000, MaxSearchQueryRunes: 500, MaxAnswerRunes: 8000, MaxChatHistory: 1000,
			MeetingQuota: domain.MeetingQuota{MaxMeetings: 100, MaxPending: 10, MaxStorageBytes: 200 * 1024 * 1024},
		},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		remoteFile:  telegram.RemoteFile{FilePath: "documents/meeting.txt", FileSize: 63},
		fileContent: "Анна отвечает за релиз. Релиз запланирован на пятницу.",
	}
	handler := newTestHandler(t, service, api)
	upload := telegram.Update{
		UpdateID: 100,
		Message: &telegram.Message{
			From: &telegram.User{ID: 42},
			Chat: telegram.Chat{ID: 42, Type: "private"},
			Document: &telegram.Attachment{
				FileID:   "meeting-file",
				FileName: "meeting.txt",
				MIMEType: "text/plain",
				FileSize: 63,
			},
		},
	}
	if err := handler.Handle(ctx, upload); err != nil {
		t.Fatalf("upload Handle() error: %v", err)
	}

	workerPool, err := worker.NewPool(
		repository,
		mock.NewSpeech(time.Millisecond),
		llm,
		worker.Config{
			Count:              2,
			PollInterval:       5 * time.Millisecond,
			SpeechTimeout:      time.Second,
			LLMTimeout:         time.Second,
			LeaseDuration:      time.Second,
			HeartbeatInterval:  50 * time.Millisecond,
			RecoveryInterval:   100 * time.Millisecond,
			MaxTranscriptRunes: 500_000,
			MaxSummaryRunes:    12_000,
		},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- workerPool.Run(workerCtx) }()

	meetingID := int64(1)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		meeting, err := service.Status(ctx, "telegram:42", meetingID)
		if err == nil && meeting.Status == domain.StatusCompleted {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("meeting was not completed: status=%s error=%v", meeting.Status, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancelWorker()
	if err := <-workerDone; err != nil {
		t.Fatalf("worker Run() error: %v", err)
	}

	api.messages = nil
	for index, command := range []string{"/list", "/status 1", "/get 1", "/find релиз", "/chat Кто отвечает за релиз?"} {
		if err := handler.Handle(ctx, privateTextUpdate(int64(101+index), 42, command)); err != nil {
			t.Fatalf("%s Handle() error: %v", command, err)
		}
	}
	joined := strings.Join(api.messages, "\n")
	for _, expected := range []string{"#1", "completed", "Анна отвечает за релиз", "Результаты поиска", "По материалам встречи #1"} {
		assert.Contains(t, joined, expected)
	}
}

func telegramTestPool(
	t *testing.T,
	ctx context.Context,
	databaseURL string,
) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	schema := fmt.Sprintf("telegram_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		_, _ = admin.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
