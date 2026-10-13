package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// EnvToken returns the value of the PAD_TOKEN environment variable,
// trimmed of surrounding whitespace. When non-empty, it overrides any
// credential stored in ~/.pad/credentials.json — the same convention as
// gh's GH_TOKEN (issue #879, layer 1).
//
// Why an env override at all: the credential store is per-server, not
// per-process, so several agent processes on one machine cannot act as
// different Pad users without contending over the file. Reads never
// write credentials.json (only login/setup/logout do), so a read-only
// override sidesteps the identity-switching contention completely —
// each process carries its own token in its environment and the store
// is never touched.
//
// Accepts either token type the server takes (padsess_ session or
// pad_ API token); API tokens are the intended fit (mint under
// Settings → API tokens in the web UI).
//
// Lifecycle asymmetry, deliberate: `pad auth logout` never invalidates
// the env token's own server-side session — it pins its Logout() call
// to the STORED token and only deletes the stored entry. The env
// token's lifecycle belongs to whoever minted it (revoke it where it
// was minted), exactly gh's GH_TOKEN posture. This is the read-only
// contract applied to logout, not an oversight.
func EnvToken() string {
	return strings.TrimSpace(os.Getenv("PAD_TOKEN"))
}

// PAD_TOKEN is bound to the server the user chose (BUG-3646): PAD_URL, --url,
// the user config, or the default. A repository's .pad.toml can replace that
// URL (BUG-1535), and anyone can commit a .pad.toml, so a checkout must not be
// able to redirect the exported token to a server of its choosing. When the
// override names a different server, EnvTokenFor withholds PAD_TOKEN from it
// and the client falls back to the credential saved for that server, as if
// PAD_TOKEN were unset. Setting PAD_URL to that server, or passing --url,
// makes it the chosen server and the token is sent.
var (
	padTomlOverrideMu sync.Mutex
	padTomlChosenURL  string // the destination before .pad.toml replaced it
	padTomlURL        string // the destination .pad.toml replaced it with
	envTokenWarnOnce  sync.Once
	// envTokenWarnOut is where the one-line withheld notice goes; a var so
	// tests can capture it.
	envTokenWarnOut io.Writer = os.Stderr
)

// NotePadTomlURLOverride records that .pad.toml replaced the chosen server
// URL with another. Called with two empty strings when no override applies,
// so a later call in one process never inherits an earlier one's state.
func NotePadTomlURLOverride(chosen, override string) {
	padTomlOverrideMu.Lock()
	defer padTomlOverrideMu.Unlock()
	padTomlChosenURL, padTomlURL = chosen, override
}

// EnvTokenFor returns PAD_TOKEN when it may be sent to baseURL, and "" when
// baseURL is a server only .pad.toml chose (see NotePadTomlURLOverride).
// Every decision about whether the env token is in play goes through here, so
// no command reports the token as in use where it is withheld.
func EnvTokenFor(baseURL string) string {
	tok := EnvToken()
	if tok == "" {
		return ""
	}
	padTomlOverrideMu.Lock()
	chosen, override := padTomlChosenURL, padTomlURL
	padTomlOverrideMu.Unlock()
	if override == "" || !sameServer(baseURL, override) || sameServer(override, chosen) {
		return tok
	}
	envTokenWarnOnce.Do(func() {
		fmt.Fprintf(envTokenWarnOut, "pad: PAD_TOKEN not sent to %s, which comes from .pad.toml; it is bound to %s. To send it there, set PAD_URL=%s or pass --url.\n",
			override, chosen, override)
	})
	return ""
}

// sameServer compares two base URLs the way saved credentials are keyed
// (normalizeServerURL): a differently spelled URL counts as a different
// server, which errs toward withholding.
func sameServer(a, b string) bool {
	return normalizeServerURL(a) == normalizeServerURL(b)
}
