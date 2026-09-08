package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/timaogurtzova/meetnote/internal/app"
	"github.com/timaogurtzova/meetnote/internal/clients/mock"
	"github.com/timaogurtzova/meetnote/internal/config"
	"github.com/timaogurtzova/meetnote/internal/domain"
	"github.com/timaogurtzova/meetnote/internal/postgres"
	"github.com/timaogurtzova/meetnote/internal/storage"
	telegramadapter "github.com/timaogurtzova/meetnote/internal/telegram"
	"github.com/timaogurtzova/meetnote/internal/worker"
)

var (
	buildVersion = "dev"
	buildDate    = "unknown"
	buildCommit  = "unknown"
)

type mode string

const (
	modeBot     mode = "bot"
	modeWorker  mode = "worker"
	modeMigrate mode = "migrate"
	modeHealth  mode = "health"
	modeHelp    mode = "help"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка: %s\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	runMode, err := parseMode(args)
	if err != nil {
		usage(stderr)
		return err
	}
	if runMode == modeHelp {
		usage(stdout)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	if runMode == modeBot {
		if err := cfg.ValidateTelegram(); err != nil {
			return fmt.Errorf("validate Telegram configuration: %w", err)
		}
	}

	logger := newLogger(stderr, cfg.LogLevel)
	logger.InfoContext(ctx, "application started",
		"mode", runMode,
		"build_version", buildVersion,
		"build_date", buildDate,
		"build_commit", buildCommit)
	defer logger.Info("application stopped", "mode", runMode)

	connectCtx, cancelConnect := context.WithTimeout(ctx, cfg.Runtime.OperationTimeout)
	pool, err := postgres.Open(connectCtx, cfg.Database)
	cancelConnect()
	if err != nil {
		logger.ErrorContext(ctx, "database connection failed", "error", err)
		return err
	}
	defer pool.Close()

	if (cfg.Database.AutoMigrate && runMode != modeHealth) || runMode == modeMigrate {
		migrationCtx, cancelMigration := context.WithTimeout(ctx, 30*time.Second)
		err = postgres.Migrate(migrationCtx, pool)
		cancelMigration()
		if err != nil {
			logger.ErrorContext(ctx, "database migration failed", "error", err)
			return err
		}
	}
	if runMode == modeMigrate {
		_, err := fmt.Fprintln(stdout, "Миграции применены.")
		return err
	}
	if runMode == modeHealth {
		healthCtx, cancelHealth := context.WithTimeout(ctx, cfg.Runtime.OperationTimeout)
		err := pool.Ping(healthCtx)
		cancelHealth()
		if err != nil {
			return fmt.Errorf("database health check: %w", err)
		}
		_, err = fmt.Fprintln(stdout, "ok")
		return err
	}

	repository, err := postgres.NewRepository(pool)
	if err != nil {
		return err
	}

	switch runMode {
	case modeWorker:
		speechClient, err := newSpeechClient(cfg)
		if err != nil {
			return err
		}
		llmClient, err := newLLMClient(cfg)
		if err != nil {
			return err
		}
		workerPool, err := worker.NewPool(
			repository,
			speechClient,
			llmClient,
			worker.Config{
				Count:              cfg.Worker.Count,
				PollInterval:       cfg.Worker.PollInterval,
				SpeechTimeout:      cfg.Worker.SpeechTimeout,
				LLMTimeout:         cfg.Worker.LLMTimeout,
				LeaseDuration:      cfg.Worker.LeaseDuration,
				HeartbeatInterval:  cfg.Worker.HeartbeatInterval,
				RecoveryInterval:   cfg.Worker.RecoveryInterval,
				MaxTranscriptRunes: cfg.Limits.MaxTranscriptRunes,
				MaxSummaryRunes:    cfg.Limits.MaxSummaryRunes,
			},
			logger,
		)
		if err != nil {
			return err
		}
		return workerPool.Run(ctx)
	case modeBot:
		llmClient, err := newLLMClient(cfg)
		if err != nil {
			return err
		}
		return runTelegramBot(ctx, cfg, repository, llmClient, logger)
	default:
		return fmt.Errorf("unsupported application mode %q", runMode)
	}
}

func runTelegramBot(
	ctx context.Context,
	cfg config.Config,
	repository *postgres.Repository,
	llmClient app.LLMClient,
	logger *slog.Logger,
) (runErr error) {
	fileStore, err := storage.NewLocal(cfg.Storage.Directory, cfg.Storage.MaxFileSize)
	if err != nil {
		return fmt.Errorf("initialize file storage: %w", err)
	}
	defer func() {
		if closeErr := fileStore.Close(); closeErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close file storage: %w", closeErr))
		}
	}()
	service, err := app.NewService(repository, fileStore, llmClient, app.Limits{
		MaxQuestionRunes:    cfg.Limits.MaxQuestionRunes,
		MaxSearchQueryRunes: cfg.Limits.MaxSearchQueryRunes,
		MaxAnswerRunes:      cfg.Limits.MaxAnswerRunes,
		MaxChatHistory:      cfg.Limits.MaxChatHistoryPerUser,
		MeetingQuota: domain.MeetingQuota{
			MaxMeetings:     cfg.Limits.MaxMeetingsPerUser,
			MaxPending:      cfg.Limits.MaxPendingPerUser,
			MaxStorageBytes: cfg.Limits.MaxStorageBytesPerUser,
		},
	}, logger)
	if err != nil {
		return err
	}
	maintenanceCtx, cancelMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		runOrphanCleanup(maintenanceCtx, service, cfg.Runtime.OperationTimeout, logger)
	}()
	defer func() {
		cancelMaintenance()
		<-maintenanceDone
	}()
	telegramClient, err := telegramadapter.NewClient(
		cfg.Telegram.Token,
		cfg.Telegram.APIURL,
		&http.Client{},
	)
	if err != nil {
		return err
	}
	handler, err := telegramadapter.NewHandler(
		service,
		telegramClient,
		telegramadapter.HandlerConfig{
			RequestTimeout:        cfg.Telegram.RequestTimeout,
			DownloadTimeout:       cfg.Telegram.DownloadTimeout,
			MaxFileSize:           cfg.Storage.MaxFileSize,
			InlineTranscriptRunes: cfg.Limits.InlineTranscriptRunes,
			MaxResponseParts:      cfg.Limits.MaxTelegramResponseParts,
		},
		logger,
	)
	if err != nil {
		return err
	}
	bot, err := telegramadapter.NewBot(
		telegramClient,
		handler,
		repository,
		telegramadapter.BotConfig{
			PollTimeout:        cfg.Telegram.PollTimeout,
			RequestTimeout:     cfg.Telegram.RequestTimeout,
			PersistenceTimeout: cfg.Runtime.OperationTimeout,
			RetryMin:           cfg.Telegram.RetryMin,
			RetryMax:           cfg.Telegram.RetryMax,
			UpdateWorkers:      cfg.Telegram.UpdateWorkers,
			UpdateLease:        cfg.Telegram.UpdateLease,
			UpdateMaxAttempts:  cfg.Telegram.UpdateMaxAttempts,
			UpdateRetention:    cfg.Telegram.UpdateRetention,
		},
		logger,
	)
	if err != nil {
		return err
	}
	return bot.Run(ctx)
}

func runOrphanCleanup(ctx context.Context, service *app.Service, timeout time.Duration, logger *slog.Logger) {
	const (
		cleanupInterval = time.Hour
		orphanGrace     = 15 * time.Minute
	)
	cleanup := func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(ctx, timeout)
		_, err := service.CleanupOrphans(cleanupCtx, orphanGrace)
		cancelCleanup()
		if err != nil && ctx.Err() == nil {
			logger.ErrorContext(ctx, "orphan upload cleanup failed", "error", err)
		}
	}
	cleanup()
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

func parseMode(args []string) (mode, error) {
	if len(args) == 0 {
		return modeBot, nil
	}
	if len(args) != 1 {
		return "", errors.New("ожидается одна служебная команда")
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "bot":
		return modeBot, nil
	case "worker":
		return modeWorker, nil
	case "migrate":
		return modeMigrate, nil
	case "health":
		return modeHealth, nil
	case "help", "-h", "--help":
		return modeHelp, nil
	default:
		return "", fmt.Errorf("неизвестная служебная команда %q", args[0])
	}
}

func usage(output io.Writer) {
	fmt.Fprint(output, "MeetNote Telegram bot\n\n"+
		"Использование:\n"+
		"  meetnote bot      запустить Telegram long polling (по умолчанию)\n"+
		"  meetnote worker   запустить фоновые обработчики\n"+
		"  meetnote migrate  применить миграции PostgreSQL\n"+
		"  meetnote health   проверить PostgreSQL\n")
}

func newSpeechClient(cfg config.Config) (app.SpeechClient, error) {
	switch cfg.Clients.SpeechProvider {
	case "mock":
		return mock.NewSpeech(cfg.Clients.MockDelay), nil
	default:
		return nil, fmt.Errorf("speech provider %q is not implemented", cfg.Clients.SpeechProvider)
	}
}

func newLLMClient(cfg config.Config) (app.LLMClient, error) {
	switch cfg.Clients.LLMProvider {
	case "mock":
		return mock.NewLLM(cfg.Clients.MockDelay), nil
	default:
		return nil, fmt.Errorf("LLM provider %q is not implemented", cfg.Clients.LLMProvider)
	}
}

func newLogger(output io.Writer, configuredLevel string) *slog.Logger {
	level := slog.LevelInfo
	switch configuredLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))
}
