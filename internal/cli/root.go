package cli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
)

// exitError carries an explicit process exit code. Commands that render their
// own structured output (the axi surface) return one of these so they can map
// outcomes onto AXI exit-code conventions (0 success/no-op, 1 error, 2 usage)
// without cobra printing the Go error to the user. A nil inner err prints
// nothing to stderr; a non-nil err is surfaced as a diagnostic.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return ""
}

func (e *exitError) Unwrap() error { return e.err }

// Execute runs the root CLI command.
func Execute() int {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.err != nil {
				fmt.Fprintln(root.ErrOrStderr(), ee.err)
			}
			return ee.code
		}
		fmt.Fprintln(root.ErrOrStderr(), err)
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	var autoYes bool
	var skipValue string

	cmd := &cobra.Command{
		Use:     "no-mistakes",
		Short:   "Local Git proxy that validates code before pushing to the configured target",
		Version: buildinfo.String(),
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			setColorProfileForOutput(cmd.OutOrStdout())
			return guardGateControl(cmd)
		},
		// Silence cobra's default error/usage printing — we handle it ourselves.
		SilenceErrors: true,
		SilenceUsage:  true,
		// When run without a subcommand, attach to the current branch run or
		// route users into the setup wizard when no run is active. The default
		// wizard flow is interactive, while --yes auto-accepts defaults and can
		// still fall back to headless mode when no TTY is available.
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("root", func() error {
				skipSteps, err := parseSkipSteps(skipValue)
				if err != nil {
					return err
				}
				return attachRun(cmd.Context(), cmd.OutOrStdout(), "", true, autoYes, skipSteps)
			})
		},
	}

	cmd.Flags().BoolVarP(&autoYes, "yes", "y", false, "run setup wizard and accept defaults automatically")
	cmd.Flags().StringVar(&skipValue, "skip", "", "comma-separated pipeline steps to skip for a new run")

	cmd.AddCommand(newInitCmd())
	cmd.AddCommand(newEjectCmd())
	cmd.AddCommand(newUpdateCmd())
	cmd.AddCommand(newDaemonCmd())
	cmd.AddCommand(newAttachCmd())
	cmd.AddCommand(newRerunCmd())
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newSyncCmd())
	cmd.AddCommand(newRunsCmd())
	cmd.AddCommand(newReposCmd())
	cmd.AddCommand(newQACmd())
	cmd.AddCommand(newStatsCmd())
	cmd.AddCommand(newDoctorCmd())
	cmd.AddCommand(newEvalCmd())
	cmd.AddCommand(newAxiCmd())

	return cmd
}

func setColorProfileForOutput(w io.Writer) {
	lipgloss.SetColorProfile(termenv.NewOutput(w).EnvColorProfile())
}

// findRepo looks up the repo for the current directory. If the working
// directory is inside a git worktree, it falls back to the main repository
// root so that worktrees work out of the box when the main repo is
// already initialized.
func findRepo(d *db.DB) (*db.Repo, error) {
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		return nil, fmt.Errorf("not in a git repository")
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo != nil {
		return refuseConnectedRepo(repo)
	}
	// Try the main worktree root (handles git worktrees).
	mainRoot, err := git.FindMainRepoRoot(".")
	if err != nil || mainRoot == gitRoot {
		return nil, fmt.Errorf("repo not initialized (run 'no-mistakes init' first)")
	}
	repo, err = d.GetRepoByPath(mainRoot)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("repo not initialized (run 'no-mistakes init' first)")
	}
	return refuseConnectedRepo(repo)
}

// refuseConnectedRepo rejects a connected repository at the CLI choke point.
//
// findRepo answers "which repository is this working directory", which a
// connected repository has no answer to: it is registered from the operator's
// configuration by remote URL and its working_path is a daemon-owned identity
// stub, not a checkout. Every cwd-based author surface resolves through here, so
// this one refusal covers sync, runs, attach, rerun, status, and axi. It names
// the repository by its operator-chosen name and never by URL, which for a
// connected repository routinely carries a token.
func refuseConnectedRepo(repo *db.Repo) (*db.Repo, error) {
	if !repo.Connected() {
		return repo, nil
	}
	name := strings.TrimSpace(repo.SourceName)
	if name == "" {
		name = repo.ID
	}
	return nil, fmt.Errorf("%q is a connected repository with no developer checkout, so this command does not apply to it; see 'no-mistakes repos show %s'", name, name)
}

// resolveRepo answers "which repository does this command mean".
//
// An empty selector keeps today's behaviour exactly: the repository this working
// directory belongs to, with connected repositories refused, because a
// connected repository is not what any directory is "in".
//
// A non-empty selector is an explicit choice, so it resolves a connected
// repository too and leaves the decision about whether that is allowed to the
// caller: read surfaces can show one, author-side mutations still must not.
// Matching is by operator-chosen name first, then full ID, then a unique ID
// prefix - an ambiguous prefix is an error rather than a guess.
func resolveRepo(d *db.DB, selector string) (*db.Repo, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return findRepo(d)
	}
	repos, err := d.GetRepos()
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	for _, repo := range repos {
		if strings.TrimSpace(repo.SourceName) == selector || repo.ID == selector {
			return repo, nil
		}
	}
	var matches []*db.Repo
	for _, repo := range repos {
		if strings.HasPrefix(repo.ID, selector) {
			matches = append(matches, repo)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("no repository matches %q (see 'no-mistakes repos list')", selector)
	default:
		names := make([]string, 0, len(matches))
		for _, repo := range matches {
			names = append(names, repoSelectorLabel(repo))
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%q matches more than one repository: %s", selector, strings.Join(names, ", "))
	}
}

// repoSelectorLabel names a repository in a message: its operator-chosen name
// when it has one, otherwise its ID. Never its URL, which for a connected
// repository routinely carries a token.
func repoSelectorLabel(repo *db.Repo) string {
	if name := strings.TrimSpace(repo.SourceName); name != "" {
		return name
	}
	return repo.ID
}

// openResources initializes paths, ensures directories exist, and opens the DB.
// Caller must close the returned DB.
func openResources() (*paths.Paths, *db.DB, error) {
	p, err := paths.New()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve paths: %w", err)
	}
	if err := p.EnsureDirs(); err != nil {
		return nil, nil, fmt.Errorf("create directories: %w", err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	return p, d, nil
}
