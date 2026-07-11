package cli

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/covaudit"
	"github.com/kunchenguid/no-mistakes/internal/coverage"
	"github.com/spf13/cobra"
)

// newCoverageCmd builds the `no-mistakes coverage` command tree: the write and
// read interface to the coverage ledger (design §5). Gates (or an in-run agent)
// record one entry per changed hunk with its verification state; `coverage
// audit` backfills those entries against captured instrumentation truth and runs
// the §4.4c machine audit. The runtime-verified state is not taken on faith —
// `audit` overwrites it from coverage data.
func newCoverageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "coverage",
		Short:         "Record and audit the per-hunk coverage ledger for the current run",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.AddCommand(newCoverageAddCmd())
	cmd.AddCommand(newCoverageListCmd())
	cmd.AddCommand(newCoverageAuditCmd())
	return cmd
}

func newCoverageAddCmd() *cobra.Command {
	var file, state, reason, source string
	var start, end int
	var evidenceIDs []string
	cmd := &cobra.Command{
		Use:   "add --file <path> --start <n> --end <n> --state <state> [--evidence ev-…] [--reason …]",
		Short: "Record a coverage-ledger entry for a changed hunk",
		Long: "States: runtime-verified | static-verified | attested | unverified.\n" +
			"runtime-verified is authoritative only after `coverage audit` confirms it\n" +
			"against instrumentation; a runtime-verified label with no coverage behind it\n" +
			"is downgraded. static-verified must carry captured executable static\n" +
			"evidence (typecheck/AST output via `evidence exec`), not a prose note.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(file) == "" {
				return fmt.Errorf("--file is required")
			}
			if start <= 0 || end <= 0 || end < start {
				return fmt.Errorf("--start and --end must be positive with end >= start")
			}
			if !coverage.ValidState(state) {
				return fmt.Errorf("invalid --state %q (want runtime-verified|static-verified|attested|unverified)", state)
			}
			if state == coverage.StateUnverified && strings.TrimSpace(reason) == "" {
				return fmt.Errorf("--reason is required for unverified hunks")
			}
			if source == "" {
				source = "agent"
			}
			c, err := openInRunContext(cmd.Context())
			if err != nil {
				return err
			}
			defer c.close()
			if c.run == nil {
				return fmt.Errorf("no active run for this worktree; coverage entries must be recorded during a run")
			}
			entry, err := c.d.InsertCoverageEntry(coverage.LedgerEntry{
				RunID:     c.run.ID,
				File:      file,
				StartLine: start,
				EndLine:   end,
				State:     state,
				Reason:    reason,
				Evidence:  evidenceIDs,
				Source:    source,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s:%d-%d\t%s\n", entry.ID, entry.File, entry.StartLine, entry.EndLine, entry.State)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "changed file path, repo-relative (required)")
	cmd.Flags().IntVar(&start, "start", 0, "hunk start line (required)")
	cmd.Flags().IntVar(&end, "end", 0, "hunk end line (required)")
	cmd.Flags().StringVar(&state, "state", "", "runtime-verified|static-verified|attested|unverified (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why unverified / context (required for unverified)")
	cmd.Flags().StringSliceVar(&evidenceIDs, "evidence", nil, "evidence IDs backing the state")
	cmd.Flags().StringVar(&source, "source", "", "the gate/agent recording the entry (default agent)")
	return cmd
}

func newCoverageListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List coverage-ledger entries for the current run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openInRunContext(cmd.Context())
			if err != nil {
				return err
			}
			defer c.close()
			if c.run == nil {
				return fmt.Errorf("no active run for this worktree")
			}
			entries, err := c.d.GetCoverageEntriesByRun(c.run.ID)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(w, "no coverage ledger entries")
				return nil
			}
			for _, e := range entries {
				reason := ""
				if e.Reason != "" {
					reason = "\t" + e.Reason
				}
				fmt.Fprintf(w, "%s\t%s:%d-%d\t%s%s\n", e.ID, e.File, e.StartLine, e.EndLine, e.State, reason)
			}
			return nil
		},
	}
	return cmd
}

func newCoverageAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Backfill the ledger from instrumentation and run the §4.4c coverage audit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openInRunContext(cmd.Context())
			if err != nil {
				return err
			}
			defer c.close()
			if c.run == nil {
				return fmt.Errorf("no active run for this worktree")
			}
			res, err := covaudit.Run(cmd.Context(), c.d, c.run.ID, c.repoRoot, c.p.EvidenceKeyFile(), c.run.BaseSHA, c.run.HeadSHA)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintln(w, res.Report.String())
			for _, dg := range res.Downgrades {
				fmt.Fprintf(w, "downgraded %s:%d-%d %s→%s (%s)\n", dg.Hunk.File, dg.Hunk.Start, dg.Hunk.End, dg.From, dg.To, dg.Reason)
			}
			for _, is := range res.Report.Issues {
				fmt.Fprintf(w, "issue [%s] %s:%d-%d — %s\n", is.Kind, is.Hunk.File, is.Hunk.Start, is.Hunk.End, is.Detail)
			}
			if res.Report.Pass {
				fmt.Fprintln(w, "coverage audit: PASS")
			} else {
				fmt.Fprintln(w, "coverage audit: FAIL")
			}
			return nil
		},
	}
	return cmd
}
