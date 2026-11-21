package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/secret"
)

var secretType = reflect.TypeOf(secret.Secret{})

// persistedFields lists the toml-tagged fields of t in declaration order,
// skipping toml:"-".
func persistedFields(t reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// TestConfigFileMirrorsConfig guards the file DTO Save encodes: it must carry
// every persisted Config field, with the same toml tag, in the same order, and
// every credential as a plain string (a Secret would be encoded as REDACTED).
func TestConfigFileMirrorsConfig(t *testing.T) {
	cfgFields := persistedFields(reflect.TypeOf(Config{}))
	fileFields := persistedFields(reflect.TypeOf(configFile{}))
	if len(cfgFields) != len(fileFields) {
		t.Fatalf("Config has %d persisted fields, configFile has %d", len(cfgFields), len(fileFields))
	}
	secrets := 0
	for i, cf := range cfgFields {
		ff := fileFields[i]
		if cf.Tag.Get("toml") != ff.Tag.Get("toml") || cf.Name != ff.Name {
			t.Errorf("field %d: Config %s `%s`, configFile %s `%s`", i, cf.Name, cf.Tag.Get("toml"), ff.Name, ff.Tag.Get("toml"))
			continue
		}
		switch {
		case cf.Type == secretType:
			secrets++
			if ff.Type.Kind() != reflect.String {
				t.Errorf("%s is a Secret in Config but %s in configFile, want string", cf.Name, ff.Type)
			}
		case cf.Type != ff.Type:
			t.Errorf("%s: Config %s, configFile %s", cf.Name, cf.Type, ff.Type)
		}
	}
	if secrets != 7 {
		t.Errorf("Config has %d Secret fields, want 7", secrets)
	}
}

// TestConfigNeverRendersCredentials: a whole Config handed to fmt or slog
// shows none of its credentials.
func TestConfigNeverRendersCredentials(t *testing.T) {
	c := populatedConfig(t.TempDir())
	check := func(where, out string) {
		t.Helper()
		if strings.Contains(out, "fixture-SECRET") {
			t.Errorf("%s leaked a credential: %s", where, out)
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		check(verb, fmt.Sprintf(verb, c))
		check(verb+" value", fmt.Sprintf(verb, *c))
	}
	for _, mk := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var buf bytes.Buffer
		slog.New(mk(&buf)).Info("config", "cfg", c)
		check("slog", buf.String())
	}
}
