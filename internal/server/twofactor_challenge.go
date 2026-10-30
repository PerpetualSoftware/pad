package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const twoFAChallengeExpiry = 5 * time.Minute

// A challenge's binding field names its KIND, so a token can never be read as
// the other one (TASK-4137): "ip:<addr>" is held to the address that minted
// it, "pkce:<S256 challenge>" to whoever holds the verifier. Tokens minted
// before the kinds existed (5-minute TTL) carry a bare address; one is read
// as IP-bound only if the whole field parses as an IP, which a PKCE value
// (43 base64url chars) never does.
const (
	twoFAIPBindingPrefix   = "ip:"
	twoFAPKCEBindingPrefix = "pkce:"
)

// errTwoFAChallengeBinding is every binding failure, so all answer alike.
var errTwoFAChallengeBinding = fmt.Errorf("challenge binding mismatch")

// charsetLen reports whether s is min-max characters of A-Z a-z 0-9 - _ plus
// those in extra.
func charsetLen(s string, min, max int, extra string) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || strings.ContainsRune(extra, c)) {
			return false
		}
	}
	return true
}

// isS256Challenge: an RFC 7636 S256 code_challenge, exactly 43 base64url
// characters (a 32-byte digest, unpadded).
func isS256Challenge(s string) bool { return charsetLen(s, 43, 43, "") }

// isPKCEVerifier: an RFC 7636 code_verifier, 43-128 unreserved characters.
func isPKCEVerifier(s string) bool { return charsetLen(s, 43, 128, ".~") }

// pkceVerifierMatches compares sha256(verifier) with the bound challenge on
// the decoded 32-byte digests, in constant time.
func pkceVerifierMatches(verifier, challenge string) bool {
	want, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// generateTwoFAChallengePKCE mints a challenge bound to a PKCE S256 challenge
// instead of an address. The caller has checked isS256Challenge.
func generateTwoFAChallengePKCE(userID, s256Challenge string, epoch int64, secret []byte) string {
	return signTwoFAChallenge(userID, twoFAPKCEBindingPrefix+s256Challenge, epoch, secret)
}

// generateTwoFASecret creates a random 32-byte secret for signing 2FA challenge tokens.
func generateTwoFASecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate 2FA secret: %w", err)
	}
	return b, nil
}

// generateTwoFAChallengeAt creates a short-lived, HMAC-signed challenge token
// that proves the user already passed password verification. The token is
// bound to the user ID and client IP, and expires after 5 minutes. It also
// carries the account's credential_epoch as of the FIRST factor's check
// (BUG-3382), so the session minted when the second
// factor completes is fenced on the epoch the password (or provider) was
// checked under, not the one read minutes later. A negative epoch omits it.
func generateTwoFAChallengeAt(userID, clientIP string, epoch int64, secret []byte) string {
	return signTwoFAChallenge(userID, twoFAIPBindingPrefix+clientIP, epoch, secret)
}

func signTwoFAChallenge(userID, binding string, epoch int64, secret []byte) string {
	expires := time.Now().UTC().Add(twoFAChallengeExpiry).Unix()
	payload := fmt.Sprintf("%s|%s|%d", userID, binding, expires)
	if epoch >= 0 {
		payload += fmt.Sprintf("|%d", epoch)
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return encoded + "." + sig
}

// validateTwoFAChallengeEpoch verifies a challenge token's signature, expiry,
// and IP binding. Returns the user ID and the credential epoch the challenge
// was issued under, or -1 for a token that carries none (issued before
// BUG-3382; they expire within minutes), or an error describing the failure.
func validateTwoFAChallengeEpoch(token, clientIP string, secret []byte) (string, int64, error) {
	return validateTwoFAChallengeBound(token, clientIP, "", secret)
}

// validateTwoFAChallengeBound is validateTwoFAChallengeEpoch with the
// presented code_verifier ("" for none). A pkce-bound token needs a verifier
// whose S256 matches; an IP-bound one needs the minting address AND no
// verifier. Any other pairing is errTwoFAChallengeBinding.
func validateTwoFAChallengeBound(token, clientIP, verifier string, secret []byte) (string, int64, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", -1, fmt.Errorf("malformed challenge token")
	}

	encoded, sig := parts[0], parts[1]

	// Verify HMAC
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encoded))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return "", -1, fmt.Errorf("invalid challenge signature")
	}

	// Decode payload
	payloadBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", -1, fmt.Errorf("invalid challenge encoding")
	}
	payload := string(payloadBytes)

	fields := strings.SplitN(payload, "|", 4)
	if len(fields) != 3 && len(fields) != 4 {
		return "", -1, fmt.Errorf("invalid challenge payload")
	}

	userID := fields[0]
	binding := fields[1]
	expiryStr := fields[2]

	// Check expiry
	expiryUnix, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return "", -1, fmt.Errorf("invalid challenge expiry")
	}
	if time.Now().UTC().Unix() > expiryUnix {
		return "", -1, fmt.Errorf("challenge token expired")
	}

	switch {
	case strings.HasPrefix(binding, twoFAPKCEBindingPrefix):
		if verifier == "" || !pkceVerifierMatches(verifier, strings.TrimPrefix(binding, twoFAPKCEBindingPrefix)) {
			return "", -1, errTwoFAChallengeBinding
		}
	case strings.HasPrefix(binding, twoFAIPBindingPrefix):
		if verifier != "" || strings.TrimPrefix(binding, twoFAIPBindingPrefix) != clientIP {
			return "", -1, errTwoFAChallengeBinding
		}
	default:
		// Minted before the kinds existed: a bare address, and only that.
		if net.ParseIP(binding) == nil || verifier != "" || binding != clientIP {
			return "", -1, errTwoFAChallengeBinding
		}
	}

	epoch := int64(-1)
	if len(fields) == 4 {
		e, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || e < 0 {
			return "", -1, fmt.Errorf("invalid challenge epoch")
		}
		epoch = e
	}
	return userID, epoch, nil
}
