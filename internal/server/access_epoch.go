package server

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// accessEpochUnrestricted is the epoch for a caller with no collection
// filtering at all (owner, editor with "all" access, cookie-session admin).
//
// It is a SENTINEL rather than a hash of the empty set, because the empty set
// is a real and opposite state: a restricted member with zero visible
// collections. Hashing both to the same value would make the widest and the
// narrowest access indistinguishable — a caller whose access was revoked down
// to nothing would carry the same epoch as an owner, and a promotion from
// nothing to everything would signal no change at all.
const accessEpochUnrestricted = "all"

// computeAccessEpoch fingerprints the caller's EFFECTIVE visible resource set
// so a client can tell that the set changed without being told what it is.
//
// IDEA-2898. The delta stream (/items-changes) can only express changes to
// ROWS. A revocation that writes no item — the ordinary shape of revocation —
// therefore produces no signal at all, and a client's warm local index goes on
// serving rows for a collection the caller can no longer see. This value is
// that missing signal: the client compares it against the one it stored and,
// on a change, runs the authoritative resync it already implements.
//
// Inputs are the two lists the item doors already resolve before writing their
// response, so this costs no additional query:
//
//   - visibleCollectionIDs: nil means "no filtering" (see the sentinel above);
//     a non-nil slice, possibly empty, is the caller's collection-level set.
//   - grantedItemIDs: the caller's item-level grants, empty for most callers.
//
// The hash is over a CANONICAL form — both lists sorted, and the two lists
// separated by a byte that cannot occur in an ID — so a set that has not
// changed cannot produce a different epoch just because the store returned its
// rows in a different order. An epoch that flapped on ordering alone would
// trigger a full resync per poll, which is the one failure mode that would
// make this worse than the defect it closes.
//
// The value is derived only from the caller's OWN access and is opaque: it
// names no collection and no item, and two callers with different access
// cannot learn anything about each other from it.
func computeAccessEpoch(visibleCollectionIDs, grantedItemIDs []string) string {
	if visibleCollectionIDs == nil {
		// Grants only narrow within a filtered set; an unrestricted caller
		// cannot also be item-filtered, and the doors never pass both.
		return accessEpochUnrestricted
	}
	h := sha256.New()
	writeSortedIDs(h, visibleCollectionIDs)
	// Separator: a newline cannot appear in a UUID, so ["a","b"]+[] and
	// ["a"]+["b"] cannot collide.
	_, _ = h.Write([]byte("\n--\n"))
	writeSortedIDs(h, grantedItemIDs)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// writeSortedIDs feeds ids to h in sorted order, one per line. The input slice
// is COPIED before sorting: the callers pass slices they go on to use for the
// query itself, and reordering those under them would be an invisible
// side effect of computing a fingerprint.
func writeSortedIDs(h interface{ Write([]byte) (int, error) }, ids []string) {
	sorted := make([]string, len(ids))
	copy(sorted, ids)
	sort.Strings(sorted)
	for _, id := range sorted {
		_, _ = h.Write([]byte(id))
		_, _ = h.Write([]byte("\n"))
	}
}
