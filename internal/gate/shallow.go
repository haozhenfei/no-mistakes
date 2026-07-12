package gate

import (
	"fmt"
	"strings"
)

// shallowRejection is the phrase git's receive-pack prints when it refuses a
// push whose history is truncated at a shallow boundary.
const shallowRejection = "shallow update not allowed"

// ShallowPushRejected reports whether err is git refusing a push from a shallow
// clone because the receiving repo cannot reconstruct the truncated history.
func ShallowPushRejected(err error) bool {
	return err != nil && strings.Contains(err.Error(), shallowRejection)
}

// ShallowPushHelp returns the remedies for a shallow push rejection, ordered by
// cost. Re-running init is the fix (it sets receive.shallowUpdate on the gate);
// unshallowing the working repo is the fallback that works on any gate.
func ShallowPushHelp() []string {
	return []string{
		"Re-run `no-mistakes init` here: it configures the gate repo to accept pushes from a shallow clone (gates created before this was supported reject them)",
		"Or restore the full history once with `git fetch --unshallow` (every worktree shares one .git, so a single fetch fixes all of them)",
	}
}

// ExplainPushError turns git's bare "shallow update not allowed" into an
// actionable message naming the cause and the fix. Any other error is returned
// unchanged.
func ExplainPushError(err error) error {
	if !ShallowPushRejected(err) {
		return err
	}
	return fmt.Errorf("this repository is a shallow clone and the gate repo rejected its truncated history (%s); %s: %w",
		shallowRejection, strings.Join(ShallowPushHelp(), "; "), err)
}
