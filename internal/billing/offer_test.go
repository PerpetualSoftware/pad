package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TASK-3577: the billing mode and the sidecar's offer.

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name        string
		pad         string
		padSet      bool
		legacy      string
		legacySet   bool
		want        string
		wantWarning bool
	}{
		{"both unset: auto (lead ruling)", "", false, "", false, ModeAuto, false},
		{"PAD_BILLING=auto", "auto", true, "", false, ModeAuto, false},
		{"PAD_BILLING=off", "off", true, "", false, ModeOff, false},
		{"PAD_BILLING trimmed, any case", " Off ", true, "", false, ModeOff, false},
		{"PAD_BILLING wins over the alias", "off", true, "true", true, ModeOff, false},
		{"PAD_BILLING unreadable: off", "yes", true, "", false, ModeOff, true},
		{"PAD_BILLING empty but set: off", "", true, "", false, ModeOff, true},
		{"alias true: auto", "", false, "true", true, ModeAuto, true},
		{"alias 1: auto", "", false, "1", true, ModeAuto, true},
		{"alias false: off", "", false, "false", true, ModeOff, true},
		{"alias empty but set: off", "", false, "", true, ModeOff, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warning := ResolveMode(tc.pad, tc.padSet, tc.legacy, tc.legacySet)
			if got != tc.want || (warning != "") != tc.wantWarning {
				t.Fatalf("ResolveMode = %q, warning %q; want %q, warning %v", got, warning, tc.want, tc.wantWarning)
			}
		})
	}
}

type fetchFunc func(ctx context.Context) (*BillingConfig, error)

func (f fetchFunc) GetBillingConfig(ctx context.Context) (*BillingConfig, error) { return f(ctx) }

var served = &BillingConfig{
	Available: true,
	Intervals: []OfferInterval{
		{Interval: "monthly", LookupKey: "pad_pro_monthly", UnitAmount: 800, Currency: "usd"},
		{Interval: "annual", LookupKey: "pad_pro_annual", UnitAmount: 8000, Currency: "usd"},
	},
	TrialDays: 14,
}

func TestOfferCache_UnavailableUntilTheSidecarAnswers(t *testing.T) {
	c := NewOfferCache(fetchFunc(func(context.Context) (*BillingConfig, error) { return served, nil }))
	if c.Current().Available {
		t.Fatal("available before any fetch")
	}
	if wait := c.Refresh(context.Background()); wait != offerRefresh {
		t.Fatalf("next refresh in %v, want %v", wait, offerRefresh)
	}
	got := c.Current()
	if !got.Available || len(got.Intervals) != 2 || got.Intervals[0].UnitAmount != 800 || got.TrialDays != 14 {
		t.Fatalf("Current = %+v", got)
	}
}

// Fail closed: an error after a good answer takes the offer away, it does
// not keep showing a price nothing can currently sell.
func TestOfferCache_AFailedRefreshFailsClosed(t *testing.T) {
	fail := false
	c := NewOfferCache(fetchFunc(func(context.Context) (*BillingConfig, error) {
		if fail {
			return nil, errors.New("sidecar down")
		}
		return served, nil
	}))
	c.Refresh(context.Background())
	fail = true
	if wait := c.Refresh(context.Background()); wait != offerRetry {
		t.Fatalf("after a failure, next refresh in %v, want %v", wait, offerRetry)
	}
	if c.Current().Available {
		t.Fatal("still available after a failed refresh")
	}
}

func TestOfferCache_ServesTheSidecarsPrice(t *testing.T) {
	price := int64(800)
	c := NewOfferCache(fetchFunc(func(context.Context) (*BillingConfig, error) {
		return &BillingConfig{Available: true, Intervals: []OfferInterval{{Interval: "monthly", LookupKey: "k", UnitAmount: price, Currency: "USD"}}}, nil
	}))
	c.Refresh(context.Background())
	price = 1200
	c.Refresh(context.Background())
	got := c.Current()
	if got.Intervals[0].UnitAmount != 1200 || got.Intervals[0].Currency != "usd" {
		t.Fatalf("Current = %+v, want the new price, currency lowercased", got)
	}
}

func TestOfferCache_UnusableAnswersReadAsUnavailable(t *testing.T) {
	cases := map[string]*BillingConfig{
		"unavailable":            {Available: false, Intervals: served.Intervals},
		"available, no interval": {Available: true},
		"unknown interval":       {Available: true, Intervals: []OfferInterval{{Interval: "weekly", UnitAmount: 100, Currency: "usd"}}},
		"zero amount":            {Available: true, Intervals: []OfferInterval{{Interval: "monthly", UnitAmount: 0, Currency: "usd"}}},
		"no currency":            {Available: true, Intervals: []OfferInterval{{Interval: "monthly", UnitAmount: 800}}},
		"nil":                    nil,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewOfferCache(fetchFunc(func(context.Context) (*BillingConfig, error) { return cfg, nil }))
			if wait := c.Refresh(context.Background()); wait != offerRetry {
				t.Fatalf("next refresh in %v, want the retry %v", wait, offerRetry)
			}
			if got := c.Current(); got.Available || len(got.Intervals) != 0 {
				t.Fatalf("Current = %+v, want unavailable", got)
			}
		})
	}
}

func TestOfferCache_DropsDuplicateAndBadIntervalsButKeepsGoodOnes(t *testing.T) {
	c := NewOfferCache(fetchFunc(func(context.Context) (*BillingConfig, error) {
		return &BillingConfig{Available: true, TrialDays: -3, Intervals: []OfferInterval{
			{Interval: "monthly", UnitAmount: 800, Currency: "usd"},
			{Interval: "monthly", UnitAmount: 1, Currency: "usd"},
			{Interval: "annual", UnitAmount: -1, Currency: "usd"},
		}}, nil
	}))
	c.Refresh(context.Background())
	got := c.Current()
	if !got.Available || len(got.Intervals) != 1 || got.Intervals[0].UnitAmount != 800 || got.TrialDays != 0 {
		t.Fatalf("Current = %+v", got)
	}
}

func TestGetBillingConfig_SendsTheSecretAndFailsOnNon200(t *testing.T) {
	var gotSecret, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret, gotPath = r.Header.Get("X-Cloud-Secret"), r.URL.Path
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := NewCloudClient(srv.URL, "s3cret").GetBillingConfig(context.Background())
	var se *SidecarError
	if !errors.As(err, &se) || se.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a 503 SidecarError", err)
	}
	if gotSecret != "s3cret" || gotPath != "/billing/config" {
		t.Fatalf("request: secret %q path %q", gotSecret, gotPath)
	}
}

// The sidecar contract: GET /billing/config as pad-cloud's real handler
// shapes it (generated there by TestSidecarContract_BillingConfig and copied
// here), decoded by the real client.
func TestSidecarContract_BillingConfig(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "sidecar_contract", "billing_config.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c := NewOfferCache(NewCloudClient(srv.URL, "secret"))
	c.Refresh(context.Background())
	got := c.Current()
	if !got.Available || len(got.Intervals) != 2 || got.TrialDays != 14 {
		t.Fatalf("Current = %+v", got)
	}
	m := got.Intervals[0]
	if m.Interval != "monthly" || m.LookupKey != "pad_pro_monthly" || m.UnitAmount != 800 || m.Currency != "usd" {
		t.Fatalf("monthly = %+v", m)
	}
	if a := got.Intervals[1]; a.Interval != "annual" || a.UnitAmount != 8000 {
		t.Fatalf("annual = %+v", a)
	}
}
