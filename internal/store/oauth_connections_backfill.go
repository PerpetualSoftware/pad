package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// One-shot backfill from session.Extra["allowed_workspaces"] into the
// new per-connection tables (PLAN-1519 / TASK-1522 / IDEA-1517 §2).
//
// Phase A (TASK-1520) shipped oauth_connections + oauth_connection_workspaces
// empty. Phase C2 (TASK-1523) will rewrite /authorize/decide to write
// these tables on consent. THIS file seeds existing grant chains so the
// new read path (rewritten ListUserOAuthConnections in connected_apps.go)
// can render every connection — both pre-migration and post-migration —
// uniformly off the new tables. Without backfill, the rewritten read
// path would show an empty list for every existing OAuth connection.
//
// Why a Go function instead of a SQL migration: the backfill needs to
// parse JSON session.Extra blobs out of session_data and resolve
// workspace slugs → workspace_id rows for the join table. SQLite's
// json_extract() could do the parse half but the cross-table slug
// resolution + the dialect symmetry (SQLite vs Postgres JSON shape)
// would make for an awkward .sql file. A Go pass is shorter and the
// idempotent INSERT OR IGNORE / ON CONFLICT DO NOTHING semantics keep
// it safe to re-run on every startup — which is exactly what
// cmd/pad/main.go does post-OAuth-server-wire.
//
// Idempotence: every INSERT is OR-IGNORE / ON CONFLICT DO NOTHING; the
// chains scan over the existing oauth_*_tokens tables which never
// shrink (revoked rows stay around with active=false). Subsequent
// runs are no-ops with a near-zero cost beyond the scan itself.

// BackfillOAuthConnectionsResult reports how the backfill shaped the
// tables — useful for the startup log so operators can see "N chains
// seeded, M skipped" and notice if a re-run finds new work (which
// would indicate live token issuance between server starts).
type BackfillOAuthConnectionsResult struct {
	// ChainsSeen is the count of distinct request_id chains the scan
	// touched (sum across access + refresh tokens, deduplicated).
	ChainsSeen int

	// ConnectionsCreated is the count of NEW oauth_connections rows
	// the backfill inserted. Repeat runs report 0 here once stable.
	ConnectionsCreated int

	// WorkspacesAdded is the count of (request_id, workspace_id) join
	// rows inserted across all chains with explicit allow-lists.
	WorkspacesAdded int

	// UnresolvedSlugs counts cases where a slug from session.Extra
	// didn't match any workspace row (workspace deleted, renamed,
	// typo'd in a fixture, etc.). Logged at WARN so they're visible
	// without failing the backfill — a missing slug is data hygiene,
	// not a correctness blocker.
	UnresolvedSlugs int
}

// BackfillOAuthConnections walks every distinct grant chain found in
// oauth_access_tokens and oauth_refresh_tokens, parses the newest
// chain row's session_data.Extra.allowed_workspaces, and seeds the new
// oauth_connections + oauth_connection_workspaces tables.
//
// Shape mapping (IDEA-1517 §2 backfill spec):
//
//   - No key (pre-TASK-952 token)   → all_current_workspaces=1, no join rows.
//   - ["*"] wildcard                → all_current_workspaces=1, no join rows.
//   - Explicit slug list            → all_current_workspaces=0, one join
//     row per resolvable slug, all
//     added_by='user'.
//
// In all cases:
//
//   - name defaults to ” (Phase D UI prompts on first connections-
//     page visit to name unnamed connections).
//   - may_create_workspaces, include_future_workspaces both default
//     ON (the new-grant default per §2a's consent screen).
//
// Operates outside any transaction. The individual INSERTs are
// idempotent, and crashing mid-run leaves the tables in a consistent
// partial state that the next run completes. Wrapping in a single
// transaction would extend the writer hold across the entire grant-
// chain scan, which for cloud-mode deployments with O(thousands) of
// active chains could measurably stall concurrent /authorize traffic
// on startup.
//
// If chain-scale ever becomes a problem the right answer is per-chain
// transactions or a chunked walk (PLAN-1519 Risks bullet); both stay
// within idempotent semantics so a v2 doesn't have to migrate state.
func (s *Store) BackfillOAuthConnections() (BackfillOAuthConnectionsResult, error) {
	result := BackfillOAuthConnectionsResult{}

	// One scan that unions access + refresh tokens, grouped by
	// request_id with the newest row's session_data per chain. The
	// chain may exist in either table (refresh-only when access has
	// expired but refresh hasn't been used yet) so we walk both.
	//
	// We don't filter on active=true here: an inactive chain still
	// renders on /console/connected-apps as long as a token row
	// exists — the existing read path (and the rewritten one) hides
	// inactive chains itself. Seeding inactive chains gives the
	// rewrite a clean "every chain has a connection row" invariant.
	//
	// Per-dialect: SQLite needs the ORDER BY in the inner query
	// because GROUP BY's row-selection is undefined; Postgres allows
	// DISTINCT ON. Keeping a uniform LEFT-JOIN-style aggregation in
	// Go is simpler than two SQL paths.
	chains, err := s.collectGrantChainsForBackfill()
	if err != nil {
		return result, fmt.Errorf("oauth_connections backfill: collect chains: %w", err)
	}
	result.ChainsSeen = len(chains)
	if len(chains) == 0 {
		return result, nil
	}

	for requestID, chain := range chains {
		created, slugsAdded, slugsMissed, err := s.backfillOneChain(requestID, chain)
		if err != nil {
			// Per-chain failure is logged + continues. A single
			// malformed session_data row shouldn't poison the rest
			// of the backfill.
			slog.Warn("oauth_connections backfill: chain skipped",
				"request_id", requestID, "error", err)
			continue
		}
		if created {
			result.ConnectionsCreated++
		}
		result.WorkspacesAdded += slugsAdded
		result.UnresolvedSlugs += slugsMissed
	}
	return result, nil
}

// backfillChain is the per-chain payload the scan returns. Only the
// fields the seeder needs travel — keeping this struct lean lets the
// chain map stay cheap even on cloud-mode datasets.
type backfillChain struct {
	UserID      string
	SessionData string
}

// collectGrantChainsForBackfill returns a map keyed by request_id with
// the newest session_data + the subject (user_id) per chain. Walks
// both oauth_access_tokens and oauth_refresh_tokens — chains may live
// only on the refresh side after their access token expires. Uses the
// in-memory dedup pattern from ListUserOAuthConnections (same constraint
// — SQLite/PG GROUP BY semantics differ for non-aggregated columns).
func (s *Store) collectGrantChainsForBackfill() (map[string]backfillChain, error) {
	chains := map[string]backfillChain{}
	// requestedAtPerChain remembers the newest ISO timestamp seen so
	// the access + refresh walks can each contribute and the latest
	// row across both still wins.
	requestedAtPerChain := map[string]string{}

	scan := func(query string) error {
		rows, err := s.db.Query(s.q(query))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var requestID, subject, requestedAt, sessionData string
			if err := rows.Scan(&requestID, &subject, &requestedAt, &sessionData); err != nil {
				return err
			}
			if subject == "" {
				// A token row with no subject can't be attributed to
				// a user — skip, the chain won't show up on any
				// connections page anyway.
				continue
			}
			// String compare on ISO-8601 lexicographic order matches
			// chronological order for this format. Cheaper than parsing
			// timestamps inside the hot loop.
			if existing, ok := requestedAtPerChain[requestID]; ok && requestedAt <= existing {
				continue
			}
			requestedAtPerChain[requestID] = requestedAt
			chains[requestID] = backfillChain{UserID: subject, SessionData: sessionData}
		}
		return rows.Err()
	}

	if err := scan(`
        SELECT request_id, subject, requested_at, session_data
        FROM oauth_access_tokens
    `); err != nil {
		return nil, fmt.Errorf("scan access tokens: %w", err)
	}
	if err := scan(`
        SELECT request_id, subject, requested_at, session_data
        FROM oauth_refresh_tokens
    `); err != nil {
		return nil, fmt.Errorf("scan refresh tokens: %w", err)
	}
	return chains, nil
}

// backfillOneChain seeds the oauth_connections + (when applicable)
// oauth_connection_workspaces rows for a single grant chain.
//
// Returns:
//   - created       — true iff we inserted a fresh oauth_connections row
//     (false on idempotent re-runs).
//   - slugsAdded    — count of join rows successfully inserted.
//   - slugsMissed   — count of slugs we couldn't resolve to a workspace
//     ID (deleted workspace, renamed since consent,
//     etc.); logged at WARN by the caller via the
//     aggregated UnresolvedSlugs counter.
//   - err           — only for I/O failures the caller should surface;
//     parse failures map to "wildcard" (no allow-list
//     constraint) rather than erroring, matching the
//     defensive posture in parseAllowedWorkspacesFromSession.
func (s *Store) backfillOneChain(requestID string, chain backfillChain) (created bool, slugsAdded, slugsMissed int, err error) {
	allowed := extractAllowedWorkspacesFromSessionExtra(chain.SessionData)
	allCurrent, explicit := allowed.toShape()

	// Per-chain transaction (Codex review #583 round 3) so the parent
	// row and ALL join rows land atomically. Previous round 2's gate
	// "only seed slugs when parent was just created" protected
	// user-removed workspaces from resurrection but introduced a new
	// failure mode: if the process crashed (or AddConnectionWorkspace
	// errored) between parent insert and the slug loop completing, a
	// subsequent backfill would see created=false and skip the
	// unfinished slug seeding — leaving the connection permanently
	// scoped to a partial allow-list.
	//
	// The atomic invariant: either all rows for this chain commit, or
	// none do. A crash mid-loop rolls the parent insert back too, so
	// the NEXT backfill sees the chain as un-seeded and retries from
	// scratch — preserving both Codex round 2's "no resurrection"
	// guarantee and round 3's "no permanent partial seed."
	//
	// Scope: per-chain (small) tx, not whole-backfill. The original
	// rationale for the no-transaction design was to avoid holding a
	// writer lock across O(thousands) of chains; that concern doesn't
	// apply at chain granularity (one row + a handful of join rows
	// inserts, sub-millisecond hold).
	tx, err := s.db.Begin()
	if err != nil {
		return false, 0, 0, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Existence check inside the tx so concurrent writers (theoretical:
	// the OAuth-server's /authorize/decide handler running while
	// backfill is up) can't insert between probe and seed.
	var one int
	probeErr := tx.QueryRow(s.q(`SELECT 1 FROM oauth_connections WHERE request_id = ?`), requestID).Scan(&one)
	switch {
	case probeErr == nil:
		// Parent already exists from a prior backfill or live consent
		// flow. The new tables are authoritative — leave the row + its
		// join entries alone. This is the path that protects user
		// removals (Codex review round 2) from getting resurrected.
		if commitErr := tx.Commit(); commitErr != nil {
			return false, 0, 0, fmt.Errorf("commit (no-op): %w", commitErr)
		}
		committed = true
		return false, 0, 0, nil
	case !errors.Is(probeErr, sql.ErrNoRows):
		return false, 0, 0, fmt.Errorf("probe parent: %w", probeErr)
	}

	// Parent missing — atomic seed begins. Insert parent.
	insertStmt := `
        INSERT INTO oauth_connections (
            request_id, user_id, name,
            may_create_workspaces, all_current_workspaces, include_future_workspaces
        ) VALUES (?, ?, '', ?, ?, ?)
    `
	// No need for OR IGNORE / ON CONFLICT here — we just probed inside
	// the same tx and confirmed absence. A racing inserter would
	// surface as a PK violation; let it propagate so the rollback
	// fires cleanly and the next backfill retries.
	if _, err := tx.Exec(s.q(insertStmt),
		requestID, chain.UserID,
		s.dialect.BoolToInt(true), // may_create_workspaces default ON
		s.dialect.BoolToInt(allCurrent),
		s.dialect.BoolToInt(true), // include_future_workspaces default ON
	); err != nil {
		return false, 0, 0, fmt.Errorf("insert parent: %w", err)
	}

	if explicit == nil {
		// Wildcard / pre-TASK-952 — commit parent-only, no join rows.
		if commitErr := tx.Commit(); commitErr != nil {
			return false, 0, 0, fmt.Errorf("commit (wildcard): %w", commitErr)
		}
		committed = true
		return true, 0, 0, nil
	}

	// Explicit-list path: resolve each slug + insert join row within
	// the same tx. Workspace lookups use the regular DB handle (not
	// the tx) because the workspaces table is read-only relative to
	// this transaction — no risk of dirty reads, and keeping the
	// lookup off the tx avoids holding the writer lock during the
	// (very rare) deleted-slug fast-path. SQLite + Postgres both honor
	// repeatable reads of static data here.
	for _, slug := range explicit {
		ws, getErr := s.GetWorkspaceBySlug(slug)
		if getErr != nil || ws == nil {
			slugsMissed++
			continue
		}
		// Plain INSERT (no OR IGNORE / ON CONFLICT) — by construction
		// the parent row was just minted in this tx, so the join
		// table has no pre-existing rows for this request_id. A
		// duplicate slug in the source list is the only edge that
		// could trip the PK; treat it as a programmer-error data
		// shape and let the rollback fire so the operator notices.
		if _, err := tx.Exec(s.q(`
            INSERT INTO oauth_connection_workspaces (request_id, workspace_id, added_by)
            VALUES (?, ?, ?)
        `), requestID, ws.ID, AddedByUser); err != nil {
			return false, 0, 0, fmt.Errorf("insert workspace %s: %w", slug, err)
		}
		slugsAdded++
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return false, slugsAdded, slugsMissed, fmt.Errorf("commit: %w", commitErr)
	}
	committed = true
	return true, slugsAdded, slugsMissed, nil
}

// extractedAllowed captures one of the three IDEA-1517 §2 backfill
// input shapes parsed out of session.Extra.allowed_workspaces.
type extractedAllowed struct {
	hasKey bool // true if session.Extra had the key at all
	isStar bool // true iff the only entry is "*" (wildcard)
	slugs  []string
}

// toShape converts the parsed extract into the (all_current, explicit)
// pair the seeder consumes. Three branches:
//
//   - hasKey == false        → all_current=true, explicit=nil
//     (pre-TASK-952 token; legacy semantic was "any workspace")
//   - isStar == true         → all_current=true, explicit=nil
//     (post-TASK-952 wildcard)
//   - explicit slug list     → all_current=false, explicit=<slugs>
//
// An explicit empty list (consent flow rejected) lands here too —
// all_current=false, explicit=[]string{} — yielding a connection
// scoped to nothing. That matches IDEA-1517's fail-closed posture
// (the consent flow rejects empty lists, so this case is exotic but
// preserved verbatim rather than silently widened).
func (e extractedAllowed) toShape() (allCurrent bool, explicit []string) {
	if !e.hasKey {
		return true, nil
	}
	if e.isStar {
		return true, nil
	}
	return false, e.slugs
}

// extractAllowedWorkspacesFromSessionExtra parses session_data JSON
// and returns the extracted shape. Defensive: malformed JSON,
// non-object .extra, non-string array entries, etc. all collapse to
// "no key" (wildcard semantic) — matching parseAllowedWorkspacesFromSession's
// posture. The backfill is one-shot data motion; a few malformed
// rows shouldn't fail the run.
func extractAllowedWorkspacesFromSessionExtra(sessionData string) extractedAllowed {
	if sessionData == "" {
		return extractedAllowed{}
	}
	var outer struct {
		Extra map[string]interface{} `json:"extra"`
	}
	if err := json.Unmarshal([]byte(sessionData), &outer); err != nil || outer.Extra == nil {
		return extractedAllowed{}
	}
	raw, ok := outer.Extra["allowed_workspaces"]
	if !ok {
		return extractedAllowed{}
	}
	out := extractedAllowed{hasKey: true}
	var collected []string
	switch v := raw.(type) {
	case []string:
		collected = append([]string(nil), v...)
	case []interface{}:
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				collected = append(collected, s)
			}
		}
	default:
		// Key present but with a non-array shape: treat as if absent
		// rather than fail-closed; preserves the legacy "unrecognized
		// payload = unrestricted" behaviour of the existing helper.
		return extractedAllowed{}
	}
	// A list containing exactly one "*" is the wildcard sentinel.
	// Other shapes (["*","foo"], multiple wildcards, etc.) fall
	// through to the explicit-list branch where the seeder drops
	// the wildcard via the slug-resolve step (no workspace has
	// slug "*", so it lands in UnresolvedSlugs).
	if len(collected) == 1 && collected[0] == "*" {
		out.isStar = true
		return out
	}
	out.slugs = collected
	return out
}
