package store

import (
	"fmt"
	"log/slog"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// dedupeTraitDeclarations resolves workspaces that hold more than one
// collection declaring the same kernel trait, so the partial unique indexes in
// migrations 087 / 064 can be created (TASK-2710).
//
// WHY THIS IS GO AND NOT SQL, and why it runs BEFORE migrate(). The ruling
// requires every resolution to be REPORTED — it silently changes which
// collection owns a kernel behavior, so an operator has to be able to see what
// moved. A SQL migration cannot log: Postgres RAISE NOTICE goes nowhere
// (this store registers no notice handler) and SQLite has no equivalent at
// all. Splitting "decide" into SQL and "report" into Go would give one rule two
// spellings that must be kept in step forever — the exact defect class the
// composite key in IDEA-2883 was introduced to close. So the rule is written
// once, here, and the SQL migrations only create the indexes.
//
// Running before migrate() is what makes that ordering work: the index
// migration would FAIL on precisely the databases that need repairing, so the
// repair has to precede it. On a fresh database there is no collections table
// yet, which is the no-op case below.
//
// THE RULE (lead ruling, day-58, after three proposals each retired by a
// measurement):
//
//  1. The collection holding the most items the USER wrote — source <>
//     'template' — wins.
//  2. Ties break on lowest (created_at, id), which is ARBITRARY and is
//     reported as such. It is NOT an age criterion: created_at is
//     second-resolution RFC3339 and newID() is a random UUID v4, so when two
//     collections are created in the same second — the normal case for the
//     reproduction this repairs — the pair carries no age information at all.
//     Measured: over 12 trials of the reproduction, "oldest wins" picked the
//     original 7 times and the accidental duplicate 5, with created_at
//     differing in 0 of them.
//  3. The loser keeps every item. It loses only the declaration.
//
// Why not recency, which was proposed first: the duplicate is typically
// created by a template reseed, so ITS items are the newest and an activity
// criterion picks the accident. Counting only non-template items inverts that
// — a reseed has none by construction, so it can tie but never win. Measured:
// 8/8 for the user's collection on a fixture where someone had written one
// convention first, versus a 4/4 coin flip on the bare reproduction where
// nobody had written anything and no rule could do better.
//
// ROUTING MAY CHANGE on an affected deployment. There is no current behavior
// to preserve: with two declarations live, resolution is by result order, and
// on Postgres that was measured flipping between runs on identical data.
func (s *Store) dedupeTraitDeclarations() error {
	if !s.tableHasColumn("collections", "traits") {
		return nil // fresh database, or one predating collection traits
	}

	type candidate struct {
		id        string
		slug      string
		workspace string
		traits    string
		userItems int
		createdAt string
	}

	rows, err := s.db.Query(s.q(`
		SELECT c.id, c.slug, c.workspace_id, c.traits, c.created_at,
		       (SELECT COUNT(*) FROM items i
		         WHERE i.collection_id = c.id
		           AND i.deleted_at IS NULL
		           AND i.source <> 'template')
		FROM collections c
		WHERE c.deleted_at IS NULL
		ORDER BY c.workspace_id, c.created_at ASC, c.id ASC`))
	if err != nil {
		return fmt.Errorf("scan collections for trait duplicates: %w", err)
	}
	defer rows.Close()

	// kindOwners[workspace][kind] and fieldOwners[workspace] collect the
	// candidates competing for one declaration.
	kindOwners := map[string]map[string][]candidate{}
	fieldOwners := map[string][]candidate{}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.slug, &c.workspace, &c.traits, &c.createdAt, &c.userItems); err != nil {
			return fmt.Errorf("scan collection: %w", err)
		}
		parsed, perr := models.ParseCollectionTraits(c.traits)
		if perr != nil {
			// A blob that does not parse declares nothing, which is how every
			// other reader treats it (ListTraitedCollections). It cannot
			// compete for a declaration and it cannot violate the index.
			continue
		}
		if parsed.ArtifactKind != nil && parsed.ArtifactKind.Kind != "" {
			if kindOwners[c.workspace] == nil {
				kindOwners[c.workspace] = map[string][]candidate{}
			}
			k := parsed.ArtifactKind.Kind
			kindOwners[c.workspace][k] = append(kindOwners[c.workspace][k], c)
		}
		if parsed.InvocationField != "" {
			fieldOwners[c.workspace] = append(fieldOwners[c.workspace], c)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	type resolution struct {
		loser    candidate
		what     string
		winner   candidate
		byRule   string
		stripAll bool // invocation_field is a string, artifact_kind a pointer
	}
	var losers []resolution

	pick := func(cands []candidate) (candidate, string) {
		best := cands[0]
		decisive := false
		for _, c := range cands[1:] {
			if c.userItems > best.userItems {
				best, decisive = c, true
				continue
			}
			if c.userItems < best.userItems {
				decisive = true
			}
		}
		// The scan is already ordered by (created_at, id) within a workspace,
		// so cands[0] is the terminator's answer when no user content decides.
		if !decisive {
			return cands[0], "arbitrary (tie on user-written items; lowest created_at,id — NOT an age)"
		}
		return best, "most user-written items"
	}

	for _, byKind := range kindOwners {
		for kind, cands := range byKind {
			if len(cands) < 2 {
				continue
			}
			winner, rule := pick(cands)
			for _, c := range cands {
				if c.id == winner.id {
					continue
				}
				losers = append(losers, resolution{c, "artifact_kind=" + kind, winner, rule, false})
			}
		}
	}
	for _, cands := range fieldOwners {
		if len(cands) < 2 {
			continue
		}
		winner, rule := pick(cands)
		for _, c := range cands {
			if c.id == winner.id {
				continue
			}
			losers = append(losers, resolution{c, "invocation_field", winner, rule, true})
		}
	}
	if len(losers) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, l := range losers {
		parsed, perr := models.ParseCollectionTraits(l.loser.traits)
		if perr != nil {
			continue
		}
		if l.stripAll {
			parsed.InvocationField = ""
		} else {
			parsed.ArtifactKind = nil
		}
		encoded, eerr := parsed.JSON()
		if eerr != nil {
			return fmt.Errorf("re-encode traits for %s: %w", l.loser.slug, eerr)
		}
		if _, err := tx.Exec(s.q(`UPDATE collections SET traits = ? WHERE id = ?`), encoded, l.loser.id); err != nil {
			return fmt.Errorf("strip duplicate declaration from %s: %w", l.loser.slug, err)
		}
		// One line per resolution, carrying everything an operator needs to
		// tell a considered resolution from a coin flip.
		slog.Warn("trait de-duplication: a collection lost a kernel declaration; routing for it may have changed",
			"workspace_id", l.loser.workspace,
			"declaration", l.what,
			"winner", l.winner.slug,
			"winner_user_items", l.winner.userItems,
			"loser", l.loser.slug,
			"loser_user_items", l.loser.userItems,
			"decided_by", l.byRule,
			"loser_items_kept", true)
	}
	return tx.Commit()
}

// tableHasColumn reports whether a column exists, so the de-dup pass can no-op
// on a database that predates collection traits (or has no schema at all).
func (s *Store) tableHasColumn(table, column string) bool {
	var query string
	switch s.dialect.Driver() {
	case DriverPostgres:
		query = `SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`
	default:
		// SQLite has no information_schema; pragma_table_info is the
		// queryable form of PRAGMA table_info.
		query = `SELECT 1 FROM pragma_table_info(?) WHERE name = ?`
	}
	var one int
	// sql.ErrNoRows means the column is absent; any other error means we
	// cannot tell, and the safe answer there is "absent" so the pass no-ops
	// rather than running against a schema it could not inspect.
	return s.db.QueryRow(query, table, column).Scan(&one) == nil
}
