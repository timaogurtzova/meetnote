package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		want    mode
		wantErr bool
	}{
		{name: "default bot", want: modeBot},
		{name: "bot", args: []string{"bot"}, want: modeBot},
		{name: "worker", args: []string{"worker"}, want: modeWorker},
		{name: "migrate", args: []string{"migrate"}, want: modeMigrate},
		{name: "health", args: []string{"health"}, want: modeHealth},
		{name: "help", args: []string{"--help"}, want: modeHelp},
		{name: "unknown", args: []string{"list"}, wantErr: true},
		{name: "extra arguments", args: []string{"bot", "extra"}, wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseMode(test.args)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestNewLoggerWritesFilteredJSON(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger := newLogger(&output, "warn")
	logger.Info("ignored")
	logger.Warn("visible", "component", "test")

	var event map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event))
	assert.Equal(t, "WARN", event["level"])
	assert.Equal(t, "test", event["component"])
	assert.Equal(t, "visible", event["msg"])
}
