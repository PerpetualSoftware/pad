package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TASK-3577: whether Pad Pro can be bought, and at what price, comes from the
// pad-cloud sidecar (GET /billing/config), which holds the Stripe key and
// resolves the lookup keys checkout sells. It replaces PAD_BILLING_AVAILABLE,
// a manual flag that drifted (TASK-3572), and the web's hard-coded $8/$80,
// which would have shown one price and charged another after a Stripe change.

// Billing modes (PAD_BILLING).
const (
	// ModeAuto: billing is offered exactly when the sidecar says it is.
	ModeAuto = "auto"
	// ModeOff: never offered ("Pro — coming soon"), whatever the sidecar says.
	// For the walkthrough window with live keys.
	ModeOff = "off"
)

// ResolveMode picks the billing mode from PAD_BILLING and, for one release,
// its predecessor PAD_BILLING_AVAILABLE (lead ruling on TASK-3577):
//
//   - PAD_BILLING set: it decides. "auto" or "off" (any case, trimmed); any
//     other value is off, with a warning, since an unreadable switch must not
//     turn billing on.
//   - Otherwise PAD_BILLING_AVAILABLE, when present: "true" or "1" is auto,
//     anything else off, which is what an old deploy meant by it.
//   - Both unset: auto. Safe, because auto still needs the sidecar to answer
//     available: a deploy with no sidecar fails closed.
func ResolveMode(padBilling string, padBillingSet bool, legacy string, legacySet bool) (mode string, warning string) {
	if padBillingSet {
		switch v := strings.ToLower(strings.TrimSpace(padBilling)); v {
		case ModeAuto, ModeOff:
			return v, ""
		default:
			return ModeOff, fmt.Sprintf("PAD_BILLING=%q is not auto or off; billing stays off", padBilling)
		}
	}
	if legacySet {
		if v := strings.TrimSpace(legacy); v == "true" || v == "1" {
			return ModeAuto, "PAD_BILLING_AVAILABLE is deprecated; set PAD_BILLING=auto instead"
		}
		return ModeOff, "PAD_BILLING_AVAILABLE is deprecated; set PAD_BILLING=off instead"
	}
	return ModeAuto, ""
}

// OfferInterval is one interval as checkout sells it. UnitAmount is in the
// currency's minor unit (cents for usd), as Stripe reports it.
type OfferInterval struct {
	Interval   string `json:"interval"`
	LookupKey  string `json:"lookup_key"`
	UnitAmount int64  `json:"unit_amount"`
	Currency   string `json:"currency"`
}

// BillingConfig is pad-cloud's GET /billing/config response.
type BillingConfig struct {
	Available bool            `json:"available"`
	Intervals []OfferInterval `json:"intervals"`
	TrialDays int             `json:"trial_days"`
}

// configTimeout bounds one GET /billing/config. Short: it runs in the
// background, and a sidecar that cannot answer in this long is unavailable.
const configTimeout = 5 * time.Second

// GetBillingConfig asks pad-cloud whether Pad Pro can be bought, and at what
// price. Auth is the X-Cloud-Secret header, as for GetBillingMetrics.
//
// Request: GET {baseURL}/billing/config
// 200 OK:  BillingConfig JSON
//
// Any non-200 is a *SidecarError; a transport failure is a bare error.
func (c *CloudClient) GetBillingConfig(ctx context.Context) (*BillingConfig, error) {
	if c == nil {
		return nil, errors.New("billing: GetBillingConfig called on nil CloudClient")
	}
	if c.baseURL == "" || c.cloudSecret == "" {
		return nil, errors.New("billing: CloudClient is not configured (missing baseURL or cloudSecret)")
	}
	ctx, cancel := context.WithTimeout(ctx, configTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/billing/config", nil)
	if err != nil {
		return nil, fmt.Errorf("billing: build billing-config request: %w", err)
	}
	req.Header.Set("X-Cloud-Secret", c.cloudSecret)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("billing: billing-config request failed: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if resp.StatusCode != http.StatusOK {
		return nil, &SidecarError{Status: resp.StatusCode, Body: string(body)}
	}
	if readErr != nil {
		return nil, fmt.Errorf("billing: read billing-config response: %w", readErr)
	}
	var out BillingConfig
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("billing: decode billing-config response: %w", err)
	}
	return &out, nil
}

// ConfigFetcher is the one call OfferCache needs; *CloudClient satisfies it.
type ConfigFetcher interface {
	GetBillingConfig(ctx context.Context) (*BillingConfig, error)
}

// Refresh cadence: a served offer is re-read every offerRefresh, so a price
// changed in Stripe reaches the web within about that long. After a failure
// it is retried sooner, so a sidecar that comes back is picked up quickly.
const (
	offerRefresh = 5 * time.Minute
	offerRetry   = 30 * time.Second
)

// OfferCache holds the last billing config the sidecar served. It FAILS
// CLOSED: until a fetch succeeds, and after any fetch fails or answers
// something unusable, Current reports unavailable.
type OfferCache struct {
	fetch ConfigFetcher

	mu  sync.RWMutex
	cur BillingConfig
}

// NewOfferCache returns a cache that reports unavailable until its first
// successful Refresh.
func NewOfferCache(f ConfigFetcher) *OfferCache {
	return &OfferCache{fetch: f}
}

// Current is the offer to show now: Available with at least one interval, or
// unavailable. The returned value is a copy.
func (c *OfferCache) Current() BillingConfig {
	if c == nil {
		return BillingConfig{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := c.cur
	out.Intervals = append([]OfferInterval(nil), c.cur.Intervals...)
	return out
}

// Refresh fetches once and returns how long to wait before the next fetch.
func (c *OfferCache) Refresh(ctx context.Context) time.Duration {
	cfg, err := c.fetch.GetBillingConfig(ctx)
	next := sanitize(cfg)
	c.mu.Lock()
	was := c.cur.Available
	c.cur = next
	c.mu.Unlock()
	if err != nil {
		slog.Warn("billing: pad-cloud billing config unavailable; billing is not offered until it answers", "error", err)
		return offerRetry
	}
	if was != next.Available {
		slog.Info("billing: offer availability changed", "available", next.Available, "intervals", len(next.Intervals))
	}
	if !next.Available {
		return offerRetry
	}
	return offerRefresh
}

// Run refreshes now and then on the cadence above, until ctx ends.
func (c *OfferCache) Run(ctx context.Context) {
	for {
		wait := c.Refresh(ctx)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// knownIntervals are the intervals checkout sells.
var knownIntervals = map[string]bool{"monthly": true, "annual": true}

// sanitize keeps only what can be shown honestly: known intervals, each once,
// with a positive amount and a currency. Available is true only with at
// least one of them left, so a sidecar that says available with nothing
// sellable is read as unavailable.
func sanitize(cfg *BillingConfig) BillingConfig {
	if cfg == nil || !cfg.Available {
		return BillingConfig{}
	}
	out := BillingConfig{TrialDays: cfg.TrialDays}
	if out.TrialDays < 0 {
		out.TrialDays = 0
	}
	seen := map[string]bool{}
	for _, iv := range cfg.Intervals {
		if !knownIntervals[iv.Interval] || seen[iv.Interval] || iv.UnitAmount <= 0 || strings.TrimSpace(iv.Currency) == "" {
			continue
		}
		seen[iv.Interval] = true
		iv.Currency = strings.ToLower(strings.TrimSpace(iv.Currency))
		out.Intervals = append(out.Intervals, iv)
	}
	out.Available = len(out.Intervals) > 0
	if !out.Available {
		return BillingConfig{}
	}
	return out
}
