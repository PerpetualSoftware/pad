package email

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
)

// UnsubscribeURL generates an HMAC-signed unsubscribe link for the given email.
// The secret should be a stable server-side key (e.g. derived from the API key).
func UnsubscribeURL(baseURL, emailAddr, secret string) string {
	token := signEmail(emailAddr, secret)
	return fmt.Sprintf("%s/api/v1/unsubscribe?email=%s&token=%s",
		baseURL,
		url.QueryEscape(emailAddr),
		url.QueryEscape(token),
	)
}

// ValidateUnsubscribeToken checks that the token is a valid HMAC for the email.
func ValidateUnsubscribeToken(emailAddr, token, secret string) bool {
	expected := signEmail(emailAddr, secret)
	return hmac.Equal([]byte(expected), []byte(token))
}

func signEmail(emailAddr, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("unsubscribe:" + emailAddr))
	return hex.EncodeToString(mac.Sum(nil))
}
