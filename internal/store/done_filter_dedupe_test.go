package store

import (
	"reflect"
	"testing"
)

// TestResolveCollectionDoneFilterDedupesOptions: a select that repeats an
// option (in any case) offers that terminal once, so close_options never
// lists a value twice (TASK-4537).
func TestResolveCollectionDoneFilterDedupesOptions(t *testing.T) {
	schema := `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done","Done","done"]}]}`
	f := resolveCollectionDoneFilter("c1", schema, `{}`)
	if want := []string{"done"}; !reflect.DeepEqual(f.writableTerminals, want) {
		t.Fatalf("writableTerminals = %#v, want %#v", f.writableTerminals, want)
	}
	if want := []string{"done"}; !reflect.DeepEqual(f.closeOptions(), want) {
		t.Fatalf("closeOptions() = %#v, want %#v", f.closeOptions(), want)
	}
}
