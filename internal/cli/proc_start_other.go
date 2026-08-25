//go:build !linux

package cli

import "errors"

var errProcStartUnsupported = errors.New("process start token unsupported on this platform")

// procStartToken has no portable non-Linux implementation, so it returns
// ok=false and a headless owner is judged by bare pid-liveness on unix
// platforms such as macOS — the documented residual on the secondary
// (headless, cwd-keyed) arming path; the sanctioned headless path is
// .pad.toml auto_arm, which carries no pid identity at all. On Windows
// the pid cannot be probed either, so the verdict there is unknown (see
// OwnerLiveness), not a bare-liveness alive.
func procStartToken(pid int) (string, bool) {
	return "", false
}

// procStartTokenErr reports the platform limitation as an error, so a
// caller distinguishing "gone" from "cannot examine" lands on the latter.
func procStartTokenErr(pid int) (string, error) {
	return "", errProcStartUnsupported
}
