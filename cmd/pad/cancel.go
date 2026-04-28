package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// errCancelled is returned by interactive prompts (template picker, mode
// picker) when the user explicitly aborts the flow. The init RunEs catch
// it and exit cleanly via cancelInit().
var errCancelled = errors.New("cancelled by user")

// cancelInit is the canonical exit path for both Ctrl+C and explicit
// "Cancel" actions during pad init / pad workspace init. It prints a
// brief message to stderr and exits with code 130 (the conventional
// shell exit code for SIGINT).
//
// We exit via os.Exit because the SIGINT case fires asynchronously in a
// goroutine while the main flow is blocked reading stdin — there is no
// way to gracefully unwind that read. Using the same path for the
// explicit-cancel case keeps the user-visible behavior identical.
func cancelInit() {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Cancelled.")
	os.Exit(130)
}

// installInitCancelHandler installs a SIGINT/SIGTERM handler for the
// duration of an interactive init run. On signal receipt it calls
// cancelInit().
//
// The returned function MUST be deferred — it stops the signal listener
// and lets the goroutine return when init completes successfully.
//
// Why explicit handling is needed: bufio.Reader.ReadString blocks on
// stdin. Go's default SIGINT behavior terminates the process with no
// printed message, which can surface oddly in some terminal stacks. A
// custom handler guarantees a friendly "Cancelled." line before the
// process exits.
func installInitCancelHandler() func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			cancelInit()
		case <-done:
		}
	}()

	return func() {
		close(done)
		signal.Stop(sigCh)
	}
}

// isCancellation reports whether err represents a user-initiated abort
// from any interactive init prompt. Init RunEs use this to convert the
// sentinel into the canonical cancelInit() exit instead of letting cobra
// render an "Error:" line.
func isCancellation(err error) bool {
	return errors.Is(err, errCancelled)
}
