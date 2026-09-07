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
// Single word: first 3-5 letters, removing trailing "s" for plurals
// Multi-word: first letter of each word, capped at 5 chars
//
// THE RESULT IS A-Z ONLY, and that is a hard constraint rather than a style
// choice (BUG-2943). NOTE THE SCOPE: this function is one of four doors that
// can put a prefix on a collection, and it is the only one that enforces the
// constraint. An EXPLICIT prefix — `collection create --prefix`, `collection
// update --prefix`, the HTTP/MCP `prefix` field, or a workspace import — is
// still stored verbatim and unvalidated (codex round 1 [P2] on this unit,
// with the call sites named on BUG-2943's trail). So this makes DERIVED
// prefixes safe; it does not make the invariant hold. `parseItemRef` (internal/store/items.go) resolves a
// PREFIX-NUMBER ref only when every prefix character is A-Z, and falls
// through to a slug lookup otherwise — so a prefix carrying anything else
// makes every item in that collection unresolvable by the issue ID the
// product itself prints. Measured: a collection named "TEMP Rook A 2870" got
// the prefix "TRA2", `pad item show TRA2-2942` answered "item not found",
// and only the slug worked.
//
// This used to take the first BYTE of each word (`strings.ToUpper(w)[0]`),
// which admitted digits, punctuation and — worse, because the result is not
// even valid text — the lead byte of a multi-byte rune, so a collection named
// in most non-Latin scripts produced a broken prefix. Non-letters are now
// skipped rather than mapped: there is no honest A-Z substitute for "2" or
// for "Ω", and inventing one would put a character in the ID that is in
// nobody's collection name.
//
// A name with no ASCII letters at all yields "", which the caller
// (store.CreateCollection) turns into the existing "ITEM" fallback. That is
// deliberately the caller's decision and not this function's: it is the layer
// that knows a prefix is mandatory.
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
		word := keepASCIILetters(words[0])
		if word == "" {
			return ""
		}
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

	// Multi-word: first LETTER of each word. A word contributing no letter
	// ("2870", "—") is skipped entirely rather than contributing a character
	// parseItemRef would reject.
	var prefix strings.Builder
	for _, w := range words {
		if prefix.Len() >= 5 {
			break
		}
		if letters := keepASCIILetters(w); letters != "" {
			prefix.WriteByte(letters[0])
		}
	}
	if prefix.Len() == 0 {
		// Every word was letterless. Fall back to the single-word treatment
		// of the whole name, which also yields "" here — stated explicitly so
		// the empty return is a decision rather than a path nobody considered.
		return ""
	}
	return prefix.String()
}

// IsValidPrefix reports whether s is a well-formed collection prefix: an
// uppercase ASCII letter followed by uppercase letters or digits.
//
// ONE DEFINITION, used by every door that can put a prefix on a collection
// (derive, explicit create, update, import) AND by the ref parser that has to
// resolve IDs built from it (internal/store parseItemRef). BUG-2943 happened
// because there were two implicit definitions — the generator admitted any
// first byte, the parser accepted only A-Z — and the disagreement surfaced at
// READ time, on an identifier the product itself had minted and printed.
// Anything that decides what a prefix may contain calls this, or the two
// definitions start drifting again.
//
// Digits are permitted after the first character, which is what lets an
// existing collection carrying a prefix like "AB1" resolve its items the
// moment this ships — no migration, no rewriting an identifier a user's other
// records may reference. The first character must be a LETTER so a ref can
// never begin with a digit, keeping `PREFIX-NUMBER` unambiguous to read.
func IsValidPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			// always fine
		case r >= '0' && r <= '9':
			if i == 0 {
				return false // a prefix may not START with a digit
			}
		default:
			return false
		}
	}
	return true
}

// keepASCIILetters uppercases s and drops every character that is not A-Z.
// ASCII-only on purpose: the prefix has to satisfy parseItemRef's A-Z test,
// and there is no faithful mapping from a non-Latin letter into that range.
func keepASCIILetters(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
