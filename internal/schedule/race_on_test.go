//go:build race

package schedule

// raceSample makes TestNeverAPartialAnswer run a deterministic sample under
// -race (TASK-4100): fixed strides, every MaxSteps value, both RDATE
// placements.
const raceSample = true
