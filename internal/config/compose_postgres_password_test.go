package config

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// composeRef matches a compose reference to POSTGRES_PASSWORD in any of its
// forms (${POSTGRES_PASSWORD}, ${POSTGRES_PASSWORD:?msg}, ...).
var composeRef = regexp.MustCompile(`\$\{POSTGRES_PASSWORD(?:[:?\-+][^}]*)?\}`)

// renderCompose substitutes POSTGRES_PASSWORD the way docker compose does:
// literally, with no quoting or escaping of the value.
func renderCompose(value, password string) string {
	return composeRef.ReplaceAllLiteralString(value, password)
}

// The pad service must connect with whatever POSTGRES_PASSWORD the postgres
// service was initialised with. Compose pastes the value into the environment
// verbatim, so a password inside the keyword DSN breaks on spaces, quotes and
// backslashes. The password travels as PGPASSWORD instead, which pgx (the
// server) and libpq (pad db backup/restore, which inherit the environment)
// read as given.
func TestComposePostgresPasswordReachesTheServerVerbatim(t *testing.T) {
	cf := parseComposeFile(t, filepath.Join("..", "..", "docker-compose.yml"))
	env := cf.Services["pad"].Environment

	dsn, ok := env["PAD_DATABASE_URL"]
	if !ok {
		t.Fatal("docker-compose.yml services.pad.environment has no PAD_DATABASE_URL")
	}
	if composeRef.MatchString(dsn) || strings.Contains(dsn, "password=") {
		t.Errorf("PAD_DATABASE_URL carries the password; pass it as PGPASSWORD instead: %q", dsn)
	}
	if !composeRef.MatchString(env["PGPASSWORD"]) {
		t.Errorf("services.pad.environment.PGPASSWORD = %q, want a ${POSTGRES_PASSWORD...} reference", env["PGPASSWORD"])
	}
	if !strings.Contains(env["PGPASSWORD"], ":?") {
		t.Errorf("services.pad.environment.PGPASSWORD = %q must keep the required (:?) form so compose refuses an unset password", env["PGPASSWORD"])
	}

	for _, password := range []string{
		"one two",
		`one\two`,
		`it's "quoted"`,
		"trailing ",
		"a=b c=d",
		"base64/+:@==",
	} {
		t.Run(password, func(t *testing.T) {
			t.Setenv("PGPASSWORD", renderCompose(env["PGPASSWORD"], password))
			cfg, err := pgconn.ParseConfig(renderCompose(dsn, password))
			if err != nil {
				t.Fatalf("rendered DSN does not parse: %v", err)
			}
			if cfg.Password != password {
				t.Errorf("server would connect with password %q, want %q", cfg.Password, password)
			}
		})
	}
}
