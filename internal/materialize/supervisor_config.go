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
	// EnvTimeout is the per-job hard deadline, in Go duration syntax
	// ("2s", "1500ms"). Past it the worker process is KILLED.
	EnvTimeout = "PAD_MATERIALIZE_TIMEOUT"
	// EnvMemLimit is how much memory one job may add to the loaded worker
	// (the cap is baseline + limit; what "memory" is differs per OS, see
	// memCapImpl): a whole number of bytes, or a whole number followed by a
	// BINARY unit, KiB, MiB or GiB ("2048MiB", "2GiB"). Decimal units (MB,
	// GB) are refused rather than guessed at.
	EnvMemLimit = "PAD_MATERIALIZE_MEM_LIMIT"
)

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

// ConfigFromEnv reads EnvTimeout and EnvMemLimit through getenv (os.Getenv in
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
	return cfg
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
