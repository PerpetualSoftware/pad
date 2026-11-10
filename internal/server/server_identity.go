package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"sync"

	"github.com/PerpetualSoftware/pad/internal/serveridentity"
)

type serverIdentityState struct {
	once sync.Once
	key  ed25519.PrivateKey
	err  error
}

func (s *Server) serverIdentityKey() (ed25519.PrivateKey, error) {
	s.serverIdentityState.once.Do(func() {
		seed, err := s.store.DeriveServerKey(serveridentity.KeyLabel)
		if err != nil {
			s.serverIdentityState.err = err
			return
		}
		s.serverIdentityState.key = ed25519.NewKeyFromSeed(seed)
	})
	return s.serverIdentityState.key, s.serverIdentityState.err
}

func (s *Server) handleServerIdentity(w http.ResponseWriter, r *http.Request) {
	if s.rateLimiters != nil && !s.rateLimiters.ServerIdentity.allow(rateLimitAddr(clientIP(r))) {
		w.Header().Set("Content-Type", "application/json")
		writeRateLimitResponse(w, s.rateLimiters.ServerIdentity.config)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	values := r.URL.Query()["challenge"]
	var challenge []byte
	if len(values) == 1 && len(values[0]) == 43 {
		value := values[0]
		valid := true
		for i := range value {
			c := value[i]
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				valid = false
				break
			}
		}
		if valid {
			decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
			if err == nil {
				challenge = decoded
			}
		}
	}
	if len(challenge) != 32 {
		writeError(w, http.StatusBadRequest, "invalid_challenge", "Invalid challenge")
		return
	}
	key, err := s.serverIdentityKey()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_identity_unavailable", "Server identity is unavailable")
		return
	}
	pub := key.Public().(ed25519.PublicKey)
	signature := ed25519.Sign(key, serveridentity.Message(pub, challenge))
	writeJSON(w, http.StatusOK, struct {
		Version   int    `json:"version"`
		Algorithm string `json:"algorithm"`
		PublicKey string `json:"public_key"`
		Signature string `json:"signature"`
	}{1, "ed25519", base64.RawURLEncoding.EncodeToString(pub), base64.RawURLEncoding.EncodeToString(signature)})
}
