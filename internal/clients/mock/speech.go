// Пакет mock содержит детерминированные офлайн-реализации внешних клиентов.
package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/timaogurtzova/meetnote/internal/domain"
)

type Speech struct {
	delay time.Duration
}

func NewSpeech(delay time.Duration) *Speech { return &Speech{delay: delay} }

func (s *Speech) Transcribe(ctx context.Context, filePath string) (string, error) {
	if err := wait(ctx, s.delay); err != nil {
		return "", err
	}
	name := filepath.Base(filePath)
	extension := strings.ToLower(filepath.Ext(name))
	if extension == ".txt" || extension == ".md" {
		content, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("read test transcript: %w", err)
		}
		transcript := strings.TrimSpace(string(content))
		if transcript == "" {
			return "", errors.New("mock speech: test transcript is empty")
		}
		if strings.Contains(strings.ToLower(transcript), "[speech-error]") {
			return "", fmt.Errorf("%w: mock speech failure", domain.ErrUnavailable)
		}
		return transcript, nil
	}
	return "Тестовая транскрипция аудиозаписи " + name + ". Команда обсудила задачи, сроки и ответственных.", nil
}
