package cli

import "github.com/PerpetualSoftware/pad/internal/config"

// Arm-consent resolution (PLAN-2613 S2, D4).
//
// "Arming" is a session's declaration that it consents to receive `pad
// push` notifications — the security gate S1 built server-side (a stream
// declares armed=true at connect and only armed streams receive
// KindPush; see internal/server/session_identity.go). Nothing decided
// WHETHER to arm yet. This file is that decision for the AUTO path: a
// repository opts its sessions in through .pad.toml, a user can veto it
// globally, and the default everywhere is off.
//
// The EXPLICIT path — an in-session `pad session arm` toggle — layers on
// top of this (its state overrides the resolved value); the running
// plugin monitor's mid-session re-arm is the S3 lockfile's job. This
// resolver is the piece both paths and the status verb share, so it is a
// pure function of its two inputs with a separate loader for the IO.

// ResolveAutoArm decides whether a session should auto-arm at connect,
// from the two config sources, with default OFF (PLAN-2613 D4).
//
// The full table (repoAutoArm = .pad.toml [push] auto_arm; userAutoArm =
// ~/.pad/config.toml [push] auto_arm):
//
//	repo=false, user=nil    → false  (default: not opted in)
//	repo=false, user=&true  → false  (a per-user true is NOT an enabler —
//	                                   D4 forbids a machine-global
//	                                   always-on, so it cannot arm a repo
//	                                   that didn't opt in itself)
//	repo=false, user=&false → false
//	repo=true,  user=nil    → true   (repo opted in, user has no opinion)
//	repo=true,  user=&true  → true
//	repo=true,  user=&false → false  (VETO — deny-wins: a per-user
//	                                   explicit false forces off even over
//	                                   a repo opt-in)
//
// The last row is the one decision the design under-specified ("deny-
// wins: .pad.toml over per-user"). It is resolved deny-wins — the
// security-conservative reading for a consent gate: a user who has said
// "never auto-arm me" must not be re-armed by a committed .pad.toml they
// pulled from someone else's opt-in. The alternative (repo overrides the
// user's global off) is exactly the surprise a consent gate exists to
// prevent, so a per-repo enable cannot override a per-user disable.
func ResolveAutoArm(repoAutoArm bool, userAutoArm *bool) bool {
	if userAutoArm != nil && !*userAutoArm {
		return false // per-user veto — deny-wins
	}
	return repoAutoArm
}

// ArmDecision is the resolved arm state plus the inputs that produced it,
// so callers (notably `pad session status`) can explain WHY a session is
// or isn't arming rather than just reporting the bit.
type ArmDecision struct {
	// Armed is the resolved auto-arm value — the same bool a connecting
	// stream would declare for this repo, absent an explicit in-session
	// override.
	Armed bool
	// RepoAutoArm is the .pad.toml [push] auto_arm value (false when no
	// workspace is linked).
	RepoAutoArm bool
	// UserVeto is true when the per-user config explicitly set auto_arm
	// to false — the one thing that forces Armed off over a repo opt-in.
	UserVeto bool
}

// ResolveAutoArmFromDisk loads both config sources (the nearest .pad.toml
// and the user's config.toml) and returns the resolved auto-arm
// decision. Failures degrade to the safe default: an unreadable or
// missing source contributes "not opted in" / "no veto" rather than an
// error, because a consent gate must fail CLOSED (off), and a torn config
// file should never silently arm a session — nor block one path of a CLI
// verb that has other useful things to report.
func ResolveAutoArmFromDisk() ArmDecision {
	repoAutoArm := false
	if pt, err := LoadPadToml(); err == nil {
		repoAutoArm = pt.PadTomlAutoArm()
	}

	var userAutoArm *bool
	if cfg, err := config.Load(); err == nil {
		userAutoArm = cfg.PushAutoArm()
	}

	return ArmDecision{
		Armed:       ResolveAutoArm(repoAutoArm, userAutoArm),
		RepoAutoArm: repoAutoArm,
		UserVeto:    userAutoArm != nil && !*userAutoArm,
	}
}
