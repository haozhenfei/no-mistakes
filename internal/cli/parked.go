package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/park"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// newParkedCmd reads the durable parked record.
//
// It is deliberately daemon-independent: it opens <NM_HOME>/parked.json and
// nothing else. The whole point of the record is that a supervisor who was
// never listening - or whose listener died - can still find out what is waiting
// on them, so answering that question must not require a live daemon, a live
// socket, or a live event stream.
func newParkedCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "parked",
		Short: "Show runs parked at a gate, waiting for an answer",
		Long: "Reads <NM_HOME>/parked.json, the durable record of every run currently\n" +
			"parked at a gate: which step, which gate, since when, over which findings,\n" +
			"and which answers the gate accepts.\n\n" +
			"Exit code 0 when something is parked, 1 when nothing is.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := paths.New()
			if err != nil {
				return err
			}
			records, err := park.Load(p.ParkedFile())
			if err != nil {
				return err
			}
			return renderParked(cmd.OutOrStdout(), records, asJSON, time.Now())
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the record as JSON")
	return cmd
}

// renderParked prints the parked set. Nothing parked exits 1 so a supervisor's
// shell can branch on it (`no-mistakes parked || sleep 60`) without parsing.
func renderParked(w io.Writer, records []park.Record, asJSON bool, now time.Time) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if records == nil {
			records = []park.Record{}
		}
		if err := enc.Encode(records); err != nil {
			return err
		}
		if len(records) == 0 {
			return &exitError{code: 1}
		}
		return nil
	}

	if len(records) == 0 {
		fmt.Fprintln(w, "No run is parked.")
		return &exitError{code: 1}
	}

	for i, rec := range records {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "run %s is PARKED at the %s gate on step %s\n", rec.RunID, rec.Gate, rec.Step)
		if rec.Repo != "" || rec.Branch != "" {
			fmt.Fprintf(w, "  repo:     %s (%s)\n", rec.Repo, rec.Branch)
		}
		fmt.Fprintf(w, "  waiting:  %s (since %s)\n",
			park.FormatDuration(now.Sub(rec.Since)), rec.Since.Format("2006-01-02 15:04:05 -0700"))
		if rec.Reminders > 0 {
			fmt.Fprintf(w, "  reminders sent: %d\n", rec.Reminders)
		}
		if len(rec.Findings) > 0 {
			fmt.Fprintf(w, "  findings (%d):\n", len(rec.Findings))
			for _, f := range rec.Findings {
				fmt.Fprintf(w, "    - %s [%s] (%s) %s %s\n", f.ID, f.Severity, f.Action, f.File, f.Description)
			}
		}
		if len(rec.Actions) > 0 {
			fmt.Fprintf(w, "  accepted answers: %s\n", joinComma(rec.Actions))
		}
		for _, c := range rec.Respond {
			fmt.Fprintf(w, "    %s\n", c)
		}
	}
	return nil
}
