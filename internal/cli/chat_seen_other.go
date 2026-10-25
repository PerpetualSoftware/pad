//go:build !unix

package cli

import "os"

// This platform has no O_NOFOLLOW; the regular-file check after the open is
// the only guard here.
const chatSeenOpenFlags = os.O_WRONLY | os.O_APPEND | os.O_CREATE
