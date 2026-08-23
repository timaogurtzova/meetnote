package telegram_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/domain"
	"github.com/timaogurtzova/meetnote/internal/telegram"
)

const telegramMessageLimit = 4000

func TestHandlerRoutesRequiredCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		text      string
		wantCall  string
		wantID    int64
		wantValue string
		wantKey   string
	}{
		{name: "start", text: "/start", wantCall: "start"},
		{name: "load", text: "/load"},
		{name: "list", text: "/list", wantCall: "list"},
		{name: "status with bot suffix", text: "/status@meetnote_bot 42", wantCall: "status", wantID: 42},
		{name: "get", text: "/get 42", wantCall: "get", wantID: 42},
		{name: "find", text: "/find релиз в пятницу", wantCall: "find", wantValue: "релиз в пятницу"},
		{name: "find after newline", text: "/find\nрелиз в пятницу", wantCall: "find", wantValue: "релиз в пятницу"},
		{name: "chat", text: "/chat кто отвечает?", wantCall: "chat", wantValue: "кто отвечает?", wantKey: "telegram:update:7"},
		{name: "retry", text: "/retry 42", wantCall: "retry", wantID: 42, wantKey: "telegram:update:7"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			application := &fakeApplication{}
			api := &fakeAPI{}
			handler := newTestHandler(t, application, api)

			err := handler.Handle(context.Background(), privateTextUpdate(7, 1001, test.text))
			require.NoError(t, err)
			assert.Equal(t, test.wantCall, application.call)
			if test.wantCall != "" && application.userID != "telegram:1001" {
				t.Fatalf("user id = %q", application.userID)
			}
			if application.meetingID != test.wantID {
				t.Fatalf("meeting id = %d, want %d", application.meetingID, test.wantID)
			}
			if application.value != test.wantValue {
				t.Fatalf("value = %q, want %q", application.value, test.wantValue)
			}
			if application.requestKey != test.wantKey {
				t.Fatalf("request key = %q, want %q", application.requestKey, test.wantKey)
			}
			if len(api.messages) == 0 {
				t.Fatal("bot sent no response")
			}
		})
	}
}

func TestHandlerLoadsVoiceMessage(t *testing.T) {
	t.Parallel()
	application := &fakeApplication{}
	api := &fakeAPI{
		remoteFile:  telegram.RemoteFile{FilePath: "voice/file_1.oga", FileSize: 13},
		fileContent: "meeting text",
	}
	handler := newTestHandler(t, application, api)
	update := telegram.Update{
		UpdateID: 55,
		Message: &telegram.Message{
			From: &telegram.User{ID: 9001},
			Chat: telegram.Chat{ID: 9001, Type: "private"},
			Voice: &telegram.Attachment{
				FileID:   "telegram-file",
				MIMEType: "audio/ogg",
				FileSize: 13,
			},
		},
	}

	if err := handler.Handle(context.Background(), update); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if application.call != "load" || application.userID != "telegram:9001" {
		t.Fatalf("load call = %q, user = %q", application.call, application.userID)
	}
	if application.filename != "voice-55.ogg" || application.upload != "meeting text" || application.size != 13 {
		t.Fatalf("upload filename=%q content=%q size=%d", application.filename, application.upload, application.size)
	}
	if application.requestKey != "telegram:update:55" {
		t.Fatalf("load request key = %q", application.requestKey)
	}
	if api.requestedFileID != "telegram-file" || api.openedFilePath != "voice/file_1.oga" {
		t.Fatalf("download calls: file id=%q path=%q", api.requestedFileID, api.openedFilePath)
	}
}

func TestHandlerRejectsOversizedFileBeforeDownload(t *testing.T) {
	t.Parallel()
	application := &fakeApplication{}
	api := &fakeAPI{}
	handler := newTestHandler(t, application, api)
	update := telegram.Update{
		UpdateID: 56,
		Message: &telegram.Message{
			From:     &telegram.User{ID: 1},
			Chat:     telegram.Chat{ID: 1, Type: "private"},
			Document: &telegram.Attachment{FileID: "large", FileName: "large.mp3", FileSize: 21 * 1024 * 1024},
		},
	}

	if err := handler.Handle(context.Background(), update); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if application.call != "" || api.requestedFileID != "" {
		t.Fatalf("oversized upload reached application/API: call=%q file=%q", application.call, api.requestedFileID)
	}
	if len(api.messages) != 1 || !strings.Contains(api.messages[0], "20 МБ") {
		t.Fatalf("response = %#v", api.messages)
	}
}

func TestHandlerReturnsClearQuotaError(t *testing.T) {
	t.Parallel()
	application := &fakeApplication{err: domain.ErrQuotaExceeded}
	api := &fakeAPI{
		remoteFile:  telegram.RemoteFile{FilePath: "documents/meeting.txt", FileSize: 5},
		fileContent: "notes",
	}
	handler := newTestHandler(t, application, api)
	update := telegram.Update{
		UpdateID: 57,
		Message: &telegram.Message{
			From: &telegram.User{ID: 1}, Chat: telegram.Chat{ID: 1, Type: "private"},
			Document: &telegram.Attachment{FileID: "quota", FileName: "meeting.txt", FileSize: 5},
		},
	}

	if err := handler.Handle(context.Background(), update); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if len(api.messages) != 1 || !strings.Contains(api.messages[0], "достигнут лимит") {
		t.Fatalf("quota response = %#v", api.messages)
	}
}

func TestHandlerReturnsRussianCommandError(t *testing.T) {
	t.Parallel()
	application := &fakeApplication{}
	api := &fakeAPI{}
	handler := newTestHandler(t, application, api)

	if err := handler.Handle(context.Background(), privateTextUpdate(58, 1, "/status abc")); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if application.call != "" || len(api.messages) != 1 {
		t.Fatalf("invalid command reached application or returned no message: call=%q messages=%#v", application.call, api.messages)
	}
	if strings.Contains(api.messages[0], "invalid input") || !strings.Contains(api.messages[0], "положительным числом") {
		t.Fatalf("command error = %q", api.messages[0])
	}
}

func TestHandlerReportsConfiguredFileLimit(t *testing.T) {
	t.Parallel()
	application := &fakeApplication{}
	api := &fakeAPI{}
	handler, err := telegram.NewHandler(
		application,
		api,
		telegram.HandlerConfig{
			RequestTimeout:        time.Second,
			DownloadTimeout:       time.Second,
			MaxFileSize:           5 * 1024 * 1024,
			InlineTranscriptRunes: 12_000,
			MaxResponseParts:      8,
		},
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	update := telegram.Update{
		UpdateID: 57,
		Message: &telegram.Message{
			From:     &telegram.User{ID: 1},
			Chat:     telegram.Chat{ID: 1, Type: "private"},
			Document: &telegram.Attachment{FileID: "large", FileName: "large.mp3", FileSize: 6 * 1024 * 1024},
		},
	}

	if err := handler.Handle(context.Background(), update); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if len(api.messages) != 1 || !strings.Contains(api.messages[0], "5 МБ") {
		t.Fatalf("response = %#v", api.messages)
	}
}

func TestHandlerDoesNotExposeMeetingsInGroupChats(t *testing.T) {
	t.Parallel()
	application := &fakeApplication{}
	api := &fakeAPI{}
	handler := newTestHandler(t, application, api)
	update := privateTextUpdate(10, 77, "/list")
	update.Message.Chat.Type = "group"

	if err := handler.Handle(context.Background(), update); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if application.call != "" {
		t.Fatalf("application call = %q", application.call)
	}
	if len(api.messages) != 1 || !strings.Contains(api.messages[0], "личных чатах") {
		t.Fatalf("response = %#v", api.messages)
	}
}

func TestHandlerSplitsLongTranscript(t *testing.T) {
	t.Parallel()
	application := &fakeApplication{transcript: strings.Repeat("я", telegramMessageLimit+100)}
	api := &fakeAPI{}
	handler := newTestHandler(t, application, api)

	if err := handler.Handle(context.Background(), privateTextUpdate(11, 88, "/get 1")); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if len(api.messages) != 2 {
		t.Fatalf("message chunks = %d, want 2", len(api.messages))
	}
	for _, message := range api.messages {
		if len([]rune(message)) > telegramMessageLimit {
			t.Fatalf("chunk contains %d runes", len([]rune(message)))
		}
	}
}

func TestHandlerSendsVeryLongTranscriptAsDocument(t *testing.T) {
	t.Parallel()
	transcript := strings.Repeat("я", 12_001)
	application := &fakeApplication{transcript: transcript}
	api := &fakeAPI{}
	handler := newTestHandler(t, application, api)

	if err := handler.Handle(context.Background(), privateTextUpdate(12, 88, "/get 1")); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if len(api.messages) != 0 || len(api.documents) != 1 || api.documents[0] != transcript {
		t.Fatalf("messages=%d documents=%d", len(api.messages), len(api.documents))
	}
}

func TestHandlerFormatsMeetingTimeInUTC(t *testing.T) {
	t.Parallel()
	value := time.Date(2026, time.August, 20, 18, 30, 0, 0, time.FixedZone("MSK", 3*60*60))
	application := &fakeApplication{statusMeeting: domain.Meeting{Status: domain.StatusCompleted, CreatedAt: value, UpdatedAt: value}}
	api := &fakeAPI{}
	handler := newTestHandler(t, application, api)

	if err := handler.Handle(context.Background(), privateTextUpdate(13, 88, "/status 1")); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if len(api.messages) != 1 || strings.Count(api.messages[0], "2026-08-20 15:30:00 UTC") != 2 {
		t.Fatalf("status response = %q", strings.Join(api.messages, "\n"))
	}
}

func TestHandlerRemovesPathComponentsFromFilename(t *testing.T) {
	t.Parallel()
	application := &fakeApplication{}
	api := &fakeAPI{
		remoteFile:  telegram.RemoteFile{FilePath: "documents/notes.md", FileSize: 5},
		fileContent: "notes",
	}
	handler := newTestHandler(t, application, api)
	update := telegram.Update{
		UpdateID: 59,
		Message: &telegram.Message{
			From: &telegram.User{ID: 1},
			Chat: telegram.Chat{ID: 1, Type: "private"},
			Document: &telegram.Attachment{
				FileID: "document", FileName: filepath.Join("..", "..", "notes.md"), FileSize: 5,
			},
		},
	}

	if err := handler.Handle(context.Background(), update); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}
	if application.filename != "notes.md" {
		t.Fatalf("stored filename = %q", application.filename)
	}
}

func newTestHandler(t *testing.T, application telegram.Application, api telegram.API) *telegram.Handler {
	t.Helper()
	handler, err := telegram.NewHandler(
		application,
		api,
		telegram.HandlerConfig{
			RequestTimeout:        time.Second,
			DownloadTimeout:       time.Second,
			MaxFileSize:           20 * 1024 * 1024,
			InlineTranscriptRunes: 12_000,
			MaxResponseParts:      8,
		},
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func privateTextUpdate(updateID, userID int64, text string) telegram.Update {
	return telegram.Update{
		UpdateID: updateID,
		Message: &telegram.Message{
			From: &telegram.User{ID: userID},
			Chat: telegram.Chat{ID: userID, Type: "private"},
			Text: text,
		},
	}
}

type fakeApplication struct {
	call          string
	userID        string
	meetingID     int64
	value         string
	filename      string
	upload        string
	size          int64
	transcript    string
	requestKey    string
	statusMeeting domain.Meeting
	err           error
}

func (f *fakeApplication) Start(_ context.Context, userID string) error {
	f.call, f.userID = "start", userID
	return f.err
}

func (f *fakeApplication) Load(
	_ context.Context,
	userID, filename string,
	source io.Reader,
	size int64,
	requestKey string,
) (domain.Meeting, error) {
	f.call, f.userID, f.filename, f.size = "load", userID, filename, size
	f.requestKey = requestKey
	content, _ := io.ReadAll(source)
	f.upload = string(content)
	return domain.Meeting{ID: 9, Status: domain.StatusCreated}, f.err
}

func (f *fakeApplication) List(_ context.Context, userID string) ([]domain.Meeting, error) {
	f.call, f.userID = "list", userID
	return []domain.Meeting{{ID: 1, Status: domain.StatusCompleted, CreatedAt: time.Now()}}, f.err
}

func (f *fakeApplication) Status(_ context.Context, userID string, meetingID int64) (domain.Meeting, error) {
	f.call, f.userID, f.meetingID = "status", userID, meetingID
	if f.statusMeeting.Status != "" {
		f.statusMeeting.ID = meetingID
		return f.statusMeeting, f.err
	}
	return domain.Meeting{ID: meetingID, Status: domain.StatusProcessing, CreatedAt: time.Now(), UpdatedAt: time.Now()}, f.err
}

func (f *fakeApplication) Get(_ context.Context, userID string, meetingID int64) (string, error) {
	f.call, f.userID, f.meetingID = "get", userID, meetingID
	if f.transcript != "" {
		return f.transcript, f.err
	}
	return "transcript", f.err
}

func (f *fakeApplication) Find(_ context.Context, userID, query string) ([]domain.SearchResult, error) {
	f.call, f.userID, f.value = "find", userID, query
	return []domain.SearchResult{{MeetingID: 1, Status: domain.StatusCompleted, CreatedAt: time.Now(), Snippet: "релиз"}}, f.err
}

func (f *fakeApplication) Chat(_ context.Context, userID, question, requestKey string) (string, error) {
	f.call, f.userID, f.value = "chat", userID, question
	f.requestKey = requestKey
	return "answer", f.err
}

func (f *fakeApplication) Retry(_ context.Context, userID, requestKey string, meetingID int64) error {
	f.call, f.userID, f.meetingID = "retry", userID, meetingID
	f.requestKey = requestKey
	return f.err
}

type fakeAPI struct {
	messages        []string
	documents       []string
	requestedFileID string
	openedFilePath  string
	remoteFile      telegram.RemoteFile
	fileContent     string
}

func (f *fakeAPI) DeleteWebhook(context.Context) error                      { return nil }
func (f *fakeAPI) SetCommands(context.Context, []telegram.BotCommand) error { return nil }
func (f *fakeAPI) GetUpdates(context.Context, int64, time.Duration) ([]telegram.Update, error) {
	return nil, nil
}
func (f *fakeAPI) SendMessage(_ context.Context, _ int64, text string) error {
	f.messages = append(f.messages, text)
	return nil
}
func (f *fakeAPI) SendDocument(_ context.Context, _ int64, _ string, content io.Reader, _ int64, _ string) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.documents = append(f.documents, string(data))
	return nil
}
func (f *fakeAPI) GetFile(_ context.Context, fileID string) (telegram.RemoteFile, error) {
	f.requestedFileID = fileID
	return f.remoteFile, nil
}
func (f *fakeAPI) OpenFile(_ context.Context, filePath string) (io.ReadCloser, error) {
	f.openedFilePath = filePath
	return io.NopCloser(strings.NewReader(f.fileContent)), nil
}
