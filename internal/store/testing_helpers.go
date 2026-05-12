package store

// SetBcryptCostForTesting overrides the bcrypt cost used by CreateUser
// and UpdateUser for the lifetime of a test binary. It returns a
// restore function — call it from TestMain (or defer it) to leave
// the package state clean, though process exit also suffices since
// the override is local to the test binary.
//
// Why this exists: under the race detector, bcrypt.GenerateFromPassword
// at the production cost (12) takes ~3s per call. The internal/server
// and internal/store test suites bootstrap dozens of users each; the
// cumulative cost exceeded the 30m CI -race timeout (BUG-1371). Tests
// that don't care about hash strength call this once per binary in
// TestMain to drop the cost to bcrypt.MinCost (= 4), which restores
// the race step to well under the timeout.
//
// Production code MUST NOT call this. The "ForTesting" suffix is the
// grep signal — any non-test caller is a bug.
func SetBcryptCostForTesting(cost int) func() {
	prev := bcryptCost
	bcryptCost = cost
	return func() { bcryptCost = prev }
}
