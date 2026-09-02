package server

import (
	"net/http"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// The workspace import's --repair-nul consent flag (DOC-2823 S3, BUG-2810).
//
// WHY IT EXISTS. A self-hoster whose database predates the NUL enforcement can
// EXPORT a workspace and then not import it back: the server emits a payload it
// will refuse. That is the sentence in BUG-2810 that made it an item rather
// than a note — the product has no path forward for somebody restoring their
// own backup.
//
// WHAT IT IS NOT. It is not an exemption from the gate. The gate still runs, on
// the repaired bytes, and still decides; the flag buys the body one repair
// attempt first. BUG-2803's filing named this endpoint as carrying the largest
// attacker-controlled body in the product and explicitly ruled out exempting
// it, so nothing here may become a decode path that skips the check.
//
// Dave's day-54 ruling: the flag ships, the default stays strict, the strict
// refusal names the flag, and a test drives that named remedy against the exact
// failing fixture.

// NULRepairQueryParam is how the flag reaches the server. The CLI's
// `pad workspace import --repair-nul` sets it.
//
// EXPORTED so the CLI sends the name this package reads, rather than a second
// spelling of it. A query parameter and a response header are a two-sided
// contract, and the failure mode of the two sides drifting is silent: the
// import simply stops repairing, or stops reporting, with nothing to see.
const NULRepairQueryParam = "repair_nul"

// NULRepairHeader reports how many VALUES were rewritten, so the operator is
// told what changed rather than only that something did. A header rather than a
// response field because the success body is the created workspace and its
// shape is a public contract.
//
// Values, not escapes: the repair works on the DECODED body, where the escape
// form has already been resolved and one nested document may have carried
// several. "Two values were rewritten" is also the sentence an operator can
// check against the rows they end up with.
const NULRepairHeader = "X-Pad-Repaired-NUL-Values"

// nulRepairTally carries the flag through an import, counts what it changed,
// and records any reason it declined to act.
type nulRepairTally struct {
	Enabled  bool
	Replaced int
	// Declined explains why the repair did not run on a body it was asked to
	// repair — today, only a payload with duplicate object members, where
	// repairing would change which value is imported. Empty when the repair
	// ran, whether or not it found anything.
	Declined string
}

// wantsNULRepair reports whether the request asked for the repair.
func wantsNULRepair(r *http.Request) bool {
	switch r.URL.Query().Get(NULRepairQueryParam) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// Apply repairs a JSON body's NUL-carrying values when the flag is set, and
// returns the bytes the gate should judge.
//
// A nil tally, or one with the flag unset, returns the input untouched — so a
// caller that forgets to thread the flag gets the strict behaviour, which is
// the safe direction for this particular forgetting.
func (t *nulRepairTally) Apply(raw []byte) []byte {
	if t == nil || !t.Enabled {
		return raw
	}
	repaired, n, declined := repairBodyNULEscapes(raw)
	t.Replaced += n
	if declined != "" {
		t.Declined = declined
	}
	return repaired
}

// SetHeader reports the count on a successful import.
func (t *nulRepairTally) SetHeader(w http.ResponseWriter) {
	if t == nil || !t.Enabled {
		return
	}
	w.Header().Set(NULRepairHeader, strconv.Itoa(t.Replaced))
}

// nulRepairRemedy is the sentence appended to a strict refusal, naming the flag
// that would have accepted it.
//
// It says something DIFFERENT when the flag was already given, because at that
// point the value is one the repair could not fix, and telling an operator to
// re-run with a flag they just used is worse than saying nothing (PATTE-135 in
// the other direction: a remedy that does not work is still a contract claim).
func nulRepairRemedy(t *nulRepairTally) string {
	if t != nil && t.Declined != "" {
		// The one case where the repair KNOWS why it did nothing, so it says so
		// rather than falling through to the message below, which would tell an
		// operator the repair ran when it deliberately did not.
		return ". The import's NUL repair did not run: " + t.Declined +
			". Repair the source database with '" + store.RepairNULCommand + "' and export again"
	}
	if t != nil && t.Enabled {
		// DELIBERATELY DOES NOT NAME A CAUSE, and this branch should now be
		// unreachable in practice: the repair walks the decoded body with the
		// same classing the gate uses, so a body the gate refuses afterwards
		// means the two walks have diverged — which is a defect here, not a
		// property of the operator's data. Guessing at a cause would send them
		// to a fix for a problem they do not have.
		//
		// (Two earlier wordings named causes that were wrong. The first said
		// "a raw NUL byte rather than an escape"; the second said "not a plain
		// NUL escape in the document". Both stopped being true when the repair
		// moved from scanning raw bytes to walking the decoded body.)
		return ". The import's NUL repair ran and the value is still refused. Repair the source database" +
			" with '" + store.RepairNULCommand + "' and export again"
	}
	// Worded for BOTH doors. The web UI posts to this same endpoint, and a
	// browser user has no command line to add a flag to — so the message names
	// the option and then where to find it, rather than assuming a terminal.
	return ". This export was written by a Pad older than the check that now refuses it. Re-run the import" +
		" with the NUL repair option (--repair-nul on the CLI) to replace each NUL with U+FFFD, or repair" +
		" the source database first with '" + store.RepairNULCommand + "'"
}
