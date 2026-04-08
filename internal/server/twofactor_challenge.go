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

// generateTwoFAChallenge creates a short-lived, HMAC-signed challenge token
// that proves the user already passed password verification. The token is
// bound to the user ID and client IP, and expires after 5 minutes.
func generateTwoFAChallenge(userID, clientIP string, secret []byte) string {
	expires := time.Now().UTC().Add(twoFAChallengeExpiry).Unix()
	payload := fmt.Sprintf("%s|%s|%d", userID, clientIP, expires)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return encoded + "." + sig
}

// validateTwoFAChallenge verifies a challenge token's signature, expiry, and
// IP binding. Returns the user ID if valid, or an error describing the failure.
func validateTwoFAChallenge(token, clientIP string, secret []byte) (string, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("malformed challenge token")
	}

	encoded, sig := parts[0], parts[1]

	// Verify HMAC
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encoded))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return "", fmt.Errorf("invalid challenge signature")
	}

	// Decode payload
	payloadBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("invalid challenge encoding")
	}
	payload := string(payloadBytes)

	fields := strings.SplitN(payload, "|", 3)
	if len(fields) != 3 {
		return "", fmt.Errorf("invalid challenge payload")
	}

	userID := fields[0]
	tokenIP := fields[1]
	expiryStr := fields[2]

	// Check expiry
	expiryUnix, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid challenge expiry")
	}
	if time.Now().UTC().Unix() > expiryUnix {
		return "", fmt.Errorf("challenge token expired")
	}

	// Check IP binding
	if tokenIP != clientIP {
		return "", fmt.Errorf("challenge IP mismatch")
	}

	return userID, nil
}
