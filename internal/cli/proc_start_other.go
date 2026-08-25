//go:build !linux

package cli

// procStartToken has no portable non-Linux implementation, so it returns
// ok=false and the headless arm-state liveness check falls back to bare
// pid-liveness. This is the documented residual on the secondary
// (headless, cwd-keyed) arming path — the sanctioned headless path is
// .pad.toml auto_arm, which carries no pid identity at all. See
// armStateOwnerAlive.
func procStartToken(pid int) (string, bool) {
	return "", false
}

// procStartTokenErr reports the platform limitation as an error, so a
// caller distinguishing "gone" from "cannot examine" lands on the latter.
func procStartTokenErr(pid int) (string, error) {
	return "", errProcStartUnsupported
}
