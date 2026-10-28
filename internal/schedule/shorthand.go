package schedule

import (
	"sort"
	"strings"
)

var shorthands = map[string]string{
	"daily":        "FREQ=DAILY",
	"weekdays":     "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR",
	"weekly":       "FREQ=WEEKLY",
	"biweekly":     "FREQ=WEEKLY;INTERVAL=2",
	"monthly":      "FREQ=MONTHLY", // the day comes from Start; the 29th-31st skip short months
	"monthly-last": "FREQ=MONTHLY;BYMONTHDAY=-1",
	"quarterly":    "FREQ=MONTHLY;INTERVAL=3",
	"yearly":       "FREQ=YEARLY",
}

// Shorthand returns the rule for a cadence name such as "weekdays". The name
// is trimmed and case-insensitive.
func Shorthand(name string) (rule string, ok bool) {
	rule, ok = shorthands[strings.ToLower(strings.TrimSpace(name))]
	return rule, ok
}

// ShorthandNames lists the shorthand names, sorted, for help text.
func ShorthandNames() []string {
	names := make([]string, 0, len(shorthands))
	for n := range shorthands {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// NormalizeRule turns a shorthand or RRULE text into the canonical rule. One
// leading "RRULE:" and surrounding space are stripped. It checks grammar only;
// the checks that need a Start happen in Compile. Every refusal is a
// *MalformedError.
func NormalizeRule(in string) (string, error) {
	in = strings.TrimSpace(in)
	if r, ok := Shorthand(in); ok {
		return r, nil
	}
	if len(in) >= 6 && strings.EqualFold(in[:6], "RRULE:") {
		in = strings.TrimSpace(in[6:])
	}
	r, err := ParseRule(in)
	if err != nil {
		return "", err
	}
	return r.String(), nil
}
