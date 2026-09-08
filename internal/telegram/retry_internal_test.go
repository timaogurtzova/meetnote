package telegram

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestExponentialDelayDoesNotOverflow(t *testing.T) {
	t.Parallel()
	maximum := time.Duration(1<<63 - 1)
	assert.Equal(t, maximum, exponentialDelay(maximum/2+1, maximum, 3))
}

func TestRetryDelayDoesNotExceedConfiguredMaximum(t *testing.T) {
	t.Parallel()
	err := &APIError{RetryAfter: time.Hour}
	assert.Equal(t, 30*time.Second, retryDelayFor(err, time.Second, 30*time.Second))
}
