package config_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/config"
)

func TestLoadConfiguresSafeDefaultUserQuotas(t *testing.T) {
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 100, cfg.Limits.MaxMeetingsPerUser)
	assert.Equal(t, 10, cfg.Limits.MaxPendingPerUser)
	assert.Equal(t, int64(200*1024*1024), cfg.Limits.MaxStorageBytesPerUser)
	assert.Equal(t, 1000, cfg.Limits.MaxChatHistoryPerUser)
}

func TestLoadConfiguresValidTaskLease(t *testing.T) {
	t.Setenv("MEETNOTE_TASK_LEASE", "90s")
	t.Setenv("MEETNOTE_LEASE_HEARTBEAT", "15s")

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, cfg.Worker.LeaseDuration)
	assert.Equal(t, 15*time.Second, cfg.Worker.HeartbeatInterval)
}

func TestLoadRejectsHeartbeatLongerThanLease(t *testing.T) {
	t.Setenv("MEETNOTE_TASK_LEASE", "10s")
	t.Setenv("MEETNOTE_LEASE_HEARTBEAT", "10s")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "MEETNOTE_LEASE_HEARTBEAT") {
		t.Fatalf("Load() error = %v, want heartbeat validation error", err)
	}
}

func TestLoadRejectsMalformedExplicitValues(t *testing.T) {
	t.Setenv("MEETNOTE_AUTO_MIGRATE", "flase")
	t.Setenv("MEETNOTE_WORKERS", "many")
	t.Setenv("MEETNOTE_TELEGRAM_POLL_TIMEOUT", "soon")

	_, err := config.Load()
	require.Error(t, err)
	for _, variable := range []string{
		"MEETNOTE_AUTO_MIGRATE",
		"MEETNOTE_WORKERS",
		"MEETNOTE_TELEGRAM_POLL_TIMEOUT",
	} {
		if !strings.Contains(err.Error(), variable) {
			t.Errorf("Load() error %q does not mention %s", err, variable)
		}
	}
}

func TestValidateTelegramRequiresTokenAndEnforcesDownloadLimit(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	cfg.Telegram.Token = ""
	cfg.Storage.MaxFileSize = config.TelegramDownloadLimit + 1

	err = cfg.ValidateTelegram()
	if err == nil {
		t.Fatal("ValidateTelegram() expected error")
	}
	if !strings.Contains(err.Error(), "MEETNOTE_TELEGRAM_TOKEN") ||
		!strings.Contains(err.Error(), "download limit") {
		t.Fatalf("ValidateTelegram() error = %v", err)
	}
}

func TestLoadRejectsUpdateLeaseThatCannotCoverLongestCommand(t *testing.T) {
	t.Setenv("MEETNOTE_TELEGRAM_REQUEST_TIMEOUT", "10s")
	t.Setenv("MEETNOTE_TELEGRAM_DOWNLOAD_TIMEOUT", "10s")
	t.Setenv("MEETNOTE_TELEGRAM_MAX_RESPONSE_PARTS", "8")
	t.Setenv("MEETNOTE_TELEGRAM_UPDATE_LEASE", "90s")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "MEETNOTE_TELEGRAM_UPDATE_LEASE") {
		t.Fatalf("Load() error = %v, want update lease budget validation error", err)
	}
}

func TestLoadAcceptsUpdateLeaseAboveLongestCommandBudget(t *testing.T) {
	t.Setenv("MEETNOTE_TELEGRAM_REQUEST_TIMEOUT", "10s")
	t.Setenv("MEETNOTE_TELEGRAM_DOWNLOAD_TIMEOUT", "10s")
	t.Setenv("MEETNOTE_TELEGRAM_MAX_RESPONSE_PARTS", "8")
	t.Setenv("MEETNOTE_TELEGRAM_UPDATE_LEASE", "101s")

	if _, err := config.Load(); err != nil {
		t.Fatalf("Load() error: %v", err)
	}
}

func TestLoadRejectsUnsafeConcurrencyConfiguration(t *testing.T) {
	t.Setenv("MEETNOTE_WORKERS", "65")
	t.Setenv("MEETNOTE_TELEGRAM_UPDATE_WORKERS", "65")
	t.Setenv("MEETNOTE_DATABASE_MAX_CONNS", "101")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() expected safe concurrency bound errors")
	}
	for _, variable := range []string{
		"MEETNOTE_WORKERS",
		"telegram update worker settings",
		"MEETNOTE_DATABASE_MAX_CONNS",
	} {
		if !strings.Contains(err.Error(), variable) {
			t.Errorf("Load() error %q does not mention %s", err, variable)
		}
	}
}

func TestLoadRejectsInvalidUserQuotas(t *testing.T) {
	t.Setenv("MEETNOTE_MAX_MEETINGS_PER_USER", "5")
	t.Setenv("MEETNOTE_MAX_PENDING_MEETINGS_PER_USER", "6")
	t.Setenv("MEETNOTE_MAX_STORAGE_MB_PER_USER", "19")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() expected quota validation errors")
	}
	for _, variable := range []string{
		"MEETNOTE_MAX_PENDING_MEETINGS_PER_USER",
		"MEETNOTE_MAX_STORAGE_MB_PER_USER",
	} {
		if !strings.Contains(err.Error(), variable) {
			t.Errorf("Load() error %q does not mention %s", err, variable)
		}
	}
}

func TestLoadRejectsInvalidChatHistoryLimit(t *testing.T) {
	t.Setenv("MEETNOTE_MAX_CHAT_HISTORY_PER_USER", "10001")

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "MEETNOTE_MAX_CHAT_HISTORY_PER_USER") {
		t.Fatalf("Load() error = %v, want chat history limit error", err)
	}
}

func TestLoadRejectsStorageQuotaOverflow(t *testing.T) {
	overflowingMB := uint64(math.MaxInt64/(1024*1024)) + 1
	t.Setenv("MEETNOTE_MAX_STORAGE_MB_PER_USER", fmt.Sprint(overflowingMB))

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "MEETNOTE_MAX_STORAGE_MB_PER_USER is too large") {
		t.Fatalf("Load() error = %v, want storage quota overflow", err)
	}
}
