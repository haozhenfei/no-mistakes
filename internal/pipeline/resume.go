package pipeline

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ConfigHash returns a stable digest of the effective pipeline config. Resume
// uses this as an input-version check: if the config changes, previously
// completed step rows are not reused.
func ConfigHash(cfg *config.Config) string {
	data, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

// ResumableStatus reports whether a terminal run can be used as a resume
// source. Failed, cancelled, and daemon-interrupted runs are all potential
// sources; completed runs do not need resume.
func ResumableStatus(status types.RunStatus) bool {
	switch status {
	case types.RunFailed, types.RunCancelled, types.RunInterrupted:
		return true
	default:
		return false
	}
}

// CompletedStepReusable is the conservative step-skip predicate for explicit
// resume. A row is reusable only when it completed for the exact head/config
// the new run is about to validate.
func CompletedStepReusable(step *db.StepResult, headSHA, configHash string) bool {
	if step == nil || step.Status != types.StepStatusCompleted {
		return false
	}
	if step.ValidatedHeadSHA == nil || *step.ValidatedHeadSHA != headSHA {
		return false
	}
	if step.ConfigHash == nil || *step.ConfigHash != configHash {
		return false
	}
	return true
}

// SkipSet turns a skip list into the lookup the executor and resume predicates
// use. It returns nil for an empty list so a nil map means "skip nothing".
func SkipSet(names []types.StepName) map[types.StepName]bool {
	if len(names) == 0 {
		return nil
	}
	set := make(map[types.StepName]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// ResumeStepReusable is the leading-prefix predicate for explicit resume: a
// step needs no rerun when it completed for this exact head/config, or when the
// run is configured to skip it and the row already records it as skipped.
//
// Head/config validation deliberately does not apply to the skipped case. A
// skipped step validated nothing, so nothing about it can go stale; the run's
// persisted skip set (runs.skip_steps) says it must not run, and resume must
// honor that instead of reviving it (a skipped row is not `completed`, so
// without this the prefix would break here and re-execute the very step the
// caller paid to skip).
func ResumeStepReusable(step *db.StepResult, headSHA, configHash string, skips map[types.StepName]bool) bool {
	if CompletedStepReusable(step, headSHA, configHash) {
		return true
	}
	return step != nil && skips[step.StepName] && step.Status == types.StepStatusSkipped
}
