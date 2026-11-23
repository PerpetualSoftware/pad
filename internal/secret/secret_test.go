package secret_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/logging"
	"github.com/PerpetualSoftware/pad/internal/secret"
)

const fixture = "fixture-SECRET-hunter2"

type exported struct {
	Name string
	Key  secret.Secret
}

type unexported struct {
	name string
	key  secret.Secret
}

func assertHidden(t *testing.T, where, out string) {
	t.Helper()
	if strings.Contains(out, fixture) || strings.Contains(out, "hunter2") {
		t.Errorf("%s leaked the secret: %s", where, out)
	}
}

func TestSecretNeverRendersThroughFmt(t *testing.T) {
	s := secret.New(fixture)
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"}
	values := map[string]any{
		"value":      s,
		"pointer":    &s,
		"exported":   exported{Name: "n", Key: s},
		"exported*":  &exported{Name: "n", Key: s},
		"unexported": unexported{name: "n", key: s},
		"slice":      []secret.Secret{s, s},
		"map":        map[string]secret.Secret{"k": s},
	}
	for name, v := range values {
		for _, verb := range verbs {
			assertHidden(t, name+" "+verb, fmt.Sprintf(verb, v))
		}
	}
	for _, verb := range []string{"%v", "%s", "%q"} {
		if got := fmt.Sprintf(verb, s); !strings.Contains(got, secret.Redacted) {
			t.Errorf("Sprintf(%s) = %q, want it to contain %s", verb, got, secret.Redacted)
		}
	}
	if got := fmt.Sprintf("%+v", exported{Name: "n", Key: s}); got != "{Name:n Key:REDACTED}" {
		t.Errorf("%%+v of an exported field = %q", got)
	}
	if got := fmt.Sprintf("%#v", s); got != `secret.Secret("REDACTED")` {
		t.Errorf("%%#v = %q", got)
	}
	if got := s.String(); got != secret.Redacted {
		t.Errorf("String() = %q", got)
	}
}

func TestSecretThroughSlogHandlers(t *testing.T) {
	s := secret.New(fixture)
	type holder struct {
		Name   string
		Secret secret.Secret
		inner  secret.Secret
	}
	h := holder{Name: "n", Secret: s, inner: s}
	emit := func(l *slog.Logger) {
		l.Info("top", "key", s)
		l.Info("pointer", "key", &s)
		l.Info("group", slog.Group("g", "key", s, "other", 1))
		l.Info("slice", "keys", []secret.Secret{s, s})
		l.Info("struct", "holder", h)
		l.Info("struct pointer", "holder", &h)
		l.With("bound", s).Info("with")
		l.Info("error", "err", fmt.Errorf("wrap: %v", s))
	}
	for _, format := range []string{"json", "text"} {
		t.Run("plain "+format, func(t *testing.T) {
			var buf bytes.Buffer
			var handler slog.Handler
			if format == "json" {
				handler = slog.NewJSONHandler(&buf, nil)
			} else {
				handler = slog.NewTextHandler(&buf, nil)
			}
			emit(slog.New(handler))
			assertHidden(t, format, buf.String())
			if !strings.Contains(buf.String(), secret.Redacted) {
				t.Errorf("no %s marker in %s", secret.Redacted, buf.String())
			}
		})
		t.Run("logging "+format, func(t *testing.T) {
			prev := slog.Default()
			t.Cleanup(func() { slog.SetDefault(prev) })
			var buf bytes.Buffer
			logging.SetupWriter(&buf, "debug", format)
			emit(slog.Default())
			assertHidden(t, format, buf.String())
			if !strings.Contains(buf.String(), secret.Redacted) {
				t.Errorf("no %s marker in %s", secret.Redacted, buf.String())
			}
		})
	}
}

func TestSecretJSON(t *testing.T) {
	type body struct {
		Password secret.Secret `json:"password"`
	}
	out, err := json.Marshal(body{Password: secret.New(fixture)})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"password":"REDACTED"}` {
		t.Errorf("Marshal = %s", out)
	}
	text, err := secret.New(fixture).MarshalText()
	if err != nil || string(text) != secret.Redacted {
		t.Errorf("MarshalText = %q, %v", text, err)
	}

	var in body
	if err := json.Unmarshal([]byte(`{"password":"`+fixture+`"}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.Password.Reveal() != fixture {
		t.Errorf("Unmarshal string = %q", in.Password.Reveal())
	}
	if err := json.Unmarshal([]byte(`{"password":null}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.Password.Reveal() != fixture {
		t.Errorf("null changed the value to %q", in.Password.Reveal())
	}

	// A wrong type is refused with the type error a plain string field gives:
	// same JSON value kind, same Go type. Only the field path can differ,
	// because encoding/json adds it to an UnmarshalJSON method's error on
	// some Go releases and not others.
	var wantErr error
	{
		type body struct {
			Password string `json:"password"`
		}
		var p body
		wantErr = json.Unmarshal([]byte(`{"password":42}`), &p)
	}
	gotErr := json.Unmarshal([]byte(`{"password":42}`), &in)
	var want, got *json.UnmarshalTypeError
	if !errors.As(wantErr, &want) || !errors.As(gotErr, &got) || got.Value != want.Value || got.Type != want.Type {
		t.Errorf("number error = %v, want the type error of %v", gotErr, wantErr)
	}

	var u secret.Secret
	if err := u.UnmarshalText([]byte(fixture)); err != nil || u.Reveal() != fixture {
		t.Errorf("UnmarshalText = %q, %v", u.Reveal(), err)
	}
}

func TestSecretErrors(t *testing.T) {
	s := secret.New(fixture)
	base := errors.New("base")
	for _, err := range []error{
		fmt.Errorf("v: %v", s),
		fmt.Errorf("s: %s", s),
		fmt.Errorf("w: %w (%v)", base, s),
		errors.Join(base, fmt.Errorf("%v", s)),
	} {
		assertHidden(t, "error", err.Error())
		if !strings.Contains(err.Error(), secret.Redacted) {
			t.Errorf("error %q has no marker", err)
		}
	}
	func() {
		defer func() {
			r := recover()
			assertHidden(t, "panic", fmt.Sprint(r))
		}()
		panic(s)
	}()
}

func TestSecretZeroAndReveal(t *testing.T) {
	var zero secret.Secret
	if !zero.IsZero() || zero.Reveal() != "" {
		t.Errorf("zero Secret: IsZero=%v Reveal=%q", zero.IsZero(), zero.Reveal())
	}
	if !secret.New("").IsZero() {
		t.Error(`New("") is not zero`)
	}
	s := secret.New(fixture)
	if s.IsZero() || s.Reveal() != fixture {
		t.Errorf("New: IsZero=%v Reveal=%q", s.IsZero(), s.Reveal())
	}
	if !s.Equal(fixture) || s.Equal(fixture+"x") || s.Equal("") || s.Equal(fixture[:len(fixture)-1]) {
		t.Error("Equal disagrees with string equality")
	}
	if !zero.Equal("") || zero.Equal("x") {
		t.Error("zero Equal disagrees with string equality")
	}
	// Secret is deliberately not comparable (a [0]func() field), so
	// secret.New("a") == secret.New("a") does not compile: == would compare
	// pointers and report equal secrets as different. Use Equal.
}
