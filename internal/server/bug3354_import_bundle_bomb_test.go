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
	t.Parallel()
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
	// The refusal tells an operator which setting raises the limit.
	if !strings.Contains(rr.Body.String(), "PAD_IMPORT_BUNDLE_MAX_BYTES") ||
		!strings.Contains(rr.Body.String(), fmt.Sprint(dest.decompressedBundleCap())) {
		t.Fatalf("the 413 must state the effective limit and the setting that raises it: %s", rr.Body.String())
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
	t.Parallel()
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
	t.Parallel()
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

// rawTarEntry writes one ustar entry with an arbitrary typeflag (tar.Writer
// refuses to emit raw extension headers itself), fixing up the checksum.
func rawTarEntry(t *testing.T, name string, flag byte, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	raw := out.Bytes()
	raw[156] = flag
	for i := 148; i < 156; i++ {
		raw[i] = ' '
	}
	sum := 0
	for _, b := range raw[:512] {
		sum += int(b)
	}
	copy(raw[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return raw
}

func paxRecord(key, value string) string {
	payload := key + "=" + value + "\n"
	n := len(payload) + 2
	for {
		record := fmt.Sprintf("%d %s", n, payload)
		if len(record) == n {
			return record
		}
		n = len(record)
	}
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Codex r1 P1: a GNU sparse entry's holes are synthesized by the tar reader
// above the counted stream, so a few KiB of physical bytes declared hundreds
// of MiB. Each entry's declared (logical) size is charged against its own
// budget.
func TestBUG3354_SparseEntriesChargedLogically(t *testing.T) {
	raw := rawTarEntry(t, "pad-export.json", tar.TypeReg, exportJSONFrom(t))
	const logicalSize = 96 << 20
	for i := 0; i < 3; i++ {
		pax := paxRecord("GNU.sparse.major", "0") +
			paxRecord("GNU.sparse.minor", "1") +
			paxRecord("GNU.sparse.size", fmt.Sprint(logicalSize)) +
			paxRecord("GNU.sparse.numblocks", "1") +
			paxRecord("GNU.sparse.map", "0,0")
		raw = append(raw, rawTarEntry(t, "pax", tar.TypeXHeader, []byte(pax))...)
		raw = append(raw, rawTarEntry(t, fmt.Sprintf("filler-%d.bin", i), tar.TypeReg, nil)...)
	}
	raw = append(raw, make([]byte, 1024)...)
	srv, _ := testServerWithAttachments(t)
	srv.SetImportBundleMaxBytes(1 << 20)
	if 3*logicalSize <= srv.decompressedBundleCap() {
		t.Fatalf("precondition: %d logical bytes must pass the %d ceiling", 3*logicalSize, srv.decompressedBundleCap())
	}
	rr := postBundle(srv, "SparseWS", gzipBytes(t, raw))
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "bundle_too_large") {
		t.Fatalf("sparse entries: got %d %s, want 413 bundle_too_large", rr.Code, rr.Body.String())
	}
	if workspaceListed(t, srv, "SparseWS") {
		t.Fatal("the partial workspace was not rolled back")
	}
}

// Codex r1 P2: extension headers are consumed inside Next, invisible to the
// entry count. The header blocks Next reads are counted instead.
func TestBUG3354_ExtensionHeaderChainRefused(t *testing.T) {
	t.Parallel()
	raw := rawTarEntry(t, "pad-export.json", tar.TypeReg, exportJSONFrom(t))
	extension := rawTarEntry(t, "pax", tar.TypeXHeader, nil) // one block each
	raw = append(raw, bytes.Repeat(extension, int(importBundleMaxHeaderBlocks)+1)...)
	raw = append(raw, rawTarEntry(t, "filler.bin", tar.TypeReg, nil)...)
	raw = append(raw, make([]byte, 1024)...)
	srv, _ := testServerWithAttachments(t)
	srv.SetImportBundleMaxBytes(1 << 20)
	body := gzipBytes(t, raw)
	if int64(len(body)) >= srv.effectiveImportBundleMaxBytes() || int64(len(raw)) >= srv.decompressedBundleCap() {
		t.Fatalf("precondition: the chain must fit both byte ceilings (body %d, raw %d)", len(body), len(raw))
	}
	rr := postBundle(srv, "ChainWS", body)
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "header blocks") {
		t.Fatalf("extension-header chain: got %d %s, want 413 naming the header-block cap", rr.Code, rr.Body.String())
	}
	if workspaceListed(t, srv, "ChainWS") {
		t.Fatal("the partial workspace was not rolled back")
	}
}

// The counted reader itself trips, not the declared-size check: the entry
// declares a size that fits, and only its padding crosses the ceiling. The
// deferred conversion answers 413 and the minted workspace is rolled back.
func TestBUG3354_ReaderOverflowConvertsTo413(t *testing.T) {
	t.Parallel()
	srv, _ := testServerWithAttachments(t)
	srv.SetImportBundleMaxBytes(1 << 20)
	var out bytes.Buffer
	out.Write(rawTarEntry(t, "pad-export.json", tar.TypeReg, exportJSONFrom(t)))
	tw := tar.NewWriter(&out)
	size := srv.decompressedBundleCap() - int64(out.Len()) - 512 - 1
	if err := tw.WriteHeader(&tar.Header{Name: "filler.bin", Mode: 0o644, Size: size}); err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for left := size; left > 0; {
		n := int64(len(chunk))
		if n > left {
			n = left
		}
		if _, err := tw.Write(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		left -= n
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	rr := postBundle(srv, "PaddingWS", gzipBytes(t, out.Bytes()))
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "bundle_too_large") {
		t.Fatalf("reader overflow: got %d %s, want 413 bundle_too_large", rr.Code, rr.Body.String())
	}
	if workspaceListed(t, srv, "PaddingWS") {
		t.Fatal("the partial workspace was not rolled back")
	}
}

// Codex r2: the tar reader accepts a negative base-256 size on a directory
// header, and charging it raised the logical budget, re-opening the sparse
// bypass. A negative size is refused outright.
func TestBUG3354_NegativeSizeRefused(t *testing.T) {
	raw := rawTarEntry(t, "pad-export.json", tar.TypeReg, exportJSONFrom(t))
	dir := rawTarEntry(t, "dir/", tar.TypeDir, nil)
	// Size field (bytes 124..135) in base-256: high bit set, two's complement.
	negSize := int64(-1) << 40
	neg := uint64(negSize)
	for i := 124; i < 128; i++ {
		dir[i] = 0xff
	}
	for i := 0; i < 8; i++ {
		dir[128+i] = byte(neg >> (56 - 8*i))
	}
	for i := 148; i < 156; i++ {
		dir[i] = ' '
	}
	sum := 0
	for _, b := range dir[:512] {
		sum += int(b)
	}
	copy(dir[148:156], fmt.Sprintf("%06o\x00 ", sum))
	raw = append(raw, dir...)
	// Behind it, sparse entries whose logical size only fits the budget the
	// negative size would have inflated.
	for i := 0; i < 3; i++ {
		pax := paxRecord("GNU.sparse.major", "0") +
			paxRecord("GNU.sparse.minor", "1") +
			paxRecord("GNU.sparse.size", fmt.Sprint(96<<20)) +
			paxRecord("GNU.sparse.numblocks", "1") +
			paxRecord("GNU.sparse.map", "0,0")
		raw = append(raw, rawTarEntry(t, "pax", tar.TypeXHeader, []byte(pax))...)
		raw = append(raw, rawTarEntry(t, fmt.Sprintf("filler-%d.bin", i), tar.TypeReg, nil)...)
	}
	raw = append(raw, make([]byte, 1024)...)

	// Precondition: the reader really does hand back the negative size.
	tr := tar.NewReader(bytes.NewReader(raw))
	if _, err := tr.Next(); err != nil {
		t.Fatalf("read export: %v", err)
	}
	if hdr, err := tr.Next(); err != nil || hdr.Size >= 0 {
		t.Fatalf("precondition: want a negative-size directory header, got %+v, %v", hdr, err)
	}

	srv, _ := testServerWithAttachments(t)
	srv.SetImportBundleMaxBytes(1 << 20)
	rr := postBundle(srv, "NegWS", gzipBytes(t, raw))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "negative size") {
		t.Fatalf("negative size: got %d %s, want 400 naming the negative size", rr.Code, rr.Body.String())
	}
	if workspaceListed(t, srv, "NegWS") {
		t.Fatal("the partial workspace was not rolled back")
	}
}

// countingBody counts the bytes the handler pulls from the request body.
type countingBody struct {
	r io.Reader
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Codex r3: Next walks a whole extension chain internally, so a header limit
// checked after it returns is refused only after all the work is done. The
// limit is enforced inside the walk: a chain of three times the cap, under
// the default byte ceilings, is refused having read about a third of it.
func TestBUG3354_HeaderLimitStopsTheWalk(t *testing.T) {
	t.Parallel()
	raw := rawTarEntry(t, "pad-export.json", tar.TypeReg, exportJSONFrom(t))
	extension := rawTarEntry(t, "pax", tar.TypeXHeader, nil) // one block each
	raw = append(raw, bytes.Repeat(extension, 3*int(importBundleMaxHeaderBlocks))...)
	raw = append(raw, rawTarEntry(t, "filler.bin", tar.TypeReg, nil)...)
	raw = append(raw, make([]byte, 1024)...)
	body := gzipBytes(t, raw)

	srv, _ := testServerWithAttachments(t)
	if int64(len(raw)) >= srv.decompressedBundleCap() {
		t.Fatalf("precondition: the chain must fit the byte ceiling, so only the header limit can stop it")
	}
	cb := &countingBody{r: bytes.NewReader(body)}
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=WalkWS", cb)
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "header blocks") {
		t.Fatalf("chain: got %d %s, want 413 naming the header-block cap", rr.Code, rr.Body.String())
	}
	if cb.n*2 > int64(len(body)) {
		t.Fatalf("refused only after reading %d of %d body bytes: the limit did not stop the walk", cb.n, len(body))
	}
}
