package main

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/handler"
)

func TestExternalStatusSyncIssueUsesStatusChangedFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{
			name: "current status event",
			payload: map[string]any{
				"issue":          handler.IssueResponse{Status: "done"},
				"status_changed": true,
			},
			want: true,
		},
		{
			name: "non-status update",
			payload: map[string]any{
				"issue":          handler.IssueResponse{Status: "done"},
				"status_changed": false,
			},
		},
		{
			name: "legacy changes map is not the event contract",
			payload: map[string]any{
				"issue":   handler.IssueResponse{Status: "done"},
				"changes": map[string]any{"status": true},
			},
		},
		{
			name: "non-syncable status",
			payload: map[string]any{
				"issue":          handler.IssueResponse{Status: "todo"},
				"status_changed": true,
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, got := externalStatusSyncIssue(tt.payload)
			if got != tt.want {
				t.Fatalf("externalStatusSyncIssue() ok = %v, want %v", got, tt.want)
			}
		})
	}
}
