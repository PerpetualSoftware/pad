package usertz

import (
	"strings"
	"testing"
	_ "time/tzdata"
)

func TestValidate(t *testing.T) {
	for _, ok := range []string{"America/New_York", "UTC", "Pacific/Kiritimati"} {
		if _, err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", "Local", "Mars/Base", "America/New_York\x00", strings.Repeat("a", 65)} {
		if _, err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) accepted", bad)
		}
	}
}

func TestResolve(t *testing.T) {
	ny, mars, empty := "America/New_York", "Mars/Base", ""
	if z := Resolve(&ny); !z.Set || z.Name != ny || z.Loc.String() != ny {
		t.Errorf("set zone resolved to %+v", z)
	}
	for _, v := range []*string{nil, &mars, &empty} {
		if z := Resolve(v); z.Set || z.Name != "UTC" || z.Loc.String() != "UTC" {
			t.Errorf("unset/bad resolved to %+v", z)
		}
	}
	if !strings.Contains(Note(Resolve(nil)), "pad auth timezone") {
		t.Error("unset Note must name pad auth timezone")
	}
	if Note(Resolve(&ny)) != "" || Note(Resolve(nil)) == "" {
		t.Error("Note must be empty when set and non-empty when unset")
	}
}
