package collections

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/acceptance"
)

// An unedited spec has an acceptance-criteria section whose every line is a
// placeholder: the parser must read the template's own skeleton that way, or
// a fresh spec would report two real criteria (PLAN-3598).
func TestSpecTemplateAcceptanceCriteriaAreAllPlaceholders(t *testing.T) {
	got := acceptance.Parse(specContentTemplate)
	if !got.Found {
		t.Fatal("spec template has no acceptance-criteria section")
	}
	if len(got.Criteria) != 0 {
		t.Fatalf("template skeleton read as real criteria: %#v", got.Criteria)
	}
	if want := []string{"AC-1", "AC-2"}; !reflect.DeepEqual(got.Placeholders, want) {
		t.Fatalf("Placeholders = %#v, want %#v", got.Placeholders, want)
	}
}
