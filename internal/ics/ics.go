// Package ics writes the title-only iCalendar representation of an agenda.
package ics

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

type Calendar struct {
	Name   string
	Agent  bool
	Now    time.Time
	Events []Event
}

type Event struct {
	// UID is the local part; the writer adds @pad and removes unsafe characters.
	UID      string
	Summary  string
	URL      string
	Target   string
	End      string
	Deadline string
}

func (e Event) Count() int {
	if e.Deadline != "" && e.Deadline != e.Target {
		return 2
	}
	return 1
}

// Write emits RFC 5545 content lines, folded at 75 octets including the
// continuation space. Only title, reference, link and dates are published.
func Write(w io.Writer, c Calendar) error {
	b := bufio.NewWriter(w)
	line := func(s string) error {
		for len(s) > 75 {
			n := 75
			for !utf8.RuneStart(s[n]) {
				n--
			}
			if _, err := b.WriteString(s[:n] + "\r\n"); err != nil {
				return err
			}
			s = " " + s[n:]
		}
		_, err := b.WriteString(s + "\r\n")
		return err
	}
	for _, s := range []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Perpetual Software LLC//Pad//EN", "CALSCALE:GREGORIAN", "METHOD:PUBLISH", "X-WR-CALNAME:" + escape(c.Name)} {
		if err := line(s); err != nil {
			return err
		}
	}
	if c.Agent {
		if err := line("COLOR:mediumpurple"); err != nil {
			return err
		}
	}
	for _, e := range c.Events {
		if err := writeEvent(line, e, c.Now); err != nil {
			return err
		}
		if e.Count() == 2 {
			e.UID += ".deadline"
			e.Summary = "Deadline: " + e.Summary
			e.Target, e.End = e.Deadline, ""
			if err := writeEvent(line, e, c.Now); err != nil {
				return err
			}
		}
	}
	if err := line("END:VCALENDAR"); err != nil {
		return err
	}
	return b.Flush()
}
func writeEvent(line func(string) error, e Event, now time.Time) error {
	uid := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, e.UID)
	lines := []string{"BEGIN:VEVENT", "UID:" + uid + "@pad", "DTSTAMP:" + now.UTC().Format("20060102T150405Z"), "SUMMARY:" + escape(e.Summary)}
	if len(e.Target) == 10 {
		d, err := time.Parse("2006-01-02", e.Target)
		if err != nil {
			return fmt.Errorf("ics: invalid event date")
		}
		lines = append(lines, "DTSTART;VALUE=DATE:"+d.Format("20060102"), "DTEND;VALUE=DATE:"+d.AddDate(0, 0, 1).Format("20060102"))
	} else {
		d, err := time.Parse(time.RFC3339, e.Target)
		if err != nil {
			return fmt.Errorf("ics: invalid event instant")
		}
		lines = append(lines, "DTSTART:"+d.UTC().Format("20060102T150405Z"))
		if e.End != "" {
			end, err := time.Parse(time.RFC3339, e.End)
			if err != nil {
				return fmt.Errorf("ics: invalid event end")
			}
			lines = append(lines, "DTEND:"+end.UTC().Format("20060102T150405Z"))
		}
	}
	if e.URL != "" {
		lines = append(lines, "URL:"+strings.NewReplacer("\r", "", "\n", "").Replace(e.URL))
	}
	lines = append(lines, "END:VEVENT")
	for _, s := range lines {
		if err := line(s); err != nil {
			return err
		}
	}
	return nil
}
func escape(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.NewReplacer("\\", "\\\\", ";", "\\;", ",", "\\,", "\n", "\\n").Replace(s)
}
