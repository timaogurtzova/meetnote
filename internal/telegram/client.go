// Пакет telegram реализует адаптер Telegram Bot API.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/timaogurtzova/meetnote/internal/domain"
)

const maxResponseSize = 2 << 20

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

type Message struct {
	From     *User       `json:"from,omitempty"`
	Chat     Chat        `json:"chat"`
	Text     string      `json:"text,omitempty"`
	Document *Attachment `json:"document,omitempty"`
	Audio    *Attachment `json:"audio,omitempty"`
	Voice    *Attachment `json:"voice,omitempty"`
}

type User struct {
	ID int64 `json:"id"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Attachment struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type RemoteFile struct {
	FileSize int64  `json:"file_size,omitempty"`
	FilePath string `json:"file_path,omitempty"`
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// API описывает вызовы Telegram, нужные боту и обработчику команд.
type API interface {
	DeleteWebhook(ctx context.Context) error
	SetCommands(ctx context.Context, commands []BotCommand) error
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) error
	SendDocument(ctx context.Context, chatID int64, filename string, content io.Reader, size int64, caption string) error
	GetFile(ctx context.Context, fileID string) (RemoteFile, error)
	OpenFile(ctx context.Context, filePath string) (io.ReadCloser, error)
}

type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

func NewClient(token, baseURL string, httpClient *http.Client) (*Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("telegram token must not be empty")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("telegram API URL must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("telegram API URL must use HTTPS outside loopback tests")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{token: token, baseURL: baseURL, httpClient: httpClient}, nil
}

func (c *Client) DeleteWebhook(ctx context.Context) error {
	return callTelegram(ctx, c, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

func (c *Client) SetCommands(ctx context.Context, commands []BotCommand) error {
	return callTelegram(ctx, c, "setMyCommands", map[string]any{"commands": commands}, nil)
}

func (c *Client) GetUpdates(
	ctx context.Context,
	offset int64,
	timeout time.Duration,
) ([]Update, error) {
	seconds := int(timeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	var updates []Update
	err := callTelegram(ctx, c, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         seconds,
		"allowed_updates": []string{"message"},
	}, &updates)
	return updates, err
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, message string) error {
	if chatID == 0 || message == "" {
		return fmt.Errorf("%w: chat id and message are required", domain.ErrInvalidInput)
	}
	return callTelegram(ctx, c, "sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    message,
	}, nil)
}

func (c *Client) SendDocument(
	ctx context.Context,
	chatID int64,
	filename string,
	content io.Reader,
	size int64,
	caption string,
) error {
	filename = strings.TrimSpace(filename)
	if chatID == 0 || filename == "" || content == nil || size < 0 {
		return fmt.Errorf("%w: chat id, filename, content, and non-negative size are required", domain.ErrInvalidInput)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return fmt.Errorf("encode Telegram document chat id: %w", err)
	}
	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return fmt.Errorf("encode Telegram document caption: %w", err)
		}
	}
	part, err := writer.CreateFormFile("document", filename)
	if err != nil {
		return fmt.Errorf("encode Telegram document metadata: %w", err)
	}
	written, err := io.Copy(part, content)
	if err != nil {
		return fmt.Errorf("encode Telegram document content: %w", err)
	}
	if written != size {
		return fmt.Errorf("%w: Telegram document size mismatch: declared %d, read %d", domain.ErrInvalidInput, size, written)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish Telegram document request: %w", err)
	}
	endpoint := c.baseURL + "/bot" + c.token + "/sendDocument"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return fmt.Errorf("create Telegram sendDocument request: %w", domain.ErrInvalidInput)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return performTelegramRequest(ctx, c, "sendDocument", request, nil)
}

func (c *Client) GetFile(ctx context.Context, fileID string) (RemoteFile, error) {
	if strings.TrimSpace(fileID) == "" {
		return RemoteFile{}, fmt.Errorf("%w: telegram file id is required", domain.ErrInvalidInput)
	}
	var file RemoteFile
	err := callTelegram(ctx, c, "getFile", map[string]any{"file_id": fileID}, &file)
	if err != nil {
		return RemoteFile{}, err
	}
	if file.FilePath == "" {
		return RemoteFile{}, fmt.Errorf("telegram getFile response has no file path: %w", domain.ErrInvalidClientData)
	}
	return file, nil
}

func (c *Client) OpenFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	escapedPath, err := safeRemotePath(filePath)
	if err != nil {
		return nil, err
	}
	endpoint := c.baseURL + "/file/bot" + c.token + "/" + escapedPath
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create telegram file request: %w", domain.ErrInvalidInput)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("download telegram file: %w", domain.ErrUnavailable)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
		response.Body.Close()
		return nil, &APIError{Method: "downloadFile", Code: response.StatusCode, Description: http.StatusText(response.StatusCode), RetryAfter: retryAfter}
	}
	return response.Body, nil
}

type apiEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  struct {
		RetryAfter int `json:"retry_after,omitempty"`
	} `json:"parameters,omitempty"`
}

// APIError хранит код Telegram и задержку повтора, не раскрывая токен бота.
type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s failed (code %d): %s", e.Method, e.Code, e.Description)
}

func (e *APIError) Unwrap() error { return domain.ErrUnavailable }

func callTelegram(
	ctx context.Context,
	client *Client,
	method string,
	payload any,
	result any,
) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode telegram %s request: %w", method, err)
	}
	endpoint := client.baseURL + "/bot" + client.token + "/" + method
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create telegram %s request: %w", method, domain.ErrInvalidInput)
	}
	request.Header.Set("Content-Type", "application/json")
	return performTelegramRequest(ctx, client, method, request, result)
}

func performTelegramRequest(
	ctx context.Context,
	client *Client,
	method string,
	request *http.Request,
	result any,
) error {
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Ошибка net/http содержит URL с токеном, поэтому ее нельзя передавать в логи.
		return fmt.Errorf("telegram %s request: %w", method, domain.ErrUnavailable)
	}
	defer response.Body.Close()

	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseSize))
	var envelope apiEnvelope
	decodeErr := decoder.Decode(&envelope)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		description := http.StatusText(response.StatusCode)
		code := response.StatusCode
		retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
		if decodeErr == nil {
			if value := strings.TrimSpace(envelope.Description); value != "" {
				description = value
			}
			if envelope.ErrorCode != 0 {
				code = envelope.ErrorCode
			}
			if envelope.Parameters.RetryAfter > 0 {
				retryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
			}
		}
		return &APIError{Method: method, Code: code, Description: description, RetryAfter: retryAfter}
	}
	if decodeErr != nil {
		return fmt.Errorf("decode telegram %s response: %v: %w", method, decodeErr, domain.ErrUnavailable)
	}
	if !envelope.OK {
		description := strings.TrimSpace(envelope.Description)
		if description == "" {
			description = http.StatusText(response.StatusCode)
		}
		code := response.StatusCode
		if envelope.ErrorCode != 0 {
			code = envelope.ErrorCode
		}
		return &APIError{
			Method: method, Code: code, Description: description,
			RetryAfter: time.Duration(envelope.Parameters.RetryAfter) * time.Second,
		}
	}
	if result == nil {
		return nil
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "true" || string(envelope.Result) == "null" {
		return fmt.Errorf("decode telegram %s result: missing result: %w", method, domain.ErrInvalidClientData)
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("decode telegram %s result: %v: %w", method, err, domain.ErrInvalidClientData)
	}
	return nil
}

func retryDelayFor(err error, fallback, maximum time.Duration) time.Duration {
	delay := fallback
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		delay = apiErr.RetryAfter
	}
	if maximum > 0 && delay > maximum {
		return maximum
	}
	return delay
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func safeRemotePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, "/")
	if value == "" || len(parts) == 0 {
		return "", fmt.Errorf("%w: telegram file path is empty", domain.ErrInvalidInput)
	}
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: unsafe telegram file path", domain.ErrInvalidInput)
		}
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/"), nil
}
