//go:build race

package schedule

// raceSample makes TestNeverAPartialAnswer run a deterministic sample under
// -race (TASK-4100 r3 D4): fixed strides, every MaxSteps value, both RDATE
// placements.
const raceSample = true
