package main

import (
	"net/http/httptest"
	"time"
)

// mcpRecorder is an httptest.ResponseRecorder that takes a write deadline.
// /mcp refuses a writer that cannot (BUG-3808): its writes could not be
// bounded.
type mcpRecorder struct{ *httptest.ResponseRecorder }

func newMCPRecorder() *mcpRecorder { return &mcpRecorder{httptest.NewRecorder()} }

func (mcpRecorder) SetWriteDeadline(time.Time) error { return nil }
