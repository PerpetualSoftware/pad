package cli

import (
	"os"
	"strings"
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
