package server

import (
	"reflect"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// refillWindow is the time a bucket needs to go from empty back to full.
func refillWindow(burst int, r rate.Limit) time.Duration {
	return time.Duration(float64(burst) / float64(r) * float64(time.Second))
}

// TestRateLimiters_RetentionCoversRefill (BUG-3638): every limiter the
// server builds must keep an idle bucket at least as long as it takes to
// refill. Otherwise cleanup evicts a partly drained bucket and the owner
// comes back with a full one. Fields are found by reflection so a limiter
// added later is checked without being listed here.
func TestRateLimiters_RetentionCoversRefill(t *testing.T) {
	rls := NewRateLimiters()
	defer rls.Stop()

	v := reflect.ValueOf(rls).Elem()
	checked := 0
	for i := 0; i < v.NumField(); i++ {
		field := v.Type().Field(i)
		if field.Type != reflect.TypeOf((*ipRateLimiter)(nil)) {
			continue
		}
		rl := v.Field(i).Interface().(*ipRateLimiter)
		if rl == nil {
			t.Errorf("%s: limiter is nil", field.Name)
			continue
		}
		checked++
		want := refillWindow(rl.config.Burst, rl.config.Rate)
		if rl.retention < want {
			t.Errorf("%s: retention %v < refill window %v (burst %d, rate %v/s)",
				field.Name, rl.retention, want, rl.config.Burst, float64(rl.config.Rate))
		}
	}
	if checked == 0 {
		t.Fatal("no limiters found on RateLimiters; reflection walk is broken")
	}
}

// TestNewIPRateLimiter_RaisesShortRetention (BUG-3638): a retention shorter
// than the refill window is raised to it, whether set explicitly or left to
// the default.
func TestNewIPRateLimiter_RaisesShortRetention(t *testing.T) {
	cases := []struct {
		name string
		cfg  rateLimitConfig
		want time.Duration
	}{
		{
			name: "explicit retention shorter than refill",
			cfg: rateLimitConfig{
				Rate:      rate.Limit(10.0 / 3600.0),
				Burst:     10,
				Retention: time.Minute,
			},
			want: time.Hour,
		},
		{
			name: "default retention shorter than refill",
			cfg: rateLimitConfig{
				Rate:  rate.Limit(3.0 / 3600.0),
				Burst: 3,
			},
			want: time.Hour,
		},
		{
			name: "retention already longer than refill is kept",
			cfg: rateLimitConfig{
				Rate:      rate.Limit(3.0 / 3600.0),
				Burst:     3,
				Retention: 2 * time.Hour,
			},
			want: 2 * time.Hour,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rl := newIPRateLimiter(tc.cfg)
			defer rl.Stop()
			// burst / rate is float arithmetic, so allow a millisecond of
			// rounding around the exact window.
			if d := rl.retention - tc.want; d < -time.Millisecond || d > time.Millisecond {
				t.Errorf("retention = %v, want %v", rl.retention, tc.want)
			}
		})
	}
}
