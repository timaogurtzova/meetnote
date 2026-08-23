package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/domain"
	"github.com/timaogurtzova/meetnote/internal/storage"
)

func TestLocalSaveAndRemove(t *testing.T) {
	t.Parallel()
	store, err := storage.NewLocal(t.TempDir(), 1024)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	stored, err := store.Save(context.Background(), "alice", "meeting.txt", strings.NewReader("meeting text"), 12)
	require.NoError(t, err)
	content, err := os.ReadFile(stored.Path)
	require.NoError(t, err)
	assert.Equal(t, "meeting text", string(content))
	assert.Equal(t, "meeting.txt", stored.OriginalFilename)
	require.NoError(t, store.Remove(context.Background(), stored.Path))
}

func TestLocalRemoveOrphansKeepsReferencedAndRecentFiles(t *testing.T) {
	t.Parallel()
	store, err := storage.NewLocal(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	keep, err := store.Save(context.Background(), "alice", "keep.txt", strings.NewReader("keep"), 4)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := store.Save(context.Background(), "alice", "orphan.txt", strings.NewReader("old"), 3)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := store.Save(context.Background(), "alice", "recent.txt", strings.NewReader("new"), 3)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(keep.Path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan.Path, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveOrphans(
		context.Background(),
		map[string]struct{}{keep.Path: {}},
		time.Now().Add(-time.Minute),
	)
	if err != nil || removed != 1 {
		t.Fatalf("RemoveOrphans() = %d, %v", removed, err)
	}
	if _, err := os.Stat(keep.Path); err != nil {
		t.Fatalf("referenced file was removed: %v", err)
	}
	if _, err := os.Stat(recent.Path); err != nil {
		t.Fatalf("recent file was removed: %v", err)
	}
	if _, err := os.Stat(orphan.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old orphan still exists: %v", err)
	}
}

func TestLocalRemoveDoesNotFollowSymlinkOutsideStorage(t *testing.T) {
	t.Parallel()
	storageDirectory := t.TempDir()
	outsideDirectory := t.TempDir()
	outsidePath := filepath.Join(outsideDirectory, "private.txt")
	require.NoError(t, os.WriteFile(outsidePath, []byte("private"), 0o600))
	require.NoError(t, os.Symlink(outsideDirectory, filepath.Join(storageDirectory, "outside")))

	store, err := storage.NewLocal(storageDirectory, 1024)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	err = store.Remove(context.Background(), filepath.Join(storageDirectory, "outside", "private.txt"))
	require.Error(t, err)
	content, err := os.ReadFile(outsidePath)
	require.NoError(t, err)
	assert.Equal(t, "private", string(content))
}

func TestLocalRejectsUnsupportedAndLargeFiles(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	store, err := storage.NewLocal(filepath.Join(directory, "storage"), 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	if _, err := store.Save(context.Background(), "alice", "meeting.exe", strings.NewReader("x"), 1); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Fatalf("Save() error = %v, want unsupported format", err)
	}
	if _, err := store.Save(context.Background(), "alice", "meeting.txt", strings.NewReader("large"), 5); !errors.Is(err, domain.ErrFileTooLarge) {
		t.Fatalf("Save() error = %v, want file too large", err)
	}
}

func TestLocalEnforcesActualStreamSizeAndSanitizesFilename(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := storage.NewLocal(root, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	if _, err := store.Save(
		context.Background(),
		"alice",
		"meeting.txt",
		strings.NewReader("larger"),
		0,
	); !errors.Is(err, domain.ErrFileTooLarge) {
		t.Fatalf("Save() error = %v, want file too large", err)
	}
	stored, err := store.Save(
		context.Background(),
		"alice",
		"../../note.md",
		strings.NewReader("ok"),
		2,
	)
	if err != nil {
		t.Fatalf("Save() sanitized filename error: %v", err)
	}
	if stored.OriginalFilename != "note.md" {
		t.Fatalf("original filename = %q", stored.OriginalFilename)
	}
}

func TestLocalRejectsUnsafeOriginalFilename(t *testing.T) {
	t.Parallel()
	store, err := storage.NewLocal(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, filename := range []string{strings.Repeat("а", 256) + ".txt", "line\nbreak.txt"} {
		if _, err := store.Save(context.Background(), "alice", filename, strings.NewReader("notes"), 5); !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("Save(%q) error = %v, want invalid input", filename, err)
		}
	}
}
