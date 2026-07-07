// Package claims defines the claim model: the smallest semantic unit the
// pipeline reasons about (design §5). A claim is a single statement an agent
// makes about the change ("login no longer overflows on mobile") bound to the
// evidence IDs that back it and, after the verify step, a verdict.
//
// The one machine-enforced rule lives here: a claim with no evidence is
// "self-attested" and can never appear in the dossier's conclusions. Binding
// evidence is only a SYNTACTIC guarantee that a link exists; whether the
// evidence actually supports the claim is a semantic judgement the verify step
// makes, not something this package asserts (design principle 3).
package claims

// Verdict values recorded by the verify step (design §4.4a).
const (
	VerdictUnverified = "" // no verdict recorded yet
	VerdictConfirmed  = "CONFIRMED"
	VerdictPlausible  = "PLAUSIBLE"
	VerdictRefuted    = "REFUTED"
)

// Kind values (design §5).
const (
	KindBehavior        = "behavior"
	KindRegressionFixed = "regression-fixed"
	KindRuleCompliance  = "rule-compliance"
	KindNonGoal         = "non-goal"
)

// Claim is a statement bound to evidence and, eventually, a verdict.
type Claim struct {
	ID        string
	RunID     string
	Step      string
	Text      string
	Kind      string
	Evidence  []string
	Verdict   string
	VerdictBy string
	Hunks     []string
	CreatedAt int64
}

// SelfAttested reports whether the claim carries no evidence. Such claims are
// demoted out of the conclusions section of the dossier by machine rule; only
// evidence-bound claims can ever be presented as verified conclusions.
func (c Claim) SelfAttested() bool {
	return len(c.Evidence) == 0
}

// ValidKind reports whether kind is one of the recognized claim kinds. Callers
// use it to reject or normalize agent-supplied input.
func ValidKind(kind string) bool {
	switch kind {
	case KindBehavior, KindRegressionFixed, KindRuleCompliance, KindNonGoal:
		return true
	default:
		return false
	}
}
