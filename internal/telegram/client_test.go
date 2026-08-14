package telegram_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/domain"
	"github.com/timaogurtzova/meetnote/internal/telegram"
)

func TestClientCallsTelegramBotAPIAndDownloadsFile(t *testing.T) {
	t.Parallel()
	const token = "123456:test-token"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/bot" + token + "/deleteWebhook", "/bot" + token + "/setMyCommands":
			fmt.Fprint(response, `{"ok":true,"result":true}`)
		case "/bot" + token + "/getUpdates":
			var payload struct {
				Offset  int64 `json:"offset"`
				Timeout int   `json:"timeout"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Errorf("decode getUpdates: %v", err)
			}
			if payload.Offset != 10 || payload.Timeout != 2 {
				t.Errorf("getUpdates payload = %#v", payload)
			}
			fmt.Fprint(response, `{"ok":true,"result":[{"update_id":10,"message":{"message_id":1,"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"/start"}}]}`)
		case "/bot" + token + "/sendMessage":
			fmt.Fprint(response, `{"ok":true,"result":{"message_id":2}}`)
		case "/bot" + token + "/sendDocument":
			if err := request.ParseMultipartForm(1024 * 1024); err != nil {
				t.Errorf("parse sendDocument: %v", err)
			}
			file, _, err := request.FormFile("document")
			if err != nil {
				t.Errorf("read sendDocument file: %v", err)
			} else {
				defer file.Close()
				content, _ := io.ReadAll(file)
				if string(content) != "transcript" {
					t.Errorf("document content = %q", content)
				}
			}
			fmt.Fprint(response, `{"ok":true,"result":{"message_id":3}}`)
		case "/bot" + token + "/getFile":
			fmt.Fprint(response, `{"ok":true,"result":{"file_id":"f1","file_size":5,"file_path":"voice/file.ogg"}}`)
		case "/file/bot" + token + "/voice/file.ogg":
			fmt.Fprint(response, "audio")
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client, err := telegram.NewClient(token, server.URL, server.Client())
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, client.DeleteWebhook(ctx))
	require.NoError(t, client.SetCommands(ctx, []telegram.BotCommand{{Command: "start", Description: "Начать работу"}}))
	updates, err := client.GetUpdates(ctx, 10, 2*time.Second)
	if err != nil || len(updates) != 1 || updates[0].UpdateID != 10 {
		t.Fatalf("GetUpdates() = %#v, %v", updates, err)
	}
	require.NoError(t, client.SendMessage(ctx, 7, "ok"))
	require.NoError(t, client.SendDocument(ctx, 7, "meeting.txt", strings.NewReader("transcript"), 10, "caption"))
	file, err := client.GetFile(ctx, "f1")
	if err != nil || file.FilePath != "voice/file.ogg" {
		t.Fatalf("GetFile() = %#v, %v", file, err)
	}
	body, err := client.OpenFile(ctx, file.FilePath)
	if err != nil {
		t.Fatalf("OpenFile() error: %v", err)
	}
	defer body.Close()
	content, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, "audio", string(content))
}

func TestClientExposesTelegramRetryAfter(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(response, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":17}}`)
	}))
	defer server.Close()
	client, err := telegram.NewClient("token", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.SendMessage(context.Background(), 1, "hello")
	var apiErr *telegram.APIError
	if !errors.Is(err, domain.ErrUnavailable) || !errors.As(err, &apiErr) || apiErr.RetryAfter != 17*time.Second {
		t.Fatalf("SendMessage() error=%v", err)
	}
}

func TestClientClassifiesNonJSONHTTPFailureAsUnavailable(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Retry-After", "3")
		response.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(response, "upstream unavailable")
	}))
	defer server.Close()
	client, err := telegram.NewClient("token", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.SendMessage(context.Background(), 1, "hello")
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("SendMessage() error = %v, want unavailable", err)
	}
	var apiErr *telegram.APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != 3*time.Second {
		t.Fatalf("SendMessage() API error = %#v", apiErr)
	}
}

func TestClientRejectsMalformedSuccessfulResult(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{"ok":true,"result":"not-a-file"}`)
	}))
	defer server.Close()
	client, err := telegram.NewClient("token", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetFile(context.Background(), "file-id")
	if !errors.Is(err, domain.ErrInvalidClientData) {
		t.Fatalf("GetFile() error = %v, want invalid client data", err)
	}
}

func TestClientRejectsMissingSuccessfulResult(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{"ok":true}`)
	}))
	defer server.Close()
	client, err := telegram.NewClient("token", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetUpdates(context.Background(), 0, time.Second)
	if !errors.Is(err, domain.ErrInvalidClientData) {
		t.Fatalf("GetUpdates() error = %v, want invalid client data", err)
	}
}

func TestClientNetworkErrorDoesNotLeakBotToken(t *testing.T) {
	t.Parallel()
	const token = "super-secret-token"
	client, err := telegram.NewClient(token, "http://127.0.0.1:1", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = client.SendMessage(ctx, 1, "hello")
	if err == nil {
		t.Fatal("SendMessage() expected error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked token: %v", err)
	}
}

func TestClientRequestConstructionErrorDoesNotLeakBotToken(t *testing.T) {
	t.Parallel()
	const token = "secret\nvalue"
	client, err := telegram.NewClient(token, "http://127.0.0.1:1", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	err = client.SendMessage(context.Background(), 1, "hello")
	if err == nil {
		t.Fatal("SendMessage() expected error")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "value") {
		t.Fatalf("error leaked token fragments: %v", err)
	}
}

func TestClientRejectsUnsafeFilePath(t *testing.T) {
	t.Parallel()
	client, err := telegram.NewClient("token", "https://api.telegram.org", &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.OpenFile(context.Background(), "../secret"); err == nil {
		t.Fatal("OpenFile() expected unsafe path error")
	}
}
