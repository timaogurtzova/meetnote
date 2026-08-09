package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/timaogurtzova/meetnote/internal/domain"
)

const (
	defaultListLimit = 100
	defaultChatLimit = 10
	maxUserIDLength  = 128
	maxRequestKeyLen = 160
	cleanupTimeout   = 5 * time.Second
)

// Limits задает ограничения для пользовательских данных и ответов внешних сервисов.
type Limits struct {
	MaxQuestionRunes    int
	MaxSearchQueryRunes int
	MaxAnswerRunes      int
	MaxChatHistory      int
	MeetingQuota        domain.MeetingQuota
}

type Service struct {
	repository Repository
	files      FileStore
	llm        LLMClient
	limits     Limits
	logger     zerolog.Logger
}

func NewService(repository Repository, files FileStore, llm LLMClient, limits Limits, logger zerolog.Logger) (*Service, error) {
	if repository == nil || files == nil || llm == nil {
		return nil, errors.New("application dependencies must not be nil")
	}
	if limits.MaxQuestionRunes < 1 || limits.MaxSearchQueryRunes < 1 ||
		limits.MaxAnswerRunes < 1 || limits.MaxChatHistory < 1 {
		return nil, errors.New("application text limits must be positive")
	}
	if limits.MeetingQuota.MaxMeetings < 1 || limits.MeetingQuota.MaxPending < 1 ||
		limits.MeetingQuota.MaxPending > limits.MeetingQuota.MaxMeetings || limits.MeetingQuota.MaxStorageBytes < 1 {
		return nil, errors.New("application meeting quotas are invalid")
	}
	return &Service{repository: repository, files: files, llm: llm, limits: limits, logger: logger}, nil
}

func (s *Service) Start(ctx context.Context, userID string) error {
	userID, err := validateUserID(userID)
	if err != nil {
		return err
	}
	if err := s.repository.RegisterUser(ctx, userID); err != nil {
		return fmt.Errorf("register user: %w", err)
	}
	s.logger.Info().Ctx(ctx).Str("user_ref", safeUserRef(userID)).Msg("user registered")
	return nil
}

func (s *Service) Load(
	ctx context.Context,
	userID string,
	filename string,
	source io.Reader,
	size int64,
	requestKey string,
) (domain.Meeting, error) {
	userID, err := validateUserID(userID)
	if err != nil {
		return domain.Meeting{}, err
	}
	requestKey, err = validateRequestKey(requestKey)
	if err != nil {
		return domain.Meeting{}, err
	}
	filename = strings.TrimSpace(filename)
	if filename == "" || source == nil || size < 0 {
		return domain.Meeting{}, fmt.Errorf("%w: file name, content and non-negative size are required", domain.ErrInvalidInput)
	}
	file, err := s.files.Save(ctx, userID, filename, source, size)
	if err != nil {
		return domain.Meeting{}, fmt.Errorf("store upload: %w", err)
	}
	meeting, created, err := s.repository.CreateMeeting(ctx, userID, requestKey, file, s.limits.MeetingQuota)
	if err != nil {
		if errors.Is(err, domain.ErrQuotaExceeded) {
			s.removeUpload(ctx, file.Path, "quota-rejected upload cleanup failed", 0)
			return domain.Meeting{}, fmt.Errorf("create meeting: %w", err)
		}
		// Результат COMMIT может быть неоднозначным: PostgreSQL уже сохранил встречу,
		// хотя клиент получил сетевую ошибку. Файл остается до плановой сверки с базой,
		// чтобы не удалить данные созданной встречи.
		s.logger.Error().Ctx(ctx).Str("user_ref", safeUserRef(userID)).Err(err).
			Msg("meeting creation outcome is uncertain; upload retained for reconciliation")
		return domain.Meeting{}, fmt.Errorf("create meeting: %w", err)
	}
	if !created {
		s.removeUpload(ctx, file.Path, "duplicate upload cleanup failed", meeting.ID)
		s.logger.Info().Ctx(ctx).Int64("meeting_id", meeting.ID).Msg("duplicate meeting request reused")
		return meeting, nil
	}
	s.logger.Info().Ctx(ctx).
		Str("user_ref", safeUserRef(userID)).
		Int64("meeting_id", meeting.ID).
		Int64("file_size", file.Size).
		Msg("meeting and task created")
	return meeting, nil
}

func (s *Service) List(ctx context.Context, userID string) ([]domain.Meeting, error) {
	userID, err := s.ensureUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	meetings, err := s.repository.ListMeetings(ctx, userID, defaultListLimit)
	if err != nil {
		return nil, fmt.Errorf("list meetings: %w", err)
	}
	return meetings, nil
}

func (s *Service) Status(ctx context.Context, userID string, meetingID int64) (domain.Meeting, error) {
	if meetingID < 1 {
		return domain.Meeting{}, fmt.Errorf("%w: meeting id must be positive", domain.ErrInvalidInput)
	}
	userID, err := s.ensureUser(ctx, userID)
	if err != nil {
		return domain.Meeting{}, err
	}
	meeting, err := s.repository.GetMeeting(ctx, userID, meetingID)
	if err != nil {
		return domain.Meeting{}, fmt.Errorf("get meeting status: %w", err)
	}
	return meeting, nil
}

func (s *Service) Get(ctx context.Context, userID string, meetingID int64) (string, error) {
	if meetingID < 1 {
		return "", fmt.Errorf("%w: meeting id must be positive", domain.ErrInvalidInput)
	}
	userID, err := s.ensureUser(ctx, userID)
	if err != nil {
		return "", err
	}
	transcript, err := s.repository.GetTranscript(ctx, userID, meetingID)
	if err != nil {
		return "", fmt.Errorf("get transcript: %w", err)
	}
	return transcript, nil
}

func (s *Service) Find(ctx context.Context, userID, query string) ([]domain.SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" || utf8.RuneCountInString(query) > s.limits.MaxSearchQueryRunes {
		return nil, fmt.Errorf("%w: search query must contain 1-%d characters", domain.ErrInvalidInput, s.limits.MaxSearchQueryRunes)
	}
	userID, err := s.ensureUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	result, err := s.repository.FindMeetings(ctx, userID, query, defaultListLimit)
	if err != nil {
		return nil, fmt.Errorf("find meetings: %w", err)
	}
	return result, nil
}

func (s *Service) Chat(ctx context.Context, userID, question, requestKey string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" || utf8.RuneCountInString(question) > s.limits.MaxQuestionRunes {
		return "", fmt.Errorf("%w: question must contain 1-%d characters", domain.ErrInvalidInput, s.limits.MaxQuestionRunes)
	}
	requestKey, err := validateRequestKey(requestKey)
	if err != nil {
		return "", err
	}
	userID, err = s.ensureUser(ctx, userID)
	if err != nil {
		return "", err
	}
	if previous, found, err := s.repository.GetChatAnswer(ctx, userID, requestKey); err != nil {
		return "", fmt.Errorf("load idempotent chat answer: %w", err)
	} else if found {
		return previous, nil
	}
	documents, err := s.repository.ChatDocuments(ctx, userID, defaultChatLimit)
	if err != nil {
		return "", fmt.Errorf("load chat context: %w", err)
	}
	if len(documents) == 0 {
		return "", domain.ErrNotReady
	}
	answer, err := s.llm.Answer(ctx, question, documents)
	if err != nil {
		return "", fmt.Errorf("answer question: %w", err)
	}
	answer = strings.TrimSpace(answer)
	if answer == "" || !utf8.ValidString(answer) || utf8.RuneCountInString(answer) > s.limits.MaxAnswerRunes {
		return "", fmt.Errorf("answer question: %w", domain.ErrInvalidClientData)
	}
	answer, err = s.repository.SaveChat(ctx, userID, requestKey, question, answer, s.limits.MaxChatHistory)
	if err != nil {
		return "", fmt.Errorf("save chat history: %w", err)
	}
	s.logger.Info().Ctx(ctx).Str("user_ref", safeUserRef(userID)).Int("documents", len(documents)).Msg("chat answered")
	return answer, nil
}

func (s *Service) Retry(ctx context.Context, userID, requestKey string, meetingID int64) error {
	if meetingID < 1 {
		return fmt.Errorf("%w: meeting id must be positive", domain.ErrInvalidInput)
	}
	userID, err := s.ensureUser(ctx, userID)
	if err != nil {
		return err
	}
	requestKey, err = validateRequestKey(requestKey)
	if err != nil {
		return err
	}
	if err := s.repository.RetryMeeting(ctx, userID, requestKey, meetingID, s.limits.MeetingQuota.MaxPending); err != nil {
		return fmt.Errorf("retry meeting: %w", err)
	}
	s.logger.Info().Ctx(ctx).Str("user_ref", safeUserRef(userID)).Int64("meeting_id", meetingID).Msg("meeting queued for retry")
	return nil
}

func (s *Service) removeUpload(ctx context.Context, path, failureMessage string, meetingID int64) {
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancelCleanup()
	if err := s.files.Remove(cleanupCtx, path); err != nil {
		event := s.logger.Error().Ctx(ctx).Err(err)
		if meetingID > 0 {
			event.Int64("meeting_id", meetingID)
		}
		event.Msg(failureMessage)
	}
}

// CleanupOrphans удаляет файлы, оставшиеся после сбоя между записью на диск и транзакцией.
// Льготный период защищает загрузки, которые еще могут выполняться.
func (s *Service) CleanupOrphans(ctx context.Context, gracePeriod time.Duration) (int, error) {
	if gracePeriod <= 0 {
		return 0, fmt.Errorf("%w: orphan grace period must be positive", domain.ErrInvalidInput)
	}
	paths, err := s.repository.ListStoredPaths(ctx)
	if err != nil {
		return 0, fmt.Errorf("list referenced uploads: %w", err)
	}
	keep := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		keep[path] = struct{}{}
	}
	removed, err := s.files.RemoveOrphans(ctx, keep, time.Now().Add(-gracePeriod))
	if err != nil {
		return removed, fmt.Errorf("remove orphan uploads: %w", err)
	}
	if removed > 0 {
		s.logger.Warn().Ctx(ctx).Int("files", removed).Msg("orphan uploads removed")
	}
	return removed, nil
}

func validateRequestKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > maxRequestKeyLen {
		return "", fmt.Errorf("%w: request key must contain 1-%d characters", domain.ErrInvalidInput, maxRequestKeyLen)
	}
	return value, nil
}

func safeUserRef(userID string) string {
	digest := sha256.Sum256([]byte(userID))
	return hex.EncodeToString(digest[:6])
}

func (s *Service) ensureUser(ctx context.Context, userID string) (string, error) {
	validUserID, err := validateUserID(userID)
	if err != nil {
		return "", err
	}
	if err := s.repository.RegisterUser(ctx, validUserID); err != nil {
		return "", fmt.Errorf("ensure user: %w", err)
	}
	return validUserID, nil
}

func validateUserID(userID string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" || utf8.RuneCountInString(userID) > maxUserIDLength {
		return "", fmt.Errorf("%w: user id must contain 1-%d characters", domain.ErrInvalidInput, maxUserIDLength)
	}
	return userID, nil
}
