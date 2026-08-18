package cli

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

// newQACmd is the operator-scope command group: validation of repositories
// declared by remote URL, run without a checkout and without a push.
func newQACmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "qa",
		Short: "Validate connected repositories without a checkout",
		Long:  "QA validates repositories declared under `repos:` in config.yaml. A QA run never touches your local checkouts, and only an explicitly consented run writes code.",
	}
	cmd.AddCommand(newQARunCmd())
	return cmd
}

func newQARunCmd() *cobra.Command {
	var repoSelector, branch, mode string
	var yes bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run QA once against a connected repository",
		Long: "Run the pipeline once against a connected repository's branch.\n\n" +
			"Modes: report (default) and comment validate read-only; fix-pr is the only mode that writes code, " +
			"and it requires --yes because a person must consent to that specific run.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runMode := types.RunMode(strings.TrimSpace(mode))
			if runMode == "" {
				// Deliberately not qa.default_mode: that key is what a WATCH
				// inherits, and a command a person just typed should not silently
				// pick up a mode configured for unattended work.
				runMode = types.RunModeReport
			}
			if !types.ValidRunMode(string(runMode)) {
				return fmt.Errorf("--mode must be one of report, comment, fix-pr (got %q)", mode)
			}
			if strings.TrimSpace(repoSelector) == "" {
				return fmt.Errorf("--repo is required: QA runs against a connected repository (see 'no-mistakes repos list')")
			}

			return trackCommand("qa-run", func() error {
				p, d, err := openResources()
				if err != nil {
					return err
				}
				defer d.Close()

				repo, err := resolveRepo(d, repoSelector)
				if err != nil {
					return err
				}
				if !repo.Connected() {
					return fmt.Errorf("%q is a local repository; QA runs only against connected repositories (see 'no-mistakes repos list')", repoSelectorLabel(repo))
				}
				targetBranch := strings.TrimSpace(branch)
				if targetBranch == "" {
					targetBranch = repo.DefaultBranch
				}
				// Consent for a bounded scope, never a quiet default: the one mode
				// that writes code says exactly what it may do, and refuses to
				// proceed unless the person typed --yes.
				if runMode.WritesCode() && !yes {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\n", qaFixPRConsentNotice(repo, targetBranch))
					return fmt.Errorf("--mode fix-pr requires --yes")
				}

				if err := daemon.EnsureDaemon(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					return fmt.Errorf("connect to daemon: %w", err)
				}
				defer client.Close()

				var result ipc.QARunResult
				if err := client.Call(ipc.MethodQARun, &ipc.QARunParams{
					RepoID: repo.ID,
					Branch: targetBranch,
					Mode:   runMode,
				}, &result); err != nil {
					return fmt.Errorf("start qa run: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s QA run started for %s %s %s\n",
					sGreen.Render("✓"), repoSelectorLabel(repo), targetBranch, sDim.Render(result.RunID))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&repoSelector, "repo", "", "connected repository name or ID")
	cmd.Flags().StringVar(&branch, "branch", "", "branch to validate (default: the repository's default branch)")
	cmd.Flags().StringVar(&mode, "mode", "", "report (default), comment, or fix-pr")
	cmd.Flags().BoolVar(&yes, "yes", false, "consent to a code-writing run (required with --mode fix-pr)")
	return cmd
}

// qaFixPRConsentNotice states exactly what a fix-pr run may do, naming the
// repository and branch it would write to. It never prints a URL: a connected
// repository's URL routinely carries a token.
func qaFixPRConsentNotice(repo *db.Repo, branch string) string {
	target := repoSelectorLabel(repo)
	base := strings.TrimSpace(repo.DefaultBranch)
	if base == "" {
		base = "the default branch"
	}
	return fmt.Sprintf(
		"  %s --mode fix-pr writes code: it may commit fixes, push %s to %s, and open a pull request against %s.\n  Re-run with --yes to consent to this run.",
		sYellow.Render("!"), branch, target, base)
}
