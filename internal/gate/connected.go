package gate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// connectedIDDomain separates the connected-repository ID namespace from the
// working-clone one. Local repo IDs are sha256 of an absolute filesystem path;
// connected IDs are sha256 of a remote identity. Without a domain prefix the two
// would be each other's preimages, and a crafted path or URL could be made to
// collide deliberately rather than only by 48-bit accident.
const connectedIDDomain = "no-mistakes/connected/v1\x00"

// RemoteIdentity returns the canonical, credential-free identity of a remote
// URL: "<host>/<path>", lowercased, with userinfo and port stripped and one
// trailing ".git" removed.
//
// It is the stable identity of a connected repository across transports and
// credential rotation, so these all resolve to "github.com/acme/api":
//
//	https://token@github.com/Acme/API.git
//	git@github.com:acme/api.git
//	ssh://git@github.com:2222/acme/api
//
// The implementation delegates to inspectRefreshRemote, which already performs
// exactly this canonicalization for the URL-refresh path, so there is one
// normalizer rather than two that could disagree about what counts as the same
// repository.
//
// It keys off a non-empty identity rather than a nil error on purpose.
// inspectRefreshRemote reports "credential-bearing remote" as an ERROR while
// still populating the identity, because for its own caller a credential in a
// discovered clone remote is a reason to refuse. Here the opposite is true: an
// operator's configured URL routinely carries a token, and that URL is exactly
// what we must be able to identify.
func RemoteIdentity(raw string) (string, error) {
	info, err := inspectRefreshRemote(raw)
	if strings.TrimSpace(info.identity) != "" {
		return info.identity, nil
	}
	if err == nil {
		err = fmt.Errorf("invalid remote")
	}
	return "", err
}

// ConnectedRepoID derives the stable repository ID for a canonical remote
// identity.
//
// The result has the same shape as a working-clone repo ID - 12 lowercase hex
// characters - so every existing consumer keeps working unchanged: the gate
// directory is still "<id>.git", startup gate migration still recognizes it, the
// post-receive hook still resolves a repo from the gate path, and the gate-context
// classifier still trims the ".git" suffix.
//
// The ID is derived from the remote identity and NOT from the operator-chosen
// name, for two reasons. A name could contain characters that escape the repos
// directory when joined into a path. And deriving from identity means renaming a
// configuration entry preserves the gate and every run row, while repointing an
// entry at a genuinely different repository correctly produces a different ID.
func ConnectedRepoID(identity string) string {
	h := sha256.Sum256([]byte(connectedIDDomain + strings.TrimSpace(identity)))
	return fmt.Sprintf("%x", h[:6])
}

// ProvisionConnectedGate creates or repairs the bare gate for a repository
// connected by remote URL.
//
// It is provisionGate without the working-clone half: there is no developer
// checkout to attach a "no-mistakes" remote to. Everything else is identical, so
// startup gate migration, the gate-config stamp, and the gate-context classifier
// see exactly one gate shape. See provisionGateCore for why both receive hooks
// are still installed on a gate nothing is ever pushed to.
//
// upstreamURL may carry a credential. It is written only to the gate's own origin
// remote, which is where the pipeline recovers it from at run time; callers must
// persist a redacted copy to the database instead.
func ProvisionConnectedGate(ctx context.Context, bareDir, upstreamURL string) error {
	return provisionGateCore(ctx, bareDir, upstreamURL)
}

// EnsureConnected registers or refreshes a repository connected by remote URL.
//
// It is idempotent, and deliberately so at the level of IDENTITY rather than
// name: renaming a configuration entry keeps the same identity-derived ID, and
// therefore the same gate and the same run history. Repointing an entry at a
// genuinely different repository correctly produces a different ID.
//
// On any failure before the database insert, it removes only what this call
// created. That mirrors InitWithFork's `existing == nil` discipline: a repair
// that fails must never tear down an already-registered gate.
func EnsureConnected(ctx context.Context, d *db.DB, p *paths.Paths, spec config.RepoSpec) (*db.Repo, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return nil, fmt.Errorf("connected repository has no name")
	}
	identity, err := RemoteIdentity(spec.URL)
	if err != nil {
		// The URL is not echoed: it routinely carries the operator's token.
		return nil, fmt.Errorf("repos.%s: resolve remote identity: %w", name, err)
	}
	id := ConnectedRepoID(identity)

	existing, err := d.GetRepo(id)
	if err != nil {
		return nil, fmt.Errorf("repos.%s: check existing registration: %w", name, err)
	}
	if existing != nil {
		// A local repository holding this ID is a 48-bit collision against a
		// path hash. Refuse loudly rather than adopt or overwrite a working gate.
		if !existing.Connected() {
			return nil, fmt.Errorf("repos.%s: id %s is already registered to a local repository; this is an ID collision, not a re-registration", name, id)
		}
		if existing.SourceIdentity != identity {
			return nil, fmt.Errorf("repos.%s: id %s is already registered to a different remote; this is an ID collision", name, id)
		}
	}
	// An identity registered under a DIFFERENT id can only mean the ID
	// derivation changed underneath an existing row, which would silently strand
	// its gate and history. Refuse rather than register a second copy.
	//
	// Note this cannot detect two configuration entries naming one repository:
	// seeing a single spec, "acme-api-again" pointing at an already-registered
	// remote is indistinguishable from renaming "acme-api" to it, and renaming is
	// explicitly supported. Only a caller holding the whole spec set can tell
	// those apart, which is why that check belongs to ReconcileConnected.
	if other, err := d.GetRepoBySourceIdentity(identity); err != nil {
		return nil, fmt.Errorf("repos.%s: check identity conflict: %w", name, err)
	} else if other != nil && other.ID != id {
		return nil, fmt.Errorf("repos.%s: remote is already registered under id %s; refusing to register it twice", name, other.ID)
	}

	bareDir := p.RepoDir(id)
	stubDir := p.SourceDir(id)
	createdGate := existing == nil && !dirExists(bareDir)
	createdStub := existing == nil && !dirExists(stubDir)
	cleanup := func() {
		if existing != nil {
			return
		}
		if createdGate {
			_ = os.RemoveAll(bareDir)
		}
		if createdStub {
			_ = os.RemoveAll(stubDir)
		}
	}

	if err := ProvisionConnectedGate(ctx, bareDir, spec.URL); err != nil {
		cleanup()
		return nil, fmt.Errorf("repos.%s: provision gate: %w", name, err)
	}
	if err := ensureIdentityStub(ctx, stubDir, spec); err != nil {
		cleanup()
		return nil, fmt.Errorf("repos.%s: %w", name, err)
	}

	defaultBranch := strings.TrimSpace(spec.DefaultBranch)
	if defaultBranch == "" {
		// ls-remote --symref against the gate's own origin. It works from a bare
		// repository through git.RunBare, and falls back to "main".
		defaultBranch = git.DefaultBranch(ctx, bareDir, "origin")
	}

	if existing != nil {
		repo, err := d.UpdateConnectedRepoSpec(id, name, defaultBranch)
		if err != nil {
			return nil, fmt.Errorf("repos.%s: refresh registration: %w", name, err)
		}
		return repo, nil
	}

	// The stored working_path is symlink-resolved, because every reader resolves
	// before it looks up: findRepo, Eject, and branchsync.OpenCurrent all ask git
	// for the root, and git reports the resolved path (/private/var rather than
	// /var on macOS, the long name rather than an 8.3 short name on Windows).
	// The local path already stores what FindMainRepoRoot resolved; storing an
	// unresolved stub path here would mean no lookup ever matched it, and the
	// connected guards would degrade to "repo not initialized".
	//
	// SECURITY: the stored URL is redacted. The credential lives only on the
	// gate's own origin remote, which is where the pipeline recovers it from at
	// run time. This is the database half of invariant C1.
	repo, err := d.InsertConnectedRepo(id, resolvePathForCompare(stubDir), safeurl.Redact(spec.URL), identity, name, defaultBranch)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("repos.%s: register: %w", name, err)
	}
	return repo, nil
}

// ensureIdentityStub creates the daemon-owned identity stub for a connected
// repository and pins the commit identity its run worktrees will copy.
//
// The stub is deliberately NOT a clone: the objects live in the bare gate. It
// exists so a connected repository has a unique working_path satisfying the
// repos table's NOT NULL UNIQUE constraint without a table rebuild, and so there
// is somewhere for git.CopyLocalUserIdentity to read an identity from. It gets
// no remotes, no refs, and no objects, which is what makes the author-side
// guards fail safe: GetConfiguredRemoteURLs(stub, "origin") errors, so local
// discovery can never overwrite the configuration authority.
//
// It fails closed when no identity can be resolved. CopyLocalUserIdentity
// CONTINUES past an empty user.name/user.email rather than erroring, so an
// unpinned stub would make commits silently fall back to the daemon's global git
// identity - which on a headless host often does not exist, surfacing much later
// as an opaque failure inside a fix commit.
func ensureIdentityStub(ctx context.Context, dir string, spec config.RepoSpec) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create identity stub: %w", err)
	}
	if _, err := git.Run(ctx, dir, "rev-parse", "--git-dir"); err != nil {
		if _, err := git.Run(ctx, filepath.Dir(dir), "init", dir); err != nil {
			return fmt.Errorf("create identity stub: %w", err)
		}
	}
	name, email, err := resolveCommitIdentity(ctx, dir, spec)
	if err != nil {
		return err
	}
	for key, value := range map[string]string{"user.name": name, "user.email": email} {
		if _, err := git.Run(ctx, dir, "config", "--local", key, value); err != nil {
			return fmt.Errorf("pin %s on identity stub: %w", key, err)
		}
	}
	return nil
}

// resolveCommitIdentity resolves the commit identity for a connected
// repository: the operator's explicit per-repo values first, then whatever
// identity git itself already resolves on this host, then an error naming the
// configuration keys that would fix it.
func resolveCommitIdentity(ctx context.Context, dir string, spec config.RepoSpec) (string, string, error) {
	name := strings.TrimSpace(spec.CommitName)
	email := strings.TrimSpace(spec.CommitEmail)
	if name == "" {
		name, _ = git.Run(ctx, dir, "config", "--get", "--default", "", "user.name")
		name = strings.TrimSpace(name)
	}
	if email == "" {
		email, _ = git.Run(ctx, dir, "config", "--get", "--default", "", "user.email")
		email = strings.TrimSpace(email)
	}
	if name == "" || email == "" {
		return "", "", fmt.Errorf("no commit identity is resolvable for this connected repository; set commit_name and commit_email on its config.yaml entry, or configure git's global user.name and user.email for the daemon's user")
	}
	return name, email, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// RefreshConnectedRepoURLs points a connected repository at a new URL for the
// same repository - a rotated credential, or a switch between https and ssh.
//
// It is the connected counterpart of RefreshRepoURLs, which refuses connected
// repositories outright. The authority here is the operator's configuration,
// not anything discovered on disk.
//
// # Invariant C1
//
//	safeurl.Redact(<gate origin>) == repos.upstream_url
//
// C1 is what keeps credential recovery working at run time: resolveUpstreamURL
// accepts the worktree's origin only when its redaction matches the stored row,
// because the stored row is redacted and cannot supply the credential itself.
//
// # Write order
//
// The redacted row is written FIRST, then the gate remote, and a failed remote
// write rolls the row back. The reverse order leaves a window in which the gate
// already points at the new repository while the row still names the old one,
// and a fallback resolving the old URL could push this run's branch to the
// PREVIOUS repository. Rolling back on failure keeps C1 true in both directions
// rather than leaving a half-applied rotation.
func RefreshConnectedRepoURLs(ctx context.Context, d *db.DB, p *paths.Paths, repo *db.Repo, url string) (*db.Repo, bool, error) {
	if d == nil || p == nil || repo == nil || !repo.Connected() {
		return nil, false, refreshFailure(RefreshConfigMismatch)
	}
	identity, err := RemoteIdentity(url)
	if err != nil {
		return nil, false, refreshFailure(RefreshInvalidRemote)
	}
	// Repointing at a DIFFERENT repository is not a refresh: it would silently
	// move a registration's history onto another project. Reconciliation
	// registers the new identity as its own entry instead.
	if identity != repo.SourceIdentity {
		return nil, false, refreshFailure(RefreshConfigMismatch)
	}

	redacted := safeurl.Redact(url)
	if redacted == repo.UpstreamURL {
		// Still verify the gate agrees before reporting no-op: the row matching
		// is not evidence that C1 holds.
		if err := AssertConnectedGateURLBinding(ctx, p, repo); err == nil {
			return repo, false, nil
		}
	}

	previous := repo.UpstreamURL
	updated, err := d.ReplaceRepoURLs(repo.ID, redacted, "")
	if err != nil {
		return nil, false, refreshFailure(RefreshDatabaseWrite)
	}
	if _, err := git.RunBare(ctx, p.RepoDir(repo.ID), "config", "remote.origin.url", url); err != nil {
		// Roll the row back so it names what the gate still points at.
		if _, rollbackErr := d.ReplaceRepoURLs(repo.ID, previous, ""); rollbackErr != nil {
			return nil, false, refreshFailure(RefreshDatabaseWrite)
		}
		return nil, false, refreshFailure(RefreshGateWrite)
	}
	return updated, true, nil
}

// AssertConnectedGateURLBinding verifies invariant C1 for one connected
// repository immediately before a run uses its gate.
//
// The connected path fails the run on a mismatch, deliberately unlike the local
// path, which logs "refresh skipped; continuing". For a local repository the
// fallback is the operator's own row about their own clone. For a connected
// repository a stale row may name a DIFFERENT repository, and continuing could
// push someone's branch to it.
//
// Errors are URL-free: a connected repository's URL routinely carries a token.
func AssertConnectedGateURLBinding(ctx context.Context, p *paths.Paths, repo *db.Repo) error {
	if p == nil || repo == nil || !repo.Connected() {
		return nil
	}
	origin, err := git.RunBare(ctx, p.RepoDir(repo.ID), "config", "--get", "remote.origin.url")
	if err != nil {
		return fmt.Errorf("connected repository %q: gate origin is unreadable", repo.SourceName)
	}
	if safeurl.Redact(strings.TrimSpace(origin)) != strings.TrimSpace(repo.UpstreamURL) {
		return fmt.Errorf("connected repository %q: the gate's origin does not match its registration; refusing to run", repo.SourceName)
	}
	return nil
}

// ReconcileResult reports what one reconciliation pass did. Counts rather than
// URLs, so it is safe to log.
type ReconcileResult struct {
	Registered []string
	Refreshed  []string
	Detached   []string
	Failed     map[string]error
}

// ReconcileConnected brings the registry in line with the operator's declared
// inventory.
//
// It is STRICTLY ADDITIVE. An entry that disappears from configuration is
// marked detached, never deleted: reconciliation must not destroy a gate or run
// history because someone edited or commented out a line. Re-adding the URL
// clears the marker and reuses the same identity-derived ID, so the gate and the
// full run history survive - the concrete payoff of deriving IDs from identity
// rather than from a name. Deletion is only ever the explicit
// `no-mistakes repos remove`.
//
// Local repositories are never read for detachment and never written. Only rows
// with source_kind='connected' are considered.
//
// One entry failing never stops the others: an operator with five repositories
// and one typo still gets the other four reconciled, with the failure reported.
func ReconcileConnected(ctx context.Context, d *db.DB, p *paths.Paths, specs map[string]config.RepoSpec) (ReconcileResult, error) {
	result := ReconcileResult{Failed: map[string]error{}}
	if d == nil || p == nil {
		return result, fmt.Errorf("reconcile connected repositories: missing database or paths")
	}

	// Two configuration entries naming one repository would fight over the same
	// gate and run history on every pass. Only a caller holding the WHOLE spec
	// set can see this, which is why the check lives here rather than in
	// EnsureConnected, where a second name is indistinguishable from a rename.
	byIdentity := map[string]string{}
	declared := map[string]bool{}
	for _, name := range sortedSpecNames(specs) {
		spec := specs[name]
		identity, err := RemoteIdentity(spec.URL)
		if err != nil {
			result.Failed[name] = fmt.Errorf("resolve remote identity: %w", err)
			continue
		}
		if first, clash := byIdentity[identity]; clash {
			result.Failed[name] = fmt.Errorf("names the same repository as %q; give one entry per repository", first)
			continue
		}
		byIdentity[identity] = name
		declared[identity] = true

		existing, err := d.GetRepoBySourceIdentity(identity)
		if err != nil {
			result.Failed[name] = fmt.Errorf("check existing registration: %w", err)
			continue
		}
		repo, err := EnsureConnected(ctx, d, p, spec)
		if err != nil {
			result.Failed[name] = err
			continue
		}
		if existing == nil {
			result.Registered = append(result.Registered, repo.SourceName)
		} else {
			result.Refreshed = append(result.Refreshed, repo.SourceName)
		}
	}

	repos, err := d.GetRepos()
	if err != nil {
		return result, fmt.Errorf("list repositories: %w", err)
	}
	now := time.Now().Unix()
	for _, repo := range repos {
		if !repo.Connected() || declared[repo.SourceIdentity] || repo.DetachedAt != nil {
			continue
		}
		if err := d.SetRepoDetachedAt(repo.ID, &now); err != nil {
			result.Failed[repo.SourceName] = fmt.Errorf("mark detached: %w", err)
			continue
		}
		result.Detached = append(result.Detached, repo.SourceName)
	}
	return result, nil
}

// RemoveConnected deletes a connected repository's registration, and with
// --delete-history its gate, worktrees, and identity stub as well.
//
// This is the only path that destroys anything, which is why reconciliation
// detaches instead. It refuses while runs are active: those runs hold worktrees
// carved from the gate this would remove.
func RemoveConnected(ctx context.Context, d *db.DB, p *paths.Paths, name string, deleteHistory bool) error {
	repo, err := d.GetRepoBySourceName(strings.TrimSpace(name))
	if err != nil {
		return fmt.Errorf("look up %q: %w", name, err)
	}
	if repo == nil || !repo.Connected() {
		return fmt.Errorf("no connected repository named %q", name)
	}
	active, err := d.GetActiveRuns()
	if err != nil {
		return fmt.Errorf("check active runs: %w", err)
	}
	for _, run := range active {
		if run.RepoID == repo.ID {
			return fmt.Errorf("%q has active runs; wait for them to finish or cancel them first", name)
		}
	}
	if deleteHistory {
		if err := os.RemoveAll(p.RepoDir(repo.ID)); err != nil {
			return fmt.Errorf("remove gate: %w", err)
		}
		if err := os.RemoveAll(p.SourceDir(repo.ID)); err != nil {
			return fmt.Errorf("remove identity stub: %w", err)
		}
		if err := os.RemoveAll(filepath.Join(p.WorktreesDir(), repo.ID)); err != nil {
			return fmt.Errorf("remove worktrees: %w", err)
		}
	}
	if err := d.DeleteRepo(repo.ID); err != nil {
		return fmt.Errorf("delete registration: %w", err)
	}
	return nil
}

func sortedSpecNames(specs map[string]config.RepoSpec) []string {
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
