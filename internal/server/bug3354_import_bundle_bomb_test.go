package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG-3354: the bundle cap bounds the COMPRESSED body only. A small gzip
// body could decompress without limit, and entries the import discards were
// inflated in full anyway. The tar stream now has its own ceilings.

type bundleEntry struct {
	name string
	body []byte
}

func gzipTar(t *testing.T, entries []bundleEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(gzw)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body))}); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("write body %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func postBundle(srv *Server, name string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name="+name, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func exportJSONFrom(t *testing.T) []byte {
	t.Helper()
	src, slug := testServerWithAttachments(t)
	rr := doRequest(src, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	return rr.Body.Bytes()
}

func workspaceListed(t *testing.T, srv *Server, name string) bool {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list workspaces: %d", rr.Code)
	}
	return strings.Contains(rr.Body.String(), `"name":"`+name+`"`)
}

// A body well inside the compressed cap that inflates past the decompressed
// ceiling is refused with 413 before the oversize entry is inflated, and the
// workspace its pad-export.json already minted is rolled back.
func TestBUG3354_DecompressionBombRefused(t *testing.T) {
	export := exportJSONFrom(t)
	const bodyCap = 1 << 20 // decompressed ceiling: 4 MiB + 200 MiB of metadata room
	bomb := gzipTar(t, []bundleEntry{
		{"pad-export.json", export},
		{"filler.bin", make([]byte, 300<<20)}, // 300 MiB of zeros, ~300 KiB compressed
	})
	dest, _ := testServerWithAttachments(t)
	dest.SetImportBundleMaxBytes(bodyCap)
	if int64(len(bomb)) >= bodyCap || int64(300<<20) <= dest.decompressedBundleCap() {
		t.Fatalf("precondition: bomb body %d must fit the %d compressed cap and inflate past %d",
			len(bomb), bodyCap, dest.decompressedBundleCap())
	}
	rr := postBundle(dest, "BombWS", bomb)
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "bundle_too_large") {
		t.Fatalf("bomb: got %d %s, want 413 bundle_too_large", rr.Code, rr.Body.String())
	}
	if workspaceListed(t, dest, "BombWS") {
		t.Fatal("the workspace minted before the bomb entry was not rolled back")
	}

	// Control: the same bundle with a filler under the ceiling imports.
	ok := gzipTar(t, []bundleEntry{
		{"pad-export.json", export},
		{"filler.bin", make([]byte, 64<<10)},
	})
	ctl, _ := testServerWithAttachments(t)
	ctl.SetImportBundleMaxBytes(bodyCap)
	if rr := postBundle(ctl, "FitsWS", ok); rr.Code != http.StatusCreated {
		t.Fatalf("control bundle under the ceiling: got %d %s", rr.Code, rr.Body.String())
	}
}

// Many entries, each under the remaining budget, that together pass it are
// refused too: the declared-size check is per entry, so the cumulative
// count is what catches them.
func TestBUG3354_CumulativeEntriesRefused(t *testing.T) {
	export := exportJSONFrom(t)
	const bodyCap = 1 << 20 // decompressed ceiling: 4 MiB + 200 MiB of metadata room
	filler := make([]byte, 50<<20)
	entries := []bundleEntry{{"pad-export.json", export}}
	for i := 0; i < 5; i++ {
		entries = append(entries, bundleEntry{fmt.Sprintf("filler-%d.bin", i), filler})
	}
	dest, _ := testServerWithAttachments(t)
	dest.SetImportBundleMaxBytes(bodyCap)
	rr := postBundle(dest, "ManyWS", gzipTar(t, entries))
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "bundle_too_large") {
		t.Fatalf("cumulative: got %d %s, want 413 bundle_too_large", rr.Code, rr.Body.String())
	}
	if workspaceListed(t, dest, "ManyWS") {
		t.Fatal("the partial workspace was not rolled back")
	}
}

// More tar headers than importBundleMaxEntries are refused, however small.
func TestBUG3354_EntryCountRefused(t *testing.T) {
	entries := make([]bundleEntry, importBundleMaxEntries+1)
	for i := range entries {
		entries[i] = bundleEntry{name: fmt.Sprintf("e%d", i)}
	}
	srv, _ := testServerWithAttachments(t)
	rr := postBundle(srv, "CountWS", gzipTar(t, entries))
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "entries") {
		t.Fatalf("entry count: got %d %s, want 413 naming the entry cap", rr.Code, rr.Body.String())
	}
}

func TestBUG3354_BudgetReader(t *testing.T) {
	read := func(src string, limit int64) (string, error) {
		b := &bundleBudgetReader{r: strings.NewReader(src), remaining: limit}
		// Bounded rather than io.ReadAll, so a reader that stops making
		// progress fails here instead of hanging the test binary.
		var got []byte
		buf := make([]byte, 3)
		for i := 0; i < 100; i++ {
			n, err := b.Read(buf)
			got = append(got, buf[:n]...)
			if err == io.EOF {
				return string(got), nil
			}
			if err != nil {
				return string(got), err
			}
		}
		t.Fatalf("reader made no progress over %q (limit %d)", src, limit)
		return "", nil
	}
	// A stream ending exactly at the limit reads to EOF.
	if got, err := read("abcd", 4); err != nil || got != "abcd" {
		t.Fatalf("exact limit: got %q %v", got, err)
	}
	// One byte past it fails, having handed over only the budget.
	if got, err := read("abcde", 4); !errors.Is(err, errBundleTooLarge) || got != "abcd" {
		t.Fatalf("past limit: got %q %v, want errBundleTooLarge after %q", got, err, "abcd")
	}
	// Under the limit is untouched.
	if got, err := read("ab", 4); err != nil || got != "ab" {
		t.Fatalf("under limit: got %q %v", got, err)
	}
}
