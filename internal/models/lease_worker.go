package models

// A lease worker id (TASK-3613) is the opaque identity an agent session holds
// item leases under, separate from its display name: two agents both called
// "implementer" on one account get two ids, so they cannot share a lease. The
// client mints it ("wk_" + 128 random bits as 32 lowercase hex) and reuses it
// for the life of its session; the server only checks the shape and always
// matches it together with the authenticated account (lead ruling).

const workerIDPrefix = "wk_"

// workerIDHexLen is 128 bits in hex.
const workerIDHexLen = 32

// ValidWorkerID reports whether s is a well-formed worker id. The shape is
// strict (lowercase hex only), which is also what lets the column skip the
// NUL triggers: an accepted id cannot contain one.
func ValidWorkerID(s string) bool {
	if len(s) != len(workerIDPrefix)+workerIDHexLen || s[:len(workerIDPrefix)] != workerIDPrefix {
		return false
	}
	for _, c := range s[len(workerIDPrefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ShortWorkerID is the display form ("wk_3f2a"): enough to tell two holders
// apart in a conflict message or an item's detail, never the full id, which
// is the credential-like half of the lease identity (lead ruling: the full id
// never reaches the UI). An invalid id yields "".
func ShortWorkerID(s string) string {
	if !ValidWorkerID(s) {
		return ""
	}
	return s[:len(workerIDPrefix)+4]
}
