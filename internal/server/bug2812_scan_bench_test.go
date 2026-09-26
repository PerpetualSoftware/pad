package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BenchmarkPerf2812 is the instrument behind BUG-2812's before/after table: the
// whole request decode (body scan plus typed decode, decodeJSONBytes) on a
// small typical PATCH body and on two 1.54 MiB shapes, each with and without
// the harmless doubled-backslash text that makes the scan look for NULs. Run
// it against two trees INTERLEAVED on a quiet box; a sequential pair on a
// loaded one measured noise larger than the difference.
func perfProse(trigger bool) []byte {
	var b strings.Builder
	b.WriteString(`{"title":"t","fields_patch":{"status":"open"},"content":"`)
	para := strings.Repeat("lorem ipsum dolor sit amet ", 40)
	for b.Len() < 1_540_000 {
		b.WriteString(para)
		b.WriteString(`\n`)
	}
	if trigger {
		b.WriteString(`\\u0000`)
	}
	b.WriteString(`"}`)
	return []byte(b.String())
}

func perfItems(trigger bool) []byte {
	var b bytes.Buffer
	b.WriteString(`{"items":[`)
	for i := 0; b.Len() < 1_540_000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"title":"item","status":"open","priority":"low","tags":["a","b"],"fields":"{\"status\":\"open\",\"n\":1}","content":"short body"}`)
	}
	if trigger {
		b.WriteString(`,{"title":"note \\u0000"}`)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

func perfSmall(trigger bool) []byte {
	c := "moving on"
	if trigger {
		c = `the \\u0000 escape`
	}
	return []byte(`{"title":"Fix the thing","fields_patch":{"status":"in-progress","priority":"high"},"comment":"` + c + `"}`)
}

func BenchmarkPerf2812(b *testing.B) {
	type items struct {
		Items []models.ItemCreate `json:"items"`
	}
	for _, trig := range []bool{false, true} {
		tn := "plain"
		if trig {
			tn = "trigger"
		}
		for _, c := range []struct {
			name string
			body []byte
			dst  func() any
		}{
			{"small", perfSmall(trig), func() any { return &models.ItemUpdate{} }},
			{"prose-1.54MiB", perfProse(trig), func() any { return &models.ItemUpdate{} }},
			{"members-1.54MiB", perfItems(trig), func() any { return &items{} }},
		} {
			b.Run(c.name+"/"+tn, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := decodeJSONBytes(c.body, c.dst()); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
