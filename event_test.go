package redisq

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEventJson(t *testing.T) {
	tests := []struct {
		name    string
		event   event
		wantErr bool
	}{
		{
			name: "valid event",
			event: event{
				Action:  "test_action",
				Message: []byte("test message"),
			},
			wantErr: false,
		},
		{
			name: "empty event",
			event: event{
				Action:  "",
				Message: nil,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			json, err := tt.event.Json()
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			// Parse back and verify
			parsed, err := parseToEvent(json)
			assert.NoError(t, err)
			assert.Equal(t, tt.event.Action, parsed.Action)
			assert.Equal(t, tt.event.Message, parsed.Message)
		})
	}
}

func TestParseToEvent(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		want    event
		wantErr bool
	}{
		{
			name:  "valid json",
			input: []byte(`{"action":"test","message":"dGVzdA=="}`),
			want: event{
				Action:  "test",
				Message: []byte("test"),
			},
			wantErr: false,
		},
		{
			name:    "invalid json",
			input:   []byte(`invalid json`),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseToEvent(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.want.Action, got.Action)
			if tt.want.Message != nil {
				assert.Equal(t, tt.want.Message, got.Message)
			}
		})
	}
}
