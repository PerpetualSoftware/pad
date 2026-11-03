package materialize

import (
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Environment knobs for the materializer worker (TASK-2198). Documented in
// docs/deployment.md, "Op-log materializer worker".
const (
	// EnvTimeout is the BASE of the per-job hard deadline, in Go duration
	// syntax ("2s", "1500ms"); a job also gets PerKiBTimeout per KiB of its
	// op-log rows, up to MaxTimeout (BUG-3521). Past it the worker process is
	// KILLED.
	EnvTimeout = "PAD_MATERIALIZE_TIMEOUT"
	// EnvMemLimit is how much memory one job may add to the loaded worker
	// (the cap is baseline + limit; what "memory" is differs per OS, see
	// memCapImpl): a whole number of bytes, or a whole number followed by a
	// BINARY unit, KiB, MiB or GiB ("2048MiB", "2GiB"). Decimal units (MB,
	// GB) are refused rather than guessed at.
	EnvMemLimit = "PAD_MATERIALIZE_MEM_LIMIT"
	// EnvIdleTimeout is how long the worker may sit with no job before it is
	// stopped, in Go duration syntax ("5m"). "0" means never. A loaded worker
	// holds ~200 MB resident, which an install that rarely materializes should
	// not pay for all day; the next job starts a new one, as the first did.
	EnvIdleTimeout = "PAD_MATERIALIZE_IDLE_TIMEOUT"
)

// EnvSwitch turns op-log recovery off entirely: PAD_MATERIALIZE=off (or 0,
// false, no; case-insensitive) and the server constructs no Supervisor,
// installs no trigger and never spawns a worker. Unset, or any other value,
// leaves it on; a value that is neither a recognised on nor off word is
// logged and read as on.
const EnvSwitch = "PAD_MATERIALIZE"

// Enabled reads EnvSwitch through getenv.
func Enabled(getenv func(string) string, logger *slog.Logger) bool {
	if logger == nil {
		logger = slog.Default()
	}
	v := strings.ToLower(strings.TrimSpace(getenv(EnvSwitch)))
	switch v {
	case "off", "0", "false", "no", "disabled":
		return false
	case "", "on", "1", "true", "yes", "enabled":
		return true
	}
	logger.Warn("materialize: unrecognised "+EnvSwitch+" value; op-log recovery stays ON (set it to off to disable)",
		"value", getenv(EnvSwitch))
	return true
}

// PerKiBTimeout is what each KiB of a job's op-log rows adds to its hard
// deadline (BUG-3521). The receipt: the real op-log of an 86 KB plan body
// after three rounds of live edits (278 content-bearing rows, 470 KiB),
// materialized in-process with the worker's engine, 7 runs each: median
// 1.36 s on an idle 8-core box, 1.77 s pinned to one core at GOMAXPROCS=1,
// about 3.8 ms per KiB at the slow end. The flat 2 s deadline (1.8 s soft)
// left that document on the edge before IPC or load, and anything larger
// failed every attempt. 20 ms/KiB is ~5x the single-core rate, so the same
// document gets ~11 s. What it does NOT cover: cost that grows with the
// document's structure rather than its op-log bytes (deep nesting; see the
// adversarial cases in measure_linux_test.go), which MaxTimeout and the
// memory cap still bound.
const PerKiBTimeout = 20 * time.Millisecond

// jobTimeout is one job's hard deadline: the configured base plus
// PerKiBTimeout for each KiB of its rows, never below the base and never
// above MaxTimeout.
func jobTimeout(base time.Duration, job Job) time.Duration {
	n := 0
	for _, r := range job.Rows {
		n += len(r)
	}
	d := base + time.Duration(n/1024)*PerKiBTimeout
	if d > MaxTimeout {
		d = MaxTimeout
	}
	if d < base {
		d = base
	}
	return d
}

const (
	DefaultTimeout  = 2 * time.Second
	MinTimeout      = 250 * time.Millisecond
	MaxTimeout      = 60 * time.Second
	DefaultMemLimit = 2 << 30 // 2 GiB
	// MinMemLimit: the limit is GROWTH beyond the loaded worker (see
	// memCapImpl), and the measured growth of real work is small
	// (TestSupervisorBaselineMeasurements, real worker, cgo build,
	// GOMAXPROCS=2 as the supervisor sets it): the 50-case corpus adds
	// +0 MiB of address space and +24 MiB resident, and a 4,000-deep nested
	// document — pathological, not a real item — at most +193 MiB of address
	// space. Threads created after the baseline: 0 in every run at
	// GOMAXPROCS=2 (warmThreads makes sure of it at higher counts too). 256 MiB
	// lets all of that through. The old 1536 MiB floor belonged to the
	// absolute cap, which had to clear the runtime's own reservation.
	MinMemLimit = 256 << 20
	MaxMemLimit = 16 << 30

	DefaultIdleTimeout = 5 * time.Minute
	// MinIdleTimeout keeps a busy install from paying the ~2.6 s bundle load
	// between jobs that arrive seconds apart.
	MinIdleTimeout = 30 * time.Second
	MaxIdleTimeout = 24 * time.Hour
	// NeverIdle, as SupervisorConfig.IdleTimeout, keeps the worker until Close
	// (EnvIdleTimeout "0"). The zero value is the default, as for the other
	// fields.
	NeverIdle time.Duration = -1
)

var memLimitRE = regexp.MustCompile(`^([0-9]+)(KiB|MiB|GiB)?$`)

// ParseMemLimit reads the EnvMemLimit syntax: a whole number of bytes, or a
// whole number with a KiB, MiB or GiB suffix (case-sensitive, no space).
func ParseMemLimit(s string) (uint64, error) {
	m := memLimitRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("%q is not a byte count or a whole number of KiB, MiB or GiB", s)
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", s, err)
	}
	shift := map[string]uint{"": 0, "KiB": 10, "MiB": 20, "GiB": 30}[m[2]]
	if n > math.MaxUint64>>shift {
		return 0, fmt.Errorf("%q overflows", s)
	}
	return n << shift, nil
}

// ConfigFromEnv reads EnvTimeout, EnvMemLimit and EnvIdleTimeout through getenv (os.Getenv in
// production). An unset variable takes the default; an unparseable one takes
// the default and is logged as a warning. Out-of-range values are clamped by
// NewSupervisor, not here, so a config built by hand gets the same clamp.
func ConfigFromEnv(getenv func(string) string, logger *slog.Logger) SupervisorConfig {
	if logger == nil {
		logger = slog.Default()
	}
	cfg := SupervisorConfig{Logger: logger}
	if v := getenv(EnvTimeout); v != "" {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			logger.Warn("materialize: ignoring invalid "+EnvTimeout+"; using the default",
				"value", v, "error", err, "default", DefaultTimeout.String())
		} else {
			cfg.Timeout = d
		}
	}
	if v := getenv(EnvMemLimit); v != "" {
		n, err := ParseMemLimit(v)
		if err != nil {
			logger.Warn("materialize: ignoring invalid "+EnvMemLimit+"; using the default",
				"value", v, "error", err, "default", formatBytes(DefaultMemLimit))
		} else {
			cfg.MemLimit = n
		}
	}
	if v := getenv(EnvIdleTimeout); v != "" {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		switch {
		case err != nil:
			logger.Warn("materialize: ignoring invalid "+EnvIdleTimeout+"; using the default",
				"value", v, "error", err, "default", DefaultIdleTimeout.String())
		case d < 0:
			logger.Warn("materialize: ignoring negative "+EnvIdleTimeout+"; using the default (0 means never)",
				"value", v, "default", DefaultIdleTimeout.String())
		case d == 0:
			cfg.IdleTimeout = NeverIdle
		default:
			cfg.IdleTimeout = d
		}
	}
	return cfg
}

// effectiveIdleTimeout applies the default and the clamp; a negative request
// (NeverIdle) is returned as NeverIdle. The second result says whether a
// positive request was changed.
func effectiveIdleTimeout(d time.Duration) (time.Duration, bool) {
	switch {
	case d < 0:
		return NeverIdle, false
	case d == 0:
		return DefaultIdleTimeout, false
	case d < MinIdleTimeout:
		return MinIdleTimeout, true
	case d > MaxIdleTimeout:
		return MaxIdleTimeout, true
	}
	return d, false
}

// effectiveTimeout applies the default and the clamp. The second result says
// whether a non-zero request was changed.
func effectiveTimeout(d time.Duration) (time.Duration, bool) {
	switch {
	case d == 0:
		return DefaultTimeout, false
	case d < MinTimeout:
		return MinTimeout, true
	case d > MaxTimeout:
		return MaxTimeout, true
	}
	return d, false
}

func effectiveMemLimit(n uint64) (uint64, bool) {
	switch {
	case n == 0:
		return DefaultMemLimit, false
	case n < MinMemLimit:
		return MinMemLimit, true
	case n > MaxMemLimit:
		return MaxMemLimit, true
	}
	return n, false
}

// formatBytes renders n in the EnvMemLimit syntax, exactly when it can.
func formatBytes(n uint64) string {
	switch {
	case n != 0 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n != 0 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	}
	return strconv.FormatUint(n, 10)
}
