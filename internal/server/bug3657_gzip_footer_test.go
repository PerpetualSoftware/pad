package server

import (
	"net/http"
	"strings"
	"testing"
)

// BUG-3657: the bundle import stopped at the tar terminator and answered 201
// without reading the rest of the gzip stream, so a truncated or corrupt
// gzip footer (the CRC-32 and length that verify the whole archive) was never
// checked. A tar EOF is not success until the gzip stream ends cleanly.
func TestBUG3657_CorruptGzipFooterIsRefused(t *testing.T) {
	good := packInterchangeBundle(t, readInterchangeGolden(t))
	cases := map[string][]byte{
		"footer truncated": good[:len(good)-8],
		"half the footer":  good[:len(good)-4],
		"CRC-32 flipped": func() []byte {
			b := append([]byte(nil), good...)
			b[len(b)-8] ^= 0xff
			return b
		}(),
		"length flipped": func() []byte {
			b := append([]byte(nil), good...)
			b[len(b)-1] ^= 0xff
			return b
		}(),
	}
	for name, bundle := range cases {
		t.Run(name, func(t *testing.T) {
			dest, _ := testServerWithAttachments(t)
			rr := postBundle(dest, "Footer", bundle)
			if rr.Code == http.StatusCreated {
				t.Fatalf("a bundle with a bad gzip footer imported: %d", rr.Code)
			}
			if workspaceListed(t, dest, "Footer") {
				t.Errorf("the workspace from a corrupt bundle is still listed: %s", rr.Body.String())
			}
			if !strings.Contains(strings.ToLower(rr.Body.String()), "gzip") && !strings.Contains(rr.Body.String(), "checksum") {
				t.Errorf("the refusal should name the archive damage, got %d %s", rr.Code, rr.Body.String())
			}
		})
	}
	// Control: the same bundle intact imports.
	dest, _ := testServerWithAttachments(t)
	if rr := postBundle(dest, "Intact", good); rr.Code != http.StatusCreated {
		t.Fatalf("control: the intact bundle did not import: %d %s", rr.Code, rr.Body.String())
	}
}
