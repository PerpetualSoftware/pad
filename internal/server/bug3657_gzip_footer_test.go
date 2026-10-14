package server

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"testing"
)

// gzipMember compresses b as one complete, valid gzip member.
func gzipMember(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

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
		// Past the end of the gzip member (codex r1): raw bytes, and a second,
		// intact member carrying non-padding data after the tar terminator.
		"raw bytes after the member": append(append([]byte(nil), good...), []byte("trailing garbage")...),
		"a second member with data":  append(append([]byte(nil), good...), gzipMember(t, []byte("not tar padding"))...),
		"a second member, truncated": append(append([]byte(nil), good...), gzipMember(t, make([]byte, 512))[:20]...),
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
			if body := strings.ToLower(rr.Body.String()); !strings.Contains(body, "gzip") && !strings.Contains(body, "checksum") && !strings.Contains(body, "after its end") {
				t.Errorf("the refusal should name the archive damage, got %d %s", rr.Code, rr.Body.String())
			}
		})
	}
	// Controls: the bundle intact imports, and so does one whose tar is padded
	// out with zeros to a full 10 KiB record, as GNU tar writes it, here in a
	// second gzip member.
	dest, _ := testServerWithAttachments(t)
	if rr := postBundle(dest, "Intact", good); rr.Code != http.StatusCreated {
		t.Fatalf("control: the intact bundle did not import: %d %s", rr.Code, rr.Body.String())
	}
	padded := append(append([]byte(nil), good...), gzipMember(t, make([]byte, 10240))...)
	if rr := postBundle(dest, "Padded", padded); rr.Code != http.StatusCreated {
		t.Fatalf("control: a zero-padded bundle did not import: %d %s", rr.Code, rr.Body.String())
	}
}
