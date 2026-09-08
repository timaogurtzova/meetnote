package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/config"
	"github.com/timaogurtzova/meetnote/internal/postgres"
)

func TestOpenDoesNotExposePasswordFromMalformedConnectionString(t *testing.T) {
	t.Parallel()
	const password = "super-secret-password"
	_, err := postgres.Open(context.Background(), config.Database{
		URL:      "postgres://meetnote:" + password + "@%zz",
		MaxConns: 1,
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), password)
}
