package cli

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/spf13/cobra"
)

// newEvidenceCmd builds the `no-mistakes evidence` command tree. It is the
// worktree-side client of the Evidence Vault: the in-run agent calls it to
// capture command output as signed evidence, register its own artifacts, and
// list what has been recorded so it can reference evidence IDs in claims.
//
// MVP trust boundary: collection runs locally in this CLI process rather than
// in the daemon. The signing key stays outside the worktree (under NM_HOME), so
// evidence written into the worktree cannot forge a signature by editing the
// manifest — but a same-user agent could in principle read the key. Daemon-side
// collection is the deferred hardening (see design §3.1/§11.6).
func newEvidenceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "evidence",
		Short: "Capture and register signed evidence for the current run",
		Long: "Evidence Vault client. `evidence exec` runs a command and records its\n" +
			"output as signed, captured evidence; `evidence attach` registers an\n" +
			"agent-supplied file as attested evidence; `evidence list` shows recorded\n" +
			"entries so you can reference their IDs in `no-mistakes claim add`.",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.AddCommand(newEvidenceExecCmd())
	cmd.AddCommand(newEvidenceAttachCmd())
	cmd.AddCommand(newEvidenceListCmd())
	return cmd
}

// openEvidenceStore resolves the run context and opens the branch's evidence
// store with the signing key from NM_HOME.
func openEvidenceStore(c *inRunContext) (*evidence.Store, error) {
	key, err := evidence.LoadOrCreateKey(c.p.EvidenceKeyFile())
	if err != nil {
		return nil, err
	}
	return evidence.Open(evidence.DirForBranch(c.repoRoot, c.branch), key)
}

func newEvidenceExecCmd() *cobra.Command {
	var label string
	var claimIDs []string
	cmd := &cobra.Command{
		Use:   "exec --label <label> -- <cmd> [args...]",
		Short: "Run a command and record its output as captured evidence",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(label) == "" {
				return fmt.Errorf("--label is required")
			}
			c, err := openInRunContext(cmd.Context())
			if err != nil {
				return err
			}
			defer c.close()
			store, err := openEvidenceStore(c)
			if err != nil {
				return err
			}
			entry, err := store.Exec(cmd.Context(), evidence.ExecOpts{
				Label:    label,
				Argv:     args,
				Dir:      c.repoRoot,
				RepoRoot: c.repoRoot,
				Commit:   c.headSHA,
				RunID:    c.runID(),
				Branch:   c.branch,
				Claims:   claimIDs,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\tcaptured\texit=%d\t%s\n", entry.ID, entry.ExitCode, entry.Label)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "human-readable evidence label (required)")
	cmd.Flags().StringSliceVar(&claimIDs, "claim", nil, "claim IDs this evidence supports")
	return cmd
}

func newEvidenceAttachCmd() *cobra.Command {
	var label, file string
	var claimIDs []string
	cmd := &cobra.Command{
		Use:   "attach --file <path> --label <label>",
		Short: "Register an agent-supplied file as attested evidence",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(file) == "" {
				return fmt.Errorf("--file is required")
			}
			if strings.TrimSpace(label) == "" {
				return fmt.Errorf("--label is required")
			}
			c, err := openInRunContext(cmd.Context())
			if err != nil {
				return err
			}
			defer c.close()
			store, err := openEvidenceStore(c)
			if err != nil {
				return err
			}
			entry, err := store.Attach(evidence.AttachOpts{
				Label:    label,
				File:     file,
				RepoRoot: c.repoRoot,
				Commit:   c.headSHA,
				RunID:    c.runID(),
				Branch:   c.branch,
				Claims:   claimIDs,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\tattested\t%s\n", entry.ID, entry.Label)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to the artifact to register (required)")
	cmd.Flags().StringVar(&label, "label", "", "human-readable evidence label (required)")
	cmd.Flags().StringSliceVar(&claimIDs, "claim", nil, "claim IDs this evidence supports")
	return cmd
}

func newEvidenceListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recorded evidence entries for the current run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openInRunContext(cmd.Context())
			if err != nil {
				return err
			}
			defer c.close()
			key, err := evidence.LoadOrCreateKey(c.p.EvidenceKeyFile())
			if err != nil {
				return err
			}
			entries, err := evidence.LoadAll(c.repoRoot, key)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(w, "no evidence recorded")
				return nil
			}
			for _, e := range entries {
				prov := e.EffectiveProvenance()
				if e.Tampered() {
					prov += " (signature invalid)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.ID, e.Kind, prov, e.Label)
			}
			return nil
		},
	}
	return cmd
}
