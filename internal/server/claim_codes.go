package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"time"
)

// Stateless 6-digit claim codes (PLAN-1519 / TASK-1521 / IDEA-1517 §4).
//
// A claim code lets a user grant an existing OAuth connection access to
// one specific workspace they own/belong to. The user generates the
// code in the web UI (Phase E "Connect project" modal), reads it to
// their agent, and the agent calls `pad_workspace.action: claim` —
// which calls POST /api/v1/oauth/claim with {workspace, code}.
//
// **Why stateless.** Generating + storing + GC'ing per-code rows would
// pollute the schema and require background sweeps. Instead we
// HMAC-derive the code from (user_id, workspace_id, 5-min time bucket)
// using a server-stable secret. Server re-derives on claim and
// constant-time compares. Verifier checks the current AND previous
// bucket so the effective lifetime is 5–10 minutes (sliding window) —
// long enough for the user to paste the prompt, short enough that a
// snoop with the code has limited replay surface.
//
// **Threat model.** An attacker who knows the secret can mint codes
// for any (user, workspace) pair — same posture as anyone who can
// mint fosite tokens, which uses the same secret material. An attacker
// who DOESN'T know the secret can't fabricate codes; they can only
// observe ones in flight (5-10 minute window) AND must also control an
// OAuth connection belonging to the same user to redeem (the claim
// handler binds redemption to the calling grant's owner — see
// handleOAuthClaim).
//
// **Code format.** Six decimal digits, zero-padded. Truncated from
// HMAC-SHA256(secret, payload) modulo 10^6. Birthday collisions are
// irrelevant — verification re-derives + compares for a specific
// (user, workspace) tuple, not against a global pool — so the small
// digit count just controls how guessable a fresh code is. Without
// rate limiting, brute force is 10^6 attempts on the claim endpoint;
// rate limiting at the /mcp gate (TASK-959) caps that in practice.
// If 6 digits ever proves too narrow, bump to 8 — the same derive +
// modulus logic applies.

const (
	// claimCodeDigits is how many decimal digits the code carries.
	// 6 is the IDEA-1517 §4 spec. Changing it is a wire-format change
	// — the modal text + agent NL parser both reference "6-digit."
	claimCodeDigits = 6

	// claimCodeModulus is 10^claimCodeDigits, used to truncate the
	// HMAC output to digit count.
	claimCodeModulus = 1_000_000

	// claimBucketSeconds is the granularity of the time component
	// going into the HMAC. Locked to 5 minutes per IDEA-1517 §4:
	// "Bucket size: hardcoded 5 minutes." Combined with the
	// current+previous bucket check below this yields a sliding
	// 5–10 minute effective lifetime.
	claimBucketSeconds = 300
)

// DeriveClaimCode produces the 6-digit code for the given (user,
// workspace) at the given wall-clock time. Exported so the Phase E
// "Connect project" modal handler can render the same code the claim
// path will verify.
//
// secret must be at least 16 bytes of cryptographically-strong material
// — production wires the 32-byte encryption key from the deployment
// config. Shorter secrets degrade to "obfuscation" rather than HMAC
// strength; the constructor in SetClaimSecret rejects them.
//
// Returns "000000"–"999999" zero-padded so the wire format is uniform
// (avoids an agent stripping leading zeros when echoing the code back).
func DeriveClaimCode(secret []byte, userID, workspaceID string, at time.Time) string {
	bucket := at.UTC().Unix() / claimBucketSeconds
	return deriveClaimCodeForBucket(secret, userID, workspaceID, bucket)
}

// deriveClaimCodeForBucket is the inner derive — split out so
// VerifyClaimCode can derive both the current and previous bucket
// without re-computing the bucket math twice.
func deriveClaimCodeForBucket(secret []byte, userID, workspaceID string, bucket int64) string {
	mac := hmac.New(sha256.New, secret)
	// Length-prefix each component so two distinct (user, workspace)
	// pairs that happen to concatenate to the same byte string (e.g.
	// userID="abc" + workspaceID="def" vs. userID="abcdef" +
	// workspaceID="") can't collide on the same code. Belt-and-
	// suspenders — UUIDs don't realistically alias, but a future
	// schema change to numeric IDs could re-introduce the risk.
	writeLenPrefixed(mac, []byte(userID))
	writeLenPrefixed(mac, []byte(workspaceID))
	var bucketBytes [8]byte
	binary.BigEndian.PutUint64(bucketBytes[:], uint64(bucket))
	mac.Write(bucketBytes[:])
	sum := mac.Sum(nil)
	// Take the first 8 bytes as an unsigned int, then mod down to
	// the digit count. Using the full 32-byte hash would be wasteful
	// (we throw away 24 bytes either way); 8 bytes gives plenty of
	// entropy before truncation.
	n := binary.BigEndian.Uint64(sum[:8]) % claimCodeModulus
	return fmt.Sprintf("%0*d", claimCodeDigits, n)
}

// writeLenPrefixed writes a 4-byte big-endian length followed by the
// raw bytes. Cheap collision-resistant separator for HMAC inputs.
func writeLenPrefixed(w interface{ Write([]byte) (int, error) }, b []byte) {
	var lenBytes [4]byte
	binary.BigEndian.PutUint32(lenBytes[:], uint32(len(b)))
	w.Write(lenBytes[:])
	w.Write(b)
}

// VerifyClaimCode returns true if code matches the derived code for
// (userID, workspaceID) at the current OR previous time bucket — the
// sliding 5–10 minute lifetime described in IDEA-1517 §4.
//
// Constant-time compare prevents timing oracles from leaking which of
// the two derivations failed (which would reduce the effective lifetime
// signal to 5 minutes rather than 10).
//
// Empty code, empty userID, or empty workspaceID always return false —
// no need to even derive in those cases.
func VerifyClaimCode(secret []byte, userID, workspaceID, code string, at time.Time) bool {
	if code == "" || userID == "" || workspaceID == "" || len(secret) < 16 {
		return false
	}
	if len(code) != claimCodeDigits {
		return false
	}
	bucket := at.UTC().Unix() / claimBucketSeconds
	current := deriveClaimCodeForBucket(secret, userID, workspaceID, bucket)
	previous := deriveClaimCodeForBucket(secret, userID, workspaceID, bucket-1)
	codeBytes := []byte(code)
	// ConstantTimeCompare returns 1 on match. OR the two results so
	// either matching bucket passes; subtle's compare doesn't panic
	// on equal-length operands so length-mismatch isn't a concern
	// here (we already gated on claimCodeDigits above).
	return subtle.ConstantTimeCompare(codeBytes, []byte(current)) == 1 ||
		subtle.ConstantTimeCompare(codeBytes, []byte(previous)) == 1
}
