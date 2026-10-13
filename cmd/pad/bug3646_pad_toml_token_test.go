package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// BUG-3646: a committed .pad.toml `url` replaced the configured server, and
// NewClientFromURL attached PAD_TOKEN to whatever URL it was given, so a
// checkout could send the exported token to a server of its choosing.

// tokenRecorder records the Authorization header of every request.
type tokenRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string
}

func newTokenRecorder(t *testing.T) *tokenRecorder {
	t.Helper()
	r := &tokenRecorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auth = append(r.auth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *tokenRecorder) headers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auth...)
}

// writePadTomlURL links the current directory to serverURL.
func writePadTomlURL(t *testing.T, serverURL string) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	body := "workspace = \"w\"\nurl = \"" + serverURL + "\"\n"
	if err := os.WriteFile(filepath.Join(cwd, ".pad.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// requestVia builds the client the way client commands do and sends one
// request.
func requestVia(t *testing.T) {
	t.Helper()
	cfg := getConfiguredConfig()
	_, _ = cli.NewClientFromURL(cfg.BaseURL()).GetCurrentUser()
}

func TestBUG3646_PadTomlURLDoesNotReceivePADTOKEN(t *testing.T) {
	setTempHomeMain(t)
	chosen := newTokenRecorder(t)
	other := newTokenRecorder(t)
	t.Setenv("PAD_URL", chosen.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")
	writePadTomlURL(t, other.URL)

	requestVia(t)

	got := other.headers()
	if len(got) == 0 {
		t.Fatal("no request reached the .pad.toml server; the override did not apply, so this proves nothing")
	}
	for _, h := range got {
		if h == "Bearer pad_envtoken" {
			t.Fatalf("PAD_TOKEN was sent to the server .pad.toml chose: %q", h)
		}
	}
}

// The fallback is the credential saved for that server, as if PAD_TOKEN were
// unset.
func TestBUG3646_PadTomlURLGetsItsOwnSavedCredential(t *testing.T) {
	home := setTempHomeMain(t)
	chosen := newTokenRecorder(t)
	other := newTokenRecorder(t)
	t.Setenv("PAD_URL", chosen.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")
	writeCredStore(t, home, other.URL, "padsess_saved")
	writePadTomlURL(t, other.URL)

	requestVia(t)

	got := other.headers()
	if len(got) == 0 || got[0] != "Bearer padsess_saved" {
		t.Fatalf("Authorization to the .pad.toml server = %v, want the saved credential", got)
	}
}

// Naming the same server explicitly is the opt-in: PAD_URL equal to the
// .pad.toml url sends the token there.
func TestBUG3646_PADURLNamingTheServerSendsTheToken(t *testing.T) {
	setTempHomeMain(t)
	srv := newTokenRecorder(t)
	t.Setenv("PAD_URL", srv.URL+"/")
	t.Setenv("PAD_TOKEN", "pad_envtoken")
	writePadTomlURL(t, srv.URL)

	requestVia(t)

	got := srv.headers()
	if len(got) == 0 || got[0] != "Bearer pad_envtoken" {
		t.Fatalf("Authorization = %v, want PAD_TOKEN (PAD_URL names this server)", got)
	}
}

// Without a .pad.toml url nothing changes: PAD_TOKEN goes to the chosen
// server (the #879 override).
func TestBUG3646_NoOverrideStillSendsTheToken(t *testing.T) {
	setTempHomeMain(t)
	srv := newTokenRecorder(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")

	requestVia(t)

	got := srv.headers()
	if len(got) == 0 || got[0] != "Bearer pad_envtoken" {
		t.Fatalf("Authorization = %v, want PAD_TOKEN", got)
	}
}

// A status command must not report the env token as in use where it is
// withheld: whoami against the .pad.toml server reads the store instead.
func TestBUG3646_WhoamiDoesNotClaimAWithheldToken(t *testing.T) {
	setTempHomeMain(t)
	chosen := newStubAuthServer(t, "pad_envtoken")
	other := newTokenRecorder(t)
	t.Setenv("PAD_URL", chosen.URL)
	t.Setenv("PAD_TOKEN", "pad_envtoken")
	writePadTomlURL(t, other.URL)

	cmd := whoamiCmd()
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("whoami succeeded against the .pad.toml server, which has no credential for this user")
	}
	for _, h := range other.headers() {
		if h == "Bearer pad_envtoken" {
			t.Fatalf("whoami sent PAD_TOKEN to the .pad.toml server")
		}
	}
}
