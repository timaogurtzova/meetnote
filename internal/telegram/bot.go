package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/timaogurtzova/meetnote/internal/domain"
	updateinbox "github.com/timaogurtzova/meetnote/internal/inbox"
)

type BotConfig struct {
	PollTimeout        time.Duration
	RequestTimeout     time.Duration
	PersistenceTimeout time.Duration
	RetryMin           time.Duration
	RetryMax           time.Duration
	UpdateWorkers      int
	UpdateLease        time.Duration
	UpdateMaxAttempts  int
	UpdateRetention    time.Duration
}

type Bot struct {
	api        API
	handler    *Handler
	inbox      updateinbox.Repository
	config     BotConfig
	logger     zerolog.Logger
	instanceID string
}

func NewBot(api API, handler *Handler, inbox updateinbox.Repository, config BotConfig, logger zerolog.Logger) (*Bot, error) {
	if api == nil || handler == nil || inbox == nil {
		return nil, errors.New("telegram bot dependencies must not be nil")
	}
	if config.PollTimeout <= 0 || config.RequestTimeout <= 0 || config.PersistenceTimeout <= 0 ||
		config.RetryMin <= 0 || config.RetryMax < config.RetryMin ||
		config.UpdateWorkers < 1 || config.UpdateLease <= 0 || config.UpdateMaxAttempts < 1 ||
		config.UpdateRetention < 24*time.Hour {
		return nil, errors.New("telegram bot limits, timeouts, and retry intervals are invalid")
	}
	instanceID, err := newBotInstanceID()
	if err != nil {
		return nil, err
	}
	return &Bot{api: api, handler: handler, inbox: inbox, config: config, logger: logger, instanceID: instanceID}, nil
}

// Run сохраняет каждое обновление Telegram в PostgreSQL до подтверждения получения.
// Сохраненные обновления обрабатывает пул с ограниченным числом обработчиков.
func (b *Bot) Run(ctx context.Context) error {
	setupCtx, cancelSetup := context.WithTimeout(ctx, b.config.RequestTimeout)
	err := b.api.DeleteWebhook(setupCtx)
	cancelSetup()
	if err != nil {
		return fmt.Errorf("disable telegram webhook: %w", err)
	}

	commandsCtx, cancelCommands := context.WithTimeout(ctx, b.config.RequestTimeout)
	err = b.api.SetCommands(commandsCtx, supportedCommands())
	cancelCommands()
	if err != nil {
		b.logger.Warn().Ctx(ctx).Err(err).Msg("telegram command menu setup failed")
	}

	recovered, err := b.inbox.RecoverExpired(ctx)
	if err != nil {
		return fmt.Errorf("recover Telegram inbox: %w", err)
	}
	b.logger.Info().Ctx(ctx).
		Int("update_workers", b.config.UpdateWorkers).
		Str("instance_id", b.instanceID).
		Int64("recovered_updates", recovered).
		Msg("telegram bot started")
	defer func() { b.logger.Info().Msg("telegram bot stopped") }()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	processes := b.config.UpdateWorkers + 2
	results := make(chan error, processes)
	var group sync.WaitGroup
	group.Add(processes)
	go func() {
		defer group.Done()
		results <- b.poll(runCtx)
	}()
	go func() {
		defer group.Done()
		results <- b.recover(runCtx)
	}()
	for worker := 1; worker <= b.config.UpdateWorkers; worker++ {
		go func(workerID int) {
			defer group.Done()
			results <- b.processUpdates(runCtx, workerID)
		}(worker)
	}

	var runErrors []error
	select {
	case <-ctx.Done():
	case processErr := <-results:
		if processErr != nil {
			runErrors = append(runErrors, processErr)
		}
	}
	cancelRun()
	group.Wait()
	close(results)
	for processErr := range results {
		if processErr != nil {
			runErrors = append(runErrors, processErr)
		}
	}
	return errors.Join(runErrors...)
}

func (b *Bot) poll(ctx context.Context) error {
	offset, err := b.inbox.NextOffset(ctx)
	if err != nil {
		return fmt.Errorf("load Telegram polling offset: %w", err)
	}
	retryDelay := b.config.RetryMin
	for {
		if ctx.Err() != nil {
			return nil
		}
		pollCtx, cancelPoll := context.WithTimeout(ctx, b.config.PollTimeout+b.config.RequestTimeout)
		updates, pollErr := b.api.GetUpdates(pollCtx, offset, b.config.PollTimeout)
		cancelPoll()
		if pollErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			delay := retryDelayFor(pollErr, retryDelay, b.config.RetryMax)
			b.logger.Error().Ctx(ctx).Err(pollErr).Dur("retry_in", delay).Msg("telegram polling failed")
			if !wait(ctx, delay) {
				return nil
			}
			retryDelay = growRetryDelay(retryDelay, b.config.RetryMax)
			continue
		}
		if len(updates) == 0 {
			retryDelay = b.config.RetryMin
			continue
		}
		items := make([]updateinbox.Item, 0, len(updates))
		nextOffset := offset
		for _, update := range updates {
			payload, err := json.Marshal(update)
			if err != nil {
				return fmt.Errorf("encode Telegram update %d: %w", update.UpdateID, err)
			}
			items = append(items, updateinbox.Item{
				ID:       update.UpdateID,
				OwnerKey: updateUserKey(update),
				Payload:  payload,
			})
			if update.UpdateID >= nextOffset {
				nextOffset = update.UpdateID + 1
			}
		}
		if err := b.inbox.Enqueue(ctx, items); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			b.logger.Error().Ctx(ctx).Int("updates", len(items)).Err(err).Msg("persist Telegram updates failed")
			if !wait(ctx, retryDelay) {
				return nil
			}
			retryDelay = growRetryDelay(retryDelay, b.config.RetryMax)
			continue
		}
		offset = nextOffset
		retryDelay = b.config.RetryMin
	}
}

func (b *Bot) recover(ctx context.Context) error {
	ticker := time.NewTicker(min(10*time.Second, b.config.UpdateLease/2))
	defer ticker.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			recovered, err := b.inbox.RecoverExpired(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				b.logger.Error().Ctx(ctx).Err(err).Msg("recover expired Telegram updates failed")
				continue
			}
			if recovered > 0 {
				b.logger.Warn().Ctx(ctx).Int64("updates", recovered).Msg("expired Telegram updates recovered")
			}
			if lastPrune.IsZero() || time.Since(lastPrune) >= time.Hour {
				pruned, err := b.inbox.Prune(ctx, time.Now().Add(-b.config.UpdateRetention))
				if err != nil {
					if ctx.Err() == nil {
						b.logger.Error().Ctx(ctx).Err(err).Msg("prune Telegram inbox failed")
					}
					continue
				}
				lastPrune = time.Now()
				if pruned > 0 {
					b.logger.Info().Ctx(ctx).Int64("updates", pruned).Msg("Telegram inbox pruned")
				}
			}
		}
	}
}

func (b *Bot) processUpdates(ctx context.Context, workerID int) error {
	workerName := fmt.Sprintf("%s-%d", b.instanceID, workerID)
	logger := b.logger.With().Int("update_worker", workerID).Logger()
	for {
		if ctx.Err() != nil {
			return nil
		}
		item, found, err := b.inbox.Claim(ctx, workerName, b.config.UpdateLease)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Error().Ctx(ctx).Err(err).Msg("claim Telegram update failed")
			if !wait(ctx, b.config.RetryMin) {
				return nil
			}
			continue
		}
		if !found {
			if !wait(ctx, 100*time.Millisecond) {
				return nil
			}
			continue
		}

		var update Update
		handleErr := json.Unmarshal(item.Payload, &update)
		if handleErr == nil {
			handleErr = b.handler.Handle(ctx, update)
		}
		persistCtx, cancelPersist := context.WithTimeout(context.Background(), b.config.PersistenceTimeout)
		if handleErr == nil {
			err = b.inbox.Complete(persistCtx, item)
			cancelPersist()
			if err != nil && !errors.Is(err, domain.ErrLeaseLost) {
				return fmt.Errorf("complete Telegram update %d: %w", item.ID, err)
			}
			continue
		}

		delay := retryDelayFor(handleErr, exponentialDelay(b.config.RetryMin, b.config.RetryMax, item.Attempt), b.config.RetryMax)
		terminal, retryErr := b.inbox.Retry(persistCtx, item, safeUpdateError(handleErr), delay, b.config.UpdateMaxAttempts)
		cancelPersist()
		if retryErr != nil {
			if errors.Is(retryErr, domain.ErrLeaseLost) {
				logger.Warn().Int64("update_id", item.ID).Msg("Telegram update lease lost")
				continue
			}
			return fmt.Errorf("persist retry for Telegram update %d: %w", item.ID, retryErr)
		}
		logger.Error().
			Int64("update_id", item.ID).
			Int("attempt", item.Attempt).
			Bool("terminal", terminal).
			Dur("retry_in", delay).
			Err(handleErr).
			Msg("Telegram update handling failed")
	}
}

func supportedCommands() []BotCommand {
	return []BotCommand{
		{Command: "start", Description: "Начать работу"},
		{Command: "load", Description: "Как загрузить встречу"},
		{Command: "list", Description: "Список встреч"},
		{Command: "status", Description: "Статус встречи: /status ID"},
		{Command: "get", Description: "Транскрипция: /get ID"},
		{Command: "find", Description: "Поиск: /find текст"},
		{Command: "chat", Description: "Вопрос по встречам: /chat текст"},
		{Command: "retry", Description: "Повторить failed-встречу: /retry ID"},
		{Command: "help", Description: "Справка"},
	}
}

func exponentialDelay(minDelay, maxDelay time.Duration, attempt int) time.Duration {
	delay := minDelay
	for count := 1; count < attempt && delay < maxDelay; count++ {
		delay = growRetryDelay(delay, maxDelay)
	}
	return delay
}

func growRetryDelay(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}

func safeUpdateError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "telegram update timed out"
	}
	if errors.Is(err, domain.ErrUnavailable) {
		return "telegram API unavailable"
	}
	return "telegram update handling failed"
}

func updateUserKey(update Update) string {
	if update.Message != nil && update.Message.From != nil && update.Message.From.ID > 0 {
		return fmt.Sprintf("telegram:%d", update.Message.From.ID)
	}
	return fmt.Sprintf("telegram-update:%d", update.UpdateID)
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func newBotInstanceID() (string, error) {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate Telegram bot instance id: %w", err)
	}
	return "bot-" + hex.EncodeToString(value), nil
}
