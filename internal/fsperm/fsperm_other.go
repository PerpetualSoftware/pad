//go:build !unix

package fsperm

import "os"

// On non-Unix platforms callers do not tighten modes, so these flags only
// need to open the path.
const (
	NoFollowFile = os.O_RDONLY
	NoFollowDir  = os.O_RDONLY
)
