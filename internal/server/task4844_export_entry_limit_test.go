package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// measureImporterTarCounts replays importBundle's exact header-block and entry
// counting loop over raw tar bytes so tests can assert exact parity with the
// importer.
func measureImporterTarCounts(t *testing.T, tarBytes []byte) (entries int, headerBlocks int64) {
	t.Helper()
	budget := &bundleBudgetReader{r: bytes.NewReader(tarBytes), remaining: int64(len(tarBytes)) + 1024}
	tr := tar.NewReader(budget)
	for {
		before := budget.remaining
		hdr, err := tr.Next()
		headerBlocks += (before - budget.remaining) / 512
		if err == io.EOF {
			return entries, headerBlocks
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		entries++
		if _, err := readEntry(tr, hdr.Size); err != nil {
			t.Fatalf("readEntry %q: %v", hdr.Name, err)
		}
	}
}

func gunzipBytes(t *testing.T, gzBytes []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(gzBytes))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read gunzipped bundle: %v", err)
	}
	return raw
}

func uploadDistinctPNGs(t *testing.T, srv *Server, slug string, filenames []string) {
	t.Helper()
	for i, name := range filenames {
		body := append(realPNG(), byte(i), byte(i>>8), 0x42)
		rr := doMultipartUpload(srv, slug, name, body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("upload %q: %d %s", name, rr.Code, rr.Body.String())
		}
	}
}

type exportEntryTooLargeEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Entries           int   `json:"entries"`
			LimitEntries      int   `json:"limit_entries"`
			HeaderBlocks      int64 `json:"header_blocks"`
			LimitHeaderBlocks int64 `json:"limit_header_blocks"`
		} `json:"details"`
	} `json:"error"`
}

func TestTask4844_ExportOverEntryCapRefusesBeforeAnyByte(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)

	uploadDistinctPNGs(t, srv, slug, []string{"one.png", "two.png", "three.png"})

	// 2 metadata entries + 3 attachments = 5 entries; cap at 4.
	srv.importBundleMaxEntriesOverride = 4
	defer func() { srv.importBundleMaxEntriesOverride = 0 }()

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "application/gzip") || !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json (not application/gzip)", ct)
	}
	if got := rr.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q, want empty on 422", got)
	}
	if got := rr.Header().Get("Trailer"); got != "" {
		t.Errorf("Trailer = %q, want empty on 422", got)
	}

	var env exportEntryTooLargeEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode 422 body: %v (raw=%s)", err, rr.Body.String())
	}
	if env.Error.Code != "export_too_large" {
		t.Errorf("code = %q, want export_too_large", env.Error.Code)
	}
	if env.Error.Details.Entries != 5 {
		t.Errorf("entries = %d, want 5", env.Error.Details.Entries)
	}
	if env.Error.Details.LimitEntries != 4 {
		t.Errorf("limit_entries = %d, want 4", env.Error.Details.LimitEntries)
	}
	// 2 metadata + 3 short ASCII attachments + 2 EOF zero blocks = 7 header blocks.
	if env.Error.Details.HeaderBlocks != 7 {
		t.Errorf("header_blocks = %d, want 7", env.Error.Details.HeaderBlocks)
	}
	if env.Error.Details.LimitHeaderBlocks != importBundleMaxHeaderBlocks {
		t.Errorf("limit_header_blocks = %d, want %d", env.Error.Details.LimitHeaderBlocks, importBundleMaxHeaderBlocks)
	}
	if !strings.Contains(env.Error.Message, "5") || !strings.Contains(env.Error.Message, "4") {
		t.Errorf("message should name count (5) and limit (4): %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "pad workspace export --json") {
		t.Errorf("message should suggest plain JSON pad workspace export: %q", env.Error.Message)
	}
	if strings.Contains(env.Error.Message, "PAD_") {
		t.Errorf("message must not suggest an env var: %q", env.Error.Message)
	}
	assertSpoolDirEmpty(t, spoolDir)

	// Plain JSON export carries no blobs and must still succeed.
	jsonRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if jsonRR.Code != http.StatusOK {
		t.Fatalf("plain JSON export should still succeed: got %d %s", jsonRR.Code, jsonRR.Body.String())
	}
	assertSpoolDirEmpty(t, spoolDir)
}

func TestTask4844_ExportAtCapRoundTripsOnSameServer(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)

	uploadDistinctPNGs(t, srv, slug, []string{"a.png", "b.png", "c.png"})

	// Exact entry cap: 2 metadata + 3 attachments = 5 entries.
	t.Run("at_entry_cap", func(t *testing.T) {
		srv.importBundleMaxEntriesOverride = 5
		srv.importBundleMaxHeaderBlocksOverride = 0
		defer func() { srv.importBundleMaxEntriesOverride = 0 }()

		expRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
		if expRR.Code != http.StatusOK {
			t.Fatalf("export at exact entry cap 5: got %d %s", expRR.Code, expRR.Body.String())
		}
		assertSpoolDirEmpty(t, spoolDir)

		impRR := postBundle(srv, "AtEntryCap", expRR.Body.Bytes())
		if impRR.Code != http.StatusCreated {
			t.Fatalf("import at exact entry cap 5: got %d %s", impRR.Code, impRR.Body.String())
		}

		// Lowering the shared cap by 1 makes the importer reject the very same bundle.
		srv.importBundleMaxEntriesOverride = 4
		overRR := postBundle(srv, "OverEntryCap", expRR.Body.Bytes())
		if overRR.Code != http.StatusRequestEntityTooLarge || !strings.Contains(overRR.Body.String(), "bundle_too_large") {
			t.Fatalf("import over entry cap 4: got %d %s, want 413 bundle_too_large", overRR.Code, overRR.Body.String())
		}
	})

	// Exact header-block cap: 2 metadata + 3 short attachments + 2 EOF blocks = 7 header blocks.
	t.Run("at_header_block_cap", func(t *testing.T) {
		srv.importBundleMaxEntriesOverride = 0
		srv.importBundleMaxHeaderBlocksOverride = 7
		defer func() { srv.importBundleMaxHeaderBlocksOverride = 0 }()

		expRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
		if expRR.Code != http.StatusOK {
			t.Fatalf("export at exact header-block cap 7: got %d %s", expRR.Code, expRR.Body.String())
		}
		assertSpoolDirEmpty(t, spoolDir)

		impRR := postBundle(srv, "AtHeaderBlockCap", expRR.Body.Bytes())
		if impRR.Code != http.StatusCreated {
			t.Fatalf("import at exact header-block cap 7: got %d %s", impRR.Code, impRR.Body.String())
		}

		// Lowering the shared header-block cap by 1 makes the importer reject the same bundle.
		srv.importBundleMaxHeaderBlocksOverride = 6
		overRR := postBundle(srv, "OverHeaderBlockCap", expRR.Body.Bytes())
		if overRR.Code != http.StatusRequestEntityTooLarge || !strings.Contains(overRR.Body.String(), "bundle_too_large") {
			t.Fatalf("import over header-block cap 6: got %d %s, want 413 bundle_too_large", overRR.Code, overRR.Body.String())
		}
	})
}

func TestTask4844_PAXHeaderBlocksMatchImporterAndRefuseExport(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)

	longExtName := "long." + strings.Repeat("a", 80)
	nonASCIIExtName := "photo.pñg"
	shortName := "short.png"
	uploadDistinctPNGs(t, srv, slug, []string{longExtName, nonASCIIExtName, shortName})

	// First export with default limits to capture the actual bundle bytes.
	expRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if expRR.Code != http.StatusOK {
		t.Fatalf("baseline export: %d %s", expRR.Code, expRR.Body.String())
	}
	rawTar := gunzipBytes(t, expRR.Body.Bytes())
	importerEntries, importerHeaderBlocks := measureImporterTarCounts(t, rawTar)
	files := readBundle(t, expRR.Body.Bytes())

	// Collect the exact attachment rows the exporter sees.
	var exportedAtts []models.Attachment
	if err := srv.store.StreamWorkspaceExport(t.Context(), slug, store.ExportOptions{
		WithAttachments: true,
		IncludeVersions: true,
	}, func(_ *models.WorkspaceExport, atts []models.Attachment, _ store.VersionIter) error {
		exportedAtts = append([]models.Attachment(nil), atts...)
		return nil
	}); err != nil {
		t.Fatalf("StreamWorkspaceExport: %v", err)
	}

	counts, err := countExportBundleTar(
		int64(len(files["pad-export.json"])),
		int64(len(files["attachments/manifest.json"])),
		exportedAtts,
	)
	if err != nil {
		t.Fatalf("countExportBundleTar: %v", err)
	}
	if counts.Entries != importerEntries {
		t.Errorf("helper entries = %d, importer counted %d", counts.Entries, importerEntries)
	}
	if counts.HeaderBlocks != importerHeaderBlocks {
		t.Errorf("helper header blocks = %d, importer counted %d", counts.HeaderBlocks, importerHeaderBlocks)
	}
	// Precondition: PAX headers really did inflate header blocks past 1 per entry + 2 EOF.
	if importerHeaderBlocks <= int64(importerEntries)+2 {
		t.Fatalf("precondition: expected PAX headers to exceed 1 block/entry + 2 EOF; got entries=%d headerBlocks=%d",
			importerEntries, importerHeaderBlocks)
	}

	// Refuse when header-block cap is 1 below actual, even though entry cap has headroom.
	srv.importBundleMaxEntriesOverride = counts.Entries + 10
	srv.importBundleMaxHeaderBlocksOverride = counts.HeaderBlocks - 1
	defer func() {
		srv.importBundleMaxEntriesOverride = 0
		srv.importBundleMaxHeaderBlocksOverride = 0
	}()

	refusedRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if refusedRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("header-block refusal: got %d %s, want 422", refusedRR.Code, refusedRR.Body.String())
	}
	if ct := refusedRR.Header().Get("Content-Type"); strings.Contains(ct, "application/gzip") || !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var env exportEntryTooLargeEnvelope
	if err := json.Unmarshal(refusedRR.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode 422 body: %v (raw=%s)", err, refusedRR.Body.String())
	}
	if env.Error.Code != "export_too_large" {
		t.Errorf("code = %q, want export_too_large", env.Error.Code)
	}
	if env.Error.Details.Entries != counts.Entries || env.Error.Details.LimitEntries != counts.Entries+10 {
		t.Errorf("entries/limit = %d/%d, want %d/%d",
			env.Error.Details.Entries, env.Error.Details.LimitEntries, counts.Entries, counts.Entries+10)
	}
	if env.Error.Details.HeaderBlocks != counts.HeaderBlocks || env.Error.Details.LimitHeaderBlocks != counts.HeaderBlocks-1 {
		t.Errorf("header_blocks/limit = %d/%d, want %d/%d",
			env.Error.Details.HeaderBlocks, env.Error.Details.LimitHeaderBlocks, counts.HeaderBlocks, counts.HeaderBlocks-1)
	}
	if !strings.Contains(env.Error.Message, fmt.Sprint(counts.HeaderBlocks)) ||
		!strings.Contains(env.Error.Message, fmt.Sprint(counts.HeaderBlocks-1)) {
		t.Errorf("message should name header-block count (%d) and limit (%d): %q",
			counts.HeaderBlocks, counts.HeaderBlocks-1, env.Error.Message)
	}
	if strings.Contains(env.Error.Message, "PAD_") {
		t.Errorf("message must not suggest an env var: %q", env.Error.Message)
	}
	assertSpoolDirEmpty(t, spoolDir)

	// At the exact header-block cap, export succeeds AND imports on the same server.
	srv.importBundleMaxHeaderBlocksOverride = counts.HeaderBlocks
	atCapRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if atCapRR.Code != http.StatusOK {
		t.Fatalf("export at exact PAX header-block cap %d: got %d %s", counts.HeaderBlocks, atCapRR.Code, atCapRR.Body.String())
	}
	assertSpoolDirEmpty(t, spoolDir)

	impRR := postBundle(srv, "PAXAtCap", atCapRR.Body.Bytes())
	if impRR.Code != http.StatusCreated {
		t.Fatalf("import at exact PAX header-block cap %d: got %d %s", counts.HeaderBlocks, impRR.Code, impRR.Body.String())
	}

	// And 1 below causes the importer to refuse that exact bundle.
	srv.importBundleMaxHeaderBlocksOverride = counts.HeaderBlocks - 1
	impRefusedRR := postBundle(srv, "PAXOverCap", atCapRR.Body.Bytes())
	if impRefusedRR.Code != http.StatusRequestEntityTooLarge || !strings.Contains(impRefusedRR.Body.String(), "bundle_too_large") {
		t.Fatalf("import over PAX header-block cap %d: got %d %s, want 413 bundle_too_large",
			counts.HeaderBlocks-1, impRefusedRR.Code, impRefusedRR.Body.String())
	}
}

func TestTask4844_CountExportBundleTarMatchesRealTar(t *testing.T) {
	t.Parallel()
	baseTime := time.Date(2026, 6, 15, 12, 34, 56, 789123456, time.UTC)

	// Mix of short ASCII, no-extension (.bin), boundary 100/101-byte paths,
	// long extensions (3 blocks and 4 blocks), non-ASCII extensions, and
	// varied body sizes (0, exact 512 multiples, and non-multiples).
	const uuid = "11111111-2222-3333-4444-555555555555" // 36 bytes; "attachments/"+uuid = 48 bytes
	atts := []models.Attachment{
		{ID: uuid, Filename: "short.png", SizeBytes: 13, CreatedAt: baseTime},
		{ID: "22222222-2222-3333-4444-555555555555", Filename: "README", SizeBytes: 0, CreatedAt: baseTime},
		{ID: "33333333-2222-3333-4444-555555555555", Filename: "exact100." + strings.Repeat("x", 51), SizeBytes: 512, CreatedAt: baseTime},
		{ID: "44444444-2222-3333-4444-555555555555", Filename: "over100." + strings.Repeat("y", 52), SizeBytes: 513, CreatedAt: baseTime},
		{ID: "55555555-2222-3333-4444-555555555555", Filename: "verylong." + strings.Repeat("z", 600), SizeBytes: 1024, CreatedAt: baseTime},
		{ID: "66666666-2222-3333-4444-555555555555", Filename: "doc.café", SizeBytes: 511, CreatedAt: baseTime},
		{ID: "77777777-2222-3333-4444-555555555555", Filename: "report.日本語", SizeBytes: 1, CreatedAt: baseTime},
	}

	exportBody := bytes.Repeat([]byte("e"), 777)
	manifestBody := bytes.Repeat([]byte("m"), 512)

	// Write a real tar archive using the exporter's own header functions.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeTarFileFrom(tw, "pad-export.json", bytes.NewReader(exportBody), int64(len(exportBody))); err != nil {
		t.Fatalf("write pad-export.json: %v", err)
	}
	if err := writeTarFileFrom(tw, "attachments/manifest.json", bytes.NewReader(manifestBody), int64(len(manifestBody))); err != nil {
		t.Fatalf("write manifest.json: %v", err)
	}
	for i := range atts {
		if err := tw.WriteHeader(bundleAttachmentTarHeader(&atts[i])); err != nil {
			t.Fatalf("write attachment %d header: %v", i, err)
		}
		if _, err := tw.Write(bytes.Repeat([]byte{byte(i + 1)}, int(atts[i].SizeBytes))); err != nil {
			t.Fatalf("write attachment %d body: %v", i, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}

	wantEntries, wantHeaderBlocks := measureImporterTarCounts(t, buf.Bytes())
	got, err := countExportBundleTar(int64(len(exportBody)), int64(len(manifestBody)), atts)
	if err != nil {
		t.Fatalf("countExportBundleTar: %v", err)
	}
	if got.Entries != wantEntries {
		t.Errorf("Entries = %d, want %d", got.Entries, wantEntries)
	}
	if got.HeaderBlocks != wantHeaderBlocks {
		t.Errorf("HeaderBlocks = %d, want %d", got.HeaderBlocks, wantHeaderBlocks)
	}
	// Sanity: mix must exercise multi-block PAX headers (3-block and 4-block).
	if wantHeaderBlocks <= int64(wantEntries)+2 {
		t.Errorf("precondition: expected PAX headers in test fixture, got entries=%d headerBlocks=%d",
			wantEntries, wantHeaderBlocks)
	}
}
