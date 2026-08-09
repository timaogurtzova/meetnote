package mock

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/timaogurtzova/meetnote/internal/domain"
)

type LLM struct {
	delay time.Duration
}

var questionStopWords = map[string]struct{}{
	"без": {}, "бы": {}, "был": {}, "была": {}, "были": {}, "было": {},
	"в": {}, "во": {}, "где": {}, "для": {}, "за": {}, "и": {}, "из": {},
	"или": {}, "как": {}, "какая": {}, "какие": {}, "какой": {}, "когда": {},
	"кто": {}, "ли": {}, "на": {}, "о": {}, "об": {}, "по": {}, "про": {},
	"с": {}, "со": {}, "что": {}, "это": {},
}

func NewLLM(delay time.Duration) *LLM { return &LLM{delay: delay} }

func (l *LLM) Summarize(ctx context.Context, transcript string) (string, error) {
	if err := wait(ctx, l.delay); err != nil {
		return "", err
	}
	transcript = strings.TrimSpace(transcript)
	if strings.Contains(strings.ToLower(transcript), "[llm-error]") {
		return "", fmt.Errorf("%w: mock LLM failure", domain.ErrUnavailable)
	}
	if transcript == "" {
		return "", fmt.Errorf("%w: transcript is empty", domain.ErrInvalidInput)
	}
	return "Краткая выжимка: " + truncateRunes(transcript, 360), nil
}

func (l *LLM) Answer(ctx context.Context, question string, documents []domain.ChatDocument) (string, error) {
	if err := wait(ctx, l.delay); err != nil {
		return "", err
	}
	if len(documents) == 0 {
		return "", domain.ErrNotReady
	}
	terms := normalizedTerms(question)
	documents = append([]domain.ChatDocument(nil), documents...)
	sort.SliceStable(documents, func(i, j int) bool {
		return relevance(documents[i], terms) > relevance(documents[j], terms)
	})
	best := documents[0]
	answer := bestMatchingSentence(best.Transcript, terms)
	if answer == "" {
		answer = bestMatchingSentence(strings.TrimPrefix(best.Summary, "Краткая выжимка:"), terms)
	}
	if answer == "" {
		answer = truncateRunes(strings.TrimSpace(best.Summary), 500)
	}
	if answer == "" {
		answer = truncateRunes(strings.TrimSpace(best.Transcript), 500)
	}
	return fmt.Sprintf("По материалам встречи #%d: %s", best.MeetingID, finishSentence(answer)), nil
}

func relevance(document domain.ChatDocument, terms []string) int {
	text := strings.ToLower(document.Summary + " " + document.Transcript)
	score := 0
	for _, term := range terms {
		score += strings.Count(text, term)
	}
	return score
}

func normalizedTerms(value string) []string {
	words := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	terms := make([]string, 0, len(words))
	seen := make(map[string]struct{}, len(words))
	for _, word := range words {
		if utf8.RuneCountInString(word) < 3 {
			continue
		}
		if _, stopWord := questionStopWords[word]; stopWord {
			continue
		}
		if _, duplicate := seen[word]; duplicate {
			continue
		}
		seen[word] = struct{}{}
		terms = append(terms, word)
	}
	return terms
}

func bestMatchingSentence(text string, terms []string) string {
	sentences := strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '\n' || r == '\r'
	})
	bestScore := 0
	bestSentence := ""
	for _, sentence := range sentences {
		sentence = strings.TrimSpace(sentence)
		score := 0
		lowerSentence := strings.ToLower(sentence)
		for _, term := range terms {
			if strings.Contains(lowerSentence, term) {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			bestSentence = sentence
		}
	}
	return bestSentence
}

func finishSentence(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasSuffix(value, ".") || strings.HasSuffix(value, "!") || strings.HasSuffix(value, "?") {
		return value
	}
	return value + "."
}

func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
