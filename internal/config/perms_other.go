//go:build !unix

package config

import "os"

// Non-Unix platforms do not tighten directory modes (tightenDir returns
// early), so no symlink-refusing flag is needed.
const dirTightenOpenFlags = os.O_RDONLY
