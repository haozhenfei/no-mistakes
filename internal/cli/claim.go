package cli

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/claims"
	"github.com/spf13/cobra"
)

// newClaimCmd builds the `no-mistakes claim` command tree. The in-run agent
// registers claims bound to evidence IDs so the verify step can adjudicate them
// and the dossier can present them. A claim without evidence is stored, but is
// machine-demoted to the self-attested bucket and can never appear in the
// dossier's conclusions (design §5).
func newClaimCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "claim",
		Short:         "Register evidence-bound claims for the current run",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.AddCommand(newClaimAddCmd())
	cmd.AddCommand(newClaimListCmd())
	return cmd
}

func newClaimAddCmd() *cobra.Command {
	var text, kind, step string
	var evidenceIDs, hunks []string
	cmd := &cobra.Command{
		Use:   "add --text <text> --evidence <ev-id,...> [--kind <kind>]",
		Short: "Register a claim bound to evidence IDs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("--text is required")
			}
			if kind == "" {
				kind = claims.KindBehavior
			}
			if !claims.ValidKind(kind) {
				return fmt.Errorf("invalid --kind %q (want behavior|regression-fixed|rule-compliance|non-goal)", kind)
			}
			c, err := openInRunContext(cmd.Context())
			if err != nil {
				return err
			}
			defer c.close()
			if c.run == nil {
				return fmt.Errorf("no active run for this worktree; claims must be registered during a run")
			}
			if step == "" {
				step = "test"
			}
			claim, err := c.d.InsertClaim(claims.Claim{
				RunID:    c.run.ID,
				Step:     step,
				Text:     text,
				Kind:     kind,
				Evidence: evidenceIDs,
				Hunks:    hunks,
			})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if claim.SelfAttested() {
				fmt.Fprintf(w, "%s\tself-attested (no evidence; excluded from conclusions)\t%s\n", claim.ID, claim.Text)
			} else {
				fmt.Fprintf(w, "%s\tclaim\tevidence=%s\t%s\n", claim.ID, strings.Join(claim.Evidence, ","), claim.Text)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&text, "text", "", "the claim statement (required)")
	cmd.Flags().StringVar(&kind, "kind", "", "behavior|regression-fixed|rule-compliance|non-goal (default behavior)")
	cmd.Flags().StringVar(&step, "step", "", "the step registering the claim (default test)")
	cmd.Flags().StringSliceVar(&evidenceIDs, "evidence", nil, "evidence IDs that back this claim")
	cmd.Flags().StringSliceVar(&hunks, "hunks", nil, "diff hunks this claim covers (informational in MVP)")
	return cmd
}

func newClaimListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered claims for the current run",
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
			list, err := c.d.GetClaimsByRun(c.run.ID)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(w, "no claims registered")
				return nil
			}
			for _, cl := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cl.ID, claimBucket(cl), verdictLabel(cl.Verdict), cl.Text)
			}
			return nil
		},
	}
	return cmd
}

func claimBucket(c claims.Claim) string {
	if c.SelfAttested() {
		return "self-attested"
	}
	return "evidence-bound"
}

func verdictLabel(v string) string {
	if v == "" {
		return "unverified"
	}
	return v
}
