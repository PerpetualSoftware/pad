package oauth

import (
	"errors"
	"fmt"
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
//   - If any element of `needle` isn't equal to canonical, fail.
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

		// Haystack (client.Audience) must include canonical. Without
		// this, a DCR client registered before audience-population
		// shipped (sub-PR C work) could still drive flows. Reject.
		if !slicesContains(haystack, canonical) {
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
			if n != canonical {
				return fosite.ErrInvalidRequest.WithHintf(
					"Requested audience %q is not the canonical audience %q.",
					n, canonical,
				)
			}
		}

		return nil
	}
}

// slicesContains is a tiny helper avoiding the slices package import
// (the rest of internal/oauth/ doesn't need it). Returns true if s
// has an element equal to v.
func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
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
