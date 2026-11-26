//go:build !unix

package store

import "os"

// Non-Unix platforms do not tighten SQLite file modes (secureSQLiteFiles
// returns early), so no symlink-refusing flag is needed.
const sqliteTightenOpenFlags = os.O_RDONLY
