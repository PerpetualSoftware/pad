package cli

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Display tests for the execution lease (#1221): `item show` renders a
// Lease: line only when a live lease is present, and the list table marks
// leased rows with a glyph on the ref/title cell instead of a column.

// captureMetaStdout captures os.Stdout around fn — PrintItemMeta writes
// via fmt.Printf directly.
func captureMetaStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}

func leaseDisplayItem(withLease bool) *models.Item {
	item := &models.Item{
		Title:  "Leased item",
		Fields: `{"status":"open"}`,
		Tags:   "[]",
	}
	if withLease {
		item.Lease = &models.ItemLease{
			Holder:     "sweep-runner",
			AcquiredAt: time.Now().UTC().Add(-time.Minute),
			ExpiresAt:  time.Now().UTC().Add(12 * time.Minute),
		}
	}
	return item
}

func TestPrintItemMeta_LeaseLineOnlyWhenLive(t *testing.T) {
	out := captureMetaStdout(t, func() { PrintItemMeta(leaseDisplayItem(true)) })
	if !strings.Contains(out, "Lease:") {
		t.Errorf("live lease missing from meta header:\n%s", out)
	}
	if !strings.Contains(out, "sweep-runner") {
		t.Errorf("lease line must name the holder:\n%s", out)
	}
	if !strings.Contains(out, "expires in") {
		t.Errorf("lease line must carry the countdown:\n%s", out)
	}

	out = captureMetaStdout(t, func() { PrintItemMeta(leaseDisplayItem(false)) })
	if strings.Contains(out, "Lease:") {
		t.Errorf("unleased item must not render a Lease line:\n%s", out)
	}
}

func TestRenderItemTable_MarksLeasedRows(t *testing.T) {
	leased := *leaseDisplayItem(true)
	plain := *leaseDisplayItem(false)
	plain.Title = "Plain item"

	var sb strings.Builder
	renderItemTable(&sb, []models.Item{leased, plain}, 120)
	out := sb.String()

	leasedLine, plainLine := "", ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Leased item") {
			leasedLine = line
		}
		if strings.Contains(line, "Plain item") {
			plainLine = line
		}
	}
	if leasedLine == "" || plainLine == "" {
		t.Fatalf("both rows must render:\n%s", out)
	}
	if !strings.Contains(leasedLine, "»") {
		t.Errorf("leased row missing the lease glyph:\n%s", leasedLine)
	}
	if strings.Contains(plainLine, "»") {
		t.Errorf("unleased row must not carry the glyph:\n%s", plainLine)
	}
}

func TestLeaseCountdown(t *testing.T) {
	if got := LeaseCountdown(time.Now().Add(-time.Second)); got != "expired" {
		t.Errorf("past expiry = %q, want expired", got)
	}
	if got := LeaseCountdown(time.Now().Add(30 * time.Second)); !strings.HasPrefix(got, "expires in ") || !strings.HasSuffix(got, "s") {
		t.Errorf("sub-minute countdown = %q, want seconds form", got)
	}
	if got := LeaseCountdown(time.Now().Add(12 * time.Minute)); got != "expires in 12m" {
		t.Errorf("countdown = %q, want expires in 12m", got)
	}
}
