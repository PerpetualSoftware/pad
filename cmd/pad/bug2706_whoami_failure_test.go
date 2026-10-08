package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG-2706: whoami reported EVERY failed identity check as a rejection
// ("PAD_TOKEN is set but the server rejected it." / "Session expired.") and
// exited 0. Measured on main: PAD_TOKEN set, server unreachable, "rejected",
// exit 0. Only a 401 or 403 is a rejection; anything else means the
// credential was not checked, and every failure is an error.
func TestBUG2706_WhoamiSaysWhatTheFailureEstablished(t *testing.T) {
	const rejected = "rejected"
	cases := []struct {
		name     string
		envToken bool
		handler  http.HandlerFunc // nil: nothing listens
		want     string
		mustNot  string
	}{
		{
			name: "env token refused (JSON 401)", envToken: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"Authentication required"}}`))
			},
			want: "PAD_TOKEN is set but the server rejected it (HTTP 401)",
		},
		{
			name: "env token refused (non-JSON 403)", envToken: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`<html>forbidden</html>`))
			},
			want: "rejected it (HTTP 403)",
		},
		{
			name: "stored session refused", envToken: false,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"Authentication required"}}`))
			},
			want: "session expired",
		},
		{
			name: "server error (JSON 500)", envToken: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"boom"}}`))
			},
			want: "answered HTTP 500", mustNot: rejected + " it",
		},
		{
			name: "proxy page (502)", envToken: false,
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`<html>bad gateway</html>`))
			},
			want: "answered HTTP 502", mustNot: "expired",
		},
		{
			name: "malformed 200 body", envToken: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`not json`))
			},
			want: "no usable answer", mustNot: rejected + " it",
		},
		{
			name: "unreachable", envToken: true, handler: nil,
			want: "no usable answer", mustNot: rejected + " it",
		},
		{
			name: "unreachable, stored session", envToken: false, handler: nil,
			want: "no usable answer", mustNot: "expired",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := setTempHomeMain(t)
			var url string
			if c.handler != nil {
				srv := httptest.NewServer(c.handler)
				t.Cleanup(srv.Close)
				url = srv.URL
			} else {
				srv := httptest.NewServer(http.NotFoundHandler())
				url = srv.URL
				srv.Close() // the port now refuses connections
			}
			t.Setenv("PAD_URL", url)
			if c.envToken {
				t.Setenv("PAD_TOKEN", "pad_envtoken")
			} else {
				t.Setenv("PAD_TOKEN", "")
				writeCredStore(t, home, url, "padsess_stored")
			}

			cmd := whoamiCmd()
			err := cmd.RunE(cmd, nil)
			if err == nil {
				t.Fatal("whoami returned nil (exit 0) on a failed identity check")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %q, want it to contain %q", err, c.want)
			}
			if c.mustNot != "" && strings.Contains(err.Error(), c.mustNot) {
				t.Errorf("err = %q claims %q, but the credential was never checked", err, c.mustNot)
			}
		})
	}
}
