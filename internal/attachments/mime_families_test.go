package attachments

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// mimeFamiliesFixture is the shared MIME -> icon-family map the web client
// uses to pick an attachment's file-type icon (PLAN-2392 DR-3a).
//
// It lives under the web root rather than next to this file because vitest's
// `server.fs.allow` only permits the web root and node_modules, so the web
// test cannot read across the repo — Go can, so the Go side reads the web
// side's copy instead of the other way around.
const mimeFamiliesFixture = "../../web/src/lib/attachments/mime-families.json"

type mimeFamilies struct {
	Families []string          `json:"families"`
	MIME     map[string]string `json:"mime"`
}

func loadMIMEFamilies(t *testing.T) mimeFamilies {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(mimeFamiliesFixture))
	if err != nil {
		t.Fatalf("read %s: %v", mimeFamiliesFixture, err)
	}
	var fixture mimeFamilies
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse %s: %v", mimeFamiliesFixture, err)
	}
	if len(fixture.Families) == 0 || len(fixture.MIME) == 0 {
		t.Fatalf("%s is empty — families=%d mime=%d", mimeFamiliesFixture,
			len(fixture.Families), len(fixture.MIME))
	}
	return fixture
}

// TestMIMEFamiliesCoverAllowlist is the drift guard between this package's
// upload allowlist and the client's icon mapping. The allowlist is private Go
// data, so a hand-maintained web-side copy would rot silently: adding a MIME
// here without adding it to the fixture would leave real uploads rendering the
// generic-file fallback with nothing to notice it. This test is what notices.
func TestMIMEFamiliesCoverAllowlist(t *testing.T) {
	fixture := loadMIMEFamilies(t)

	known := make(map[string]bool, len(fixture.Families))
	for _, family := range fixture.Families {
		known[family] = true
	}

	var missing []string
	for mime := range allowed {
		family, ok := fixture.MIME[mime]
		if !ok {
			missing = append(missing, mime)
			continue
		}
		if !known[family] {
			t.Errorf("%s maps %q to unknown family %q (known: %v)",
				mimeFamiliesFixture, mime, family, fixture.Families)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("allowlisted MIME types missing from %s: %v\n"+
			"Add each to the fixture's \"mime\" object with the icon family it "+
			"should render as, so the web client doesn't fall back to the "+
			"generic file icon for a format we accept.",
			mimeFamiliesFixture, missing)
	}
}

// TestMIMEFamiliesFixtureHasNoStrays is the other direction: an entry in the
// fixture that is not (and is no longer) on the allowlist is dead weight, and
// usually means a MIME was removed from the allowlist without the client being
// updated. Prefix and extension fallbacks cover anything unlisted, so the
// fixture has no reason to carry non-allowlisted MIMEs.
func TestMIMEFamiliesFixtureHasNoStrays(t *testing.T) {
	fixture := loadMIMEFamilies(t)

	var strays []string
	for mime := range fixture.MIME {
		if _, ok := allowed[mime]; !ok {
			strays = append(strays, mime)
		}
	}

	if len(strays) > 0 {
		sort.Strings(strays)
		t.Errorf("%s lists MIME types that are not on the upload allowlist: %v",
			mimeFamiliesFixture, strays)
	}
}

// TestMIMEFamiliesKeysAreNormalized keeps fixture lookups honest: the client
// normalizes a MIME (strip parameters, lowercase) before indexing this map,
// exactly as LookupMIME does, so a key that isn't already normalized could
// never match.
func TestMIMEFamiliesKeysAreNormalized(t *testing.T) {
	fixture := loadMIMEFamilies(t)

	for mime := range fixture.MIME {
		if got := NormalizeMIME(mime); got != mime {
			t.Errorf("%s key %q is not normalized (want %q)", mimeFamiliesFixture, mime, got)
		}
	}
}
