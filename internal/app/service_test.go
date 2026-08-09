package app_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/app"
	"github.com/timaogurtzova/meetnote/internal/domain"
)

func TestServiceLoadCreatesMeetingAndTask(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{}
	files := &fakeFileStore{stored: domain.StoredFile{OriginalFilename: "meeting.txt", Path: "/safe/meeting.txt", Size: 42}}
	service := newTestService(t, repository, files, &fakeLLM{})

	meeting, err := service.Load(context.Background(), " alice ", "meeting.txt", strings.NewReader("notes"), 5, "telegram:update:1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), meeting.ID)
	assert.Equal(t, domain.StatusCreated, meeting.Status)
	if repository.createdUser != "alice" || repository.createdFile.Path != files.stored.Path {
		t.Fatalf("repository received user %q and file %#v", repository.createdUser, repository.createdFile)
	}
	if files.filename != "meeting.txt" || files.content != "notes" || files.size != 5 {
		t.Fatalf("file store received filename=%q content=%q size=%d", files.filename, files.content, files.size)
	}
}

func TestServiceLoadRetainsFileWhenCommitOutcomeMayBeAmbiguous(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{createErr: errors.New("database unavailable")}
	files := &fakeFileStore{stored: domain.StoredFile{OriginalFilename: "meeting.txt", Path: "/safe/meeting.txt"}}
	service := newTestService(t, repository, files, &fakeLLM{})

	_, err := service.Load(context.Background(), "alice", "meeting.txt", strings.NewReader("notes"), 5, "telegram:update:2")
	require.Error(t, err)
	if files.removed != "" {
		t.Fatalf("ambiguous transaction result removed %q", files.removed)
	}
}

func TestServiceLoadRemovesFileRejectedByQuota(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{createErr: domain.ErrQuotaExceeded}
	files := &fakeFileStore{stored: domain.StoredFile{OriginalFilename: "meeting.txt", Path: "/safe/rejected.txt", Size: 5}}
	service := newTestService(t, repository, files, &fakeLLM{})

	_, err := service.Load(context.Background(), "alice", "meeting.txt", strings.NewReader("notes"), 5, "telegram:update:quota")
	if !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("Load() error = %v, want quota exceeded", err)
	}
	if files.removed != files.stored.Path || files.removeCanceled || !files.removeHasDeadline {
		t.Fatalf("quota cleanup path=%q canceled=%v deadline=%v", files.removed, files.removeCanceled, files.removeHasDeadline)
	}
}

func TestServiceLoadRemovesSecondFileForDuplicateRequest(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{duplicateCreate: true}
	files := &fakeFileStore{stored: domain.StoredFile{OriginalFilename: "meeting.txt", Path: "/safe/duplicate.txt"}}
	service := newTestService(t, repository, files, &fakeLLM{})

	meeting, err := service.Load(context.Background(), "alice", "meeting.txt", strings.NewReader("notes"), 5, "telegram:update:10")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if meeting.ID != 1 || files.removed != files.stored.Path {
		t.Fatalf("duplicate result=%#v removed=%q", meeting, files.removed)
	}
	if files.removeCanceled || !files.removeHasDeadline {
		t.Fatalf("duplicate cleanup context: canceled=%v, has deadline=%v", files.removeCanceled, files.removeHasDeadline)
	}
}

func TestServiceChatUsesOnlyRepositoryContextAndStoresAnswer(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{
		documents: []domain.ChatDocument{{MeetingID: 7, Transcript: "Анна отвечает за релиз."}},
	}
	llm := &fakeLLM{answer: "Анна отвечает за релиз."}
	service := newTestService(t, repository, &fakeFileStore{}, llm)

	answer, err := service.Chat(context.Background(), "alice", "Кто отвечает за релиз?", "telegram:update:3")
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if answer != llm.answer || repository.savedQuestion == "" || repository.savedAnswer != llm.answer || repository.savedMaxHistory != 1000 {
		t.Fatalf("answer was not persisted: %q, %q", repository.savedQuestion, repository.savedAnswer)
	}
	if len(llm.documents) != 1 || llm.documents[0].MeetingID != 7 {
		t.Fatalf("LLM documents = %#v", llm.documents)
	}
}

func TestServiceValidatesInputAndState(t *testing.T) {
	t.Parallel()
	service := newTestService(t, &fakeRepository{}, &fakeFileStore{}, &fakeLLM{})
	if err := service.Start(context.Background(), ""); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("Start() error = %v, want invalid input", err)
	}
	if _, err := service.Status(context.Background(), "alice", 0); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("Status() error = %v, want invalid input", err)
	}
	if _, err := service.Chat(context.Background(), "alice", "question", "telegram:update:4"); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("Chat() error = %v, want not ready", err)
	}
}

func TestServiceRejectsEmptyLLMAnswer(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{documents: []domain.ChatDocument{{MeetingID: 1, Transcript: "notes"}}}
	service := newTestService(t, repository, &fakeFileStore{}, &fakeLLM{answer: "  "})

	_, err := service.Chat(context.Background(), "alice", "question", "telegram:update:20")
	if !errors.Is(err, domain.ErrInvalidClientData) {
		t.Fatalf("Chat() error = %v, want invalid client data", err)
	}
	if repository.savedAnswer != "" {
		t.Fatalf("empty LLM answer was persisted: %q", repository.savedAnswer)
	}
}

func TestServiceReturnsStoredAnswerWithoutSecondLLMCall(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{previousAnswer: "stored", previousFound: true}
	llm := &fakeLLM{answer: "new"}
	service := newTestService(t, repository, &fakeFileStore{}, llm)

	answer, err := service.Chat(context.Background(), "alice", "question", "telegram:update:21")
	if err != nil || answer != "stored" {
		t.Fatalf("Chat() = %q, %v", answer, err)
	}
	if llm.answerCalls != 0 {
		t.Fatalf("LLM calls = %d, want 0", llm.answerCalls)
	}
}

func TestServiceReadUseCasesNormalizeAndScopeUser(t *testing.T) {
	t.Parallel()
	meeting := domain.Meeting{ID: 7, Status: domain.StatusCompleted}
	searchResult := domain.SearchResult{MeetingID: 7, Status: domain.StatusCompleted, Snippet: "release"}
	repository := &fakeRepository{
		meetings:     []domain.Meeting{meeting},
		meeting:      meeting,
		transcript:   "meeting transcript",
		searchResult: []domain.SearchResult{searchResult},
	}
	service := newTestService(t, repository, &fakeFileStore{}, &fakeLLM{})

	meetings, err := service.List(context.Background(), " alice ")
	if err != nil || len(meetings) != 1 || meetings[0].ID != meeting.ID {
		t.Fatalf("List() = %#v, %v", meetings, err)
	}
	if repository.listUser != "alice" || repository.listLimit != 100 {
		t.Fatalf("List() repository args user=%q limit=%d", repository.listUser, repository.listLimit)
	}

	gotMeeting, err := service.Status(context.Background(), " alice ", meeting.ID)
	if err != nil || gotMeeting.ID != meeting.ID || repository.getUser != "alice" {
		t.Fatalf("Status() = %#v, %v, repository user=%q", gotMeeting, err, repository.getUser)
	}

	transcript, err := service.Get(context.Background(), " alice ", meeting.ID)
	if err != nil || transcript != repository.transcript || repository.transcriptUser != "alice" {
		t.Fatalf("Get() = %q, %v, repository user=%q", transcript, err, repository.transcriptUser)
	}

	result, err := service.Find(context.Background(), " alice ", " release ")
	if err != nil || len(result) != 1 || result[0].MeetingID != meeting.ID {
		t.Fatalf("Find() = %#v, %v", result, err)
	}
	if repository.findUser != "alice" || repository.findQuery != "release" || repository.findLimit != 100 {
		t.Fatalf("Find() repository args user=%q query=%q limit=%d", repository.findUser, repository.findQuery, repository.findLimit)
	}
	if repository.registerCalls != 4 {
		t.Fatalf("RegisterUser() calls = %d, want 4", repository.registerCalls)
	}
}

func TestServiceReadUseCasesWrapRepositoryErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("database offline")
	repository := &fakeRepository{
		listErr: sentinel, meetingErr: sentinel, transcriptErr: sentinel, findErr: sentinel,
	}
	service := newTestService(t, repository, &fakeFileStore{}, &fakeLLM{})

	checks := []struct {
		name string
		call func() error
	}{
		{name: "list", call: func() error { _, err := service.List(context.Background(), "alice"); return err }},
		{name: "status", call: func() error { _, err := service.Status(context.Background(), "alice", 1); return err }},
		{name: "get", call: func() error { _, err := service.Get(context.Background(), "alice", 1); return err }},
		{name: "find", call: func() error { _, err := service.Find(context.Background(), "alice", "release"); return err }},
	}
	for _, check := range checks {
		if err := check.call(); !errors.Is(err, sentinel) {
			t.Errorf("%s error = %v, want wrapped sentinel", check.name, err)
		}
	}
}

func TestServiceRetryIsUserScopedAndIdempotencyKeyIsNormalized(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{}
	service := newTestService(t, repository, &fakeFileStore{}, &fakeLLM{})

	if err := service.Retry(context.Background(), " alice ", " retry-key ", 9); err != nil {
		t.Fatalf("Retry() error: %v", err)
	}
	if repository.retryUser != "alice" || repository.retryKey != "retry-key" || repository.retryMeetingID != 9 {
		t.Fatalf("Retry() repository args user=%q key=%q meeting=%d", repository.retryUser, repository.retryKey, repository.retryMeetingID)
	}
}

func TestServiceCleanupOrphansPassesReferencedPathsAndCutoff(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{storedPaths: []string{"/keep/a", "/keep/b"}}
	files := &fakeFileStore{orphanRemoved: 2}
	service := newTestService(t, repository, files, &fakeLLM{})
	before := time.Now().Add(-time.Hour)

	removed, err := service.CleanupOrphans(context.Background(), time.Hour)
	if err != nil || removed != 2 {
		t.Fatalf("CleanupOrphans() = %d, %v", removed, err)
	}
	if _, ok := files.orphanKeep["/keep/a"]; !ok {
		t.Fatalf("CleanupOrphans() keep = %#v", files.orphanKeep)
	}
	if files.orphanCutoff.Before(before.Add(-time.Second)) || files.orphanCutoff.After(time.Now().Add(-time.Hour+time.Second)) {
		t.Fatalf("CleanupOrphans() cutoff = %s", files.orphanCutoff)
	}
}

func TestServiceRejectsInvalidReadRetryAndCleanupInputsBeforeRepository(t *testing.T) {
	t.Parallel()
	repository := &fakeRepository{}
	service := newTestService(t, repository, &fakeFileStore{}, &fakeLLM{})

	if _, err := service.Get(context.Background(), "alice", 0); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("Get() error = %v", err)
	}
	if _, err := service.Find(context.Background(), "alice", strings.Repeat("x", 501)); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("Find() error = %v", err)
	}
	if err := service.Retry(context.Background(), "alice", "key", 0); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("Retry() error = %v", err)
	}
	if _, err := service.CleanupOrphans(context.Background(), 0); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("CleanupOrphans() error = %v", err)
	}
	if repository.registerCalls != 0 {
		t.Fatalf("invalid calls reached repository: RegisterUser() calls = %d", repository.registerCalls)
	}
}

func newTestService(t *testing.T, repository app.Repository, files app.FileStore, llm app.LLMClient) *app.Service {
	t.Helper()
	service, err := app.NewService(repository, files, llm, app.Limits{
		MaxQuestionRunes: 2000, MaxSearchQueryRunes: 500, MaxAnswerRunes: 8000, MaxChatHistory: 1000,
		MeetingQuota: domain.MeetingQuota{MaxMeetings: 100, MaxPending: 10, MaxStorageBytes: 200 * 1024 * 1024},
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type fakeRepository struct {
	registered      string
	registerCalls   int
	createdUser     string
	createdFile     domain.StoredFile
	createdQuota    domain.MeetingQuota
	createErr       error
	duplicateCreate bool
	documents       []domain.ChatDocument
	savedQuestion   string
	savedAnswer     string
	savedMaxHistory int
	previousAnswer  string
	previousFound   bool
	meetings        []domain.Meeting
	meeting         domain.Meeting
	transcript      string
	searchResult    []domain.SearchResult
	storedPaths     []string
	listUser        string
	listLimit       int
	getUser         string
	transcriptUser  string
	findUser        string
	findQuery       string
	findLimit       int
	retryUser       string
	retryKey        string
	retryMeetingID  int64
	listErr         error
	meetingErr      error
	transcriptErr   error
	findErr         error
	retryErr        error
	storedPathsErr  error
}

func (f *fakeRepository) RegisterUser(_ context.Context, userID string) error {
	f.registered = userID
	f.registerCalls++
	return nil
}

func (f *fakeRepository) CreateMeeting(
	_ context.Context,
	userID, _ string,
	file domain.StoredFile,
	quota domain.MeetingQuota,
) (domain.Meeting, bool, error) {
	f.createdUser = userID
	f.createdFile = file
	f.createdQuota = quota
	if f.createErr != nil {
		return domain.Meeting{}, false, f.createErr
	}
	now := time.Now()
	return domain.Meeting{ID: 1, Status: domain.StatusCreated, CreatedAt: now, UpdatedAt: now}, !f.duplicateCreate, nil
}

func (f *fakeRepository) ListMeetings(_ context.Context, userID string, limit int) ([]domain.Meeting, error) {
	f.listUser, f.listLimit = userID, limit
	return f.meetings, f.listErr
}

func (f *fakeRepository) GetMeeting(_ context.Context, userID string, _ int64) (domain.Meeting, error) {
	f.getUser = userID
	return f.meeting, f.meetingErr
}

func (f *fakeRepository) GetTranscript(_ context.Context, userID string, _ int64) (string, error) {
	f.transcriptUser = userID
	return f.transcript, f.transcriptErr
}

func (f *fakeRepository) FindMeetings(_ context.Context, userID, query string, limit int) ([]domain.SearchResult, error) {
	f.findUser, f.findQuery, f.findLimit = userID, query, limit
	return f.searchResult, f.findErr
}

func (f *fakeRepository) ChatDocuments(context.Context, string, int) ([]domain.ChatDocument, error) {
	return f.documents, nil
}

func (f *fakeRepository) GetChatAnswer(context.Context, string, string) (string, bool, error) {
	return f.previousAnswer, f.previousFound, nil
}

func (f *fakeRepository) SaveChat(_ context.Context, _, _ string, question, answer string, maxHistory int) (string, error) {
	f.savedQuestion = question
	f.savedAnswer = answer
	f.savedMaxHistory = maxHistory
	return answer, nil
}

func (f *fakeRepository) RetryMeeting(_ context.Context, userID, requestKey string, meetingID int64, _ int) error {
	f.retryUser, f.retryKey, f.retryMeetingID = userID, requestKey, meetingID
	return f.retryErr
}

func (f *fakeRepository) ListStoredPaths(context.Context) ([]string, error) {
	return f.storedPaths, f.storedPathsErr
}

type fakeFileStore struct {
	stored            domain.StoredFile
	saveErr           error
	filename          string
	content           string
	size              int64
	removed           string
	removeCanceled    bool
	removeHasDeadline bool
	orphanRemoved     int
	orphanKeep        map[string]struct{}
	orphanCutoff      time.Time
	orphanErr         error
}

func (f *fakeFileStore) Save(_ context.Context, _ string, filename string, source io.Reader, size int64) (domain.StoredFile, error) {
	f.filename = filename
	f.size = size
	content, _ := io.ReadAll(source)
	f.content = string(content)
	return f.stored, f.saveErr
}

func (f *fakeFileStore) Remove(ctx context.Context, path string) error {
	f.removed = path
	f.removeCanceled = ctx.Err() != nil
	_, f.removeHasDeadline = ctx.Deadline()
	return nil
}

func (f *fakeFileStore) RemoveOrphans(_ context.Context, keep map[string]struct{}, olderThan time.Time) (int, error) {
	f.orphanKeep = keep
	f.orphanCutoff = olderThan
	return f.orphanRemoved, f.orphanErr
}

type fakeLLM struct {
	answer      string
	documents   []domain.ChatDocument
	answerCalls int
}

func (f *fakeLLM) Summarize(context.Context, string) (string, error) {
	return "summary", nil
}

func (f *fakeLLM) Answer(_ context.Context, _ string, documents []domain.ChatDocument) (string, error) {
	f.answerCalls++
	f.documents = documents
	return f.answer, nil
}
