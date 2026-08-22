package opencode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateExport(t *testing.T) {
	valid := func(id, directory, version string) []byte {
		data, err := json.Marshal(map[string]any{
			"info": map[string]any{
				"id":        id,
				"directory": directory,
				"version":   version,
				"title":     "t",
			},
			"messages": []any{},
		})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	tests := []struct {
		name      string
		payload   []byte
		sessionID string
		wantErr   string // substring; empty = must pass
		wantDir   string // expected info.Directory on success
		wantVer   string
	}{
		{
			name:      "valid export passes",
			payload:   valid("ses_ok", "/home/u/proj", "1.18.18"),
			sessionID: "ses_ok",
			wantDir:   "/home/u/proj",
			wantVer:   "1.18.18",
		},
		{
			name:      "truncated JSON is rejected",
			payload:   []byte(`{"info":{"id":"ses_t","directo`),
			sessionID: "ses_t",
			wantErr:   "not valid JSON",
		},
		{
			name:      "id mismatch is rejected",
			payload:   valid("ses_other", "/p", "1.18.18"),
			sessionID: "ses_expected",
			wantErr:   `"ses_other"`,
		},
		{
			name:      "empty directory is rejected",
			payload:   valid("ses_d", "", "1.18.18"),
			sessionID: "ses_d",
			wantErr:   "info.directory is empty",
		},
		{
			name:      "empty version is rejected",
			payload:   valid("ses_v", "/p", ""),
			sessionID: "ses_v",
			wantErr:   "info.version is empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ValidateExport(tt.payload, tt.sessionID)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				if info.Directory != tt.wantDir || info.Version != tt.wantVer {
					t.Errorf("info = %#v, want dir=%q ver=%q", info, tt.wantDir, tt.wantVer)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got none", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q missing %q", err, tt.wantErr)
			}
		})
	}
}
