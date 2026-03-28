package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"sync"
	"time"
)

const (
	sessionTTL    = 7 * 24 * time.Hour // 7 days
	sessionCookie = "pad_session"
)

// SessionManager handles in-memory session storage with HMAC-signed cookies.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]time.Time // sessionID -> expiresAt
	secret   []byte               // 32-byte HMAC signing key
}

// NewSessionManager creates a session manager with a random signing key.
func NewSessionManager() *SessionManager {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic("failed to generate session secret: " + err.Error())
	}

	sm := &SessionManager{
		sessions: make(map[string]time.Time),
		secret:   secret,
	}

	// Background cleanup of expired sessions
	go func() {
		for {
			time.Sleep(15 * time.Minute)
			sm.cleanup()
		}
	}()

	return sm
}

// Create generates a new session and returns the signed cookie value.
func (sm *SessionManager) Create() string {
	id := make([]byte, 24)
	if _, err := rand.Read(id); err != nil {
		return ""
	}

	sessionID := base64.RawURLEncoding.EncodeToString(id)
	sig := sm.sign(sessionID)

	sm.mu.Lock()
	sm.sessions[sessionID] = time.Now().Add(sessionTTL)
	sm.mu.Unlock()

	return sessionID + "." + sig
}

// Validate checks if a cookie value contains a valid, non-expired session.
func (sm *SessionManager) Validate(cookieValue string) bool {
	parts := strings.SplitN(cookieValue, ".", 2)
	if len(parts) != 2 {
		return false
	}

	sessionID, sig := parts[0], parts[1]

	// Verify HMAC signature
	expected := sm.sign(sessionID)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return false
	}

	// Check session exists and is not expired
	sm.mu.RLock()
	expiresAt, exists := sm.sessions[sessionID]
	sm.mu.RUnlock()

	return exists && time.Now().Before(expiresAt)
}

// Destroy removes a session.
func (sm *SessionManager) Destroy(cookieValue string) {
	parts := strings.SplitN(cookieValue, ".", 2)
	if len(parts) != 2 {
		return
	}
	sm.mu.Lock()
	delete(sm.sessions, parts[0])
	sm.mu.Unlock()
}

func (sm *SessionManager) sign(data string) string {
	mac := hmac.New(sha256.New, sm.secret)
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (sm *SessionManager) cleanup() {
	now := time.Now()
	sm.mu.Lock()
	for id, expiresAt := range sm.sessions {
		if now.After(expiresAt) {
			delete(sm.sessions, id)
		}
	}
	sm.mu.Unlock()
}
