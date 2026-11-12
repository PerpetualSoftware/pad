package ics

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestICSGolden(t *testing.T) {
	c := Calendar{Name: "Pad (alice): Work; personal,\\home\nnext", Agent: true, Now: time.Date(2031, 3, 10, 12, 0, 0, 0, time.UTC), Events: []Event{
		{UID: "schedule.2031-03-10T09:00", Summary: "TASK-1 Timed", URL: "https://pad.example/alice/work/tasks/task-1", Target: "2031-03-10T09:00:00+02:00", End: "2031-03-10T10:00:00+02:00", Deadline: "2031-03-11T12:00:00Z"},
		{UID: "item.due_date.2031-03-10", Summary: "TASK-2 All-day; comma, slash\\\nline", Target: "2031-03-10", Deadline: "2031-03-12"},
		{UID: "long", Summary: "TASK-3 " + strings.Repeat("界é", 24), Target: "2031-03-13"},
	}}
	var b bytes.Buffer
	if err := Write(&b, c); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/calendar.ics")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Fatalf("golden differs:\n%s", b.String())
	}
	if strings.Contains(strings.ReplaceAll(b.String(), "\r\n", ""), "\n") {
		t.Fatal("bare LF")
	}
	for _, line := range strings.Split(b.String(), "\r\n") {
		if len(line) > 75 || !utf8.ValidString(line) {
			t.Fatalf("invalid folded line: %q", line)
		}
	}
	if strings.Contains(b.String(), "VTIMEZONE") || strings.Contains(b.String(), "DESCRIPTION") {
		t.Fatal("unexpected properties")
	}
}
