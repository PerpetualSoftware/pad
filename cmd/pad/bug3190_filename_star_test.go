package main

import "testing"

// BUG-3190: the server sends a non-ASCII (or otherwise unquotable) name as an
// ASCII fallback in filename plus the exact name in filename*. This parser
// must return the exact name. These are the headers the server emits for
// those names, byte for byte (internal/server content_disposition_test.go
// pins the same form).
func TestBUG3190_parseAttachmentFilename_PrefersFilenameStar(t *testing.T) {
	for header, want := range map[string]string{
		`inline; filename="a_b.txt"; filename*=UTF-8''a%C2%A0b.txt`:              "a b.txt",
		`attachment; filename="__.pdf"; filename*=UTF-8''%E4%BC%9A%E8%AD%B0.pdf`: "会議.pdf",
		`inline; filename="plain.txt"`:                                           "plain.txt",
	} {
		if got := parseAttachmentFilename(header); got != want {
			t.Errorf("%s: got %q, want %q", header, got, want)
		}
	}
}
