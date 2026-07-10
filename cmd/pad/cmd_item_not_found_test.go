package main

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestWrapNotFound(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		ref     string
		ws      string
		wantMsg string // expected error message; "" means err should be nil
		wantNil bool   // true if err is nil
	}{
		{
			name:    "nil passes through",
			err:     nil,
			ref:     "TASK-1",
			ws:      "myws",
			wantNil: true,
		},
		{
			name:    "not_found includes ref and workspace",
			err:     &cli.APIError{Code: "not_found", Message: "not found"},
			ref:     "TASK-999999",
			ws:      "docapp",
			wantMsg: "item TASK-999999 not found in workspace docapp",
		},
		{
			name:    "other APIError passes through unchanged",
			err:     &cli.APIError{Code: "server_error", Message: "internal error"},
			ref:     "TASK-1",
			ws:      "myws",
			wantMsg: "internal error",
		},
		{
			name:    "non-APIError passes through unchanged",
			err:     errors.New("connection refused"),
			ref:     "TASK-1",
			ws:      "myws",
			wantMsg: "connection refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapNotFound(tt.err, tt.ref, tt.ws)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected error %q, got nil", tt.wantMsg)
			}
			if got.Error() != tt.wantMsg {
				t.Errorf("got %q, want %q", got.Error(), tt.wantMsg)
			}
		})
	}
}
