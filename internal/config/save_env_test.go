package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/PerpetualSoftware/pad/internal/secret"
)

// persistedEnvVars maps every environment variable Load reads into a
// configFile-backed field to a valid sample value for it.
var persistedEnvVars = map[string]string{
	"PAD_MODE":                  "remote",
	"PAD_HOST":                  "0.0.0.0",
	"PAD_PORT":                  "8123",
	"PAD_URL":                   "https://env.example.test",
	"PAD_MAILEROO_API_KEY":      "env-maileroo",
	"PAD_EMAIL_FROM":            "env@example.test",
	"PAD_EMAIL_FROM_NAME":       "Env Pad",
	"PAD_CLOUD":                 "true",
	"PAD_CLOUD_SECRET":          "env-cloud-a,env-cloud-b",
	"PAD_CLOUD_SIDECAR_URL":     "http://sidecar.env.test:7778",
	"PAD_CLOUD_OUTBOUND_SECRET": "env-outbound",
	"PAD_PUSH_GATEWAY_URL":      "https://push.env.test",
	"PAD_PUSH_GATEWAY_SECRET":   "env-push",
	"PAD_MCP_PUBLIC_URL":        "https://mcp.env.test",
	"PAD_AUTH_SERVER_URL":       "https://auth.env.test",
	"PAD_OPENAI_APPS_CHALLENGE": "env-challenge",
	"PAD_ENCRYPTION_KEY":        "env-encryption-key",
	"PAD_CORS_ORIGINS":          "https://env.example.test",
	"PAD_SECURE_COOKIES":        "true",
	"PAD_TRUSTED_PROXIES":       "192.168.0.0/16",
	"PAD_METRICS_TOKEN":         "env-metrics",
	"PAD_IP_CHANGE_ENFORCE":     "strict",
	"PAD_SSE_MAX_CONNECTIONS":   "21",
	"PAD_SSE_MAX_PER_WORKSPACE": "22",
	"PAD_REDIS_NAMESPACE":       "env-ns",
	"PAD_EVENTS_HEARTBEAT":      "true",
	"PAD_WATCH_HEARTBEAT":       "true",
	"PAD_SSE_MAX_PER_USER":      "23",
	"PAD_COLLAB_MAX_PER_USER":   "24",
}

// notPersistedEnvVars are read by Load (or envDataDir) into fields that have
// no toml key, so Save never writes them whatever it does.
var notPersistedEnvVars = map[string]string{
	"PAD_DATA_DIR":                   "DataDir (toml:\"-\")",
	"PAD_DB_PATH":                    "DBPath (toml:\"-\")",
	"PUBLIC_URL":                     "PublicURL (toml:\"-\")",
	"PAD_DB_STATEMENT_TIMEOUT":       "DBStatementTimeoutRaw (toml:\"-\")",
	"PAD_DB_LOCK_TIMEOUT":            "DBLockTimeoutRaw (toml:\"-\")",
	"PAD_DB_IDLE_IN_TX_TIMEOUT":      "DBIdleInTxTimeoutRaw (toml:\"-\")",
	"PAD_PUSH_DISPATCH_INTERVAL":     "PushDispatchIntervalRaw (toml:\"-\")",
	"PAD_PUSH_DISPATCH_SETTLE":       "PushDispatchSettleRaw (toml:\"-\")",
	"PAD_PUSH_DISPATCH_LEASE":        "PushDispatchLeaseRaw (toml:\"-\")",
	"PAD_PUSH_DISPATCH_LEASE_MARGIN": "PushDispatchLeaseMarginRaw (toml:\"-\")",
	"PAD_MCP_ENABLED":                "MCPEnabledEnv / MCPEnabledEnvRaw (toml:\"-\")",
	"PAD_EVENTS_PUBLISH_EPOCH":       "EventsPublishEpoch (toml:\"-\"; env-only, TASK-4895)",
}

// isolateEnv points Load at a fresh data dir and clears every variable Load
// reads, so an ambient PAD_* in the test runner's environment cannot leak in.
func isolateEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for k := range persistedEnvVars {
		t.Setenv(k, "")
	}
	for k := range notPersistedEnvVars {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	t.Setenv("PAD_DATA_DIR", dir)
	return dir
}

func writeConfigFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func readConfigMap(t *testing.T, dir string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if _, err := toml.DecodeFile(filepath.Join(dir, "config.toml"), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mustLoad(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func mustSave(t *testing.T, cfg *Config) {
	t.Helper()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveOmitsEnvOnlySecret(t *testing.T) {
	dir := isolateEnv(t)
	t.Setenv("PAD_CLOUD_SECRET", "s3")
	cfg := mustLoad(t)
	if got := cfg.CloudSecret.Reveal(); got != "s3" {
		t.Fatalf("CloudSecret after Load = %q, want s3", got)
	}
	mustSave(t, cfg)
	m := readConfigMap(t, dir)
	if v, ok := m["cloud_secret"]; ok && v != "" {
		t.Errorf("config.toml cloud_secret = %q, want empty", v)
	}
}

func TestSaveKeepsFileValueUnderEnvOverride(t *testing.T) {
	dir := isolateEnv(t)
	writeConfigFile(t, dir, "cloud_secret = \"fromfile\"\nhost = \"10.0.0.1\"\n")
	t.Setenv("PAD_CLOUD_SECRET", "fromenv")
	t.Setenv("PAD_HOST", "0.0.0.0")
	cfg := mustLoad(t)
	if cfg.CloudSecret.Reveal() != "fromenv" || cfg.Host != "0.0.0.0" {
		t.Fatalf("after Load: cloud_secret %q host %q, want the env values", cfg.CloudSecret.Reveal(), cfg.Host)
	}
	mustSave(t, cfg)
	m := readConfigMap(t, dir)
	if m["cloud_secret"] != "fromfile" || m["host"] != "10.0.0.1" {
		t.Errorf("config.toml cloud_secret %q host %q, want fromfile and 10.0.0.1", m["cloud_secret"], m["host"])
	}
}

func TestSavePersistsCallerChangeOverEnv(t *testing.T) {
	dir := isolateEnv(t)
	t.Setenv("PAD_URL", "https://env.example")
	cfg := mustLoad(t)
	cfg.URL = "https://chosen.example"
	mustSave(t, cfg)
	if got := readConfigMap(t, dir)["url"]; got != "https://chosen.example" {
		t.Errorf("config.toml url = %q, want https://chosen.example", got)
	}
}

func TestSavePersistsPostLoadChangeWithoutEnv(t *testing.T) {
	dir := isolateEnv(t)
	cfg := mustLoad(t)
	cfg.URL = "https://flag.example"
	mustSave(t, cfg)
	if got := readConfigMap(t, dir)["url"]; got != "https://flag.example" {
		t.Errorf("config.toml url = %q, want https://flag.example", got)
	}
}

func TestSaveTwiceIsStable(t *testing.T) {
	dir := isolateEnv(t)
	writeConfigFile(t, dir, "cloud_secret = \"fromfile\"\n")
	t.Setenv("PAD_CLOUD_SECRET", "fromenv")
	t.Setenv("PAD_HOST", "0.0.0.0")
	cfg := mustLoad(t)
	// A caller change between Load and the first Save: it must survive both.
	cfg.Editor = "nano"
	mustSave(t, cfg)
	first, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	mustSave(t, cfg)
	second, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second Save changed config.toml:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	m := readConfigMap(t, dir)
	if m["cloud_secret"] != "fromfile" || m["host"] != "127.0.0.1" || m["editor"] != "nano" {
		t.Errorf("config.toml cloud_secret %q host %q editor %q, want fromfile, 127.0.0.1, nano", m["cloud_secret"], m["host"], m["editor"])
	}
	// A change after the first Save is still written by the next one.
	cfg.Editor = "vim"
	mustSave(t, cfg)
	m = readConfigMap(t, dir)
	if m["editor"] != "vim" || m["cloud_secret"] != "fromfile" {
		t.Errorf("third Save: editor %q cloud_secret %q, want vim and fromfile", m["editor"], m["cloud_secret"])
	}
}

func TestSaveWithoutLoadWritesCurrent(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.ConfigPath = filepath.Join(dir, "config.toml")
	cfg.CloudSecret = secret.New("direct")
	mustSave(t, cfg)
	if got := readConfigMap(t, dir)["cloud_secret"]; got != "direct" {
		t.Errorf("config.toml cloud_secret = %q, want direct", got)
	}
}

func TestSaveOmitsEveryEnvOverride(t *testing.T) {
	// Guard: every variable Load reads is classified.
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "\nfunc Load()")
	if start < 0 {
		t.Fatal("func Load() not found in config.go")
	}
	body := text[start+1:]
	if end := strings.Index(body, "\nfunc "); end >= 0 {
		body = body[:end]
	}
	read := map[string]bool{"PAD_DATA_DIR": true, "PAD_DB_PATH": true} // envDataDir's two
	for _, m := range regexp.MustCompile(`os\.Getenv\("(PAD_[A-Z0-9_]+|PUBLIC_URL)"\)`).FindAllStringSubmatch(body, -1) {
		read[m[1]] = true
	}
	var missing []string
	for k := range read {
		_, p := persistedEnvVars[k]
		_, n := notPersistedEnvVars[k]
		if !p && !n {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("Load reads %v, which this test does not classify: add each to persistedEnvVars or notPersistedEnvVars", missing)
	}
	for k := range persistedEnvVars {
		if !read[k] {
			t.Errorf("persistedEnvVars lists %s, which Load no longer reads", k)
		}
	}

	defaults, err := func() (string, error) {
		var b bytes.Buffer
		err := toml.NewEncoder(&b).Encode(DefaultConfig().fileView())
		return b.String(), err
	}()
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(persistedEnvVars))
	for k := range persistedEnvVars {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			dir := isolateEnv(t)
			writeConfigFile(t, dir, defaults)
			pre := mustLoad(t).fileView()

			t.Setenv(name, persistedEnvVars[name])
			cfg := mustLoad(t)
			if reflect.DeepEqual(cfg.fileView(), pre) {
				t.Fatalf("%s=%q did not change the loaded config; pick a sample value that does", name, persistedEnvVars[name])
			}
			mustSave(t, cfg)

			t.Setenv(name, "")
			if post := mustLoad(t).fileView(); !reflect.DeepEqual(post, pre) {
				t.Errorf("%s was persisted by Save:\npre  %+v\npost %+v", name, pre, post)
			}
		})
	}
}

// A caller's choice that equals the env value is still a choice: MarkExplicit
// makes Save write it, so configure with PAD_URL set to the same URL persists.
func TestSaveWritesExplicitValueEqualToEnv(t *testing.T) {
	dir := isolateEnv(t)
	t.Setenv("PAD_URL", "https://x.example")
	cfg := mustLoad(t)
	cfg.Mode = ModeRemote
	cfg.URL = "https://x.example"
	cfg.MarkExplicit("mode", "url")
	mustSave(t, cfg)
	m := readConfigMap(t, dir)
	if m["mode"] != ModeRemote || m["url"] != "https://x.example" {
		t.Errorf("config.toml mode %q url %q, want remote and https://x.example", m["mode"], m["url"])
	}
}

// Related fields are saved together: a cloud choice under PAD_CLOUD=true over
// a file holding mode=local writes mode=cloud with the cloud URL, never
// mode=local with the cloud URL.
func TestSaveKeepsModeAndURLTogether(t *testing.T) {
	dir := isolateEnv(t)
	writeConfigFile(t, dir, "mode = \"local\"\n")
	t.Setenv("PAD_CLOUD", "true")
	cfg := mustLoad(t)
	cfg.Mode = ModeCloud
	cfg.URL = CloudBaseURL
	cfg.MarkExplicit("mode", "url")
	mustSave(t, cfg)
	m := readConfigMap(t, dir)
	if m["mode"] != ModeCloud || m["url"] != CloudBaseURL {
		t.Errorf("config.toml mode %q url %q, want cloud and %s", m["mode"], m["url"], CloudBaseURL)
	}
}

// An unmarked env-sourced field is still not written when a sibling is marked.
func TestSaveMarkExplicitIsPerField(t *testing.T) {
	dir := isolateEnv(t)
	t.Setenv("PAD_CLOUD_SECRET", "fromenv")
	cfg := mustLoad(t)
	cfg.URL = "https://chosen.example"
	cfg.MarkExplicit("url")
	mustSave(t, cfg)
	if v, _ := readConfigMap(t, dir)["cloud_secret"].(string); v != "" {
		t.Errorf("config.toml cloud_secret = %q, want empty", v)
	}
}

func TestMarkExplicitPanicsOnUnknownKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MarkExplicit(\"no_such_key\") did not panic")
		}
	}()
	DefaultConfig().MarkExplicit("no_such_key")
}
