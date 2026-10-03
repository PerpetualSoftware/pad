package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const twoFAChallengeExpiry = 5 * time.Minute

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
	expires := time.Now().UTC().Add(twoFAChallengeExpiry).Unix()
	payload := fmt.Sprintf("%s|%s|%d", userID, clientIP, expires)
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
	tokenIP := fields[1]
	expiryStr := fields[2]

	// Check expiry
	expiryUnix, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return "", -1, fmt.Errorf("invalid challenge expiry")
	}
	if time.Now().UTC().Unix() > expiryUnix {
		return "", -1, fmt.Errorf("challenge token expired")
	}

	// Check IP binding
	if tokenIP != clientIP {
		return "", -1, fmt.Errorf("challenge IP mismatch")
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
