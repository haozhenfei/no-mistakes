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
