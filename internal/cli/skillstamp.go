package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
)

// A skill file pad writes ends with one marker line (BUG-3466):
//
//	<!-- pad:skill v=<pad version> sha256=<hash of everything above> -->
//
// The file then says by itself whether it was edited (its body no longer
// hashes to the stamp) and which pad wrote it, which is what lets pad keep an
// edited skill and refuse to downgrade a newer one. It has to live in the
// file: the installation registry is per machine, and the case that matters
// most is a teammate's copy committed to the repo. An HTML comment is
// invisible in every tool's Markdown, and at the end it is clear of Claude
// Code's frontmatter.

const skillStampPrefix = "<!-- pad:skill "

var skillStampLine = regexp.MustCompile(`^<!-- pad:skill v=(\S+) sha256=([0-9a-f]{64}) -->$`)

// normalizeSkill is what every hash is taken over: CRLF made LF and trailing
// newlines dropped, so a Windows checkout (core.autocrlf) or an editor that
// adds a final newline does not read as an edit forever.
func normalizeSkill(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.TrimRight(b, "\n")
}

func skillHash(b []byte) string {
	sum := sha256.Sum256(normalizeSkill(b))
	return hex.EncodeToString(sum[:])
}

// StampSkill returns content with the marker line appended for version.
func StampSkill(content []byte, version string) []byte {
	body := normalizeSkill(content)
	var out bytes.Buffer
	out.Write(body)
	out.WriteString("\n" + skillStampPrefix + "v=" + stampVersion(version) + " sha256=" + skillHash(body) + " -->\n")
	return out.Bytes()
}

// stampVersion keeps the marker one token: a version with whitespace (never a
// real one) is written as "unknown".
func stampVersion(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\r\n") {
		return "unknown"
	}
	return v
}

// parseSkillStamp splits a file into its body and its marker. ok is false when
// the last line with any non-whitespace on it is not a well-formed marker.
func parseSkillStamp(file []byte) (body []byte, ver, hash string, ok bool) {
	// Whitespace after the stamp (an editor's trailing blank line, spaces on
	// the stamp line) must not hide it (codex r5). Trimmed only to FIND the
	// stamp; the body before it is hashed as it stands.
	n := bytes.TrimRight(normalizeSkill(file), " \t\r\n")
	i := bytes.LastIndexByte(n, '\n')
	last := n[i+1:]
	m := skillStampLine.FindSubmatch(last)
	if m == nil {
		return nil, "", "", false
	}
	if i < 0 {
		return nil, string(m[1]), string(m[2]), true
	}
	return n[:i], string(m[1]), string(m[2]), true
}

// SkillAction is what a door does with an existing (or missing) skill file.
type SkillAction int

const (
	// SkillInstall: nothing there; write it.
	SkillInstall SkillAction = iota
	// SkillUpdate: pad's own unedited text from this or an older pad (or an
	// unstamped copy a past release wrote); replace it quietly.
	SkillUpdate
	// SkillUnchanged: already this text.
	SkillUnchanged
	// SkillKeepEdited: the file is not what any pad wrote; keep it.
	SkillKeepEdited
	// SkillKeepNewer: a newer pad wrote it; keep it rather than downgrade.
	SkillKeepNewer
)

// SkillDecision is DecideSkillWrite's answer.
type SkillDecision struct {
	Action SkillAction
	// StampVersion is the version in the existing file's marker, when it has one.
	StampVersion string
}

// DecideSkillWrite decides what to do with a tool's skill file. expected is
// FormatForTool's unstamped output for this pad; running is this pad's
// version. Every door that writes a skill file asks this first; only an
// explicit --force skips it.
func DecideSkillWrite(tool AgentTool, existing []byte, exists bool, expected []byte, running string) SkillDecision {
	if !exists {
		return SkillDecision{Action: SkillInstall}
	}
	want := normalizeSkill(expected)
	body, ver, hash, stamped := parseSkillStamp(existing)
	if stamped {
		if skillHash(body) != hash {
			return SkillDecision{Action: SkillKeepEdited, StampVersion: ver}
		}
		// Before the newer-version check, deliberately: a newer pad that wrote
		// this same text leaves nothing to downgrade, so the file is current
		// and is neither warned about nor re-stamped to an older version.
		if bytes.Equal(body, want) {
			return SkillDecision{Action: SkillUnchanged, StampVersion: ver}
		}
		if skillVersionNewer(ver, running) {
			return SkillDecision{Action: SkillKeepNewer, StampVersion: ver}
		}
		return SkillDecision{Action: SkillUpdate, StampVersion: ver}
	}
	// Unstamped: written before stamps existed, or by hand.
	if bytes.Equal(normalizeSkill(existing), want) || legacySkillHashKnown(tool, skillHash(existing)) {
		return SkillDecision{Action: SkillUpdate}
	}
	return SkillDecision{Action: SkillKeepEdited}
}

// skillVersionNewer reports a stamp version strictly newer than running. Only
// when BOTH parse as semver: a dev build (or any unparseable version) never
// refuses as a downgrade, though the edit check above still applies.
func skillVersionNewer(stamp, running string) bool {
	s, okS := parseSemver(stamp)
	r, okR := parseSemver(running)
	if !okS || !okR {
		return false
	}
	return compareSemver(s, r) > 0
}

// semverParts is vMAJOR.MINOR.PATCH[-PRERELEASE], the shape pad's release
// versions take (v0.18.0, v0.18.0-rc.7). Build metadata is ignored. Kept
// here rather than importing golang.org/x/mod/semver so the module's vendor
// set (and the Nix vendorHash) does not move for one comparison.
type semverParts struct {
	core [3]string // digit strings: SemVer numbers have no size limit
	pre  []string
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// compareDigits compares two digit strings as numbers of any size.
func compareDigits(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	switch {
	case len(a) != len(b):
		if len(a) < len(b) {
			return -1
		}
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func parseSemver(v string) (semverParts, bool) {
	v = strings.TrimPrefix(v, "v")
	var p semverParts
	if i := strings.IndexByte(v, '+'); i >= 0 {
		// Build metadata is ignored for ordering, but must be well formed.
		for _, id := range strings.Split(v[i+1:], ".") {
			if !isIdentifier(id) {
				return p, false
			}
		}
		v = v[:i]
	}
	core := v
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core, p.pre = v[:i], strings.Split(v[i+1:], ".")
		for _, id := range p.pre {
			if !isIdentifier(id) || (isDigits(id) && len(id) > 1 && id[0] == '0') {
				return p, false
			}
		}
	}
	nums := strings.Split(core, ".")
	if len(nums) != 3 {
		return p, false
	}
	for i, n := range nums {
		if !isDigits(n) || (len(n) > 1 && n[0] == '0') {
			return p, false
		}
		p.core[i] = n
	}
	return p, true
}

// isIdentifier is a SemVer identifier: non-empty, [0-9A-Za-z-] only.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// compareSemver orders by core, then a release above any of its
// prereleases, then prerelease identifiers left to right: numeric ones
// numerically and below alphanumeric ones, which compare as text.
func compareSemver(a, b semverParts) int {
	for i := 0; i < 3; i++ {
		if c := compareDigits(a.core[i], b.core[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		nx, ny := isDigits(x), isDigits(y)
		switch {
		case nx && ny:
			if c := compareDigits(x, y); c != 0 {
				return c
			}
		case nx:
			return -1
		case ny:
			return 1
		case x != y:
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

// legacySkillHashKnown reports whether hash is what some past pad release
// wrote for this tool, before files carried a stamp. The list is frozen
// (skill_legacy_hashes.go, generated from the release tags): every file
// written from the stamping release on carries a marker instead.
func legacySkillHashKnown(tool AgentTool, hash string) bool {
	set, ok := legacySkillHashes[legacySkillFormat(tool)]
	return ok && set[hash]
}

// legacySkillFormat names the output shape a tool got: claude, agents and
// copilot each had their own; every other tool got the stripped body.
func legacySkillFormat(tool AgentTool) string {
	switch tool.Name {
	case "claude", "agents", "copilot":
		return tool.Name
	}
	return "body"
}

// SkillWriteResult is what WriteSkill did.
type SkillWriteResult struct {
	SkillDecision
	Path string
	// Wrote is true when the file was written (installed, updated, or forced).
	Wrote bool
}

// WriteSkill is the one door that writes a tool's skill file (BUG-3466). It
// decides with DecideSkillWrite and writes the stamped text only to install
// or update; an edited file or a newer pad's file is kept unless force.
func WriteSkill(tool AgentTool, embedded []byte, version string, force bool) (SkillWriteResult, error) {
	path := ToolSkillPath(tool)
	expected := FormatForTool(tool, embedded)
	existing, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return SkillWriteResult{Path: path}, err
	}
	res := SkillWriteResult{SkillDecision: DecideSkillWrite(tool, existing, exists, expected, version), Path: path}
	switch res.Action {
	case SkillInstall, SkillUpdate:
	case SkillUnchanged:
		return res, nil
	case SkillKeepEdited, SkillKeepNewer:
		if !force {
			return res, nil
		}
	}
	written, err := InstallForTool(tool, StampSkill(expected, version))
	if err != nil {
		return res, err
	}
	res.Path, res.Wrote = written, true
	return res, nil
}

// SkillState is DecideSkillWrite for the file currently at the tool's skill
// path, for status displays: exists is false when there is no file.
func SkillState(tool AgentTool, embedded []byte, version string) (action SkillAction, stampVersion string, exists bool) {
	data, err := os.ReadFile(ToolSkillPath(tool))
	if err != nil {
		return SkillInstall, "", false
	}
	d := DecideSkillWrite(tool, data, true, FormatForTool(tool, embedded), version)
	return d.Action, d.StampVersion, true
}
