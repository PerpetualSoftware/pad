// Package email provides transactional email sending via Maileroo.
// When no API key is configured, the server runs without email —
// invitations fall back to CLI-based join codes.
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Sender sends transactional emails via the Maileroo API.
// The from address and name are defaults; individual send methods can
// override them for contextual sender names (e.g. "Dave via Pad").
type Sender struct {
	mu       sync.RWMutex
	apiKey   string
	fromAddr string
	fromName string
	baseURL  string // Pad's public base URL for generating links
	endpoint string // Maileroo API endpoint (overridable for tests)
	client   *http.Client
}

// defaultEndpoint is the Maileroo v2 email sending API.
const defaultEndpoint = "https://smtp.maileroo.com/api/v2/emails"

// NewSender creates a new email sender.
func NewSender(apiKey, fromAddr, fromName, baseURL string) *Sender {
	return &Sender{
		apiKey:   apiKey,
		fromAddr: fromAddr,
		fromName: fromName,
		baseURL:  baseURL,
		endpoint: defaultEndpoint,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// Configure updates the sender's settings at runtime (e.g. when an admin
// changes platform settings). Thread-safe.
func (s *Sender) Configure(apiKey, fromAddr, fromName, baseURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if apiKey != "" {
		s.apiKey = apiKey
	}
	if fromAddr != "" {
		s.fromAddr = fromAddr
	}
	if fromName != "" {
		s.fromName = fromName
	}
	if baseURL != "" {
		s.baseURL = baseURL
	}
}

// SetEndpoint overrides the Maileroo API endpoint. Intended for tests that
// stand up an httptest server mimicking Maileroo's v2 API — production
// callers should leave the default in place. Thread-safe.
func (s *Sender) SetEndpoint(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoint = url
}

// BaseURL returns the configured base URL.
func (s *Sender) BaseURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.baseURL
}

// emailAddress is the Maileroo address format.
type emailAddress struct {
	Address     string `json:"address"`
	DisplayName string `json:"display_name,omitempty"`
}

// mailerooPayload is the request body for the Maileroo v2 API.
type mailerooPayload struct {
	From    emailAddress   `json:"from"`
	To      []emailAddress `json:"to"`
	Subject string         `json:"subject"`
	HTML    string         `json:"html,omitempty"`
	Plain   string         `json:"plain,omitempty"`
}

// mailerooResponse is the envelope returned by Maileroo.
type mailerooResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// Send sends an email using the default from address/name.
func (s *Sender) Send(ctx context.Context, to, toName, subject, html, plain string) error {
	s.mu.RLock()
	fromAddr := s.fromAddr
	fromName := s.fromName
	endpoint := s.endpoint
	s.mu.RUnlock()
	return s.sendWith(ctx, endpoint, fromAddr, fromName, to, toName, subject, html, plain)
}

// SendAs sends an email with a custom from name (address stays the same
// since email providers require verified sender domains).
func (s *Sender) SendAs(ctx context.Context, fromName, to, toName, subject, html, plain string) error {
	s.mu.RLock()
	fromAddr := s.fromAddr
	endpoint := s.endpoint
	s.mu.RUnlock()
	return s.sendWith(ctx, endpoint, fromAddr, fromName, to, toName, subject, html, plain)
}

// sendWith is the internal send implementation.
func (s *Sender) sendWith(ctx context.Context, endpoint, fromAddr, fromName, to, toName, subject, html, plain string) error {
	s.mu.RLock()
	apiKey := s.apiKey
	s.mu.RUnlock()

	payload := mailerooPayload{
		From: emailAddress{
			Address:     fromAddr,
			DisplayName: fromName,
		},
		To: []emailAddress{
			{Address: to, DisplayName: toName},
		},
		Subject: subject,
		HTML:    html,
		Plain:   plain,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal email payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("maileroo returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result mailerooResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode maileroo response: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("maileroo error: %s", result.Message)
	}

	return nil
}
