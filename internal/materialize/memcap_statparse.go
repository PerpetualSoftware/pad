package materialize

import (
	"fmt"
	"strconv"
	"strings"
)

// parseStatVsize extracts vsize (field 23) from a /proc/<pid>/stat line.
// Platform-neutral so it is tested everywhere.
func parseStatVsize(stat string) (uint64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("stat: no command name in %q", stat)
	}
	f := strings.Fields(stat[i+1:])
	// f[0] is field 3 (state), so field 23 is f[20].
	if len(f) < 21 {
		return 0, fmt.Errorf("stat: %d fields after the command name", len(f))
	}
	return strconv.ParseUint(f[20], 10, 64)
}
