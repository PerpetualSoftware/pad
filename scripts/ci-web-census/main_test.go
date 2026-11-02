package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestCensus(t *testing.T) {
	tests := []struct {
		name, source, manifest, allow string
		pg                            bool
		want                          string
	}{
		{name: "unlisted web reader", source: `package p; import "os"; func TestRead(t *testing.T) { os.ReadFile("../../web/foo") }`, want: "web"},
		{name: "covered reader", source: `package p; func TestRead(t *testing.T) { read("../../web/foo") }`, manifest: "^TestRead$"},
		{name: "wrong test filter", source: `package p; func TestRead(t *testing.T) { read("../../web/foo") }`, manifest: "^TestOther$", want: "TestRead"},
		{name: "embed", source: "package p\n//go:embed web/build\nvar ui string", want: "web"},
		{name: "reviewed embed", source: "package p\n//go:embed web/build\nvar ui string", allow: "web/build"},
		{name: "unknown formatted path", source: `package p; import "fmt"; func TestRead(t *testing.T) { read(fmt.Sprintf("web/%s", fromEnv())) }`, manifest: ".*", want: "unresolved"},
		{name: "unknown suffix", source: `package p; func TestRead(t *testing.T) { read(filepath.Join("..", "web", fromEnv())) }`, manifest: ".*", want: "unresolved"},
		{name: "filesystem dot import", source: `package p; import . "path/filepath"; func TestRead(t *testing.T) { read(Join("..","web",fromEnv())) }`, manifest: ".*", want: "unresolved"},
		{name: "unrelated dot import", source: `package p; import . "net/http"; var method=MethodGet`},
		{name: "alias join", source: `package p; import fp "path/filepath"; const dir="web"; func TestRead(t *testing.T) { read(fp.Join("..", dir, "foo")) }`, want: "web"},
		{name: "alias unknown suffix", source: `package p; import fp "path/filepath"; const dir="web"; func TestRead(t *testing.T) { read(fp.Join("..", dir, fromEnv())) }`, manifest: ".*", want: "unresolved"},
		{name: "joined pieces", source: `package p; func TestRead(t *testing.T) { read(path.Join("..", "web", "foo")) }`, want: "web"},
		{name: "constant and concatenation", source: `package p; const root = "we" + "b/"; func TestRead(t *testing.T) { read(root + "foo") }`, want: "web"},
		{name: "helper covered", source: `package p; func helper() { read("web/foo") }; func TestRead(t *testing.T) { helper() }`, manifest: "^TestRead$"},
		{name: "helper uncovered caller", source: `package p; func helper() { read("web/foo") }; func TestRead(t *testing.T) { helper() }; func TestOther(t *testing.T) { helper() }`, manifest: "^TestRead$", want: "TestOther"},
		{name: "pg wrapper", source: `package p; func wrapper() { storetest.NewPostgres(t) }; func TestRead(t *testing.T) { wrapper() }`, want: "PostgreSQL"},
		{name: "listed pg", source: `package p; func wrapper() { storetest.NewPostgres(t) }`, pg: true},
		{name: "pg env getter alias", source: `package p; import "os"; var getter=os.Getenv; func TestRead(t *testing.T) { getter("PAD_TEST_POSTGRES_URL") }`, want: "PostgreSQL"},
		{name: "pg env", source: `package p; import "os"; var dsn = os.Getenv("PAD_TEST_POSTGRES_TOOLS_URL")`, want: "PostgreSQL"},
		{name: "PG error type cannot connect", source: `package p; import "github.com/jackc/pgx/v5/pgconn"; func check() { var err *pgconn.PgError; _=err }`},
		{name: "conditional PG driver", source: `package p; import "database/sql"; func wrapper(pg bool) { driver:="sqlite"; if pg { driver="pgx" }; sql.Open(driver,dsn) }`, want: "PostgreSQL"},
		{name: "pg driver", source: `package p; import "database/sql"; func wrapper() { sql.Open("pgx", dsn) }`, want: "PostgreSQL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parity := map[string]string{}
			if tt.manifest != "" {
				parity["./probe"] = tt.manifest
			}
			allow := map[string]string{}
			if tt.allow != "" {
				allow["probe/reader_test.go\t"+tt.allow] = "reviewed fixture"
			}
			pg := map[string]bool{}
			if tt.pg {
				pg["./probe"] = true
			}
			errs := audit(map[string]string{"probe/reader_test.go": tt.source}, parity, allow, pg, nil)
			got := strings.Join(errs, "\n")
			if tt.want == "" && got != "" {
				t.Fatal(got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("wanted %q; got %q", tt.want, got)
			}
		})
	}
}

func TestPGTransitiveImports(t *testing.T) {
	files := map[string]string{
		"helper/helper.go":  `package helper; func Open() { storetest.NewPostgres(t) }`,
		"other/use_test.go": `package other; import h "github.com/PerpetualSoftware/pad/helper"; func TestUse(t *testing.T) { h.Open() }`,
	}
	errs := audit(files, nil, nil, map[string]bool{"./helper": true}, nil)
	if !strings.Contains(strings.Join(errs, "\n"), "other") {
		t.Fatalf("unlisted indirect PG user passed: %v", errs)
	}
}

func TestSQLiteException(t *testing.T) {
	for _, constructor := range []string{"NewSQLite", "NewPostgres"} {
		files := map[string]string{"internal/decision/use_test.go": `package decision; import "github.com/PerpetualSoftware/pad/internal/store/storetest"; func TestUse(t *testing.T) { storetest.` + constructor + `(t) }`}
		errs := audit(files, nil, nil, map[string]bool{"./internal/store/storetest": true}, map[string]string{"./internal/decision": "NewSQLite only"})
		if (len(errs) == 0) != (constructor == "NewSQLite") {
			t.Fatalf("%s: %v", constructor, errs)
		}
	}
}

func TestPGNoTestsException(t *testing.T) {
	for _, tt := range []struct {
		name, testFile, source, want string
	}{
		{name: "operator command without tests"},
		{name: "operator command gains tests", testFile: "operator/main_test.go", want: "ci-pg-packages.txt, or re-reviewed"},
		{name: "build excluded external test still invalidates review", testFile: "operator/main_windows_test.go", want: "ci-pg-packages.txt, or re-reviewed"},
		{name: "stale entry without PG references", source: `package main; func main() {}`, want: "stale no-tests"},
		{name: "library is not an operator command", source: `package operator; func Open() { storetest.NewPostgres(t) }`, want: "no-tests entry requires package main"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := tt.source
			if source == "" {
				source = `package main; import "database/sql"; func main() { sql.Open("pgx", dsn) }`
			}
			files := map[string]string{"operator/main.go": source}
			if tt.testFile != "" {
				files[tt.testFile] = "package main\n"
				if strings.HasSuffix(tt.testFile, "_windows_test.go") {
					files[tt.testFile] = "//go:build windows\n\npackage main_test\n"
				}
			}
			errs := audit(files, nil, nil, nil, map[string]string{"./operator": "no-tests: reviewed operator command"})
			got := strings.Join(errs, "\n")
			if tt.want == "" && got != "" {
				t.Fatal(got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("wanted %q; got %q", tt.want, got)
			}
		})
	}
}

func TestReviewedNonconnectingBoundary(t *testing.T) {
	body := `func Register() { sql.Open("pgx", "") }`
	files := map[string]string{
		"helper/helper.go":  `package helper; import "database/sql"; ` + body + `; func SQLite() { Register() }`,
		"other/use_test.go": `package other; import "github.com/PerpetualSoftware/pad/helper"; func TestUse(t *testing.T) { helper.SQLite() }`,
	}
	review := map[string]string{"./helper.Register": fmt.Sprintf("sha256:%x registration only", sha256.Sum256([]byte(body)))}
	pg := map[string]bool{"./helper": true}
	if errs := audit(files, nil, nil, pg, review); len(errs) != 0 {
		t.Fatal(errs)
	}
	files["helper/helper.go"] = strings.Replace(files["helper/helper.go"], `sql.Open("pgx", "")`, `sql.Open("pgx", ""); db.Ping()`, 1)
	if errs := audit(files, nil, nil, pg, review); len(errs) == 0 {
		t.Fatal("changed reviewed helper passed")
	}
}

func TestPGMethodWrapper(t *testing.T) {
	files := map[string]string{
		"helper/helper.go":  `package helper; type Helper struct{}; func (h Helper) Open() { storetest.NewPostgres(t) }`,
		"other/use_test.go": `package other; import h "github.com/PerpetualSoftware/pad/helper"; func TestUse(t *testing.T) { x:=h.Helper{}; x.Open() }`,
	}
	errs := audit(files, nil, nil, map[string]bool{"./helper": true}, nil)
	if !strings.Contains(strings.Join(errs, "\n"), "other") {
		t.Fatalf("unlisted PG method user passed: %v", errs)
	}
}

func TestTestingCleanupIsNotStoretestCleanup(t *testing.T) {
	files := map[string]string{
		"internal/store/storetest/helper.go": `package storetest; import "os"; func NewSQLite(t *testing.T) { t.Cleanup(func(){}) }; func Cleanup() { os.Getenv("PAD_TEST_POSTGRES_URL") }`,
		"internal/decision/use_test.go":      `package decision; import "github.com/PerpetualSoftware/pad/internal/store/storetest"; func TestUse(t *testing.T) { storetest.NewSQLite(t) }`,
	}
	errs := audit(files, nil, nil, map[string]bool{"./internal/store/storetest": true}, map[string]string{"./internal/decision": "NewSQLite only"})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
}
