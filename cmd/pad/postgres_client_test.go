package main

import (
	"database/sql"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPostgresClientConninfo(t *testing.T) {
	for _, tc := range []struct {
		name, input, want, password string
		hasPassword, invalid        bool
	}{
		{name: "uri", input: "postgres://u:secret@host:5544/db?sslmode=require", want: "postgres://u@host:5544/db?sslmode=require", password: "secret", hasPassword: true},
		{name: "escaped password", input: "postgresql://u:p%40ss%3A%27%5C@host/db", want: "postgresql://u@host/db", password: "p@ss:'\\", hasPassword: true},
		{name: "query wins", input: "postgres://u:old@host/db?password=first&password=last&sslmode=disable", want: "postgres://u@host/db?sslmode=disable", password: "last", hasPassword: true},
		{name: "empty userinfo password uses defaults", input: "postgres://u:@host/db", want: "postgres://u:@host/db"},
		{name: "literal plus", input: "postgres://u@host/db?password=p+ass&application_name=a+b", want: "postgres://u@host/db?application_name=a+b", password: "p+ass", hasPassword: true},
		{name: "multiple hosts", input: "postgresql://u:p@host1:5544,host2:5545/db?target_session_attrs=read-write", want: "postgresql://u@host1:5544,host2:5545/db?target_session_attrs=read-write", password: "p", hasPassword: true},
		{name: "no password", input: "postgresql://u@host/db?application_name=backup", want: "postgresql://u@host/db?application_name=backup"},
		{name: "keyword", input: "host=localhost port=5544 user=u password='p\\'a\\\\ss' dbname='data base' sslmode=disable", want: "host=localhost port=5544 user=u dbname='data base' sslmode=disable", password: "p'a\\ss", hasPassword: true},
		{name: "escaped space", input: `dbname=db password=p\ ass user=u`, want: "dbname=db user=u", password: "p ass", hasPassword: true},
		{name: "duplicate", input: "password=first dbname=db password=last", want: "dbname=db", password: "last", hasPassword: true},
		{name: "embedded equals", input: "dbname=db password=a=b", want: "dbname=db", password: "a=b", hasPassword: true},
		{name: "service", input: "service=myservice application_name=backup", want: "service=myservice application_name=backup"},
		{name: "bare", input: "my_database", want: "my_database"},
		{name: "unterminated", input: "password='secret", invalid: true},
		{name: "trailing escape", input: "password=secret\\", invalid: true},
		{name: "missing equals", input: "host localhost password=secret", invalid: true},
		{name: "bad uri", input: "postgres://u:secret%zz@host/db", invalid: true},
		{name: "bad query", input: "postgres://u:secret@host/db?password=%zz", invalid: true},
		{name: "inline ssl passphrase", input: "postgres://u@host/db?sslpassword=secret", invalid: true},
		{name: "service password precedence", input: "service=myservice password=secret", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn, password, err := postgresClientConninfo(tc.input)
			if tc.invalid {
				if err == nil || strings.Contains(err.Error(), "secret") {
					t.Fatalf("expected a redacted error, got %v", err)
				}
				return
			}
			if err != nil || dsn != tc.want || (password != nil) != tc.hasPassword {
				t.Fatalf("dsn=%q, password present=%v, err=%v", dsn, password != nil, err)
			}
			if password != nil && *password != tc.password {
				t.Fatal("password decoded incorrectly")
			}
		})
	}
}

func TestPostgresClientPasswordHandoff(t *testing.T) {
	t.Setenv("PGPASSWORD", "inherited")
	t.Setenv("PGSERVICE", "")
	if err := os.Unsetenv("PGSERVICE"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pg_dump", "psql"} {
		cmd, err := postgresClient(name, "postgres://u:explicit@host/db", "--file", "backup.sql")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(cmd.Args, " "), "explicit") {
			t.Fatal("password in process arguments")
		}
		if cmd.Args[len(cmd.Args)-2] != "--dbname" {
			t.Fatal("libpq must receive an explicit database argument")
		}
		var password string
		for _, value := range cmd.Env {
			if strings.HasPrefix(value, "PGPASSWORD=") {
				password = value
			}
		}
		if password != "PGPASSWORD=explicit" {
			t.Fatal("explicit password did not override environment")
		}
	}
}

// Use a separate source and destination database, not the suite's shared one.
// Set PAD_TEST_POSTGRES_TOOLS_URL to a database with matching native clients.
func TestPostgresBackupRestoreConnection(t *testing.T) {
	base := os.Getenv("PAD_TEST_POSTGRES_TOOLS_URL")
	if base == "" {
		t.Skip("PAD_TEST_POSTGRES_TOOLS_URL not set (requires matching pg_dump/psql clients)")
	}
	for _, name := range []string{"pg_dump", "psql"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("PostgreSQL integration tests require %s: %v", name, err)
		}
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("PAD_TEST_POSTGRES_TOOLS_URL must be a PostgreSQL URI")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	createDB := func() string {
		name := "pad_backup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec("DROP DATABASE " + name + " WITH (FORCE)"); err != nil {
				t.Error(err)
			}
		})
		copy := *u
		copy.Path, copy.RawPath = "/"+name, ""
		return copy.String()
	}
	sourceURL, targetURL := createDB(), createDB()
	source, err := sql.Open("pgx", sourceURL)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := sql.Open("pgx", targetURL)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := source.Exec("CREATE TABLE recovery_probe (value text); INSERT INTO recovery_probe VALUES ('restored row')"); err != nil {
		t.Fatal(err)
	}
	// A wrong ambient database must not override the explicit URI.
	t.Setenv("PGDATABASE", "not_the_requested_database")
	t.Setenv("PGPASSWORD", "not_the_explicit_password")
	t.Setenv("PGSERVICE", "")
	if err := os.Unsetenv("PGSERVICE"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAD_DB_DRIVER", "postgres")
	archive := filepath.Join(t.TempDir(), "backup.sql")
	keyword := func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		quote := func(value string) string {
			return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(value) + "'"
		}
		password, _ := parsed.User.Password()
		host, port := parsed.Hostname(), parsed.Port()
		if queryHost := parsed.Query().Get("host"); queryHost != "" {
			host = queryHost
		}
		if port == "" {
			port = "5432"
		}
		return "host=" + quote(host) + " port=" + quote(port) + " dbname=" + quote(strings.TrimPrefix(parsed.Path, "/")) +
			" user=" + quote(parsed.User.Username()) + " password=" + quote(password) + " sslmode=disable"
	}
	for _, format := range []string{"postgres", "postgresql", "keyword"} {
		convert := func(raw string) string {
			if format == "keyword" {
				return keyword(raw)
			}
			parsed, _ := url.Parse(raw)
			parsed.Scheme = format
			return parsed.String()
		}
		t.Setenv("PAD_DATABASE_URL", convert(sourceURL))
		backup := dbBackupCmd()
		backup.SetArgs([]string{"--output", archive})
		if err := backup.Execute(); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PAD_DATABASE_URL", convert(targetURL))
		restore := dbRestoreCmd()
		restore.SetArgs([]string{"--force", archive})
		if err := restore.Execute(); err != nil {
			t.Fatal(err)
		}
		var value string
		if err := target.QueryRow("SELECT value FROM recovery_probe").Scan(&value); err != nil || value != "restored row" {
			t.Fatalf("round trip: value=%q err=%v", value, err)
		}
	}
	missing := *u
	missing.Path, missing.RawPath = "/pad_missing_"+strings.ReplaceAll(uuid.NewString(), "-", ""), ""
	t.Setenv("PAD_DATABASE_URL", missing.String())
	backup := dbBackupCmd()
	backup.SetArgs([]string{"--output", filepath.Join(t.TempDir(), "missing.sql")})
	if err := backup.Execute(); err == nil {
		t.Fatal("backup of missing database succeeded")
	}
	t.Log("PostgreSQL backup/restore round trip verified")
}
