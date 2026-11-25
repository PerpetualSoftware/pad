package server

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/textguard"
)

func classifyExportDecodeErr(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, errJSONBodyNUL) {
		return "nul"
	}
	var rep *models.RepeatedMemberError
	if errors.As(err, &rep) {
		return "repeat:" + rep.Member
	}
	if errors.Is(err, io.EOF) {
		return "eof"
	}
	return "syntax"
}

func TestDecodeExportStream_ChecksMatchWholeBody(t *testing.T) {
	t.Parallel()

	corpus := []struct {
		name string
		raw  string
	}{
		{
			name: "valid export",
			raw:  `{"version":1,"workspace":{"name":"OK","slug":"ok"},"collections":[{"id":"c1","name":"Tasks","slug":"tasks","schema":"{}","settings":"{}"}],"items":[{"id":"i1","collection_id":"c1","title":"One","slug":"one","content":"hello","fields":"{}","tags":"[]"}]}`,
		},
		{
			name: "NUL in item content",
			raw:  `{"version":1,"workspace":{"name":"N","slug":"n"},"items":[{"id":"i1","title":"T","slug":"t","content":"a` + textguard.EscNUL + `b","fields":"{}","tags":"[]"}]}`,
		},
		{
			name: "NUL in item fields as JSON string",
			raw:  `{"version":1,"workspace":{"name":"N","slug":"n"},"items":[{"id":"i1","title":"T","slug":"t","content":"ok","fields":"{\"note\":\"x\\` + textguard.EscNUL + `y\"}","tags":"[]"}]}`,
		},
		{
			name: "NUL in nested key",
			raw:  `{"version":1,"workspace":{"name":"N","slug":"n"},"items":[{"id":"i1","bad` + textguard.EscNUL + `key":"v","title":"T","slug":"t","content":"ok","fields":"{}","tags":"[]"}]}`,
		},
		{
			name: "NUL in top-level key",
			raw:  `{"version":1,"workspace":{"name":"N","slug":"n"},"bad` + textguard.EscNUL + `key":"v"}`,
		},
		{
			name: "repeat inside an item",
			raw:  `{"version":1,"workspace":{"name":"R","slug":"r"},"items":[{"id":"i1","title":"first","title":"second"}]}`,
		},
		{
			name: "top-level folded repeat",
			raw:  `{"version":1,"Version":2,"workspace":{"name":"R","slug":"r"}}`,
		},
		{
			name: "unknown top-level fields key with nested NUL",
			raw:  `{"version":1,"workspace":{"name":"F","slug":"f"},"fields":"{\"a\":\"x\\` + textguard.EscNUL + `y\"}"}`,
		},
		{
			name: "trailing junk token",
			raw:  `{"version":1,"workspace":{"name":"J","slug":"j"}} trailing`,
		},
		{
			name: "trailing second object",
			raw:  `{"version":1,"workspace":{"name":"J","slug":"j"}} {}`,
		},
		{
			name: "empty body",
			raw:  ``,
		},
		{
			name: "whitespace-only body",
			raw:  " \t\r\n ",
		},
	}

	for _, tc := range corpus {
		t.Run(tc.name+"/strict", func(t *testing.T) {
			var want models.WorkspaceExport
			wantErr := decodeJSONBytes([]byte(tc.raw), &want)

			got, gotErr := decodeExportStream(bytes.NewReader([]byte(tc.raw)), nil)
			if classifyExportDecodeErr(gotErr) != classifyExportDecodeErr(wantErr) {
				t.Fatalf("strict error class mismatch:\n got: %s (%v)\nwant: %s (%v)",
					classifyExportDecodeErr(gotErr), gotErr,
					classifyExportDecodeErr(wantErr), wantErr)
			}
			if wantErr == nil && !reflect.DeepEqual(*got, want) {
				t.Fatalf("strict struct mismatch:\n got: %+v\nwant: %+v", *got, want)
			}
		})

		t.Run(tc.name+"/repair", func(t *testing.T) {
			wantTally := &nulRepairTally{Enabled: true}
			var want models.WorkspaceExport
			wantErr := decodeJSONBytes(wantTally.Apply([]byte(tc.raw)), &want)

			gotTally := &nulRepairTally{Enabled: true}
			got, gotErr := decodeExportStream(bytes.NewReader([]byte(tc.raw)), gotTally)
			if classifyExportDecodeErr(gotErr) != classifyExportDecodeErr(wantErr) {
				t.Fatalf("repair error class mismatch:\n got: %s (%v)\nwant: %s (%v)",
					classifyExportDecodeErr(gotErr), gotErr,
					classifyExportDecodeErr(wantErr), wantErr)
			}
			if wantErr == nil {
				if !reflect.DeepEqual(*got, want) {
					t.Fatalf("repair struct mismatch:\n got: %+v\nwant: %+v", *got, want)
				}
				if gotTally.Replaced != wantTally.Replaced {
					t.Errorf("repair count mismatch: got %d, want %d", gotTally.Replaced, wantTally.Replaced)
				}
			}
		})
	}
}
