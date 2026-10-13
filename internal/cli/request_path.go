package cli

import (
	"fmt"
	"strings"
)

// checkRequestPath refuses an API path that would route somewhere other than
// the segments it was built from (BUG-3645).
//
// Every builder escapes its dynamic segments with url.PathEscape, so a
// workspace or ref carrying '/', '?' or '#' stays inside its segment. This is
// the backstop for a builder that does not: the CLI would otherwise send an
// item delete for workspace "victim/#" to /workspaces/victim/ (the rest is a
// fragment, never sent), and the server deletes the workspace. It also covers
// what escaping cannot: PathEscape leaves "." and ".." unchanged, and a proxy
// or server that cleans the path then lands on a different route.
//
// Refused: a raw '#' anywhere; an empty, "." or ".." segment in the path; a
// raw '/' in the query, which no builder emits (query values go through
// url.Values.Encode or url.QueryEscape) and which is what a '?' smuggled into
// a segment leaves behind.
func checkRequestPath(path string) error {
	if strings.ContainsRune(path, '#') {
		return fmt.Errorf("refusing request: path %q contains '#'", path)
	}
	p, query, hasQuery := strings.Cut(path, "?")
	if hasQuery && strings.ContainsRune(query, '/') {
		return fmt.Errorf("refusing request: path %q has a '/' after its '?'", path)
	}
	if p == "" {
		return nil
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		switch seg {
		case "":
			return fmt.Errorf("refusing request: path %q has an empty segment", path)
		case ".", "..":
			return fmt.Errorf("refusing request: path %q has a %q segment", path, seg)
		}
	}
	return nil
}
