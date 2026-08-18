package cli

import (
	"fmt"
	"sort"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/spf13/cobra"
)

// newReposCmd manages connected repositories: the ones declared by remote URL
// in config.yaml rather than initialized inside a local checkout.
func newReposCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repos",
		Short: "Manage connected repositories",
		Long:  "Connected repositories are declared by remote URL in config.yaml and validated without a local checkout.",
	}
	cmd.AddCommand(newReposListCmd())
	cmd.AddCommand(newReposReconcileCmd())
	cmd.AddCommand(newReposShowCmd())
	cmd.AddCommand(newReposRemoveCmd())
	return cmd
}

func newReposListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List connected repositories",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, d, err := openResources()
			if err != nil {
				return err
			}
			defer d.Close()

			connected, err := connectedRepos(d)
			if err != nil {
				return err
			}
			if len(connected) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no connected repositories (declare them under `repos:` in config.yaml, then run `no-mistakes repos reconcile`)")
				return nil
			}
			for _, repo := range connected {
				state := "active"
				if repo.DetachedAt != nil {
					state = "detached"
				}
				// SourceIdentity is credential-free by construction; UpstreamURL is
				// redacted. Neither can leak a token here.
				fmt.Fprintf(cmd.OutOrStdout(), "%-24s  %-10s  %s\n", repo.SourceName, state, repo.SourceIdentity)
			}
			return nil
		},
	}
}

func newReposReconcileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Bring registrations in line with config.yaml",
		Long:  "Registers newly declared repositories and marks removed ones detached. Reconciliation never deletes a gate or run history; use `repos remove` for that.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, d, err := openResources()
			if err != nil {
				return err
			}
			defer d.Close()

			globalCfg, err := config.LoadGlobal(p.ConfigFile())
			if err != nil {
				return fmt.Errorf("load global config: %w", err)
			}
			result, err := gate.ReconcileConnected(cmd.Context(), d, p, globalCfg.Repos)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, name := range result.Registered {
				fmt.Fprintf(out, "registered  %s\n", name)
			}
			for _, name := range result.Refreshed {
				fmt.Fprintf(out, "refreshed   %s\n", name)
			}
			for _, name := range result.Detached {
				fmt.Fprintf(out, "detached    %s (record and history kept; `repos remove` deletes)\n", name)
			}
			if len(result.Failed) > 0 {
				names := make([]string, 0, len(result.Failed))
				for name := range result.Failed {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					fmt.Fprintf(cmd.ErrOrStderr(), "failed      %s: %v\n", name, result.Failed[name])
				}
				// A partial reconcile is a real failure: the operator asked for an
				// inventory and did not get all of it.
				return &exitError{code: 1}
			}
			if len(result.Registered)+len(result.Refreshed)+len(result.Detached) == 0 {
				fmt.Fprintln(out, "nothing to reconcile")
			}
			return nil
		},
	}
}

func newReposShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one connected repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, d, err := openResources()
			if err != nil {
				return err
			}
			defer d.Close()

			repo, err := d.GetRepoBySourceName(args[0])
			if err != nil {
				return err
			}
			if repo == nil || !repo.Connected() {
				return fmt.Errorf("no connected repository named %q", args[0])
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "name:            %s\n", repo.SourceName)
			fmt.Fprintf(out, "identity:        %s\n", repo.SourceIdentity)
			fmt.Fprintf(out, "id:              %s\n", repo.ID)
			fmt.Fprintf(out, "default branch:  %s\n", repo.DefaultBranch)
			fmt.Fprintf(out, "gate:            %s\n", p.RepoDir(repo.ID))
			if repo.DetachedAt != nil {
				fmt.Fprintf(out, "detached:        %s\n", time.Unix(*repo.DetachedAt, 0).Format(time.DateTime))
			}
			return nil
		},
	}
}

func newReposRemoveCmd() *cobra.Command {
	var deleteHistory, yes bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a connected repository's registration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, d, err := openResources()
			if err != nil {
				return err
			}
			defer d.Close()

			// Deleting a gate destroys run history that cannot be recovered, so it
			// takes an explicit confirmation rather than only a flag.
			if deleteHistory && !yes {
				return fmt.Errorf("--delete-history permanently removes %q's gate, worktrees, and run history; pass --yes to confirm", args[0])
			}
			if err := gate.RemoveConnected(cmd.Context(), d, p, args[0], deleteHistory); err != nil {
				return err
			}
			if deleteHistory {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s and deleted its gate and history\n", args[0])
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %s (gate and history kept on disk)\n", args[0])
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&deleteHistory, "delete-history", false, "also delete the gate, worktrees, and run history")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm --delete-history without prompting")
	return cmd
}

func connectedRepos(d *db.DB) ([]*db.Repo, error) {
	repos, err := d.GetRepos()
	if err != nil {
		return nil, err
	}
	var connected []*db.Repo
	for _, repo := range repos {
		if repo.Connected() {
			connected = append(connected, repo)
		}
	}
	sort.Slice(connected, func(i, j int) bool { return connected[i].SourceName < connected[j].SourceName })
	return connected, nil
}
