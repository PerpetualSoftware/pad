package cli

import (
	"errors"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

// withCollectionAliasFallback runs op against the RAW collection slug the user
// typed, and — only if that fails specifically because the collection was not
// found — retries once with the legacy singular→plural alias
// (collections.NormalizeSlug). The result of whichever attempt succeeds is
// returned; if both fail, the RAW attempt's error is returned.
//
// Why send raw first (BUG-2630). The old client behaviour normalized the slug
// BEFORE the request, so a workspace whose collection slug IS one of the seven
// hardcoded singulars ("plan", "task", …) had the user's exact name rewritten
// away and their write landed in a DIFFERENT collection, silently, with a
// success message naming the wrong one. Sending raw lets the server's
// exact-match-first resolution (BUG-2578) win, so an exact collection name can
// never be shadowed by its own alias.
//
// Why keep the alias at all. A server that predates the server-side resolver
// (BUG-2578) matches collections by exact slug only, so a raw "task" would 404
// there for a workspace whose collection is "tasks". The retry preserves
// today's shorthand UX against those older servers, at the cost of one extra
// round trip on a path that was already going to fail.
//
// Why the retry is keyed on collection-not-found specifically (see
// isCollectionNotFound). If it fired on ANY failure, a request aimed at a
// collection that really exists ("plan") but failed for another reason (a
// validation error, an auth hiccup) would be retried against the alias
// ("plans") and could silently succeed there — recreating BUG-2630 in a new
// costume. Keying on the not-found error means a collection that exists is
// never retried away from.
//
// REMOVAL CONDITION: once the minimum supported server is guaranteed to carry
// the resolver (BUG-2578), the raw slug always resolves on the first attempt
// and this fallback is dead weight — drop it and call op(rawSlug) directly.
// That is the Option 1 cleanup tracked on BUG-2630's trail, gated on a
// min-server-version floor that includes the resolver.
//
// serverResolves reports whether the server resolves collection slugs itself
// (exact-match-first + alias fallback + archived-claims refusal). When it
// returns true, a collection-not-found is AUTHORITATIVE and the alias retry is
// skipped; when false (an old build, or the probe failed), the retry runs. Pass
// nil to force the legacy always-retry behaviour (used only in tests).
//
// Exported so the cmd/pad item commands can funnel their create / list / move
// calls through this ONE implementation instead of each re-deriving the
// send-raw-then-retry dance (BUG-2630 lead ruling: one shared helper, not
// copied at the call sites).
func WithCollectionAliasFallback[T any](rawSlug string, serverResolves func() bool, op func(slug string) (T, error)) (T, error) {
	result, err := op(rawSlug)
	if err == nil {
		return result, nil
	}
	// The failure was not a missing collection (auth, validation, a 5xx): return
	// it untouched. Retrying such an error against the alias could silently
	// succeed on a DIFFERENT collection that does exist — BUG-2630 in a new
	// costume — so a collection that exists is never retried away from.
	if !isCollectionNotFound(err) {
		return result, err
	}
	normalized := collections.NormalizeSlug(rawSlug)
	if normalized == rawSlug {
		// No alias to try (the input is not one of the aliased singulars): the
		// slug genuinely names no collection. Name it in the error the user
		// sees, since it is their own word.
		return result, wrapCollectionNotFound(err, rawSlug)
	}
	// A server that resolves collections itself has ALREADY tried the alias for
	// us (server-side, BUG-2578/2630), plus enforced exact-match-first and the
	// archived-claims/hidden refusal. So its collection-not-found is
	// authoritative: the slug is absent, archived, or hidden, and retrying the
	// alias would only defeat that protection (BUG-2630 #1). Only fall back to
	// the client-side retry for an OLDER server that does not advertise
	// resolution — which never had the protection a retry could defeat.
	if serverResolves != nil && serverResolves() {
		return result, wrapCollectionNotFound(err, rawSlug)
	}
	result2, err2 := op(normalized)
	if err2 != nil {
		if isCollectionNotFound(err2) {
			// Neither the raw slug nor its alias names a collection. Surface an
			// error naming the RAW slug the user typed, not the alias the
			// client tried on their behalf — their own words are the ones they
			// can act on.
			return result, wrapCollectionNotFound(err, rawSlug)
		}
		// The alias DOES name a real collection, but the operation failed there
		// for a substantive reason (plan limit, open children, validation,
		// forbidden). That error is about a real collection and is far more
		// useful than a misleading "collection not found" — surface it verbatim.
		return result2, err2
	}
	return result2, nil
}

// wrapCollectionNotFound rewrites the server's context-free "Collection not
// found" into one that names the slug the user actually typed, so a failed
// `pad item create widget …` reads `collection "widget" not found` instead of a
// bare "Collection not found". It returns a fresh *APIError with the same Code
// (so downstream type/Code inspection — plan-limit, open-children — keeps
// working) and an enriched Message. Non-collection errors pass through
// untouched. Mirrors wrapItemNotFound's shape.
func wrapCollectionNotFound(err error, rawSlug string) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && isCollectionNotFound(err) {
		return &APIError{
			Code:    apiErr.Code,
			Message: fmt.Sprintf("collection %q not found", rawSlug),
			Details: apiErr.Details,
		}
	}
	return err
}

// isCollectionNotFound reports whether err is the server's specific
// "collection not found" response for an item create / list / move request. It
// is deliberately narrow: any other failure (auth, validation, a 5xx, an
// item-not-found) must NOT trigger the alias retry, or a request aimed at a
// collection that really exists could be silently rerouted to a different one
// (BUG-2630).
//
// The wire contract it matches is stable across the BUG-2578 server change —
// both codes predate it, so the predicate works against old and new servers:
//   - create / list: HTTP 404, code "not_found", message "Collection not found"
//   - move:          HTTP 400, code "invalid_collection"
//
// The message check on the "not_found" branch distinguishes a missing
// COLLECTION from a missing ITEM, which shares the "not_found" code on other
// endpoints — though the create/list endpoints this helper wraps only ever
// emit "not_found" for the collection, the message guard keeps the predicate
// honest if it is ever reused on a broader path.
func isCollectionNotFound(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case "invalid_collection":
		return true
	case "not_found":
		return apiErr.Message == "Collection not found"
	}
	return false
}
