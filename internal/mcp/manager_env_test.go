package mcp

import (
	"context"
	"testing"
)

func TestResolveEnvVars(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	t.Setenv("USER", "testuser")

	tests := []struct {
		name    string
		input   map[string]string
		want    map[string]string
		wantErr bool
	}{
		{
			name:    "resolves allowed env prefix",
			input:   map[string]string{"X-User": "env:USER", "X-Custom": "literal"},
			want:    map[string]string{"X-User": "testuser", "X-Custom": "literal"},
			wantErr: false,
		},
		{
			name:    "resolves HOME env var",
			input:   map[string]string{"X-Home": "env:HOME"},
			want:    map[string]string{"X-Home": "/home/testuser"},
			wantErr: false,
		},
		{
			name:    "nil map",
			input:   nil,
			want:    map[string]string{},
			wantErr: false,
		},
		{
			name:    "rejects non-allowlisted env var",
			input:   map[string]string{"Authorization": "env:AWS_SECRET_KEY"},
			wantErr: true,
		},
		{
			name:    "rejects sensitive env var",
			input:   map[string]string{"X-Token": "env:DATABASE_PASSWORD"},
			wantErr: true,
		},
	}
	m := &Manager{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.resolveEnvVars(context.Background(), tt.input)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestResolveEnvVars_SSMPassthrough verifies that ${ssm:/path} placeholders
// pass through unchanged when no resolver is configured. This is the safe
// default: a misconfigured deploy fails noisily at connect time (header
// value still reads "${ssm:...}") instead of silently dropping the secret.
func TestResolveEnvVars_SSMPassthrough(t *testing.T) {
	m := &Manager{} // no SSM resolver
	got, err := m.resolveEnvVars(context.Background(), map[string]string{
		"X-Service-Token": "${ssm:/injecting-ai/staging/mcp/gmail/service-token}",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["X-Service-Token"] != "${ssm:/injecting-ai/staging/mcp/gmail/service-token}" {
		t.Errorf("placeholder should pass through without resolver, got %q", got["X-Service-Token"])
	}
}
