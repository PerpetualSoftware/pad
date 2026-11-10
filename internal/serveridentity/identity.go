// Package serveridentity defines the signed server identity transcript.
package serveridentity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Context separates identity proofs from other uses of the signing key.
const Context = "pad-server-identity/v1/ed25519/challenge"

// KeyLabel is the HKDF-SHA-256 info used to derive the Ed25519 seed.
const KeyLabel = "pad-server-identity-ed25519-v1"

// Message concatenates the context, a zero byte, the public key and challenge.
// A 32-byte public key and 32-byte challenge produce exactly 105 bytes.
func Message(pub ed25519.PublicKey, challenge []byte) []byte {
	message := make([]byte, 0, len(Context)+1+len(pub)+len(challenge))
	message = append(message, Context...)
	message = append(message, 0)
	message = append(message, pub...)
	return append(message, challenge...)
}

// Fingerprint formats the first 16 bytes of SHA-256 of the raw public key.
func Fingerprint(pub ed25519.PublicKey) string {
	digest := sha256.Sum256(pub)
	encoded := hex.EncodeToString(digest[:16])
	groups := make([]string, 8)
	for i := range groups {
		groups[i] = encoded[i*4 : (i+1)*4]
	}
	return strings.Join(groups, "-")
}

// Verify checks a plain Ed25519 proof, refusing every invalid input length.
func Verify(pub ed25519.PublicKey, challenge, signature []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(challenge) != 32 || len(signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, Message(pub, challenge), signature)
}
