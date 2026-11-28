package store

import (
	"os"
	"testing"
	"time"
)

// BUG-3757: Postgres idempotency base_seq must preserve item sequences
// beyond the INTEGER overflow boundary at 2^31.
func TestBUG3757_PGIdempotencyBaseSeqBigint(t *testing.T) {
	t.Parallel()
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, pgURL)

	claim, err := s.ClaimIdempotencyKey("w3757", "u3757", "k3757", "patch", "h3757", time.Minute)
	if err != nil {
		t.Fatalf("claim idempotency key: %v", err)
	}
	if claim.Outcome != IdemClaimed {
		t.Fatalf("claim outcome = %q, want %q", claim.Outcome, IdemClaimed)
	}
	const baseSeq = int64(1) << 40
	if err := s.MarkIdempotencyKeyUnknown("w3757", "u3757", "k3757", claim.Token, "patch3757", "conn3757", baseSeq, "base body"); err != nil {
		t.Fatalf("mark idempotency key unknown with base seq %d: %v", baseSeq, err)
	}
	recovered, err := s.ClaimIdempotencyKey("w3757", "u3757", "k3757", "patch", "h3757", time.Minute)
	if err != nil {
		t.Fatalf("recover idempotency key: %v", err)
	}
	if recovered.Outcome != IdemUnknown {
		t.Fatalf("recovered outcome = %q, want %q", recovered.Outcome, IdemUnknown)
	}
	if recovered.BaseSeq != baseSeq {
		t.Errorf("recovered base seq = %d, want %d", recovered.BaseSeq, baseSeq)
	}

	var dataType string
	if err := s.db.QueryRow(`SELECT data_type FROM information_schema.columns WHERE table_name = 'idempotency_keys' AND column_name = 'base_seq'`).Scan(&dataType); err != nil {
		t.Fatalf("read base_seq data type: %v", err)
	}
	if dataType != "bigint" {
		t.Errorf("base_seq data type = %q, want bigint", dataType)
	}
}
