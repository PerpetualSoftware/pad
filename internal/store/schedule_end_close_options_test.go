package store

import (
	"reflect"
	"testing"
)

// TestCloseOptionsAbandoningOnlyField pins the close options a refused last
// done or skip offers (TASK-4537): delivering values in field order, and the
// abandoning values when the field has no delivering close, so the refusal
// never offers an empty list for an item that can close.
func TestCloseOptionsAbandoningOnlyField(t *testing.T) {
	cases := []struct {
		name string
		f    collectionDoneFilter
		want []string
	}{
		{
			name: "delivering values only, abandoning left out",
			f:    collectionDoneFilter{writableTerminals: []string{"done", "cancelled"}, abandoned: []string{"cancelled"}},
			want: []string{"done"},
		},
		{
			name: "abandoning-only field offers its abandoning values",
			f:    collectionDoneFilter{writableTerminals: []string{"cancelled", "wontfix"}, abandoned: []string{"Cancelled", "wontfix"}},
			want: []string{"cancelled", "wontfix"},
		},
		{
			name: "no terminal the field can hold",
			f:    collectionDoneFilter{},
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.f.closeOptions(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("closeOptions() = %#v, want %#v", got, tc.want)
			}
		})
	}
}
