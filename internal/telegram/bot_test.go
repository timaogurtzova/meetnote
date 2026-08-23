package telegram_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/inbox"
	"github.com/timaogurtzova/meetnote/internal/telegram"
)

func TestBotAdvancesOffsetAndStopsOnCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	api := &pollingAPI{cancel: cancel}
	inbox := &memoryInbox{}
	handler := newTestHandler(t, &fakeApplication{}, api)
	bot, err := telegram.NewBot(
		api,
		handler,
		inbox,
		telegram.BotConfig{
			PollTimeout:        20 * time.Millisecond,
			RequestTimeout:     20 * time.Millisecond,
			PersistenceTimeout: 20 * time.Millisecond,
			RetryMin:           time.Millisecond,
			RetryMax:           5 * time.Millisecond,
			UpdateWorkers:      2,
			UpdateLease:        time.Second,
			UpdateMaxAttempts:  3,
			UpdateRetention:    7 * 24 * time.Hour,
		},
		slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("bot did not stop")
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if !api.webhookDeleted || !api.commandsSet {
		t.Fatalf("setup calls: delete=%v commands=%v", api.webhookDeleted, api.commandsSet)
	}
	commands := make(map[string]struct{}, len(api.commands))
	for _, command := range api.commands {
		commands[command.Command] = struct{}{}
	}
	for _, required := range []string{"start", "load", "list", "status", "get", "find", "chat"} {
		if _, found := commands[required]; !found {
			t.Errorf("required command %q is missing", required)
		}
	}
	assert.Equal(t, int64(8), api.secondOffset)
	assert.Len(t, api.messages, 1)
}

type pollingAPI struct {
	mu             sync.Mutex
	calls          int
	secondOffset   int64
	webhookDeleted bool
	commandsSet    bool
	commands       []telegram.BotCommand
	messages       []string
	cancel         context.CancelFunc
}

func (p *pollingAPI) DeleteWebhook(context.Context) error {
	p.mu.Lock()
	p.webhookDeleted = true
	p.mu.Unlock()
	return nil
}

func (p *pollingAPI) SetCommands(_ context.Context, commands []telegram.BotCommand) error {
	p.mu.Lock()
	p.commandsSet = true
	p.commands = append([]telegram.BotCommand(nil), commands...)
	p.mu.Unlock()
	return nil
}

func (p *pollingAPI) GetUpdates(ctx context.Context, offset int64, _ time.Duration) ([]telegram.Update, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	if call == 2 {
		p.secondOffset = offset
	}
	p.mu.Unlock()
	if call == 1 {
		return []telegram.Update{privateTextUpdate(7, 100, "/start")}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *pollingAPI) SendMessage(_ context.Context, _ int64, text string) error {
	p.mu.Lock()
	p.messages = append(p.messages, text)
	p.mu.Unlock()
	p.cancel()
	return nil
}

func (p *pollingAPI) SendDocument(context.Context, int64, string, io.Reader, int64, string) error {
	return nil
}

func (p *pollingAPI) GetFile(context.Context, string) (telegram.RemoteFile, error) {
	return telegram.RemoteFile{}, nil
}

func (p *pollingAPI) OpenFile(context.Context, string) (io.ReadCloser, error) {
	return nil, nil
}

type memoryInbox struct {
	mu        sync.Mutex
	items     []inbox.Item
	completed map[int64]bool
}

func (m *memoryInbox) NextOffset(context.Context) (int64, error) { return 0, nil }
func (m *memoryInbox) Enqueue(_ context.Context, updates []inbox.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, update := range updates {
		duplicate := false
		for _, item := range m.items {
			if item.ID == update.ID {
				duplicate = true
			}
		}
		if !duplicate {
			m.items = append(m.items, update)
		}
	}
	return nil
}
func (m *memoryInbox) RecoverExpired(context.Context) (int64, error)   { return 0, nil }
func (m *memoryInbox) Prune(context.Context, time.Time) (int64, error) { return 0, nil }
func (m *memoryInbox) Claim(_ context.Context, _ string, _ time.Duration) (inbox.Item, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.items) == 0 {
		return inbox.Item{}, false, nil
	}
	item := m.items[0]
	m.items = m.items[1:]
	item.Attempt++
	item.LeaseToken = "lease"
	return item, true, nil
}
func (m *memoryInbox) Complete(_ context.Context, item inbox.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.completed == nil {
		m.completed = make(map[int64]bool)
	}
	m.completed[item.ID] = true
	return nil
}
func (m *memoryInbox) Retry(_ context.Context, item inbox.Item, _ string, _ time.Duration, _ int) (bool, error) {
	m.mu.Lock()
	m.items = append(m.items, item)
	m.mu.Unlock()
	return false, nil
}
