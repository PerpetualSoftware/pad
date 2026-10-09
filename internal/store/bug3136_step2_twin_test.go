package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3136: an open tab answers a joining tab's SyncStep1 with a SyncStep2
// carrying its full state, and the relay persists that answer. Its payload is
// byte-identical to the seed update the item already holds; only the subtype
// byte differs (01 vs 02), so the whole-frame twin check missed it and the item
// read pending after every first join.
//
// Both frames are REAL: rows 2 and 18 of BUG-3134's measurement instance (a
// 5-line doc, headless Chromium; row 2 is the lazy seed, row 18 the incumbent's
// answer to the first joiner). They are literals, not derived from each other,
// so the test cannot share an encoding mistake with the code under test.
var (
	probeSeedUpdate = mustDecode("0002BD020112E0E5C8B3080007010764656661756C74030768656164696E670700E0E5C8B30800060400E0E5C8B30801" +
		"0550726F62652800E0E5C8B30800056C6576656C017D0187E0E5C8B3080003097061726167726170680700E0E5C8B308" +
		"08060400E0E5C8B308092D4120706172616772617068206F6620626F6479207465787420666F7220746865206D656173" +
		"7572656D656E742E87E0E5C8B30808030A62756C6C65744C6973740700E0E5C8B3083703086C6973744974656D0700E0" +
		"E5C8B3083803097061726167726170680700E0E5C8B30839060400E0E5C8B3083A036F6E6587E0E5C8B3083803086C69" +
		"73744974656D0700E0E5C8B3083E03097061726167726170680700E0E5C8B3083F060400E0E5C8B308400374776F2800" +
		"E0E5C8B30837057469676874017887E0E5C8B30837030970617261677261706800")
	probeStep2Answer = mustDecode("0001BD020112E0E5C8B3080007010764656661756C74030768656164696E670700E0E5C8B30800060400E0E5C8B30801" +
		"0550726F62652800E0E5C8B30800056C6576656C017D0187E0E5C8B3080003097061726167726170680700E0E5C8B308" +
		"08060400E0E5C8B308092D4120706172616772617068206F6620626F6479207465787420666F7220746865206D656173" +
		"7572656D656E742E87E0E5C8B30808030A62756C6C65744C6973740700E0E5C8B3083703086C6973744974656D0700E0" +
		"E5C8B3083803097061726167726170680700E0E5C8B30839060400E0E5C8B3083A036F6E6587E0E5C8B3083803086C69" +
		"73744974656D0700E0E5C8B3083E03097061726167726170680700E0E5C8B3083F060400E0E5C8B308400374776F2800" +
		"E0E5C8B30837057469676874017887E0E5C8B30837030970617261677261706800")
)

func TestStep2AnswerDefersToItsUpdateTwin(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			seed := appendFrame(t, s, item.ID, probeSeedUpdate)
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, seed); err != nil {
				t.Fatal(err)
			}
			// PREMISE: the flushed seed alone reads clean.
			if got := contentState(t, s, item.ID); got != "" {
				t.Fatalf("premise: a flushed seed must read clean; got %q", got)
			}
			appendFrame(t, s, item.ID, probeStep2Answer)
			if got := contentState(t, s, item.ID); got != "" {
				t.Fatalf("a step2 carrying the seed's exact payload changes nothing, so the item must stay clean; got %q", got)
			}
		})
	}
}

// The equivalence runs both ways: an update whose payload an earlier step2
// already carried is equally a no-op.
func TestUpdateDefersToItsStep2Twin(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			first := appendFrame(t, s, item.ID, probeStep2Answer)
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, first); err != nil {
				t.Fatal(err)
			}
			appendFrame(t, s, item.ID, probeSeedUpdate)
			if got := contentState(t, s, item.ID); got != "" {
				t.Fatalf("an update repeating a flushed step2's payload must read clean; got %q", got)
			}
		})
	}
}

// BUG-3523: a step2 is an answer to a peer's step1 and repeats content other
// rows carry, so it is not counted even when no earlier row holds its payload.
// Before BUG-3523 such a step2 read pending.
func TestStep2WithNewPayloadIsNotContentBearing(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			seed := appendFrame(t, s, item.ID, probeSeedUpdate)
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, seed); err != nil {
				t.Fatal(err)
			}
			other := append([]byte(nil), probeStep2Answer...)
			other[len(other)-1] ^= 0x01
			appendFrame(t, s, item.ID, other)
			if got := contentState(t, s, item.ID); got != "" {
				t.Fatalf("a step2 answer must not count as pending content; got %q", got)
			}
		})
	}
}

// Control for the rule above: the same payload as an UPDATE is content.
func TestUpdateWithNewPayloadStaysContentBearing(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			seed := appendFrame(t, s, item.ID, probeSeedUpdate)
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, seed); err != nil {
				t.Fatal(err)
			}
			other := append([]byte(nil), probeStep2Answer...)
			other[len(other)-1] ^= 0x01
			other[1] = 0x02 // update, not step2
			appendFrame(t, s, item.ID, other)
			if got := contentState(t, s, item.ID); got != models.ContentOutcomeAppliedPendingFlush {
				t.Fatalf("an update with a payload no earlier row carries must read pending; got %q", got)
			}
		})
	}
}

// The step2 rule is applied at append only. A legacy row the backfill
// classifies keeps the pre-BUG-3523 rule: a step2 with no twin is content.
func TestBackfillDoesNotApplyTheStep2Rule(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			seed := appendFrame(t, s, item.ID, probeSeedUpdate)
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, seed); err != nil {
				t.Fatal(err)
			}
			other := append([]byte(nil), probeStep2Answer...)
			other[len(other)-1] ^= 0x01
			id := appendFrame(t, s, item.ID, other)
			// Make it a legacy row: unclassified, bearing by the column default.
			if _, err := s.DB().Exec(rebind(b.name, `UPDATE item_yjs_updates SET content_hash = NULL, content_bearing = `+trueLit(b.name)+` WHERE id = ?`), id); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BackfillYjsContentBearing(); err != nil {
				t.Fatal(err)
			}
			if got := contentState(t, s, item.ID); got != models.ContentOutcomeAppliedPendingFlush {
				t.Fatalf("the backfill must keep a legacy step2 with no twin as content; got %q", got)
			}
		})
	}
}

// Strictness: a frame that does not parse exactly gets no subtype twin, even
// when an earlier row is its byte-for-byte subtype swap. Here the length prefix
// claims 5 bytes and 2 follow.
func TestMalformedFrameGetsNoSubtypeTwin(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			first := appendFrame(t, s, item.ID, []byte{0x00, 0x02, 0x05, 0xAA, 0xBB})
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, first); err != nil {
				t.Fatal(err)
			}
			appendFrame(t, s, item.ID, []byte{0x00, 0x01, 0x05, 0xAA, 0xBB})
			if got := contentState(t, s, item.ID); got != models.ContentOutcomeAppliedPendingFlush {
				t.Fatalf("a malformed frame must stay content-bearing; got %q", got)
			}
		})
	}
}

// Rows classified under migration 091's rule keep that verdict until
// something re-examines them. Migration 093 re-queues them; the startup
// backfill then applies the twin rule. The counterfactual leg shows the
// migration is what does it: without it the backfill has nothing to examine.
func TestMigration093RequeuesStep2RowsForTheBackfill(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			path := "migrations/093_yjs_step2_twin_reclassify.sql"
			if b.name == "Postgres" {
				path = "pgmigrations/070_yjs_step2_twin_reclassify.sql"
			}
			sqlText, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, _, item := seedStaleItem(t, s)
			// A content-bearing row that is NOT a step2/update frame: 093 must
			// leave it alone, so the count below catches a migration that drops
			// its subtype predicate (codex round 1, P3).
			appendFrame(t, s, item.ID, []byte{0x07, 0x01, 0x02})
			seed := appendFrame(t, s, item.ID, probeSeedUpdate)
			answer := appendFrame(t, s, item.ID, probeStep2Answer)
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, seed); err != nil {
				t.Fatal(err)
			}
			// Reproduce the pre-BUG-3136 verdict on the answer row: classified
			// (hash set) and content-bearing.
			if _, err := s.DB().Exec(rebind(b.name, `UPDATE item_yjs_updates SET content_bearing = `+trueLit(b.name)+` WHERE id = ?`), answer); err != nil {
				t.Fatal(err)
			}
			if got := contentState(t, s, item.ID); got != models.ContentOutcomeAppliedPendingFlush {
				t.Fatalf("premise: the old verdict must read pending; got %q", got)
			}

			// Counterfactual: the backfill alone does not revisit a classified row.
			if res, err := s.BackfillYjsContentBearing(); err != nil || res.RowsClassified != 0 {
				t.Fatalf("premise: without 093 the backfill must classify nothing; got %+v, %v", res, err)
			}

			if _, err := s.DB().Exec(string(sqlText)); err != nil {
				t.Fatalf("migration: %v", err)
			}
			res, err := s.BackfillYjsContentBearing()
			if err != nil {
				t.Fatal(err)
			}
			// 093 re-queues BOTH bearing step2/update rows: the seed
			// (re-examined, still content: nothing earlier carries it) and the
			// answer (cleared by its twin). The non-sync row is not re-queued.
			if res.RowsClassified != 2 || res.RowsNonContent != 1 {
				t.Fatalf("want 2 re-examined and 1 cleared; got %+v", res)
			}
			if got := contentState(t, s, item.ID); got != "" {
				t.Fatalf("after 093 and the backfill the item must read clean; got %q", got)
			}
		})
	}
}

// rebind turns the single ? placeholder these tests use into Postgres's $1.
func rebind(backend, q string) string {
	if backend == "Postgres" {
		return strings.Replace(q, "?", "$1", 1)
	}
	return q
}

func trueLit(backend string) string {
	if backend == "Postgres" {
		return "TRUE"
	}
	return "1"
}

// The relay's append path (AppendSyncFrame) applies the same rule: the step2 is
// stored, so joining tabs still replay it, but not counted.
func TestRelayAppendStoresAStep2AsNonContent(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			_, _, item := seedStaleItem(t, s)
			seed := appendFrame(t, s, item.ID, probeSeedUpdate)
			if err := s.SetItemContentFlushedOpLogIDForTesting(item.ID, seed); err != nil {
				t.Fatal(err)
			}
			other := append([]byte(nil), probeStep2Answer...)
			other[len(other)-1] ^= 0x01
			got, err := s.AppendSyncFrame(item.ID, other, "1")
			if err != nil {
				t.Fatal(err)
			}
			if !got.Persisted {
				t.Fatal("the step2 must be stored, so a joining tab replays it")
			}
			if cs := contentState(t, s, item.ID); cs != "" {
				t.Fatalf("a relayed step2 answer must not count as pending content; got %q", cs)
			}
		})
	}
}
