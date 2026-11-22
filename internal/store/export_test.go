package store

// Exported for TestMain in the external test package (main_test.go), which
// tears down the internal test templates alongside storetest's.
var (
	RemoveSQLiteTemplate   = removeSQLiteTemplate
	RemovePostgresTemplate = removePostgresTemplate
)
