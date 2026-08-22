package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
)

// TestRedisUpIsAbsentUntilRegistered pins codex round 1's P2: a
// deployment with no Redis must export NO pad_redis_up series, because a
// permanent 0 reads as "Redis is down" to anything scraping it and would
// have every single-process binary alerting on a dependency it does not
// have.
//
// Both directions asserted. The absence assertion alone would pass
// against a build that never registered the gauge at all, which would be
// a different bug with the same fingerprint.
func TestRedisUpIsAbsentUntilRegistered(t *testing.T) {
	t.Parallel()

	m := New()

	// PREMISE: the registry gathers SOMETHING, so "not found" below means
	// the series is absent rather than the gather being empty.
	if names := gatheredNames(t, m); len(names) == 0 {
		t.Fatal("premise failed: a fresh registry gathered no metrics at all")
	}
	if hasMetric(t, m, "pad_redis_up") {
		t.Fatal("pad_redis_up is exported before RegisterRedisUp — a Redis-less deployment would report a permanent 0")
	}

	// Writing to the unregistered gauge must stay harmless — the health
	// prober's callback runs regardless of registration.
	m.RedisUp.Set(1)
	if hasMetric(t, m, "pad_redis_up") {
		t.Fatal("writing to the unregistered gauge exported it")
	}

	m.RegisterRedisUp()
	if !hasMetric(t, m, "pad_redis_up") {
		t.Fatal("pad_redis_up is still absent after RegisterRedisUp")
	}
}

func gatheredNames(t *testing.T, m *Metrics) []string {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make([]string, 0, len(families))
	for _, f := range families {
		names = append(names, f.GetName())
	}
	return names
}

func hasMetric(t *testing.T, m *Metrics, name string) bool {
	t.Helper()
	for _, n := range gatheredNames(t, m) {
		if n == name {
			return true
		}
	}
	return false
}

// TestRedisMetricsAreExportedByName guards the names themselves: they are
// the operator-facing contract documented in docs/deployment.md, and a
// rename would silently break every alert built on them.
func TestRedisMetricsAreExportedByName(t *testing.T) {
	t.Parallel()

	m := New()
	m.WatchNotificationsDroppedTotal.WithLabelValues("slow_subscriber").Inc()
	m.WatchSequenceGapsTotal.Inc()
	m.WatchNotificationsMissedTotal.Add(2)
	m.WatchSequenceResetsTotal.WithLabelValues("epoch_change").Inc()
	m.WatchReceiveLoopExitsTotal.Inc()
	m.SessionPresenceFailuresTotal.WithLabelValues("list").Inc()

	for _, want := range []string{
		"pad_watchevents_notifications_dropped_total",
		"pad_watchevents_sequence_gaps_total",
		"pad_watchevents_notifications_missed_total",
		"pad_watchevents_sequence_resets_total",
		"pad_watchevents_receive_loop_exits_total",
		"pad_session_presence_failures_total",
	} {
		if !hasMetric(t, m, want) {
			t.Errorf("%s is not exported; docs/deployment.md tells operators to alert on it", want)
		}
	}

	// And the values actually land, so the names are not merely declared.
	if body := gatherText(t, m); !strings.Contains(body, `pad_watchevents_notifications_missed_total 2`) {
		t.Errorf("missed-notifications counter did not carry its value:\n%s", body)
	}
}

func gatherText(t *testing.T, m *Metrics) string {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sb strings.Builder
	enc := expfmt.NewEncoder(&sb, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, f := range families {
		if err := enc.Encode(f); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	return sb.String()
}
