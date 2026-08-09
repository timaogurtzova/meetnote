package mock_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/clients/mock"
	"github.com/timaogurtzova/meetnote/internal/domain"
)

func TestSpeechReadsTestTranscript(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "meeting.txt")
	require.NoError(t, os.WriteFile(path, []byte("Обсудили релиз в пятницу."), 0o600))
	transcript, err := mock.NewSpeech(0).Transcribe(context.Background(), path)
	require.NoError(t, err)
	assert.Equal(t, "Обсудили релиз в пятницу.", transcript)
}

func TestClientsHandleFailureAndCancellation(t *testing.T) {
	t.Parallel()
	llm := mock.NewLLM(0)
	_, err := llm.Summarize(context.Background(), "notes [llm-error]")
	require.ErrorIs(t, err, domain.ErrUnavailable)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = mock.NewSpeech(time.Second).Transcribe(ctx, "meeting.wav")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestLLMAnswerUsesRelevantDocument(t *testing.T) {
	t.Parallel()
	documents := []domain.ChatDocument{
		{MeetingID: 1, Transcript: "Обсуждали бюджет."},
		{MeetingID: 2, Transcript: "Анна отвечает за релиз продукта."},
	}
	answer, err := mock.NewLLM(0).Answer(context.Background(), "Кто отвечает за релиз?", documents)
	require.NoError(t, err)
	assert.Contains(t, answer, "#2")
	assert.Contains(t, answer, "Анна отвечает за релиз продукта.")
	assert.NotContains(t, answer, "Обсуждали бюджет")
}

func TestLLMAnswerDoesNotDuplicateSummaryAndTranscript(t *testing.T) {
	t.Parallel()
	documents := []domain.ChatDocument{{
		MeetingID: 9,
		Summary:   "Краткая выжимка: Обсудили выпуск сервиса. Анна отвечает за подготовку релиза.",
		Transcript: "Обсудили выпуск сервиса.\n" +
			"Анна отвечает за подготовку релиза и должна завершить работу к пятнице.\n" +
			"Иван проверит миграции до четверга.",
	}}

	answer, err := mock.NewLLM(0).Answer(context.Background(), "Кто отвечает за подготовку релиза?", documents)
	require.NoError(t, err)
	want := "По материалам встречи #9: Анна отвечает за подготовку релиза и должна завершить работу к пятнице."
	assert.Equal(t, want, answer)
}
