package gate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

	// SECURITY: the stored URL is redacted. The credential lives only on the
	// gate's own origin remote, which is where the pipeline recovers it from at
	// run time. This is the database half of invariant C1.
	repo, err := d.InsertConnectedRepo(id, stubDir, safeurl.Redact(spec.URL), identity, name, defaultBranch)
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
