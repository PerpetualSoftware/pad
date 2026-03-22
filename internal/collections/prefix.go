package collections

import "strings"

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
