package postgres

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func randomLeaseToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate lease token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func truncateError(value string) string {
	const limit = 4000
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
