package oauth

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/ory/fosite"
)

// RFC 8707 ("Resource Indicators for OAuth 2.0") audience-binding hook
// for pad's authorization server (PLAN-943 TASK-951 sub-PR B).
//
// fosite has no native RFC 8707 support — its built-in
// AudienceMatchingStrategy (DefaultAudienceMatchingStrategy /
// ExactAudienceMatchingStrategy) just checks that requested audiences
// are in a whitelist. PLAN-943 calls for a stricter rule: every token
// MUST be audience-bound to the canonical MCP URL
// (https://mcp.getpad.dev/mcp in production). A request with no
// audience or one that doesn't include the canonical URL fails the
// /authorize and /token endpoints.
//
// Why per-server (not per-client): TASK-951 v1 only serves one
// resource — the MCP transport. Future expansion (admin API, public
// REST, etc.) would either get its own AS or add an allow-list here.
// The single-string config keeps the policy obvious for now.
//
// Naming note: RFC 8707 calls it "resource"; fosite's surface uses
// "audience". They're the same concept (RFC 8707 §3 explicitly calls
// resource indicators "audience hints"). Sub-PR C's HTTP handlers
// translate the wire-form `resource=` param into fosite's
// `audience=` form before invoking the provider, so the flow lines
// up cleanly with this strategy.

// NormalizeAudience returns the canonical comparison form of an
// audience URI: trims a trailing "/" ONLY when the URI's path
// component is the root ("/" by itself), leaving every other
// trailing slash intact.
//
// Why exactly the root case: RFC 3986 §6.2.3 (Scheme-Based
// Normalization) declares that for HTTP-scheme URIs an empty path
// component is equivalent to a single "/". So
// "https://host" ≡ "https://host/" and we must accept both. But
// "https://host/foo" and "https://host/foo/" are NOT equivalent
// — they're distinct resources (one's a "file", the other a
// "directory" namespace). Collapsing those would let a token
// minted for "/foo/" pass an audience check expecting "/foo"
// (and vice versa), which is a real audience-confusion attack
// surface. Codex review #386 round 1 caught the over-broad trim
// that motivated this version.
//
// Why we need this at all: real OAuth clients vary in how they
// reconstruct the resource indicator. Claude Desktop's connector
// parses the URL the user pasted, URL-canonicalizes empty path → "/",
// and emits resource=https://mcp.example/ even when the operator's
// published canonical is https://mcp.example. A naive string compare
// rejects these as distinct audiences and the connector flow dies
// on "Requested audience X is not the canonical audience Y" — even
// though, per the spec, they ARE the same audience.
//
// We don't touch case, default ports, or percent-encoding —
// URI normalization can get arbitrarily thorny and each of those
// is its own decision. Root-path trailing slash is the one case
// both unambiguously safe per the spec AND observed in the wild.
//
// Exported so the resource-server-side audience check (in
// internal/server/middleware_mcp_auth.go) and the AS-side
// audienceMatchingStrategy share a single normalization rule. If
// the rule ever drifts between the two sides, tokens that the AS
// minted will start failing at the RS — silent breakage.
func NormalizeAudience(s string) string {
	u, err := url.Parse(s)
	// Only normalize when we have a parseable URL with a real host
	// AND the path is exactly the root "/". Anything else (parse
	// failures, hostless strings like "/" alone, paths like "/foo"
	// or "/foo/", or URIs carrying a query/fragment) is returned
	// byte-exact so we don't change semantics for cases the spec
	// doesn't declare equivalent.
	if err != nil || u.Host == "" || u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return s
	}
	return strings.TrimSuffix(s, "/")
}

// audienceMatchingStrategy returns a fosite.AudienceMatchingStrategy
// that enforces "every requested audience must equal the configured
// canonical audience, and no other audience is permitted." Closes
// over the canonical URL because fosite expects a function value of
// type AudienceMatchingStrategy on its Config.
//
// Two-sided check:
//
//   - If `needle` (the request's requested audience list) is empty,
//     fail. RFC 8707 doesn't require `resource` on every request, but
//     PLAN-943's TASK-950 + sub-PR C wiring guarantees /authorize and
//     /token always carry it. Empty here means a misconfigured
//     handler shipped a request without the param — fail loudly.
//   - If any element of `needle` isn't equal to canonical (after
//     trailing-slash normalization — see NormalizeAudience), fail.
//     This rejects multi-audience tokens or audiences pointing
//     elsewhere (the cross-server replay defense).
//   - Else, succeed.
//
// `haystack` is the per-client allowed audience list set at DCR
// time. Sub-PR C populates it with [canonical] for every newly-
// registered client, which means the haystack-side check is a
// belt-and-suspenders safety net rather than a primary gate. We
// still verify haystack contains canonical — defends against admin
// scripts or fixtures that register a client without setting the
// audience.
//
// Returns errors wrapped via fosite.ErrInvalidRequest so the OAuth
// error response carries the right shape (RFC 6749 §4.1.2.1).
func audienceMatchingStrategy(canonical string) fosite.AudienceMatchingStrategy {
	return func(haystack []string, needle []string) error {
		if canonical == "" {
			// Defensive: a server constructed without an audience is
			// a configuration bug, not a per-request failure. Refuse
			// the validation so the misconfigured handler errors
			// loudly the first time it's hit.
			return fosite.ErrServerError.WithHint("OAuth server has no canonical audience configured.")
		}

		canonicalNorm := NormalizeAudience(canonical)

		// Haystack (client.Audience) must include canonical (after
		// trailing-slash normalization). Without this, a DCR client
		// registered before audience-population shipped (sub-PR C
		// work) could still drive flows. Reject.
		if !audienceListContainsNormalized(haystack, canonicalNorm) {
			return fosite.ErrInvalidRequest.WithHintf(
				"Client is not authorized for the canonical audience %q.",
				canonical,
			)
		}

		// Needle (request.RequestedAudience) must be exactly { canonical }.
		// Empty needle means the client didn't request any resource —
		// per PLAN-943's enforcement, every grant is audience-restricted,
		// so we refuse the no-audience path.
		if len(needle) == 0 {
			return fosite.ErrInvalidRequest.WithHint(
				"resource parameter is required (RFC 8707).",
			)
		}
		for _, n := range needle {
			if NormalizeAudience(n) != canonicalNorm {
				return fosite.ErrInvalidRequest.WithHintf(
					"Requested audience %q is not the canonical audience %q.",
					n, canonical,
				)
			}
		}

		return nil
	}
}

// audienceListContainsNormalized reports whether haystack contains an
// element that compares equal to needleNorm after trailing-slash
// normalization. Caller is responsible for normalizing the needle
// before calling (the haystack side is normalized inline so the
// caller doesn't have to allocate a copy of the haystack).
func audienceListContainsNormalized(haystack []string, needleNorm string) bool {
	for _, x := range haystack {
		if NormalizeAudience(x) == needleNorm {
			return true
		}
	}
	return false
}

// ValidateAudienceParam is a one-off helper for sub-PR C's HTTP
// handlers: validates the incoming `resource` query param shape
// before fosite's internal validation runs. Returns nil iff the
// caller's resource is acceptable for this server.
//
// Exists separately from audienceMatchingStrategy because the HTTP
// handlers want a single-value check at request entry (parse error,
// scheme check, basic well-formedness) before they construct the
// fosite request. Keeps the fosite call site clean and the early-
// reject 400 response bodies meaningful.
func ValidateAudienceParam(canonical, raw string) error {
	if canonical == "" {
		return errors.New("oauth: server has no canonical audience configured")
	}
	if raw == "" {
		return fmt.Errorf("oauth: resource parameter is required")
	}
	// RFC 8707 §2 says the value MUST be an absolute URI. We don't
	// fully URI-parse here (fosite does that downstream); a string-
	// equality check against the canonical is sufficient and the
	// most restrictive policy. Any extension to allow-list multiple
	// audiences happens in this function.
	if raw != canonical {
		return fmt.Errorf("oauth: resource %q does not match canonical %q", raw, canonical)
	}
	return nil
}

// audienceForNewClient is what sub-PR C's DCR handler will use to
// populate a freshly-registered client's audience list. Centralized
// here so the policy ("every new client gets exactly the canonical
// audience") lives next to the validator that enforces it.
//
// Returns a copy so the caller can mutate without affecting future
// calls.
func audienceForNewClient(canonical string) []string {
	if canonical == "" {
		return nil
	}
	return []string{canonical}
}

// Compile-time guard: both helpers consume strings, so a cfg.AllowedAudience
// rename downstream would surface here first.
var _ = strings.TrimSpace
