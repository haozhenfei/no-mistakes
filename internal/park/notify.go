package park

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// ShellNotifier runs the configured notify.on_park / notify.on_unpark command.
//
// The command receives the wait as environment variables rather than argv, so a
// hook can be a one-liner (`echo "$NM_PARK_SUMMARY" >> ~/inbox`) without any
// positional-argument ceremony, and adding a field later cannot shift an
// existing hook's arguments out from under it.
//
// A failing or missing hook is logged and otherwise ignored: the notification
// is the loud layer, parked.json is the durable one, and a broken hook must
// never fail a run or - worse - change how a gate resolves.
type ShellNotifier struct {
	config   ConfigFunc
	parkFile string
}

// NewShellNotifier returns a Notifier that reads its hooks from cfg at
// notification time, so editing them does not require a daemon restart.
// parkFile is passed to the hook so it can read the full record instead of
// re-deriving it.
func NewShellNotifier(cfg ConfigFunc, parkFile string) *ShellNotifier {
	if cfg == nil {
		cfg = func() Config { return Config{} }
	}
	return &ShellNotifier{config: cfg, parkFile: parkFile}
}

// Notify runs the hook for ev, if one is configured.
func (n *ShellNotifier) Notify(ctx context.Context, ev Event) {
	if n == nil {
		return
	}
	cfg := n.config()
	cmdStr := strings.TrimSpace(cfg.OnPark)
	if ev.Kind == KindUnpark {
		cmdStr = strings.TrimSpace(cfg.OnUnpark)
	}
	if cmdStr == "" {
		return
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", cmdStr)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
	shellenv.ConfigureShellCommand(cmd)
	cmd.Env = append(os.Environ(), NotifyEnv(ev, n.parkFile)...)

	out, err := shellenv.CombinedOutputShellCommand(cmd)
	if err != nil {
		slog.Warn("notify hook failed",
			"event", string(ev.Kind), "run", ev.Record.RunID, "reminder", ev.Reminder,
			"error", err, "output", strings.TrimSpace(string(out)))
		return
	}
	slog.Info("notify hook ran",
		"event", string(ev.Kind), "run", ev.Record.RunID, "step", ev.Record.Step, "reminder", ev.Reminder)
}

// NotifyEnv is the environment a notify hook sees. These names are a public
// contract for hook authors - renaming one breaks every configured hook.
func NotifyEnv(ev Event, parkFile string) []string {
	rec := ev.Record
	env := []string{
		"NM_EVENT=" + string(ev.Kind),
		"NM_REMINDER=" + strconv.Itoa(ev.Reminder),
		"NM_RUN_ID=" + rec.RunID,
		"NM_REPO=" + rec.Repo,
		"NM_BRANCH=" + rec.Branch,
		"NM_STEP=" + rec.Step,
		"NM_GATE=" + rec.Gate,
		"NM_SINCE=" + rec.Since.Format("2006-01-02T15:04:05Z07:00"),
		"NM_PARKED_FILE=" + parkFile,
		"NM_FINDINGS=" + findingLines(rec.Findings),
		"NM_ACTIONS=" + strings.Join(rec.Actions, ","),
		"NM_RESPOND=" + strings.Join(rec.Respond, "\n"),
		"NM_PARK_SUMMARY=" + Summary(ev),
	}
	return env
}

func findingLines(fs []Finding) string {
	if len(fs) == 0 {
		return ""
	}
	lines := make([]string, 0, len(fs))
	for _, f := range fs {
		parts := []string{f.ID}
		if f.Severity != "" {
			parts = append(parts, "["+f.Severity+"]")
		}
		if f.Action != "" {
			parts = append(parts, "("+f.Action+")")
		}
		if f.File != "" {
			parts = append(parts, f.File)
		}
		if f.Description != "" {
			parts = append(parts, f.Description)
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return strings.Join(lines, "\n")
}

// Summary is the one-line-plus-detail form a hook can forward verbatim to a
// human or to a supervising agent's inbox. It names the run, the step, the gate,
// how long it has waited, what it is waiting on, and how to answer - so acting
// on the message never requires opening the logs.
func Summary(ev Event) string {
	rec := ev.Record
	var b strings.Builder
	if ev.Kind == KindUnpark {
		fmt.Fprintf(&b, "no-mistakes: run %s resumed (%s gate on %s answered)", rec.RunID, rec.Gate, rec.Step)
		return b.String()
	}
	head := fmt.Sprintf("no-mistakes: run %s is PARKED at the %s gate on step %s", rec.RunID, rec.Gate, rec.Step)
	if ev.Reminder > 0 {
		head = fmt.Sprintf("%s [reminder %d]", head, ev.Reminder)
	}
	b.WriteString(head + "\n")
	if rec.Repo != "" || rec.Branch != "" {
		fmt.Fprintf(&b, "repo: %s  branch: %s\n", rec.Repo, rec.Branch)
	}
	fmt.Fprintf(&b, "waiting since %s\n", rec.Since.Format("2006-01-02 15:04:05 -0700"))
	if len(rec.Findings) > 0 {
		fmt.Fprintf(&b, "findings (%d):\n", len(rec.Findings))
		for _, line := range strings.Split(findingLines(rec.Findings), "\n") {
			fmt.Fprintf(&b, "  - %s\n", line)
		}
	}
	if len(rec.Actions) > 0 {
		fmt.Fprintf(&b, "accepted answers: %s\n", strings.Join(rec.Actions, " | "))
	}
	for _, c := range rec.Respond {
		fmt.Fprintf(&b, "  %s\n", c)
	}
	return strings.TrimRight(b.String(), "\n")
}
