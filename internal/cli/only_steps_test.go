package cli

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// An --only naming a step that does not exist must be an error, never a silent
// full run: the caller asked for one step, and running ten instead (with agents,
// a push, and a PR) is the opposite of what they asked for.
func TestParseOnlySteps_UnknownStepIsAnError(t *testing.T) {
	for _, value := range []string{"qaa", "review,nope", "watch"} {
		got, err := parseOnlySteps(value)
		if err == nil {
			t.Fatalf("parseOnlySteps(%q) = %v, want an error", value, got)
		}
	}
}

// watch is a known step name (--skip accepts it for historical rows) but is not
// a gate step, so --only watch would resolve to "skip everything".
func TestParseOnlySteps_RejectsNonGateStepThatSkipAccepts(t *testing.T) {
	if _, err := parseSkipSteps("watch"); err != nil {
		t.Fatalf("parseSkipSteps(watch) error = %v, want nil (watch is a known step)", err)
	}
	if _, err := parseOnlySteps("watch"); err == nil {
		t.Fatal("parseOnlySteps(watch) = nil error, want a rejection: watch is not a gate step")
	}
}

func TestParseOnlySteps_AcceptsGateAndOnDemandSteps(t *testing.T) {
	got, err := parseOnlySteps("qa")
	if err != nil {
		t.Fatalf("parseOnlySteps(qa) error = %v", err)
	}
	if !slices.Equal(got, []types.StepName{types.StepQA}) {
		t.Fatalf("parseOnlySteps(qa) = %v, want [qa]", got)
	}

	got, err = parseOnlySteps("review, test")
	if err != nil {
		t.Fatalf("parseOnlySteps(review, test) error = %v", err)
	}
	if !slices.Equal(got, []types.StepName{types.StepReview, types.StepTest}) {
		t.Fatalf("parseOnlySteps(review, test) = %v, want [review test]", got)
	}
}

// Naming every step is legal and means what it says: run the whole pipeline AND
// the on-demand steps. The run's selection is recorded on its own column, so
// this needs no special encoding.
func TestParseOnlySteps_SelectingEveryStepIsTheFullPipelinePlusOnDemand(t *testing.T) {
	names := make([]string, 0, len(types.SelectableSteps()))
	for _, step := range types.SelectableSteps() {
		names = append(names, string(step))
	}
	got, err := parseOnlySteps(strings.Join(names, ","))
	if err != nil {
		t.Fatalf("parseOnlySteps(every step) error = %v", err)
	}
	if !slices.Equal(got, types.SelectableSteps()) {
		t.Fatalf("parseOnlySteps(every step) = %v, want every selectable step", got)
	}
}

// The push-option transport has to reject what the flags reject. Otherwise a
// push carrying both reaches the daemon, which resolves only and silently drops
// the skip set.
func TestNotifyPush_RejectsSkipAndOnlyTogether(t *testing.T) {
	cmd := newDaemonNotifyPushCmd()
	cmd.SetArgs([]string{
		"--gate", t.TempDir(), "--ref", "refs/heads/x",
		"--old", "0000000000000000000000000000000000000000",
		"--new", "1111111111111111111111111111111111111111",
		"--push-option", "no-mistakes.skip=lint",
		"--push-option", "no-mistakes.only=qa",
	})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("notify-push accepted both no-mistakes.skip and no-mistakes.only; want a rejection")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("error = %v, want it to explain the conflict", err)
	}
}

// --skip and --only describe the same set two ways; honoring both would make the
// result depend on an order the user cannot see.
func TestParseStepSelection_SkipAndOnlyAreMutuallyExclusive(t *testing.T) {
	if _, err := parseStepSelection("lint", "qa"); err == nil {
		t.Fatal("parseStepSelection(--skip lint --only qa) = nil error, want a rejection")
	}

	sel, err := parseStepSelection("", "qa")
	if err != nil {
		t.Fatalf("parseStepSelection(--only qa) error = %v", err)
	}
	if len(sel.skip) != 0 || !slices.Equal(sel.only, []types.StepName{types.StepQA}) {
		t.Fatalf("parseStepSelection(--only qa) = %+v, want only=[qa]", sel)
	}
}

// The selection reaches the daemon over the push that starts the run, so it has
// to survive the push-option transport.
func TestStepSelection_OnlySurvivesThePushOptionRoundTrip(t *testing.T) {
	sel, err := parseStepSelection("", "qa")
	if err != nil {
		t.Fatalf("parseStepSelection error = %v", err)
	}
	options := sel.pushOptions()
	if !slices.Contains(options, "no-mistakes.only=qa") {
		t.Fatalf("push options = %v, want no-mistakes.only=qa", options)
	}

	got, err := parseOnlyPushOptions(options)
	if err != nil {
		t.Fatalf("parseOnlyPushOptions error = %v", err)
	}
	if !slices.Equal(got, []types.StepName{types.StepQA}) {
		t.Fatalf("parseOnlyPushOptions(%v) = %v, want [qa]", options, got)
	}

	// A push that carries no selection must stay a default run.
	empty, err := parseOnlyPushOptions([]string{"no-mistakes.intent=abc"})
	if err != nil {
		t.Fatalf("parseOnlyPushOptions(no selection) error = %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("parseOnlyPushOptions(no selection) = %v, want none", empty)
	}
}

// --with is additive, and it accepts on-demand steps only: every other step is
// already in the pipeline, so naming one here would silently do nothing.
func TestParseWithSteps(t *testing.T) {
	got, err := parseWithSteps("qa")
	if err != nil {
		t.Fatalf("--with qa: %v", err)
	}
	if len(got) != 1 || got[0] != types.StepQA {
		t.Fatalf("--with qa = %v, want [qa]", got)
	}

	if _, err := parseWithSteps("review"); err == nil {
		t.Fatal("--with review was accepted; review is already part of the pipeline")
	}
	if _, err := parseWithSteps("watch"); err == nil {
		t.Fatal("--with watch was accepted; a watch run is derived, never requested")
	}
	if _, err := parseWithSteps("quality-assurance"); err == nil {
		t.Fatal("--with quality-assurance was accepted; it is not a step")
	}
}

// --with composes with both --skip and --only: it answers a different question
// ("also do this") from either of them.
func TestParseStepSelectionWith_ComposesWithSkipAndOnly(t *testing.T) {
	sel, err := parseStepSelectionWith("lint", "", "qa")
	if err != nil {
		t.Fatalf("--skip lint --with qa: %v", err)
	}
	if len(sel.skip) != 1 || sel.skip[0] != types.StepLint || len(sel.with) != 1 || sel.with[0] != types.StepQA {
		t.Fatalf("selection = %+v, want skip=[lint] with=[qa]", sel)
	}
	if sel.empty() {
		t.Fatal("a selection carrying --with reports itself as the default pipeline")
	}

	sel, err = parseStepSelectionWith("", "review", "qa")
	if err != nil {
		t.Fatalf("--only review --with qa: %v", err)
	}
	if len(sel.only) != 1 || sel.only[0] != types.StepReview || len(sel.with) != 1 {
		t.Fatalf("selection = %+v, want only=[review] with=[qa]", sel)
	}

	// The push transport must carry it, or a run started by `axi run` would lose
	// the selection on its way to the daemon.
	opts := sel.pushOptions()
	found := false
	for _, opt := range opts {
		if opt == "no-mistakes.with=qa" {
			found = true
		}
	}
	if !found {
		t.Fatalf("push options = %v, want one carrying no-mistakes.with=qa", opts)
	}
	parsed, err := parseWithPushOptions(opts)
	if err != nil {
		t.Fatalf("parse push options: %v", err)
	}
	if len(parsed) != 1 || parsed[0] != types.StepQA {
		t.Fatalf("parsed with-steps = %v, want [qa]", parsed)
	}
}
