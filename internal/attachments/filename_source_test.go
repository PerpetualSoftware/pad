package attachments

import "testing"

// BUG-2819: the stored name cannot say where it came from, so the normaliser
// reports which branch decided it. Every case also checks that the name is
// NormalizeFilename's, byte for byte, so provenance can never change what is
// stored.
func TestNormalizeFilenameWithSource(t *testing.T) {
	cases := []struct {
		raw        string
		wantName   string
		wantSource FilenameSource
	}{
		{"report.pdf", "report.pdf", FilenameFromCaller},
		// The case the field exists for: a caller who really named a file
		// what the server would substitute.
		{"upload.bin", "upload.bin", FilenameFromCaller},
		{"upload.png", "upload.png", FilenameFromCaller},
		{"dir/report.pdf", "report.pdf", FilenameNormalised},
		{`dir\report.pdf`, "report.pdf", FilenameNormalised},
		{"report.pdf.", "report.pdf", FilenameNormalised},
		{"re\x07port.pdf", "report.pdf", FilenameNormalised},
		{"nul.txt", "_nul.txt", FilenameNormalised},
		{"", "upload.bin", FilenameSubstituted},
		{".", "upload.bin", FilenameSubstituted},
		{"..", "upload.bin", FilenameSubstituted},
		{"\xff\xfe.png", "upload.png", FilenameSubstituted},
		{"bad\x00name", "upload", FilenameSubstituted},
	}
	for _, tc := range cases {
		name, source := NormalizeFilenameWithSource(tc.raw)
		if name != tc.wantName || source != tc.wantSource {
			t.Errorf("NormalizeFilenameWithSource(%q) = (%q, %q), want (%q, %q)", tc.raw, name, source, tc.wantName, tc.wantSource)
		}
		if plain := NormalizeFilename(tc.raw); plain != name {
			t.Errorf("NormalizeFilename(%q) = %q, but WithSource returned %q", tc.raw, plain, name)
		}
	}
}

func TestValidFilenameSource(t *testing.T) {
	for _, v := range []string{"caller", "normalised", "substituted", "derived", "unknown"} {
		if !ValidFilenameSource(v) {
			t.Errorf("ValidFilenameSource(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "Caller", "server", "normalized"} {
		if ValidFilenameSource(v) {
			t.Errorf("ValidFilenameSource(%q) = true, want false", v)
		}
	}
}
