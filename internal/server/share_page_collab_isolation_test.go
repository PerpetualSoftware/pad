package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSharePageDoesNotImportCollab is a regression guard for TASK-1266
// (PLAN-1248 share-page read-only verification).
//
// The public share page at /s/{token} MUST keep rendering markdown
// directly (`marked` + `DOMPurify`) and MUST NOT open a WebSocket to
// the collab endpoint. Anonymous viewers can't authenticate; even if
// the upgrade succeeded, exposing per-item Y.Doc traffic to the public
// internet would be a security regression. The render path is already
// separate from the editor — this test fails fast if a future change
// accidentally drags collab into it via a stray import.
//
// We walk the import graph rooted at the share-page route so a future
// refactor that hides collab behind a sub-component (e.g. `import
// ShareContent from '$lib/share-content.svelte'`) is still caught —
// the bare-route check would otherwise miss it. Per Codex review
// round 2 [P2].
//
// External packages (node_modules) are not scanned: `marked` /
// `DOMPurify` are intentionally referenced by the share page, and
// scanning them would flood the test with their own internal symbol
// names. The forbidden list is targeted at our own source tree — if
// somebody adds `import { CollabProvider } from '@my/collab-pkg'`
// the npm-package contents wouldn't trip the substring check, but
// the import line itself would.
func TestSharePageDoesNotImportCollab(t *testing.T) {
	t.Parallel()

	// Resolve repo root from the test's CWD (internal/server/). Go
	// runs package tests from the package dir, so `../..` is reliable.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	webSrc := filepath.Join(repoRoot, "web", "src")
	rootFile := filepath.Join(webSrc, "routes", "s", "[token]", "+page.svelte")

	// These tokens must NEVER appear anywhere in the share-page
	// import closure. Each represents a hook into the collab
	// pipeline that a public viewer has no auth grant for and no
	// business opening. `Editor.svelte` is checked as a path suffix
	// rather than via the `$lib/...` alias because a relative import
	// (`../../../lib/components/editor/Editor.svelte`) would bypass
	// an alias-only check while still pulling the rich editor in.
	// `WebSocket` / `/api/v1/collab` cover the case where someone
	// inlines a hand-rolled collab client without going through the
	// `CollabProvider` class.
	forbidden := []string{
		"wsProvider",                      // CollabProvider class file
		"CollabProvider",                  // class itself
		"@tiptap/extension-collaboration", // Collaboration extension (also matches -caret)
		"@tiptap/y-tiptap",                // y-tiptap binding
		"'yjs'", `"yjs"`,                  // Y.Doc construction (both quote styles, quoted to avoid matching `dayjs`); per Codex round 4 NIT
		"y-protocols",    // wire format
		"Editor.svelte",  // rich editor mounts y-tiptap when ydoc is set; suffix-only so relative imports don't bypass
		"WebSocket",      // any hand-rolled WS client
		"/api/v1/collab", // any direct collab-endpoint hit
	}

	// Walk the import closure starting from the share-page route.
	visited := map[string]bool{}
	queue := []string{rootFile}
	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]
		if visited[path] {
			continue
		}
		visited[path] = true

		bytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(bytes)
		stripped := stripComments(src)

		for _, token := range forbidden {
			if strings.Contains(stripped, token) {
				t.Errorf(
					"share-page import closure file %s contains "+
						"forbidden token %q. The share-page render path "+
						"is read-only markdown via `marked` + `DOMPurify` "+
						"(TASK-1266); pulling in collab would expose "+
						"per-item Y.Doc traffic to anonymous viewers and "+
						"is a security regression. If you need to render "+
						"collab content, pass it through a server-side "+
						"snapshot of items.content, not by mounting the "+
						"editor.",
					path, token,
				)
			}
		}

		// Enqueue resolved imports.
		for _, spec := range extractImportSpecifiers(stripped) {
			resolved := resolveImport(spec, path, webSrc)
			if resolved != "" && !visited[resolved] {
				queue = append(queue, resolved)
			}
		}
	}

	// Positive assertions: the route file MUST still actually USE the
	// safe markdown renderer. We only check the route file (not the
	// closure) because requiring all closure files to use marked
	// would be nonsensical. Comments are stripped first so a
	// leftover `// marked(...)` comment can't satisfy the check.
	rootBytes, err := os.ReadFile(rootFile)
	if err != nil {
		t.Fatalf("re-read root file %s: %v", rootFile, err)
	}
	rootStripped := stripComments(string(rootBytes))
	requiredShapes := []struct {
		why  string
		need string
	}{
		{"marked import", "from 'marked'"},
		{"marked invocation", "marked("},
		{"DOMPurify import", "from 'dompurify'"},
		{"DOMPurify sanitize call", "DOMPurify.sanitize("},
	}
	for _, req := range requiredShapes {
		if !strings.Contains(rootStripped, req.need) {
			t.Errorf(
				"share-page route %s no longer contains %q (expected %s). "+
					"The read-only render path depends on the marked + "+
					"DOMPurify pipeline; if you've replaced the renderer, "+
					"update this test to assert on the new safe path.",
				rootFile, req.need, req.why,
			)
		}
	}
}

// stripComments removes JS/TS // line comments and /* ... */ block
// comments. Conservative — uses simple regex passes which won't
// correctly handle `//` inside string literals, but that's a
// false-positive risk for forbidden-token detection only (the test
// would error on a string that LOOKS like a forbidden import in a
// comment, which is fine — flag and review). Per Codex round 2 NIT.
func stripComments(src string) string {
	// Block comments first so // inside /* ... */ doesn't survive.
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	// Line comments.
	src = regexp.MustCompile(`//[^\n]*`).ReplaceAllString(src, "")
	return src
}

var (
	// Static imports: `import x from '...'`, `import { ... } from '...'`,
	// or side-effect `import '...'`. Also matches `from '...'` in
	// re-exports.
	staticImportRegex = regexp.MustCompile(`(?:import|from)\s+['"]([^'"]+)['"]`)
	// Dynamic imports: `import('...')` and `await import('...')`.
	// SvelteKit uses these for lazy route components, so a future
	// share-page refactor that lazy-loads a sub-component would
	// otherwise bypass the static-only walk. Per Codex round 3 [P2].
	dynamicImportRegex = regexp.MustCompile(`import\s*\(\s*['"]([^'"]+)['"]\s*\)`)
)

// extractImportSpecifiers pulls the module specifiers out of every
// import shape we know about: static `import ... from '...'`,
// side-effect `import '...'`, re-export `from '...'`, AND dynamic
// `import('...')`. Doesn't care about default vs named — the
// specifier is the only thing we need to follow. Both static and
// dynamic forms are covered so the closure walker can't be bypassed
// via lazy loading.
func extractImportSpecifiers(src string) []string {
	specs := make([]string, 0)
	for _, m := range staticImportRegex.FindAllStringSubmatch(src, -1) {
		if len(m) >= 2 {
			specs = append(specs, m[1])
		}
	}
	for _, m := range dynamicImportRegex.FindAllStringSubmatch(src, -1) {
		if len(m) >= 2 {
			specs = append(specs, m[1])
		}
	}
	return specs
}

// resolveImport maps a Svelte/TS module specifier to a file path
// inside the project's source tree, or "" for external packages /
// unresolvable paths.
//
//   - `$lib/foo` → web/src/lib/foo[.ts|.svelte|.svelte.ts]
//   - `./bar` / `../baz` → resolved relative to the importing file
//   - bare specifiers (e.g. `marked`, `@tiptap/...`) → "" (external)
//
// We probe a small set of conventional extensions and `index` files
// because Svelte/Vite resolution allows extension elision. If none
// match, return "" — likely a shimmed module the test can't follow,
// which is acceptable: any forbidden token reachable via that path
// will still surface in the leaf .svelte/.ts file when it gets
// imported by an in-tree consumer.
func resolveImport(spec, importerPath, webSrc string) string {
	var basePath string
	switch {
	case strings.HasPrefix(spec, "$lib/"):
		basePath = filepath.Join(webSrc, "lib", strings.TrimPrefix(spec, "$lib/"))
	case strings.HasPrefix(spec, "$app/"):
		// SvelteKit virtual modules — never our code, skip.
		return ""
	case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
		basePath = filepath.Join(filepath.Dir(importerPath), spec)
	default:
		// Bare specifier → npm package, not in our tree.
		return ""
	}

	// Try direct file with conventional extensions.
	candidates := []string{
		basePath,
		basePath + ".svelte",
		basePath + ".ts",
		basePath + ".js",
		basePath + ".svelte.ts",
		filepath.Join(basePath, "index.ts"),
		filepath.Join(basePath, "index.js"),
		filepath.Join(basePath, "index.svelte"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}
