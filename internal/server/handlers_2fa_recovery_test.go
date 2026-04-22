package server

import (
	"regexp"
	"testing"
)

func TestNormalizeRecoveryCode(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"ABCDEFGHIJKLMNOP", "ABCDEFGHIJKLMNOP"},   // already normalized
		{"abcdefghijklmnop", "ABCDEFGHIJKLMNOP"},   // lowercase → upper
		{"ABCD-EFGH-IJKL-MNOP", "ABCDEFGHIJKLMNOP"}, // dashes stripped
		{" abcd efgh ijkl mnop ", "ABCDEFGHIJKLMNOP"}, // whitespace stripped
		{"ABcd-EFgh\nIJkl", "ABCDEFGHIJKL"},          // mixed + newline
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeRecoveryCode(tt.in); got != tt.want {
			t.Errorf("normalizeRecoveryCode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestGenerateRecoveryCodes_EntropyShape asserts that recovery codes are
// the 16-char base32 form expected by TASK-658 (10 bytes / 80 bits of
// entropy, unpadded). The previous 8-char hex form carried only 32 bits
// — below what NIST SP 800-63B considers acceptable for backup codes.
func TestGenerateRecoveryCodes_EntropyShape(t *testing.T) {
	codes, err := generateRecoveryCodes(8)
	if err != nil {
		t.Fatalf("generateRecoveryCodes: %v", err)
	}
	if len(codes) != 8 {
		t.Fatalf("expected 8 codes, got %d", len(codes))
	}

	// 10 bytes → 16 chars of unpadded base32.
	base32Re := regexp.MustCompile(`^[A-Z2-7]{16}$`)
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if !base32Re.MatchString(c) {
			t.Errorf("code %q is not 16-char base32 (TASK-658 entropy upgrade)", c)
		}
		if seen[c] {
			t.Errorf("duplicate code %q across batch — entropy source is broken", c)
		}
		seen[c] = true
	}
}
