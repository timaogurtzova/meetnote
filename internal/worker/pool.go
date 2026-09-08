// Пакет worker запускает ограниченный пул фоновых обработчиков встреч.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/timaogurtzova/meetnote/internal/app"
	"github.com/timaogurtzova/meetnote/internal/domain"
	"golang.org/x/sync/errgroup"
)

const failureSaveTimeout = 5 * time.Second

type Config struct {
	Count              int
	PollInterval       time.Duration
	SpeechTimeout      time.Duration
	LLMTimeout         time.Duration
	LeaseDuration      time.Duration
	HeartbeatInterval  time.Duration
	RecoveryInterval   time.Duration
	MaxTranscriptRunes int
	MaxSummaryRunes    int
}

type Pool struct {
	repository app.TaskRepository
	speech     app.SpeechClient
	llm        app.LLMClient
	config     Config
	logger     *slog.Logger
	instanceID string
}

func NewPool(
	repository app.TaskRepository,
	speech app.SpeechClient,
	llm app.LLMClient,
	config Config,
	logger *slog.Logger,
) (*Pool, error) {
	if repository == nil || speech == nil || llm == nil || logger == nil {
		return nil, errors.New("worker dependencies must not be nil")
	}
	if config.Count < 1 || config.PollInterval <= 0 || config.SpeechTimeout <= 0 || config.LLMTimeout <= 0 ||
		config.LeaseDuration <= 0 || config.HeartbeatInterval <= 0 || config.RecoveryInterval <= 0 ||
		config.HeartbeatInterval >= config.LeaseDuration || config.MaxTranscriptRunes < 1 || config.MaxSummaryRunes < 1 {
		return nil, errors.New("worker configuration values must be positive")
	}
	instanceID, err := newInstanceID()
	if err != nil {
		return nil, err
	}
	return &Pool{
		repository: repository,
		speech:     speech,
		llm:        llm,
		config:     config,
		logger:     logger,
		instanceID: instanceID,
	}, nil
}

// Run восстанавливает прерванные задачи и ждет завершения обработчиков после отмены контекста.
func (p *Pool) Run(ctx context.Context) error {
	recovered, err := p.repository.RecoverInterrupted(ctx)
	if err != nil {
		return fmt.Errorf("recover interrupted tasks: %w", err)
	}
	p.logger.InfoContext(ctx, "worker pool started",
		"workers", p.config.Count,
		"instance_id", p.instanceID,
		"recovered_tasks", recovered)

	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		p.runRecovery(runCtx)
		return nil
	})
	for workerID := 1; workerID <= p.config.Count; workerID++ {
		workerID := workerID
		group.Go(func() error {
			return p.runWorker(runCtx, workerID)
		})
	}
	runErr := group.Wait()
	p.logger.InfoContext(ctx, "worker pool stopped")
	return runErr
}

func (p *Pool) runWorker(ctx context.Context, workerID int) error {
	logger := p.logger.With("worker_id", workerID)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		task, found, err := p.repository.ClaimNextTask(ctx, p.instanceID, p.config.LeaseDuration)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.ErrorContext(ctx, "claim task failed", "error", err)
			if !waitForPoll(ctx, p.config.PollInterval) {
				return nil
			}
			continue
		}
		if !found {
			if !waitForPoll(ctx, p.config.PollInterval) {
				return nil
			}
			continue
		}
		if err := p.process(ctx, logger, task); err != nil {
			return err
		}
	}
}

func (p *Pool) runRecovery(ctx context.Context) {
	ticker := time.NewTicker(p.config.RecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recovered, err := p.repository.RecoverInterrupted(ctx)
			if err != nil {
				if ctx.Err() == nil {
					p.logger.ErrorContext(ctx, "recover expired task leases failed", "error", err)
				}
				continue
			}
			if recovered > 0 {
				p.logger.WarnContext(ctx, "expired task leases recovered", "tasks", recovered)
			}
		}
	}
}

func (p *Pool) process(ctx context.Context, logger *slog.Logger, task domain.Task) error {
	logger = logger.With("task_id", task.ID, "meeting_id", task.MeetingID, "attempt", task.Attempt)
	logger.InfoContext(ctx, "task processing started")

	taskCtx, cancelTask := context.WithCancel(ctx)
	heartbeatResult := make(chan error, 1)
	go func() {
		heartbeatResult <- p.keepLease(taskCtx, task, cancelTask)
	}()

	processErr := p.processStages(taskCtx, logger, task)
	cancelTask()
	heartbeatErr := <-heartbeatResult
	if heartbeatErr != nil {
		if errors.Is(heartbeatErr, domain.ErrLeaseLost) {
			logger.WarnContext(ctx, "task lease lost; processing result discarded")
			return processErr
		}
		logger.ErrorContext(ctx, "task lease heartbeat failed", "error", heartbeatErr)
		return errors.Join(processErr, fmt.Errorf("task %d lease heartbeat: %w", task.ID, heartbeatErr))
	}
	return processErr
}

func (p *Pool) processStages(ctx context.Context, logger *slog.Logger, task domain.Task) error {
	speechCtx, cancelSpeech := context.WithTimeout(ctx, p.config.SpeechTimeout)
	logger.InfoContext(ctx, "speech client request started")
	transcript, err := p.speech.Transcribe(speechCtx, task.StoredPath)
	cancelSpeech()
	if err != nil {
		return p.saveTaskFailure(logger, task, "speech transcription", err)
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" || !utf8.ValidString(transcript) || utf8.RuneCountInString(transcript) > p.config.MaxTranscriptRunes {
		return p.saveTaskFailure(logger, task, "speech transcription", domain.ErrInvalidClientData)
	}
	if err := p.repository.SaveTranscription(ctx, task, transcript); err != nil {
		return p.saveTaskFailure(logger, task, "save transcription", err)
	}
	logger.InfoContext(ctx, "task status changed", "status", domain.StatusTranscribed)

	llmCtx, cancelLLM := context.WithTimeout(ctx, p.config.LLMTimeout)
	logger.InfoContext(ctx, "LLM summary request started")
	summary, err := p.llm.Summarize(llmCtx, transcript)
	cancelLLM()
	if err != nil {
		return p.saveTaskFailure(logger, task, "LLM summary", err)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" || !utf8.ValidString(summary) || utf8.RuneCountInString(summary) > p.config.MaxSummaryRunes {
		return p.saveTaskFailure(logger, task, "LLM summary", domain.ErrInvalidClientData)
	}
	if err := p.repository.SaveSummary(ctx, task, summary); err != nil {
		return p.saveTaskFailure(logger, task, "save summary", err)
	}
	logger.InfoContext(ctx, "task status changed", "status", domain.StatusSummarized)

	if err := p.repository.CompleteTask(ctx, task); err != nil {
		return p.saveTaskFailure(logger, task, "complete task", err)
	}
	logger.InfoContext(ctx, "task processing completed", "status", domain.StatusCompleted)
	return nil
}

func (p *Pool) keepLease(
	ctx context.Context,
	task domain.Task,
	cancelTask context.CancelFunc,
) error {
	ticker := time.NewTicker(p.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.repository.RefreshLease(ctx, task, p.config.LeaseDuration); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				cancelTask()
				return err
			}
		}
	}
}

func (p *Pool) saveTaskFailure(logger *slog.Logger, task domain.Task, stage string, processErr error) error {
	failure := processingFailureForUser(stage, processErr)
	saveCtx, cancel := context.WithTimeout(context.Background(), failureSaveTimeout)
	defer cancel()
	if err := p.repository.FailTask(saveCtx, task, failure); err != nil {
		if errors.Is(err, domain.ErrLeaseLost) {
			logger.Warn("task failure ignored because lease was lost", "stage", stage)
			return nil
		}
		logger.Error("task failure could not be persisted",
			"stage", stage, "error", processErr, "persistence_error", err)
		return fmt.Errorf("persist failure for task %d after %s: %w", task.ID, stage, err)
	}
	logger.Error("task processing failed", "stage", stage, "error", processErr, "status", domain.StatusFailed)
	return nil
}

func processingFailureForUser(stage string, err error) domain.ProcessingFailure {
	code := "processing_failed"
	message := "Обработка встречи завершилась ошибкой. Используйте /retry ID, чтобы повторить попытку."
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		code = "provider_timeout"
		message = "Сервис обработки не ответил вовремя. Используйте /retry ID, чтобы повторить попытку."
	case errors.Is(err, context.Canceled):
		code = "processing_interrupted"
		message = "Обработка была прервана. Используйте /retry ID, чтобы повторить попытку."
	case errors.Is(err, domain.ErrInvalidClientData):
		code = "invalid_provider_response"
		message = "Сервис обработки вернул некорректный результат. Используйте /retry ID, чтобы повторить попытку."
	case stage == "speech transcription":
		code = "speech_failed"
	case stage == "LLM summary":
		code = "summary_failed"
	case strings.HasPrefix(stage, "save ") || stage == "complete task":
		code = "storage_failed"
	}
	return domain.ProcessingFailure{Code: code, Message: message}
}

func waitForPoll(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func newInstanceID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate worker instance id: %w", err)
	}
	return "worker-" + hex.EncodeToString(value), nil
}
