package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// setupTimeout is the deadline for a run's pre-pipeline setup phase. A
// non-positive configured value (only reachable from a hand-built GlobalConfig,
// since LoadGlobal rejects one on disk) falls back to the default rather than
// meaning "no deadline": there is no legitimate reason to want this phase
// unbounded, and "unbounded" is the bug.
func setupTimeout(cfg *config.GlobalConfig) time.Duration {
	if cfg == nil || cfg.RunSetupTimeout <= 0 {
		return config.DefaultRunSetupTimeout
	}
	return cfg.RunSetupTimeout
}

// explainSetupFailure turns a failed setup subprocess into an error a person can
// act on.
//
// Two shapes of failure land here, and both were observed in the field against a
// daemon that had been auto-started (startDetachedDaemon) by a CLI running inside
// an agent's sandbox. A macOS seatbelt profile is inherited by every descendant
// and cannot be dropped from inside, so that daemon - a machine-wide service, by
// then serving repos the sandbox had never heard of - was permanently confined to
// one agent's scope. Its git children died two ways depending on the path they
// touched:
//
//   - killed outright by the sandbox, surfacing as "signal: killed" from an
//     otherwise ordinary `git config` against the user's clone;
//   - never returning at all, which is what made the run sit `pending` with zero
//     steps and zero log lines while the client polled it for 22 minutes.
//
// Neither message says anything about a sandbox on its own, and the daemon that
// is confined cannot detect the confinement it is inside (seatbelt is not
// introspectable from within). What it CAN do is name the one action that
// resolves it - restart the daemon from an unconfined shell - whenever it sees a
// setup subprocess die in a way that a healthy daemon never produces.
func explainSetupFailure(parentCtx, setupCtx context.Context, what string, err error) error {
	base := fmt.Errorf("%s: %w", what, err)
	if !setupFailureLooksConfined(parentCtx, setupCtx, err) {
		return base
	}
	return fmt.Errorf("%w\n\n%s", base, confinedDaemonHint)
}

const confinedDaemonHint = "The daemon's git subprocess did not return or was killed by the OS. " +
	"This is what a confined daemon looks like: a daemon auto-started from inside a sandboxed " +
	"shell (an agent harness, for example) inherits that sandbox for its whole life, and every " +
	"repository outside the sandbox's scope becomes unreachable to it - even though the same path " +
	"works fine from your own shell. Restart the daemon from an unsandboxed shell:\n" +
	"    no-mistakes daemon restart --force"

// setupFailureLooksConfined reports whether a setup failure has the fingerprint
// of an OS-confined subprocess rather than an ordinary git error (bad ref,
// missing remote, auth). Both fingerprints are things a healthy daemon does not
// produce: the setup deadline firing at all, and a child killed by a signal.
//
// parentCtx being already cancelled disqualifies both. The daemon shutting down
// mid-setup cancels the IPC handler's context, which kills the in-flight git
// child and surfaces the very same "signal: killed" - and blaming that on a
// sandbox would send someone chasing a confinement that is not there. An
// externally-cancelled setup explains its own dead child.
func setupFailureLooksConfined(parentCtx, setupCtx context.Context, err error) bool {
	if parentCtx != nil && parentCtx.Err() != nil {
		return false
	}
	if setupCtx != nil && errors.Is(setupCtx.Err(), context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// exec surfaces a signal-killed child as "signal: killed" in the ExitError
	// string; there is no typed error to match on across platforms.
	return err != nil && strings.Contains(err.Error(), "signal: killed")
}
