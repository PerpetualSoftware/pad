package config

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

type composeFile struct {
	Services map[string]struct {
		Environment map[string]string `yaml:"environment"`
	} `yaml:"services"`
}

func parseComposeFile(t *testing.T, path string) composeFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cf composeFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return cf
}

func parseEnvExample(t *testing.T, path string) (map[string]bool, []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	re := regexp.MustCompile(`^#?\s*(PAD_[A-Z0-9_]+|PUBLIC_URL)=`)
	vars := make(map[string]bool)
	var dupes []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		m := re.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		name := m[1]
		if vars[name] {
			dupes = append(dupes, name)
		}
		vars[name] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	slices.Sort(dupes)
	return vars, dupes
}

func TestComposeForwardsDocumentedEnvSettings(t *testing.T) {
	envExamplePath := filepath.Join("..", "..", ".env.example")
	baseComposePath := filepath.Join("..", "..", "docker-compose.yml")
	prodComposePath := filepath.Join("..", "..", "docker-compose.prod.yml")

	documented, dupes := parseEnvExample(t, envExamplePath)
	if len(documented) == 0 {
		t.Fatalf("no PAD_*/PUBLIC_URL variables found in %s", envExamplePath)
	}
	if len(dupes) > 0 {
		t.Errorf(".env.example documents duplicate variables: %v", dupes)
	}

	base := parseComposeFile(t, baseComposePath)
	padSvc, ok := base.Services["pad"]
	if !ok {
		t.Fatalf("%s has no services.pad", baseComposePath)
	}
	envMap := padSvc.Environment
	if len(envMap) == 0 {
		t.Fatalf("%s services.pad.environment is empty", baseComposePath)
	}

	// Variables documented in .env.example that docker-compose.yml deliberately
	// does not forward into the pad container.
	notForwarded := map[string]string{
		"PAD_DB_PATH":   "SQLite only; this compose stack runs Postgres",
		"PAD_MODE":      "CLI daemon discovery; cloud server mode uses PAD_CLOUD",
		"PAD_VERSION":   "compose-only image tag interpolation",
		"PAD_BIND_ADDR": "compose-only host port binding interpolation",
	}

	// Variables pinned by docker-compose.yml to match the container topology.
	pinnedByCompose := map[string]bool{
		"PAD_HOST":         true,
		"PAD_PORT":         true,
		"PAD_DB_DRIVER":    true,
		"PAD_DATABASE_URL": true,
		"PAD_REDIS_URL":    true,
		"PAD_DATA_DIR":     true,
	}

	// Every documented variable must be forwarded unless explicitly excluded.
	var missing []string
	for name := range documented {
		if _, excluded := notForwarded[name]; excluded {
			continue
		}
		if _, ok := envMap[name]; !ok {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("docker-compose.yml services.pad.environment is missing %d documented variables from .env.example: %v", len(missing), missing)
	}

	// Every non-pinned key in services.pad.environment must interpolate its own name.
	var badInterp []string
	for name, val := range envMap {
		if pinnedByCompose[name] {
			continue
		}
		want := "${" + name
		if !strings.Contains(val, want) {
			badInterp = append(badInterp, name+"="+val)
		}
	}
	slices.Sort(badInterp)
	if len(badInterp) > 0 {
		t.Errorf("docker-compose.yml services.pad.environment has non-pinned keys that do not interpolate their own name: %v", badInterp)
	}

	// Ensure exclusion and pinned sets stay in sync with .env.example and compose.
	for name := range notForwarded {
		if !documented[name] {
			t.Errorf("stale notForwarded entry %q: not documented in .env.example", name)
		}
		if _, present := envMap[name]; present {
			t.Errorf("contradictory notForwarded entry %q: present in docker-compose.yml services.pad.environment", name)
		}
	}
	for name := range pinnedByCompose {
		if !documented[name] && envMap[name] == "" {
			t.Errorf("stale pinnedByCompose entry %q: not documented in .env.example and absent from docker-compose.yml", name)
		}
		if _, present := envMap[name]; !present {
			t.Errorf("stale pinnedByCompose entry %q: not present in docker-compose.yml services.pad.environment", name)
		}
	}

	// Production override must keep forcing secure cookies.
	prod := parseComposeFile(t, prodComposePath)
	prodPad, ok := prod.Services["pad"]
	if !ok {
		t.Fatalf("%s has no services.pad", prodComposePath)
	}
	if got := prodPad.Environment["PAD_SECURE_COOKIES"]; got != "true" {
		t.Errorf("docker-compose.prod.yml services.pad.environment.PAD_SECURE_COOKIES = %q, want %q", got, "true")
	}
}
