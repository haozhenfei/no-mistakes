package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

var (
	daemonRun         = daemon.Run
	daemonStartFn     = daemon.Start
	daemonStopFn      = daemon.Stop
	daemonIsRunningFn = daemon.IsRunning
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the no-mistakes daemon",
	}

	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonRestartCmd())
	cmd.AddCommand(newDaemonStatusCmd())
	cmd.AddCommand(newDaemonRunCmd())
	cmd.AddCommand(newDaemonNotifyPushCmd())

	return cmd
}

func newDaemonNotifyPushCmd() *cobra.Command {
	var gate string
	var ref string
	var oldSHA string
	var newSHA string
	var pushOptions []string

	cmd := &cobra.Command{
		Use:    "notify-push",
		Short:  "Notify daemon about a git push",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			skipSteps, err := parseSkipPushOptions(pushOptions)
			if err != nil {
				return err
			}
			onlySteps, err := parseOnlyPushOptions(pushOptions)
			if err != nil {
				return err
			}
			// The transport must reject what the flags reject. Otherwise a push
			// carrying both no-mistakes.skip and no-mistakes.only reaches the
			// daemon, which resolves `only` and silently drops the skip set.
			if len(skipSteps) > 0 && len(onlySteps) > 0 {
				return fmt.Errorf("push options no-mistakes.skip and no-mistakes.only cannot be combined: only already skips every step it does not name")
			}
			withSteps, err := parseWithPushOptions(pushOptions)
			if err != nil {
				return err
			}
			allowGateConfig, err := parseAllowGateConfigPushOptions(pushOptions)
			if err != nil {
				return err
			}
			intent, err := parseIntentPushOptions(pushOptions)
			if err != nil {
				return err
			}
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}

			p, err := paths.New()
			if err != nil {
				return err
			}

			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			var result ipc.PushReceivedResult
			return client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate:            gatePath,
				Ref:             ref,
				Old:             oldSHA,
				New:             newSHA,
				SkipSteps:       skipSteps,
				OnlySteps:       onlySteps,
				WithSteps:       withSteps,
				AllowGateConfig: allowGateConfig,
				Intent:          intent,
			}, &result)
		},
	}

	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that received the push")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "previous commit SHA")
	cmd.Flags().StringVar(&newSHA, "new", "", "new commit SHA")
	cmd.Flags().StringArrayVar(&pushOptions, "push-option", nil, "git push option")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("old")
	_ = cmd.MarkFlagRequired("new")

	return cmd
}

func normalizeNotifyGatePath(gate string) (string, error) {
	if strings.TrimSpace(gate) == "" {
		return "", fmt.Errorf("gate path is required")
	}
	abs, err := filepath.Abs(gate)
	if err != nil {
		return "", fmt.Errorf("resolve gate path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func parseSkipPushOptions(options []string) ([]types.StepName, error) {
	var steps []types.StepName
	for _, option := range options {
		value, ok := strings.CutPrefix(option, "no-mistakes.skip=")
		if !ok {
			continue
		}
		parsed, err := parseSkipSteps(value)
		if err != nil {
			return nil, err
		}
		steps = append(steps, parsed...)
	}
	return dedupeSteps(steps), nil
}

func parseSkipSteps(value string) ([]types.StepName, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var steps []types.StepName
	for _, part := range strings.Split(value, ",") {
		step := types.StepName(strings.TrimSpace(part))
		if !validStep(step) {
			return nil, fmt.Errorf("unknown step %q", step)
		}
		steps = append(steps, step)
	}
	return dedupeSteps(steps), nil
}

// allowGateConfigPushOption carries the run's gate-config opt-in through the
// git push that starts it. It is a bare flag: presence means "this run's agents
// may write the gate's own config". Absence - the case for every ordinary push -
// is the default-deny.
//
// Being a push option (rather than a repo-config key) is the point: the
// permission is visible on the run that used it, and a pushed branch cannot
// grant it to itself by editing a file. Whoever starts the run has to say so.
const allowGateConfigPushOption = "no-mistakes.allow-gate-config"

// parseAllowGateConfigPushOptions reports whether the push carried the opt-in.
// Both the bare form and an explicit `=true`/`=1` are accepted; an explicit
// false-y value is refused rather than silently read as an opt-in.
func parseAllowGateConfigPushOptions(options []string) (bool, error) {
	allow := false
	for _, option := range options {
		if option == allowGateConfigPushOption {
			allow = true
			continue
		}
		value, ok := strings.CutPrefix(option, allowGateConfigPushOption+"=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes":
			allow = true
		case "0", "false", "no":
			allow = false
		default:
			return false, fmt.Errorf("push option %s: unknown value %q (use the bare option, or =true/=false)", allowGateConfigPushOption, value)
		}
	}
	return allow, nil
}

// formatAllowGateConfigPushOption renders the opt-in for the git push that
// starts a run, or "" when the run did not ask for it.
func formatAllowGateConfigPushOption(allow bool) string {
	if !allow {
		return ""
	}
	return allowGateConfigPushOption
}

// intentPushOptionPrefix carries an agent-supplied intent through a git push.
// The value is base64-encoded so multi-line or special-character intents
// survive the push-option transport (which is line-oriented).
const intentPushOptionPrefix = "no-mistakes.intent="

// formatIntentPushOption encodes intent as a single push option, or returns ""
// when there is no intent to carry.
func formatIntentPushOption(intent string) string {
	if strings.TrimSpace(intent) == "" {
		return ""
	}
	return intentPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(intent))
}

// parseIntentPushOptions extracts and decodes the intent push option, if any.
// The last occurrence wins.
func parseIntentPushOptions(options []string) (string, error) {
	intent := ""
	for _, option := range options {
		encoded, ok := strings.CutPrefix(option, intentPushOptionPrefix)
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("decode intent push option: %w", err)
		}
		intent = string(decoded)
	}
	return intent, nil
}

func formatSkipPushOptions(steps []types.StepName) []string {
	return formatStepPushOptions("no-mistakes.skip=", steps)
}

func formatOnlyPushOptions(steps []types.StepName) []string {
	return formatStepPushOptions(onlyPushOptionPrefix, steps)
}

func formatWithPushOptions(steps []types.StepName) []string {
	return formatStepPushOptions(withPushOptionPrefix, steps)
}

func formatStepPushOptions(prefix string, steps []types.StepName) []string {
	if len(steps) == 0 {
		return nil
	}
	parts := make([]string, 0, len(steps))
	for _, step := range dedupeSteps(steps) {
		parts = append(parts, string(step))
	}
	return []string{prefix + strings.Join(parts, ",")}
}

// onlyPushOptionPrefix carries an exclusive step selection through a git push,
// the transport `axi run` uses to start a run. It is the complement of
// no-mistakes.skip: the daemon turns it into the run's skip set.
const onlyPushOptionPrefix = "no-mistakes.only="

func parseOnlyPushOptions(options []string) ([]types.StepName, error) {
	var steps []types.StepName
	for _, option := range options {
		value, ok := strings.CutPrefix(option, onlyPushOptionPrefix)
		if !ok {
			continue
		}
		parsed, err := parseOnlySteps(value)
		if err != nil {
			return nil, err
		}
		steps = append(steps, parsed...)
	}
	return dedupeSteps(steps), nil
}

// parseOnlySteps validates an --only selection. Unlike --skip it accepts only
// steps a gate run can execute, so `--only watch` is rejected rather than
// producing a run with every step skipped.
func parseOnlySteps(value string) ([]types.StepName, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var steps []types.StepName
	for _, part := range strings.Split(value, ",") {
		step := types.StepName(strings.TrimSpace(part))
		if !selectableStep(step) {
			return nil, fmt.Errorf("unknown step %q", step)
		}
		steps = append(steps, step)
	}
	return dedupeSteps(steps), nil
}

// withPushOptionPrefix carries an additive on-demand selection through the git
// push that starts a run.
const withPushOptionPrefix = "no-mistakes.with="

func parseWithPushOptions(options []string) ([]types.StepName, error) {
	var steps []types.StepName
	for _, option := range options {
		value, ok := strings.CutPrefix(option, withPushOptionPrefix)
		if !ok {
			continue
		}
		parsed, err := parseWithSteps(value)
		if err != nil {
			return nil, err
		}
		steps = append(steps, parsed...)
	}
	return dedupeSteps(steps), nil
}

// parseWithSteps validates a --with selection: the on-demand steps to add to an
// otherwise normal run. It accepts ONLY on-demand steps, because that is the only
// thing --with can mean - every other step is already in the pipeline, so naming
// one here would silently do nothing.
//
// It exists because --only cannot express "the usual pipeline, plus QA": --only
// is exclusive, so `--only qa` runs QA alone. Naming the other ten steps to get
// the eleventh is not a usable interface, and "the full gate, then QA next to the
// CI watcher" is the case this whole run-kind split was built for.
func parseWithSteps(value string) ([]types.StepName, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var steps []types.StepName
	for _, part := range strings.Split(value, ",") {
		step := types.StepName(strings.TrimSpace(part))
		if !types.IsOnDemandStep(step) {
			return nil, fmt.Errorf("--with accepts only on-demand steps (%s); %q is %s",
				onDemandStepList(), step, withStepRejection(step))
		}
		steps = append(steps, step)
	}
	return dedupeSteps(steps), nil
}

func withStepRejection(step types.StepName) string {
	if selectableStep(step) || validStep(step) {
		return "already part of the pipeline - use --skip or --only to change which steps run"
	}
	return "not a known step"
}

func onDemandStepList() string {
	names := make([]string, 0, len(types.OnDemandSteps()))
	for _, step := range types.OnDemandSteps() {
		names = append(names, string(step))
	}
	return strings.Join(names, ", ")
}

func validStep(step types.StepName) bool {
	return containsStepName(types.KnownSteps(), step)
}

func selectableStep(step types.StepName) bool {
	return containsStepName(types.SelectableSteps(), step)
}

func containsStepName(known []types.StepName, step types.StepName) bool {
	for _, k := range known {
		if step == k {
			return true
		}
	}
	return false
}

// stepSelectionFlagsError rejects --skip and --only together: they are two ways
// to describe the same set, and honoring both would make the result depend on
// an evaluation order the user cannot see.
func stepSelectionFlagsError(skipValue, onlyValue string) error {
	if strings.TrimSpace(skipValue) != "" && strings.TrimSpace(onlyValue) != "" {
		return fmt.Errorf("--skip and --only cannot be combined: --only already skips every step it does not name")
	}
	return nil
}

// stepSelection is what a caller asked a new run to execute: a set of steps to
// skip (--skip) or an exclusive set to run (--only), plus any on-demand steps to
// add (--with). --skip and --only are mutually exclusive; --with composes with
// either, because it answers a different question ("also do this") from both of
// them ("do not do this" / "do nothing else").
type stepSelection struct {
	skip []types.StepName
	only []types.StepName
	with []types.StepName
}

// parseStepSelection validates --skip/--only and rejects them together.
func parseStepSelection(skipValue, onlyValue string) (stepSelection, error) {
	return parseStepSelectionWith(skipValue, onlyValue, "")
}

// parseStepSelectionWith validates the full --skip/--only/--with triple.
func parseStepSelectionWith(skipValue, onlyValue, withValue string) (stepSelection, error) {
	if err := stepSelectionFlagsError(skipValue, onlyValue); err != nil {
		return stepSelection{}, err
	}
	skip, err := parseSkipSteps(skipValue)
	if err != nil {
		return stepSelection{}, err
	}
	only, err := parseOnlySteps(onlyValue)
	if err != nil {
		return stepSelection{}, err
	}
	with, err := parseWithSteps(withValue)
	if err != nil {
		return stepSelection{}, err
	}
	return stepSelection{skip: skip, only: only, with: with}, nil
}

// pushOptions renders the selection for the git push that starts a run.
func (s stepSelection) pushOptions() []string {
	options := append(formatSkipPushOptions(s.skip), formatOnlyPushOptions(s.only)...)
	return append(options, formatWithPushOptions(s.with)...)
}

// empty reports whether the caller asked for the default pipeline.
func (s stepSelection) empty() bool {
	return len(s.skip) == 0 && len(s.only) == 0 && len(s.with) == 0
}

// stepSelectionHelp lists the step names --skip and --only accept.
func stepSelectionHelp() string {
	names := make([]string, 0, len(types.SelectableSteps()))
	for _, step := range types.SelectableSteps() {
		names = append(names, string(step))
	}
	return "Valid steps: " + strings.Join(names, ", ")
}

func dedupeSteps(steps []types.StepName) []types.StepName {
	seen := make(map[types.StepName]bool, len(steps))
	out := make([]types.StepName, 0, len(steps))
	for _, step := range steps {
		if seen[step] {
			continue
		}
		seen[step] = true
		out = append(out, step)
	}
	return out
}

func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Install or refresh the managed daemon service and start it",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.start", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := daemonStartFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon started\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.stop", force)
			return trackCommand("daemon.stop", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon stop", force); err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon stopped\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop the daemon even when pipeline runs are active")
	return cmd
}

func newDaemonRestartCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon (stop if running, then start)",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.restart", force)
			return trackCommand("daemon.restart", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon restart", force); err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return fmt.Errorf("stop daemon: %w", err)
				}
				if err := daemonStartFn(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon restarted\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restart the daemon even when pipeline runs are active")
	return cmd
}

func guardDestructiveDaemonLifecycle(p *paths.Paths, stderr io.Writer, action string, force bool) error {
	runs, err := lifecycle.ActiveRuns(p)
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}
	if len(runs) == 0 {
		return nil
	}
	if force {
		fmt.Fprintf(stderr, "FORCE: %s will stop/restart the daemon while %d active pipeline runs are in progress\n", action, len(runs))
		fmt.Fprint(stderr, lifecycle.RunList(runs))
		return nil
	}
	return fmt.Errorf("refusing %s because %d active pipeline runs are in progress; pass --force to stop/restart the daemon anyway\n%s", action, len(runs), lifecycle.RunList(runs))
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.status", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				alive, err := daemonIsRunningFn(p)
				if err != nil {
					return err
				}
				if alive {
					pid, _ := daemon.ReadPID(p)
					if pid > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running %s\n", sGreen.Render("●"), sDim.Render(fmt.Sprintf("(pid %d)", pid)))
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running\n", sGreen.Render("●"))
					}
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon not running\n", sDim.Render("○"))
				}
				return nil
			})
		},
	}
}

func newDaemonRunCmd() *cobra.Command {
	var root string

	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Run the daemon in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if root != "" {
				if err := os.Setenv("NM_HOME", root); err != nil {
					return fmt.Errorf("set NM_HOME: %w", err)
				}
			}
			return daemonRun()
		},
	}

	cmd.Flags().StringVar(&root, "root", "", "override no-mistakes data directory")
	return cmd
}
