package server

import (
	"strings"
)

// contentDisposition builds a Content-Disposition header value that carries
// name faithfully (BUG-3190).
//
// A name of printable ASCII with no quote or backslash is written as
// `filename="name"` alone: byte-identical to the `%q` form every site used,
// which is the same string for such a name. Any other name gets RFC 6266's
// two parameters: an ASCII fallback in `filename`, for clients that do not
// read the extended form, and the exact name in `filename*` (RFC 8187: the
// UTF-8 charset, an empty language tag, the percent-encoded bytes). A client
// that reads both
// prefers `filename*`; Go's mime.ParseMediaType, which the CLI and both MCP
// transports use, does exactly that.
//
// What this replaces was `filename=%q`, which put raw UTF-8 in a parameter
// defined as ISO-8859-1 and, worse, wrote Go escape TEXT for any rune
// strconv.IsPrint rejects: a stored "a<NBSP>b.txt" reached every Go client
// as the characters `a b.txt`, a different name carrying a backslash,
// which a Windows client reads as a path separator.
//
// The caller passes the name it means to serve; sanitising it (the attachment
// download drops control, quote and backslash runes) stays the caller's job.
func contentDisposition(disposition, name string) string {
	if plainHeaderFilename(name) {
		return disposition + `; filename="` + name + `"`
	}
	return disposition + `; filename="` + asciiFilenameFallback(name) + `"; filename*=UTF-8''` + encodeRFC8187(name)
}

// plainHeaderFilename reports whether name can travel as a quoted-string
// with no escaping and no loss: printable ASCII, no quote, no backslash.
func plainHeaderFilename(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return false
		}
	}
	return true
}

// asciiFilenameFallback replaces every rune plainHeaderFilename would reject
// with '_', so an ASCII extension survives ("会議.pdf" → "__.pdf").
func asciiFilenameFallback(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if r >= 0x20 && r <= 0x7e && r != '"' && r != '\\' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// encodeRFC8187 percent-encodes every byte of s that is not an RFC 8187
// attr-char (ALPHA / DIGIT / "!#$&+-.^_`|~").
func encodeRFC8187(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) * 3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') ||
			strings.IndexByte("!#$&+-.^_`|~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}
