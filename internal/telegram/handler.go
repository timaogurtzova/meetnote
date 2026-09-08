package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/timaogurtzova/meetnote/internal/domain"
)

const maxTelegramMessageRunes = 4000

type Application interface {
	Start(ctx context.Context, userID string) error
	Load(ctx context.Context, userID, filename string, source io.Reader, size int64, requestKey string) (domain.Meeting, error)
	List(ctx context.Context, userID string) ([]domain.Meeting, error)
	Status(ctx context.Context, userID string, meetingID int64) (domain.Meeting, error)
	Get(ctx context.Context, userID string, meetingID int64) (string, error)
	Find(ctx context.Context, userID, query string) ([]domain.SearchResult, error)
	Chat(ctx context.Context, userID, question, requestKey string) (string, error)
	Retry(ctx context.Context, userID, requestKey string, meetingID int64) error
}

type HandlerConfig struct {
	RequestTimeout        time.Duration
	DownloadTimeout       time.Duration
	MaxFileSize           int64
	InlineTranscriptRunes int
	MaxResponseParts      int
}

type Handler struct {
	application Application
	api         API
	config      HandlerConfig
	logger      *slog.Logger
}

func NewHandler(
	application Application,
	api API,
	config HandlerConfig,
	logger *slog.Logger,
) (*Handler, error) {
	if application == nil || api == nil || logger == nil {
		return nil, errors.New("telegram handler dependencies must not be nil")
	}
	if config.RequestTimeout <= 0 || config.DownloadTimeout <= 0 || config.MaxFileSize <= 0 ||
		config.InlineTranscriptRunes < 1 || config.MaxResponseParts < 1 {
		return nil, errors.New("telegram handler limits and timeouts must be positive")
	}
	return &Handler{application: application, api: api, config: config, logger: logger}, nil
}

func (h *Handler) Handle(ctx context.Context, update Update) error {
	message := update.Message
	if message == nil || message.From == nil || message.From.ID <= 0 || message.Chat.ID == 0 {
		return nil
	}
	if message.Chat.Type != "private" {
		return h.send(ctx, message.Chat.ID, "Для защиты материалов встреч бот работает только в личных чатах.")
	}

	userID := "telegram:" + strconv.FormatInt(message.From.ID, 10)
	requestKey := telegramRequestKey(update.UpdateID)
	if attachment, kind := messageAttachment(message); attachment != nil {
		return h.handleUpload(ctx, update.UpdateID, message.Chat.ID, userID, kind, *attachment)
	}

	command, argument, ok := parseCommand(message.Text)
	if !ok {
		return h.send(ctx, message.Chat.ID, helpText())
	}

	requestCtx, cancelRequest := context.WithTimeout(ctx, h.config.RequestTimeout)
	defer cancelRequest()

	var response string
	var err error
	switch command {
	case "start":
		err = h.application.Start(requestCtx, userID)
		response = "Готово. Отправьте голосовое сообщение, аудиофайл или документ с тестовой транскрипцией."
	case "help":
		response = helpText()
	case "load":
		response = "Прикрепите к сообщению голосовую запись, аудиофайл или файл .txt/.md."
	case "list":
		if argument != "" {
			err = invalidUsage("/list не принимает аргументы")
			break
		}
		var meetings []domain.Meeting
		meetings, err = h.application.List(requestCtx, userID)
		response = formatMeetings(meetings)
	case "status":
		var meetingID int64
		meetingID, err = parseMeetingID(argument, "/status ID")
		if err == nil {
			var meeting domain.Meeting
			meeting, err = h.application.Status(requestCtx, userID, meetingID)
			response = formatStatus(meeting)
		}
	case "get":
		var meetingID int64
		meetingID, err = parseMeetingID(argument, "/get ID")
		if err == nil {
			response, err = h.application.Get(requestCtx, userID, meetingID)
			if err == nil && utf8.RuneCountInString(response) > h.config.InlineTranscriptRunes {
				return h.sendTranscript(ctx, message.Chat.ID, meetingID, response)
			}
		}
	case "find":
		if argument == "" {
			err = invalidUsage("использование: /find ключевое слово или фраза")
			break
		}
		var result []domain.SearchResult
		result, err = h.application.Find(requestCtx, userID, argument)
		response = formatSearch(result)
	case "chat":
		if argument == "" {
			err = invalidUsage("использование: /chat вопрос")
			break
		}
		response, err = h.application.Chat(requestCtx, userID, argument, requestKey)
	case "retry":
		var meetingID int64
		meetingID, err = parseMeetingID(argument, "/retry ID")
		if err == nil {
			err = h.application.Retry(requestCtx, userID, requestKey, meetingID)
			response = fmt.Sprintf("Встреча #%d повторно поставлена в очередь.", meetingID)
		}
	default:
		err = invalidUsage("неизвестная команда; используйте /help")
	}
	if err != nil {
		return h.replyError(ctx, message.Chat.ID, update.UpdateID, err)
	}
	return h.send(ctx, message.Chat.ID, response)
}

func (h *Handler) handleUpload(
	ctx context.Context,
	updateID int64,
	chatID int64,
	userID string,
	kind string,
	attachment Attachment,
) error {
	if attachment.FileSize > h.config.MaxFileSize {
		return h.send(ctx, chatID, h.userError(domain.ErrFileTooLarge))
	}

	downloadCtx, cancelDownload := context.WithTimeout(ctx, h.config.DownloadTimeout)
	defer cancelDownload()
	remoteFile, err := h.api.GetFile(downloadCtx, attachment.FileID)
	if err != nil {
		return h.replyError(ctx, chatID, updateID, fmt.Errorf("get telegram file: %w", err))
	}
	fileSize := max(attachment.FileSize, remoteFile.FileSize)
	if fileSize > h.config.MaxFileSize {
		return h.send(ctx, chatID, h.userError(domain.ErrFileTooLarge))
	}
	body, err := h.api.OpenFile(downloadCtx, remoteFile.FilePath)
	if err != nil {
		return h.replyError(ctx, chatID, updateID, fmt.Errorf("download telegram file: %w", err))
	}
	defer body.Close()

	filename := attachmentFilename(updateID, kind, attachment)
	meeting, err := h.application.Load(downloadCtx, userID, filename, body, fileSize, telegramRequestKey(updateID))
	if err != nil {
		return h.replyError(ctx, chatID, updateID, err)
	}
	return h.send(
		ctx,
		chatID,
		fmt.Sprintf("Встреча #%d создана, статус: %s. Обработка идёт в фоне.", meeting.ID, meeting.Status),
	)
}

func (h *Handler) replyError(ctx context.Context, chatID, updateID int64, operationErr error) error {
	sendErr := h.send(ctx, chatID, h.userError(operationErr))
	if isExpectedUserError(operationErr) {
		return sendErr
	}
	h.logger.ErrorContext(ctx, "telegram business operation failed", "update_id", updateID, "error", operationErr)
	return sendErr
}

func (h *Handler) send(ctx context.Context, chatID int64, text string) error {
	chunks := splitText(text, maxTelegramMessageRunes)
	if len(chunks) > h.config.MaxResponseParts {
		chunks = chunks[:h.config.MaxResponseParts]
		notice := "\n\n[Ответ сокращён из-за лимита Telegram.]"
		last := chunks[len(chunks)-1]
		lastRunes := []rune(last)
		noticeRunes := []rune(notice)
		if len(lastRunes)+len(noticeRunes) > maxTelegramMessageRunes {
			lastRunes = lastRunes[:maxTelegramMessageRunes-len(noticeRunes)]
		}
		chunks[len(chunks)-1] = string(lastRunes) + notice
	}
	for _, chunk := range chunks {
		sendCtx, cancelSend := context.WithTimeout(ctx, h.config.RequestTimeout)
		err := h.api.SendMessage(sendCtx, chatID, chunk)
		cancelSend()
		if err != nil {
			return fmt.Errorf("send telegram response: %w", err)
		}
	}
	return nil
}

func (h *Handler) sendTranscript(ctx context.Context, chatID, meetingID int64, transcript string) error {
	filename := fmt.Sprintf("meeting-%d-transcript.txt", meetingID)
	sendCtx, cancelSend := context.WithTimeout(ctx, h.config.RequestTimeout)
	defer cancelSend()
	if err := h.api.SendDocument(
		sendCtx,
		chatID,
		filename,
		strings.NewReader(transcript),
		int64(len(transcript)),
		fmt.Sprintf("Полная транскрипция встречи #%d", meetingID),
	); err != nil {
		return fmt.Errorf("send Telegram transcript document: %w", err)
	}
	return nil
}

func telegramRequestKey(updateID int64) string {
	return fmt.Sprintf("telegram:update:%d", updateID)
}

func parseCommand(text string) (command, argument string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	name := text
	if separator := strings.IndexFunc(text, unicode.IsSpace); separator >= 0 {
		name, argument = text[:separator], text[separator:]
	}
	name = strings.TrimPrefix(name, "/")
	name, _, _ = strings.Cut(name, "@")
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", "", false
	}
	return name, strings.TrimSpace(argument), true
}

func parseMeetingID(value, usage string) (int64, error) {
	parts := strings.Fields(value)
	if len(parts) != 1 {
		return 0, invalidUsage("использование: " + usage)
	}
	meetingID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || meetingID < 1 {
		return 0, invalidUsage("идентификатор встречи должен быть положительным числом")
	}
	return meetingID, nil
}

func invalidUsage(message string) error {
	return commandInputError{message: message}
}

type commandInputError struct {
	message string
}

func (e commandInputError) Error() string { return e.message }

func (e commandInputError) Unwrap() error { return domain.ErrInvalidInput }

func messageAttachment(message *Message) (*Attachment, string) {
	switch {
	case message.Voice != nil:
		return message.Voice, "voice"
	case message.Audio != nil:
		return message.Audio, "audio"
	case message.Document != nil:
		return message.Document, "document"
	default:
		return nil, ""
	}
}

func attachmentFilename(updateID int64, kind string, attachment Attachment) string {
	if filename := filepath.Base(strings.TrimSpace(attachment.FileName)); filename != "" && filename != "." {
		return filename
	}
	extension := extensionForMIME(attachment.MIMEType)
	if extension == "" && kind == "voice" {
		extension = ".ogg"
	}
	return fmt.Sprintf("%s-%d%s", kind, updateID, extension)
}

func extensionForMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/ogg", "application/ogg":
		return ".ogg"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/flac":
		return ".flac"
	default:
		return ""
	}
}

func formatMeetings(meetings []domain.Meeting) string {
	if len(meetings) == 0 {
		return "Встреч пока нет."
	}
	var result strings.Builder
	result.WriteString("Ваши встречи:\n")
	for _, meeting := range meetings {
		fmt.Fprintf(&result, "\n#%d · %s · %s", meeting.ID, formatTime(meeting.CreatedAt), meeting.Status)
		if meeting.Summary != "" {
			fmt.Fprintf(&result, "\n%s", oneLine(meeting.Summary, 300))
		}
	}
	return result.String()
}

func formatStatus(meeting domain.Meeting) string {
	var result strings.Builder
	fmt.Fprintf(&result, "Встреча #%d\nФайл: %s\nСтатус: %s\nСоздана: %s\nОбновлена: %s",
		meeting.ID,
		meeting.OriginalFilename,
		meeting.Status,
		formatTime(meeting.CreatedAt),
		formatTime(meeting.UpdatedAt),
	)
	if meeting.Summary != "" {
		fmt.Fprintf(&result, "\nВыжимка: %s", meeting.Summary)
	}
	if meeting.Error != "" {
		fmt.Fprintf(&result, "\nОшибка: %s", meeting.Error)
	}
	return result.String()
}

func formatSearch(result []domain.SearchResult) string {
	if len(result) == 0 {
		return "Совпадений нет."
	}
	var output strings.Builder
	output.WriteString("Результаты поиска:\n")
	for _, item := range result {
		fmt.Fprintf(&output, "\n#%d · %s · %s\n%s",
			item.MeetingID,
			formatTime(item.CreatedAt),
			item.Status,
			oneLine(item.Snippet, 400),
		)
	}
	return output.String()
}

func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02 15:04:05 UTC")
}

func oneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func splitText(value string, limit int) []string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return []string{"—"}
	}
	result := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > limit {
		cut := limit
		for index := limit; index > limit/2; index-- {
			if runes[index-1] == '\n' {
				cut = index
				break
			}
		}
		result = append(result, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		result = append(result, string(runes))
	}
	return result
}

func helpText() string {
	return "Я сохраняю и конспектирую встречи.\n\n" +
		"Отправьте голосовое сообщение, аудиофайл или .txt/.md файл.\n\n" +
		"/load — подсказка по загрузке встречи\n" +
		"/list — список встреч\n" +
		"/status ID — статус обработки\n" +
		"/get ID — полная транскрипция\n" +
		"/find текст — поиск по встречам\n" +
		"/chat вопрос — вопрос по завершённым встречам\n" +
		"/retry ID — повторить failed-встречу\n" +
		"/help — эта справка"
}

func (h *Handler) userError(err error) string {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return "Встреча не найдена или принадлежит другому пользователю."
	case errors.Is(err, domain.ErrNotReady):
		return "Результат ещё не готов. Проверьте статус командой /status ID."
	case errors.Is(err, domain.ErrInvalidState):
		return "Операция недоступна для текущего статуса встречи."
	case errors.Is(err, domain.ErrUnsupportedFormat):
		return "Формат файла не поддерживается. Отправьте txt, md, wav, mp3, m4a, ogg или flac."
	case errors.Is(err, domain.ErrFileTooLarge):
		return fmt.Sprintf("Файл превышает допустимый размер %s.", formatBytes(h.config.MaxFileSize))
	case errors.Is(err, domain.ErrQuotaExceeded):
		return "Новая загрузка отклонена: достигнут лимит очереди, числа встреч или хранилища."
	case errors.Is(err, context.DeadlineExceeded):
		return "Превышено время ожидания операции. Попробуйте ещё раз."
	case errors.Is(err, context.Canceled):
		return "Операция отменена."
	case errors.Is(err, domain.ErrUnavailable):
		return "Внешний сервис временно недоступен. Попробуйте позже."
	case errors.Is(err, domain.ErrInvalidClientData):
		return "Внешний сервис вернул некорректный результат. Попробуйте позже."
	case errors.Is(err, domain.ErrInvalidInput):
		var commandErr commandInputError
		if errors.As(err, &commandErr) {
			return commandErr.Error()
		}
		return "Некорректные данные. Проверьте команду и попробуйте снова."
	default:
		return "Внутренняя ошибка. Попробуйте позже."
	}
}

func formatBytes(size int64) string {
	const megabyte = int64(1024 * 1024)
	if size%megabyte == 0 {
		return fmt.Sprintf("%d МБ", size/megabyte)
	}
	return fmt.Sprintf("%d байт", size)
}

func isExpectedUserError(err error) bool {
	return errors.Is(err, domain.ErrNotFound) ||
		errors.Is(err, domain.ErrNotReady) ||
		errors.Is(err, domain.ErrInvalidState) ||
		errors.Is(err, domain.ErrInvalidInput) ||
		errors.Is(err, domain.ErrUnsupportedFormat) ||
		errors.Is(err, domain.ErrFileTooLarge) ||
		errors.Is(err, domain.ErrQuotaExceeded) ||
		errors.Is(err, context.Canceled)
}
