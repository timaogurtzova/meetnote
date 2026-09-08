package worker_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/domain"
	"github.com/timaogurtzova/meetnote/internal/worker"
)

func TestPoolLimitsParallelismAndCompletesTasks(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const taskCount = 6
		repository := newFakeTaskRepository(taskCount)
		speech := &trackingSpeech{delay: 30 * time.Millisecond}
		pool := newTestPool(t, repository, speech, &stubLLM{}, 2)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- pool.Run(ctx) }()

		for range taskCount {
			<-repository.completed
		}
		cancel()
		err := <-done
		require.NoError(t, err)
		assert.Equal(t, int32(2), speech.maximum.Load())
		assert.Zero(t, repository.failedCount())
		if repository.refreshCount() == 0 {
			t.Fatal("task leases were not refreshed during processing")
		}
		if repository.recoveryCount() < 2 {
			t.Fatalf("periodic recovery calls = %d, want at least 2", repository.recoveryCount())
		}
	})
}

func TestPoolReturnsFailurePersistenceError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		wantErr := errors.New("database unavailable")
		repository := newFakeTaskRepository(1)
		repository.failErr = wantErr
		pool := newTestPool(t, repository, &failingSpeech{}, &stubLLM{}, 1)
		done := make(chan error, 1)
		go func() { done <- pool.Run(t.Context()) }()

		err := <-done
		if !errors.Is(err, wantErr) {
			t.Fatalf("Run() error = %v, want %v", err, wantErr)
		}
	})
}

func TestPoolCancellationPersistsFailureAndStops(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		repository := newFakeTaskRepository(1)
		speech := &blockingSpeech{}
		pool := newTestPool(t, repository, speech, &stubLLM{}, 1)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- pool.Run(ctx) }()

		<-repository.claimed
		cancel()
		<-repository.failed
		err := <-done
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
	})
}

func TestPoolRejectsEmptySuccessfulProviderResults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		speech interface {
			Transcribe(context.Context, string) (string, error)
		}
		llm *stubLLM
	}{
		{name: "empty transcript", speech: &staticSpeech{result: "   "}, llm: &stubLLM{summary: "summary"}},
		{name: "empty summary", speech: &staticSpeech{result: "transcript"}, llm: &stubLLM{summary: "\n"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repository := newFakeTaskRepository(1)
				pool := newTestPool(t, repository, test.speech, test.llm, 1)
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- pool.Run(ctx) }()
				<-repository.failed
				cancel()
				err := <-done
				if err != nil {
					t.Fatalf("Run() error: %v", err)
				}
				repository.mu.Lock()
				failure := repository.failure
				repository.mu.Unlock()
				if failure.Code != "invalid_provider_response" || failure.Message == "" {
					t.Fatalf("persisted failure = %#v", failure)
				}
			})
		})
	}
}

func newTestPool(t *testing.T, repository *fakeTaskRepository, speech interface {
	Transcribe(context.Context, string) (string, error)
}, llm *stubLLM, count int) *worker.Pool {
	t.Helper()
	pool, err := worker.NewPool(
		repository,
		speech,
		llm,
		worker.Config{
			Count:              count,
			PollInterval:       time.Millisecond,
			SpeechTimeout:      time.Second,
			LLMTimeout:         time.Second,
			LeaseDuration:      100 * time.Millisecond,
			HeartbeatInterval:  5 * time.Millisecond,
			RecoveryInterval:   5 * time.Millisecond,
			MaxTranscriptRunes: 500_000,
			MaxSummaryRunes:    12_000,
		},
		slog.New(slog.DiscardHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

type fakeTaskRepository struct {
	mu         sync.Mutex
	tasks      []domain.Task
	next       int
	failures   int
	refreshes  int
	recoveries int
	failErr    error
	failure    domain.ProcessingFailure
	claimed    chan struct{}
	completed  chan struct{}
	failed     chan struct{}
}

func newFakeTaskRepository(count int) *fakeTaskRepository {
	tasks := make([]domain.Task, count)
	for index := range tasks {
		tasks[index] = domain.Task{ID: int64(index + 1), MeetingID: int64(index + 1), StoredPath: "meeting.txt"}
	}
	return &fakeTaskRepository{
		tasks:     tasks,
		claimed:   make(chan struct{}, count),
		completed: make(chan struct{}, count),
		failed:    make(chan struct{}, count),
	}
}

func (f *fakeTaskRepository) RecoverInterrupted(context.Context) (int64, error) {
	f.mu.Lock()
	f.recoveries++
	f.mu.Unlock()
	return 0, nil
}

func (f *fakeTaskRepository) ClaimNextTask(
	_ context.Context,
	_ string,
	_ time.Duration,
) (domain.Task, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next >= len(f.tasks) {
		return domain.Task{}, false, nil
	}
	task := f.tasks[f.next]
	f.next++
	task.LeaseToken = "test-lease"
	f.claimed <- struct{}{}
	return task, true, nil
}

func (f *fakeTaskRepository) RefreshLease(context.Context, domain.Task, time.Duration) error {
	f.mu.Lock()
	f.refreshes++
	f.mu.Unlock()
	return nil
}

func (f *fakeTaskRepository) SaveTranscription(context.Context, domain.Task, string) error {
	return nil
}

func (f *fakeTaskRepository) SaveSummary(context.Context, domain.Task, string) error {
	return nil
}

func (f *fakeTaskRepository) CompleteTask(context.Context, domain.Task) error {
	f.completed <- struct{}{}
	return nil
}

func (f *fakeTaskRepository) FailTask(_ context.Context, _ domain.Task, failure domain.ProcessingFailure) error {
	f.mu.Lock()
	f.failures++
	f.failure = failure
	failErr := f.failErr
	f.mu.Unlock()
	f.failed <- struct{}{}
	return failErr
}

func (f *fakeTaskRepository) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes
}

func (f *fakeTaskRepository) failedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures
}

func (f *fakeTaskRepository) recoveryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recoveries
}

type trackingSpeech struct {
	delay   time.Duration
	active  atomic.Int32
	maximum atomic.Int32
}

func (s *trackingSpeech) Transcribe(ctx context.Context, _ string) (string, error) {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		maximum := s.maximum.Load()
		if active <= maximum || s.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "transcript", nil
	}
}

type blockingSpeech struct{}

func (b *blockingSpeech) Transcribe(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

type failingSpeech struct{}

func (f *failingSpeech) Transcribe(context.Context, string) (string, error) {
	return "", errors.New("speech failed")
}

type staticSpeech struct{ result string }

func (s *staticSpeech) Transcribe(context.Context, string) (string, error) { return s.result, nil }

type stubLLM struct{ summary string }

func (s *stubLLM) Summarize(context.Context, string) (string, error) {
	if s.summary != "" {
		return s.summary, nil
	}
	return "summary", nil
}

func (s *stubLLM) Answer(context.Context, string, []domain.ChatDocument) (string, error) {
	return "answer", nil
}
