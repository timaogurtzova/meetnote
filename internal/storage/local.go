// Пакет storage содержит адаптеры файлового хранилища.
package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/timaogurtzova/meetnote/internal/domain"
)

var supportedExtensions = map[string]struct{}{
	".txt": {}, ".md": {}, ".wav": {}, ".mp3": {}, ".m4a": {}, ".ogg": {}, ".flac": {},
}

const maxFilenameRunes = 255

type Local struct {
	rootPath string
	// os.Root проверяет каждый компонент пути и запрещает переход по символической ссылке за пределы каталога.
	root    *os.Root
	maxSize int64
}

func NewLocal(root string, maxSize int64) (*Local, error) {
	if strings.TrimSpace(root) == "" || maxSize < 1 {
		return nil, errors.New("invalid local storage configuration")
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage directory: %w", err)
	}
	if err := os.MkdirAll(absoluteRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create storage directory: %w", err)
	}
	rootHandle, err := os.OpenRoot(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("open storage directory: %w", err)
	}
	return &Local{rootPath: absoluteRoot, root: rootHandle, maxSize: maxSize}, nil
}

func (s *Local) Close() error {
	if err := s.root.Close(); err != nil {
		return fmt.Errorf("close storage directory: %w", err)
	}
	return nil
}

func (s *Local) Save(
	ctx context.Context,
	userID string,
	filename string,
	source io.Reader,
	declaredSize int64,
) (domain.StoredFile, error) {
	if err := ctx.Err(); err != nil {
		return domain.StoredFile{}, err
	}
	if source == nil || declaredSize < 0 {
		return domain.StoredFile{}, fmt.Errorf("%w: upload content and non-negative size are required", domain.ErrInvalidInput)
	}
	if declaredSize > s.maxSize {
		return domain.StoredFile{}, domain.ErrFileTooLarge
	}
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." {
		return domain.StoredFile{}, fmt.Errorf("%w: upload filename is required", domain.ErrInvalidInput)
	}
	if !utf8.ValidString(filename) || utf8.RuneCountInString(filename) > maxFilenameRunes ||
		strings.IndexFunc(filename, unicode.IsControl) >= 0 {
		return domain.StoredFile{}, fmt.Errorf("%w: upload filename is invalid or longer than %d characters", domain.ErrInvalidInput, maxFilenameRunes)
	}
	extension := strings.ToLower(filepath.Ext(filename))
	if _, ok := supportedExtensions[extension]; !ok {
		return domain.StoredFile{}, fmt.Errorf("%w: %s", domain.ErrUnsupportedFormat, extension)
	}

	userHash := sha256.Sum256([]byte(userID))
	userDirectory := hex.EncodeToString(userHash[:8])
	if err := s.root.MkdirAll(userDirectory, 0o750); err != nil {
		return domain.StoredFile{}, fmt.Errorf("create user storage: %w", err)
	}
	name, err := randomName(extension)
	if err != nil {
		return domain.StoredFile{}, err
	}
	relativePath := filepath.Join(userDirectory, name)
	destination, err := s.root.OpenFile(relativePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return domain.StoredFile{}, fmt.Errorf("create stored upload: %w", err)
	}

	copied, copyErr := copyWithContext(ctx, destination, source, s.maxSize)
	closeErr := destination.Close()
	if copyErr != nil || closeErr != nil {
		_ = s.root.Remove(relativePath)
		return domain.StoredFile{}, errors.Join(copyErr, closeErr)
	}

	return domain.StoredFile{OriginalFilename: filename, Path: filepath.Join(s.rootPath, relativePath), Size: copied}, nil
}

func (s *Local) Remove(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve stored path: %w", err)
	}
	relativePath, err := filepath.Rel(s.rootPath, absolutePath)
	if err != nil {
		return fmt.Errorf("resolve stored path relative to storage: %w", err)
	}
	if err := s.root.Remove(relativePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stored upload: %w", err)
	}
	return nil
}

func (s *Local) RemoveOrphans(
	ctx context.Context,
	keep map[string]struct{},
	olderThan time.Time,
) (int, error) {
	if keep == nil || olderThan.IsZero() {
		return 0, fmt.Errorf("%w: keep set and cutoff time are required", domain.ErrInvalidInput)
	}
	removed := 0
	err := fs.WalkDir(s.root.FS(), ".", func(relativePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		absolutePath := filepath.Join(s.rootPath, filepath.FromSlash(relativePath))
		if _, referenced := keep[absolutePath]; referenced {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(olderThan) {
			return nil
		}
		if err := s.root.Remove(filepath.FromSlash(relativePath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed++
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("walk local storage: %w", err)
	}
	return removed, nil
}

func randomName(extension string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate upload name: %w", err)
	}
	return hex.EncodeToString(value) + extension, nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader, maxSize int64) (int64, error) {
	buffer := make([]byte, 32*1024)
	limited := io.LimitReader(source, maxSize+1)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := limited.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, fmt.Errorf("write upload: %w", writeErr)
			}
			if written != read {
				return total, io.ErrShortWrite
			}
			if total > maxSize {
				return total, domain.ErrFileTooLarge
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, fmt.Errorf("read upload: %w", readErr)
		}
	}
}
