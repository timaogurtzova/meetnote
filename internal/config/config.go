// Пакет config загружает и проверяет настройки приложения из переменных окружения.
package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	env "github.com/caarlos0/env/v11"
)

const TelegramDownloadLimit int64 = 20 * 1024 * 1024

type Config struct {
	Database Database
	Storage  Storage
	Worker   Worker
	Clients  Clients
	Telegram Telegram
	Runtime  Runtime
	Limits   Limits
	LogLevel string
}

type Database struct {
	URL         string
	MaxConns    int32
	AutoMigrate bool
}

type Storage struct {
	Directory   string
	MaxFileSize int64
}

type Worker struct {
	Count             int
	PollInterval      time.Duration
	SpeechTimeout     time.Duration
	LLMTimeout        time.Duration
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	RecoveryInterval  time.Duration
}

type Clients struct {
	SpeechProvider string
	LLMProvider    string
	MockDelay      time.Duration
}

type Telegram struct {
	Token             string
	APIURL            string
	PollTimeout       time.Duration
	RequestTimeout    time.Duration
	DownloadTimeout   time.Duration
	RetryMin          time.Duration
	RetryMax          time.Duration
	UpdateWorkers     int
	UpdateLease       time.Duration
	UpdateMaxAttempts int
	UpdateRetention   time.Duration
}

type Runtime struct {
	OperationTimeout time.Duration
}

type Limits struct {
	MaxTranscriptRunes       int
	MaxSummaryRunes          int
	MaxAnswerRunes           int
	MaxQuestionRunes         int
	MaxSearchQueryRunes      int
	InlineTranscriptRunes    int
	MaxTelegramResponseParts int
	MaxMeetingsPerUser       int
	MaxPendingPerUser        int
	MaxStorageBytesPerUser   int64
	MaxChatHistoryPerUser    int
}

type environmentConfig struct {
	DatabaseURL               string        `env:"MEETNOTE_DATABASE_URL" envDefault:"postgres://meetnote:meetnote@localhost:55432/meetnote?sslmode=disable"`
	DatabaseMaxConns          int64         `env:"MEETNOTE_DATABASE_MAX_CONNS" envDefault:"10"`
	AutoMigrate               bool          `env:"MEETNOTE_AUTO_MIGRATE" envDefault:"true"`
	StorageDirectory          string        `env:"MEETNOTE_STORAGE_DIR" envDefault:"./data/uploads"`
	MaxFileMB                 int64         `env:"MEETNOTE_MAX_FILE_MB" envDefault:"20"`
	WorkerCount               int           `env:"MEETNOTE_WORKERS" envDefault:"3"`
	PollInterval              time.Duration `env:"MEETNOTE_POLL_INTERVAL" envDefault:"300ms"`
	SpeechTimeout             time.Duration `env:"MEETNOTE_SPEECH_TIMEOUT" envDefault:"30s"`
	LLMTimeout                time.Duration `env:"MEETNOTE_LLM_TIMEOUT" envDefault:"30s"`
	TaskLease                 time.Duration `env:"MEETNOTE_TASK_LEASE" envDefault:"2m"`
	LeaseHeartbeat            time.Duration `env:"MEETNOTE_LEASE_HEARTBEAT" envDefault:"20s"`
	LeaseRecoveryInterval     time.Duration `env:"MEETNOTE_LEASE_RECOVERY_INTERVAL" envDefault:"10s"`
	SpeechProvider            string        `env:"MEETNOTE_SPEECH_PROVIDER" envDefault:"mock"`
	LLMProvider               string        `env:"MEETNOTE_LLM_PROVIDER" envDefault:"mock"`
	MockDelay                 time.Duration `env:"MEETNOTE_MOCK_DELAY" envDefault:"150ms"`
	TelegramToken             string        `env:"MEETNOTE_TELEGRAM_TOKEN"`
	TelegramAPIURL            string        `env:"MEETNOTE_TELEGRAM_API_URL" envDefault:"https://api.telegram.org"`
	TelegramPollTimeout       time.Duration `env:"MEETNOTE_TELEGRAM_POLL_TIMEOUT" envDefault:"25s"`
	TelegramRequestTimeout    time.Duration `env:"MEETNOTE_TELEGRAM_REQUEST_TIMEOUT" envDefault:"10s"`
	TelegramDownloadTimeout   time.Duration `env:"MEETNOTE_TELEGRAM_DOWNLOAD_TIMEOUT" envDefault:"2m"`
	TelegramRetryMin          time.Duration `env:"MEETNOTE_TELEGRAM_RETRY_MIN" envDefault:"1s"`
	TelegramRetryMax          time.Duration `env:"MEETNOTE_TELEGRAM_RETRY_MAX" envDefault:"30s"`
	TelegramUpdateWorkers     int           `env:"MEETNOTE_TELEGRAM_UPDATE_WORKERS" envDefault:"4"`
	TelegramUpdateLease       time.Duration `env:"MEETNOTE_TELEGRAM_UPDATE_LEASE" envDefault:"3m"`
	TelegramUpdateMaxAttempts int           `env:"MEETNOTE_TELEGRAM_UPDATE_MAX_ATTEMPTS" envDefault:"8"`
	TelegramUpdateRetention   time.Duration `env:"MEETNOTE_TELEGRAM_UPDATE_RETENTION" envDefault:"168h"`
	OperationTimeout          time.Duration `env:"MEETNOTE_OPERATION_TIMEOUT" envDefault:"10s"`
	MaxTranscriptRunes        int           `env:"MEETNOTE_MAX_TRANSCRIPT_RUNES" envDefault:"500000"`
	MaxSummaryRunes           int           `env:"MEETNOTE_MAX_SUMMARY_RUNES" envDefault:"12000"`
	MaxAnswerRunes            int           `env:"MEETNOTE_MAX_ANSWER_RUNES" envDefault:"8000"`
	MaxQuestionRunes          int           `env:"MEETNOTE_MAX_QUESTION_RUNES" envDefault:"2000"`
	MaxSearchQueryRunes       int           `env:"MEETNOTE_MAX_SEARCH_QUERY_RUNES" envDefault:"500"`
	InlineTranscriptRunes     int           `env:"MEETNOTE_INLINE_TRANSCRIPT_RUNES" envDefault:"12000"`
	MaxTelegramResponseParts  int           `env:"MEETNOTE_TELEGRAM_MAX_RESPONSE_PARTS" envDefault:"8"`
	MaxMeetingsPerUser        int           `env:"MEETNOTE_MAX_MEETINGS_PER_USER" envDefault:"100"`
	MaxPendingMeetingsPerUser int           `env:"MEETNOTE_MAX_PENDING_MEETINGS_PER_USER" envDefault:"10"`
	MaxStorageMBPerUser       int64         `env:"MEETNOTE_MAX_STORAGE_MB_PER_USER" envDefault:"200"`
	MaxChatHistoryPerUser     int           `env:"MEETNOTE_MAX_CHAT_HISTORY_PER_USER" envDefault:"1000"`
	LogLevel                  string        `env:"MEETNOTE_LOG_LEVEL" envDefault:"info"`
}

// Load читает настройки из переменных MEETNOTE_*.
// Некорректное заданное значение возвращается как ошибка и не заменяется значением по умолчанию.
func Load() (Config, error) {
	values, err := env.ParseAsWithOptions[environmentConfig](env.Options{})
	if err != nil {
		return Config{}, fmt.Errorf("parse MEETNOTE environment: %w", environmentParseError(err))
	}
	maxFileSize, maxFileSizeErr := mebibytesToBytes("MEETNOTE_MAX_FILE_MB", values.MaxFileMB)
	maxStorageBytesPerUser, maxStorageErr := mebibytesToBytes("MEETNOTE_MAX_STORAGE_MB_PER_USER", values.MaxStorageMBPerUser)
	maxConns, maxConnsErr := databaseConnectionLimit(values.DatabaseMaxConns)

	cfg := Config{
		Database: Database{
			URL:         strings.TrimSpace(values.DatabaseURL),
			MaxConns:    maxConns,
			AutoMigrate: values.AutoMigrate,
		},
		Storage: Storage{
			Directory:   strings.TrimSpace(values.StorageDirectory),
			MaxFileSize: maxFileSize,
		},
		Worker: Worker{
			Count:             values.WorkerCount,
			PollInterval:      values.PollInterval,
			SpeechTimeout:     values.SpeechTimeout,
			LLMTimeout:        values.LLMTimeout,
			LeaseDuration:     values.TaskLease,
			HeartbeatInterval: values.LeaseHeartbeat,
			RecoveryInterval:  values.LeaseRecoveryInterval,
		},
		Clients: Clients{
			SpeechProvider: strings.ToLower(strings.TrimSpace(values.SpeechProvider)),
			LLMProvider:    strings.ToLower(strings.TrimSpace(values.LLMProvider)),
			MockDelay:      values.MockDelay,
		},
		Telegram: Telegram{
			Token:             strings.TrimSpace(values.TelegramToken),
			APIURL:            strings.TrimSpace(values.TelegramAPIURL),
			PollTimeout:       values.TelegramPollTimeout,
			RequestTimeout:    values.TelegramRequestTimeout,
			DownloadTimeout:   values.TelegramDownloadTimeout,
			RetryMin:          values.TelegramRetryMin,
			RetryMax:          values.TelegramRetryMax,
			UpdateWorkers:     values.TelegramUpdateWorkers,
			UpdateLease:       values.TelegramUpdateLease,
			UpdateMaxAttempts: values.TelegramUpdateMaxAttempts,
			UpdateRetention:   values.TelegramUpdateRetention,
		},
		Runtime: Runtime{OperationTimeout: values.OperationTimeout},
		Limits: Limits{
			MaxTranscriptRunes:       values.MaxTranscriptRunes,
			MaxSummaryRunes:          values.MaxSummaryRunes,
			MaxAnswerRunes:           values.MaxAnswerRunes,
			MaxQuestionRunes:         values.MaxQuestionRunes,
			MaxSearchQueryRunes:      values.MaxSearchQueryRunes,
			InlineTranscriptRunes:    values.InlineTranscriptRunes,
			MaxTelegramResponseParts: values.MaxTelegramResponseParts,
			MaxMeetingsPerUser:       values.MaxMeetingsPerUser,
			MaxPendingPerUser:        values.MaxPendingMeetingsPerUser,
			MaxStorageBytesPerUser:   maxStorageBytesPerUser,
			MaxChatHistoryPerUser:    values.MaxChatHistoryPerUser,
		},
		LogLevel: strings.ToLower(strings.TrimSpace(values.LogLevel)),
	}

	return cfg, errors.Join(maxFileSizeErr, maxStorageErr, maxConnsErr, cfg.Validate())
}

func environmentParseError(parseErr error) error {
	message := parseErr.Error()
	configType := reflect.TypeFor[environmentConfig]()
	for fieldIndex := range configType.NumField() {
		field := configType.Field(fieldIndex)
		variable := field.Tag.Get("env")
		if variable != "" {
			message = strings.ReplaceAll(message, `field "`+field.Name+`"`, variable)
		}
	}
	return errors.New(message)
}

func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.Database.URL) == "" {
		errs = append(errs, errors.New("MEETNOTE_DATABASE_URL is empty"))
	}
	if c.Database.MaxConns < 1 || c.Database.MaxConns > 100 {
		errs = append(errs, errors.New("MEETNOTE_DATABASE_MAX_CONNS must be between 1 and 100"))
	}
	if strings.TrimSpace(c.Storage.Directory) == "" || c.Storage.MaxFileSize < 1 {
		errs = append(errs, errors.New("storage directory and file size limit must be configured"))
	}
	if c.Worker.Count < 1 || c.Worker.Count > 64 || c.Worker.PollInterval <= 0 || c.Worker.SpeechTimeout <= 0 || c.Worker.LLMTimeout <= 0 ||
		c.Worker.LeaseDuration <= 0 || c.Worker.HeartbeatInterval <= 0 || c.Worker.RecoveryInterval <= 0 {
		errs = append(errs, errors.New("worker settings must be positive and MEETNOTE_WORKERS must not exceed 64"))
	}
	if c.Worker.HeartbeatInterval >= c.Worker.LeaseDuration {
		errs = append(errs, errors.New("MEETNOTE_LEASE_HEARTBEAT must be shorter than MEETNOTE_TASK_LEASE"))
	}
	if c.Runtime.OperationTimeout <= 0 {
		errs = append(errs, errors.New("MEETNOTE_OPERATION_TIMEOUT must be positive"))
	}
	if c.Telegram.PollTimeout <= 0 || c.Telegram.RequestTimeout <= 0 ||
		c.Telegram.DownloadTimeout <= 0 || c.Telegram.RetryMin <= 0 ||
		c.Telegram.RetryMax < c.Telegram.RetryMin {
		errs = append(errs, errors.New("telegram timeouts and retry intervals are invalid"))
	}
	if c.Telegram.UpdateWorkers < 1 || c.Telegram.UpdateWorkers > 64 || c.Telegram.UpdateLease <= 0 ||
		c.Telegram.UpdateMaxAttempts < 1 || c.Telegram.UpdateMaxAttempts > 100 || c.Telegram.UpdateRetention < 24*time.Hour {
		errs = append(errs, errors.New("telegram update worker settings are outside safe bounds"))
	}
	if strings.TrimSpace(c.Telegram.APIURL) == "" {
		errs = append(errs, errors.New("MEETNOTE_TELEGRAM_API_URL is empty"))
	}
	if c.Limits.MaxTranscriptRunes < 1 || c.Limits.MaxSummaryRunes < 1 || c.Limits.MaxAnswerRunes < 1 ||
		c.Limits.MaxQuestionRunes < 1 || c.Limits.MaxSearchQueryRunes < 1 || c.Limits.InlineTranscriptRunes < 1 ||
		c.Limits.MaxTelegramResponseParts < 1 {
		errs = append(errs, errors.New("content limits must be positive"))
	}
	if c.Limits.InlineTranscriptRunes > c.Limits.MaxTranscriptRunes {
		errs = append(errs, errors.New("MEETNOTE_INLINE_TRANSCRIPT_RUNES must not exceed MEETNOTE_MAX_TRANSCRIPT_RUNES"))
	}
	if c.Limits.MaxTelegramResponseParts > 16 {
		errs = append(errs, errors.New("MEETNOTE_TELEGRAM_MAX_RESPONSE_PARTS must not exceed 16"))
	}
	if c.Limits.MaxMeetingsPerUser < 1 || c.Limits.MaxMeetingsPerUser > 10_000 {
		errs = append(errs, errors.New("MEETNOTE_MAX_MEETINGS_PER_USER must be between 1 and 10000"))
	}
	if c.Limits.MaxPendingPerUser < 1 || c.Limits.MaxPendingPerUser > c.Limits.MaxMeetingsPerUser {
		errs = append(errs, errors.New("MEETNOTE_MAX_PENDING_MEETINGS_PER_USER must be positive and not exceed MEETNOTE_MAX_MEETINGS_PER_USER"))
	}
	if c.Limits.MaxStorageBytesPerUser < c.Storage.MaxFileSize {
		errs = append(errs, errors.New("MEETNOTE_MAX_STORAGE_MB_PER_USER must allow at least one maximum-size file"))
	}
	if c.Limits.MaxChatHistoryPerUser < 1 || c.Limits.MaxChatHistoryPerUser > 10_000 {
		errs = append(errs, errors.New("MEETNOTE_MAX_CHAT_HISTORY_PER_USER must be between 1 and 10000"))
	}
	// Срок блокировки должен перекрывать обработку обновления и отправку всех ответов Telegram.
	commandBudget, commandBudgetOK := safeDurationProduct(c.Telegram.RequestTimeout, c.Limits.MaxTelegramResponseParts+1)
	downloadBudget, downloadBudgetOK := safeDurationAdd(c.Telegram.DownloadTimeout, c.Telegram.RequestTimeout)
	if !commandBudgetOK || !downloadBudgetOK {
		errs = append(errs, errors.New("Telegram command timeout budget overflows time.Duration"))
	} else {
		commandBudget = max(commandBudget, downloadBudget)
		commandBudget, commandBudgetOK = safeDurationAdd(commandBudget, c.Runtime.OperationTimeout)
		if !commandBudgetOK {
			errs = append(errs, errors.New("Telegram command and persistence timeout budget overflows time.Duration"))
		} else if c.Telegram.UpdateLease <= commandBudget {
			errs = append(errs, fmt.Errorf("MEETNOTE_TELEGRAM_UPDATE_LEASE must exceed the maximum command budget of %s", commandBudget))
		}
	}
	if c.Clients.SpeechProvider != "mock" {
		errs = append(errs, fmt.Errorf("unsupported speech provider %q", c.Clients.SpeechProvider))
	}
	if c.Clients.LLMProvider != "mock" {
		errs = append(errs, fmt.Errorf("unsupported LLM provider %q", c.Clients.LLMProvider))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("unsupported MEETNOTE_LOG_LEVEL %q", c.LogLevel))
	}
	return errors.Join(errs...)
}

func safeDurationProduct(value time.Duration, multiplier int) (time.Duration, bool) {
	if value <= 0 || multiplier < 1 || value > time.Duration(math.MaxInt64/int64(multiplier)) {
		return 0, false
	}
	return value * time.Duration(multiplier), true
}

func safeDurationAdd(left, right time.Duration) (time.Duration, bool) {
	if left <= 0 || right <= 0 || left > time.Duration(math.MaxInt64)-right {
		return 0, false
	}
	return left + right, true
}

func (c Config) ValidateTelegram() error {
	var errs []error
	if strings.TrimSpace(c.Telegram.Token) == "" {
		errs = append(errs, errors.New("MEETNOTE_TELEGRAM_TOKEN is required in bot mode"))
	}
	if c.Storage.MaxFileSize > TelegramDownloadLimit {
		errs = append(errs, fmt.Errorf(
			"MEETNOTE_MAX_FILE_MB exceeds Telegram Bot API download limit of %d MB",
			TelegramDownloadLimit/(1024*1024),
		))
	}
	return errors.Join(errs...)
}

func mebibytesToBytes(variable string, value int64) (int64, error) {
	if value < 0 {
		return value, nil
	}
	if value > math.MaxInt64/(1024*1024) {
		return 0, fmt.Errorf("%s is too large", variable)
	}
	return value * 1024 * 1024, nil
}

func databaseConnectionLimit(value int64) (int32, error) {
	if value < math.MinInt32 || value > math.MaxInt32 {
		return 0, errors.New("MEETNOTE_DATABASE_MAX_CONNS is outside int32 range")
	}
	return int32(value), nil
}
