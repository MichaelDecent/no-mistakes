package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// QARunRequest is one operator-requested QA run.
type QARunRequest struct {
	// RepoID is the connected repository to validate. QA never runs against a
	// local repository - see StartQARun.
	RepoID string
	// Branch is empty for the repository's default branch.
	Branch string
	// Mode must be a declared mode. There is no default here on purpose: what a
	// run is permitted to do with what it finds is never implied.
	Mode types.RunMode
	// BaseSHA is what the branch is validated against. Empty derives it - see
	// resolveQABaseSHA - and the watcher passes the head it last validated so a
	// sweep reviews only what arrived since.
	BaseSHA string
	// Trigger labels the run in telemetry ("qa_manual", later "qa_sweep").
	Trigger string
}

// preparedQARun is everything a QA run needs that the start path cannot derive
// for itself, resolved before the run row exists.
type preparedQARun struct {
	branch  string
	headSHA string
	baseSHA string
}

// StartQARun validates a connected repository's branch without a push and
// without a checkout.
//
// It refuses a LOCAL repository. QA is operator-scope work on repositories
// declared by remote URL; a run against someone's own clone would take the
// branch lock on a branch they are working on, appear in their own status, and
// generally break the promise that QA never touches local checkouts. The
// author-side surfaces refuse connected repositories for the mirror-image
// reason (see refuseConnectedRepo in internal/cli).
//
// Everything about what the run may then DO comes from its scope: admission,
// the skip set, the commit-derived intent, the zeroed auto-fix budgets, and the
// gate policy are all applied by startRunWithIntentSource.
func (m *RunManager) StartQARun(ctx context.Context, req QARunRequest) (string, error) {
	if !types.ValidRunMode(string(req.Mode)) {
		return "", fmt.Errorf("qa run: %q is not a valid mode (report, comment, fix-pr)", req.Mode)
	}
	repo, err := m.db.GetRepo(req.RepoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repository %q", req.RepoID)
	}
	if !repo.Connected() {
		return "", fmt.Errorf("%q is a local repository; QA runs only against connected repositories declared under `repos:` in config.yaml", repoLabel(repo))
	}
	if repo.DetachedAt != nil {
		return "", fmt.Errorf("connected repository %q is detached: it is no longer declared in config.yaml, so QA will not run against it (re-declare it and run 'no-mistakes repos reconcile')", repoLabel(repo))
	}

	prepared, err := m.prepareQARun(ctx, repo, req)
	if err != nil {
		return "", err
	}

	trigger := req.Trigger
	if trigger == "" {
		trigger = "qa_manual"
	}
	// No intent and no intent source: a QA run's intent is derived from the
	// branch's own commits inside the start path, which is also where it is
	// stamped non-authoritative.
	return m.startRunWithIntentSource(ctx, repo, prepared.branch, prepared.headSHA, prepared.baseSHA,
		trigger, nil, "", "", runScope{kind: types.RunKindQA, mode: req.Mode})
}

// prepareQARun resolves the branch, brings its commits into the gate, and
// settles what the run is validated against.
//
// The order is deliberate: invariant C1 is verified BEFORE the fetch. A gate
// whose origin has drifted from its registration may name a different
// repository, and fetching from it would pull that repository's commits into
// this one's gate and validate them under this one's name.
func (m *RunManager) prepareQARun(ctx context.Context, repo *db.Repo, req QARunRequest) (preparedQARun, error) {
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = strings.TrimSpace(repo.DefaultBranch)
	}
	if branch == "" {
		return preparedQARun{}, fmt.Errorf("connected repository %q has no default branch recorded; pass an explicit branch", repoLabel(repo))
	}
	if err := gate.AssertConnectedGateURLBinding(ctx, m.paths, repo); err != nil {
		return preparedQARun{}, err
	}

	gateDir := m.paths.RepoDir(repo.ID)
	// A connected gate receives no pushes, so the branch has to be fetched
	// before anything can be carved from it. It lands on refs/heads/<branch>,
	// which is where a pushed branch would be, so every later step - rebase's
	// origin comparison, PR discovery by branch - behaves exactly as it does for
	// a gate run.
	if err := git.FetchRemoteBranchToRef(ctx, gateDir, "origin", branch, "refs/heads/"+branch); err != nil {
		return preparedQARun{}, fmt.Errorf("connected repository %q: fetch branch %q into the gate failed", repoLabel(repo), branch)
	}
	headSHA, err := git.ResolveRef(ctx, gateDir, "refs/heads/"+branch)
	if err != nil || strings.TrimSpace(headSHA) == "" {
		return preparedQARun{}, fmt.Errorf("connected repository %q: branch %q has no resolvable head", repoLabel(repo), branch)
	}
	headSHA = strings.TrimSpace(headSHA)

	return preparedQARun{
		branch:  branch,
		headSHA: headSHA,
		baseSHA: m.resolveQABaseSHA(ctx, repo, branch, headSHA, req.BaseSHA),
	}, nil
}

// resolveQABaseSHA decides what a QA run's diff is measured against.
//
// An explicit base wins: the watcher knows the head it last validated, so a
// sweep reviews only the commits that arrived since. Otherwise:
//
//   - on a non-default branch, the merge-base with the default branch, which is
//     the branch's own work and nothing else;
//   - on the default branch, its previous commit, so the newest change is what
//     gets validated rather than the entire repository.
//
// An empty result is not fatal. Every step resolves its own base through
// resolveBranchBaseSHA, and runs.base_sha is the fallback it consults.
func (m *RunManager) resolveQABaseSHA(ctx context.Context, repo *db.Repo, branch, headSHA, explicit string) string {
	if trimmed := strings.TrimSpace(explicit); trimmed != "" {
		return trimmed
	}
	gateDir := m.paths.RepoDir(repo.ID)
	defaultBranch := strings.TrimSpace(repo.DefaultBranch)
	if defaultBranch != "" && defaultBranch != branch {
		if err := git.FetchRemoteBranchToRef(ctx, gateDir, "origin", defaultBranch, "refs/heads/"+defaultBranch); err == nil {
			if base, err := git.Run(ctx, gateDir, "merge-base", "refs/heads/"+defaultBranch, headSHA); err == nil {
				if trimmed := strings.TrimSpace(base); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	if parent, err := git.Run(ctx, gateDir, "rev-parse", "--verify", headSHA+"^"); err == nil {
		return strings.TrimSpace(parent)
	}
	// A root commit has no parent: the whole tree is the change.
	return ""
}

// repoLabel names a repository the way a message may: its operator-chosen name,
// falling back to its ID. Never its URL, which for a connected repository
// routinely carries a token.
func repoLabel(repo *db.Repo) string {
	if repo == nil {
		return ""
	}
	if name := strings.TrimSpace(repo.SourceName); name != "" {
		return name
	}
	return repo.ID
}

// ApplyQAConfig sizes the run slots from the operator's qa block. It is called
// once at daemon startup, before any run can be admitted, so the documented
// qa.max_concurrent and qa.max_total_runs are the values in force rather than
// the conservative constructor defaults.
func (m *RunManager) ApplyQAConfig(cfg config.QA) {
	if cfg.MaxConcurrent <= 0 && cfg.MaxTotalRuns <= 0 {
		return
	}
	m.slots = newRunSlots(cfg.MaxConcurrent, cfg.MaxTotalRuns)
}
