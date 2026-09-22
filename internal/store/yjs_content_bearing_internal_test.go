package store

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// BUG-3124: the envelope classifier. Every NON-CONTENT verdict must be exact,
// because content_state is trusted by the migration gate (which drops the
// op-log) and by `pad item edit`'s refusal. So each non-content case has a
// near-miss twin that must stay content-bearing.
func TestYjsFrameEnvelopeClassifier(t *testing.T) {
	mustHex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	longStep1 := append([]byte{0x00, 0x00, 0x82, 0x01}, bytes.Repeat([]byte{0x01}, 130)...)
	cases := []struct {
		name       string
		frame      []byte
		nonContent bool
	}{
		// Real frames from PLAN-3114's op-log (rows 131711 and 131713).
		{"SyncStep1, empty state vector (PLAN-3114 131711)", mustHex("00000100"), true},
		{"SyncStep1, one-client state vector (PLAN-3114 131713)", mustHex("00000601E4A653C21F"), true},
		{"SyncStep1 with a multi-byte length prefix", longStep1, true},
		{"empty update", mustHex("0002020000"), true},
		{"empty SyncStep2", mustHex("0001020000"), true},

		{"non-empty update", mustHex("000203010203"), false},
		{"update of length 2 that is not the empty update", mustHex("0002020100"), false},
		{"SyncStep1 with trailing bytes after its payload", mustHex("00000100FF"), false},
		{"SyncStep1 whose length prefix overruns the frame", mustHex("0000050100"), false},
		{"multi-byte-length SyncStep1 one byte short", longStep1[:len(longStep1)-1], false},
		{"unknown sync subtype", mustHex("00030100"), false},
		{"awareness frame", mustHex("0101020304"), false},
		{"truncated after the message type", mustHex("00"), false},
		{"empty frame", []byte{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := yjsFrameIsEnvelopeNonContent(tc.frame); got != tc.nonContent {
				t.Fatalf("non-content = %v, want %v for % x", got, tc.nonContent, tc.frame)
			}
		})
	}
}
