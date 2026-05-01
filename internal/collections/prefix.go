package collections

import "strings"

// NormalizeSlug maps the common singular / shorthand collection-name
// forms users actually type ("task", "idea", "doc", etc.) to the
// canonical plural slug stored in the database. Unknown inputs pass
// through unchanged so custom collections aren't broken.
//
// Both the CLI's `pad item create / list / move` flows and the MCP
// HTTPHandlerDispatcher route table call this. Without a shared
// implementation, `pad item create task ...` works through the
// subprocess CLI but 404s through the in-process HTTP dispatcher
// (caught on PR #343 review round 3).
func NormalizeSlug(input string) string {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "task", "t":
		return "tasks"
	case "idea", "i":
		return "ideas"
	case "plan", "p", "phase", "phases":
		return "plans"
	case "doc", "d":
		return "docs"
	case "bug":
		return "bugs"
	case "convention":
		return "conventions"
	case "playbook":
		return "playbooks"
	}
	return input
}

// DerivePrefix generates a short uppercase prefix from a collection name.
// Single word: first 3-5 chars, removing trailing "s" for plurals
// Multi-word: first letter of each word, capped at 5 chars
func DerivePrefix(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	// Split into words
	words := strings.FieldsFunc(name, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_'
	})

	if len(words) == 1 {
		word := strings.ToUpper(words[0])
		// Remove trailing S for plurals
		if len(word) > 3 && strings.HasSuffix(word, "S") {
			word = word[:len(word)-1]
		}
		// Cap at 5 chars
		if len(word) > 5 {
			word = word[:5]
		}
		return word
	}

	// Multi-word: first letter of each word
	var prefix strings.Builder
	for _, w := range words {
		if len(w) > 0 && prefix.Len() < 5 {
			prefix.WriteByte(strings.ToUpper(w)[0])
		}
	}
	return prefix.String()
}
