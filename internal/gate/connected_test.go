package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// connectedEnv is a daemon state root plus a remote URL for the operator's
// repository.
//
// The URL is a well-formed but unreachable forge URL rather than a local bare
// repository, because RemoteIdentity accepts only scheme-qualified or
// host:path remotes - a filesystem path and even file:// resolve to "invalid
// remote". Registration never fetches, so an unreachable URL exercises the
// whole of EnsureConnected. DefaultBranch is always set explicitly here so no
// test attempts an ls-remote over the network.
type connectedEnv struct {
	p      *paths.Paths
	db     *db.DB
	remote string
}

func newConnectedEnv(t *testing.T) *connectedEnv {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(filepath.Join(root, "state"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	return &connectedEnv{p: p, db: database, remote: "https://example.test/acme/api.git"}
}

func (e *connectedEnv) spec(name string) config.RepoSpec {
	return config.RepoSpec{
		Name:          name,
		URL:           e.remote,
		DefaultBranch: "main",
		CommitName:    "QA Bot",
		CommitEmail:   "qa@example.test",
	}
}

// TestEnsureConnectedRegistersAndIsIdempotent pins the happy path and that a
// second call reuses the same record, gate, and ID rather than creating another.
func TestEnsureConnectedRegistersAndIsIdempotent(t *testing.T) {
	e := newConnectedEnv(t)

	repo, err := EnsureConnected(context.Background(), e.db, e.p, e.spec("acme-api"))
	if err != nil {
		t.Fatalf("EnsureConnected: %v", err)
	}
	if !repo.Connected() {
		t.Error("registered repo is not connected")
	}
	if repo.SourceName != "acme-api" {
		t.Errorf("SourceName = %q", repo.SourceName)
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want the explicitly configured main", repo.DefaultBranch)
	}
	if repo.WorkingPath != e.p.SourceDir(repo.ID) {
		t.Errorf("WorkingPath = %q, want the identity stub %q", repo.WorkingPath, e.p.SourceDir(repo.ID))
	}
	if _, err := os.Stat(filepath.Join(e.p.RepoDir(repo.ID), "HEAD")); err != nil {
		t.Errorf("bare gate was not provisioned: %v", err)
	}

	// Renaming the entry must reuse the identity-derived ID, which is what
	// preserves the gate and every run row across a configuration edit.
	renamed := e.spec("acme-api-renamed")
	again, err := EnsureConnected(context.Background(), e.db, e.p, renamed)
	if err != nil {
		t.Fatalf("second EnsureConnected: %v", err)
	}
	if again.ID != repo.ID {
		t.Errorf("ID changed on rename: %q -> %q", repo.ID, again.ID)
	}
	if again.SourceName != "acme-api-renamed" {
		t.Errorf("SourceName = %q, want the new name", again.SourceName)
	}
	repos, err := e.db.GetRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Errorf("got %d repo rows, want the one record reused", len(repos))
	}
}

// TestEnsureConnectedStoresRedactedUpstreamURL pins invariant C1's database
// half: the credential belongs only on the gate's own origin remote, which is
// where the pipeline recovers it from at run time.
func TestEnsureConnectedStoresRedactedUpstreamURL(t *testing.T) {
	e := newConnectedEnv(t)
	spec := e.spec("acme-api")
	spec.URL = "https://ghp_secrettoken@example.test/acme/api.git"

	repo, err := EnsureConnected(context.Background(), e.db, e.p, spec)
	if err != nil {
		t.Fatalf("EnsureConnected: %v", err)
	}
	if strings.Contains(repo.UpstreamURL, "ghp_secrettoken") {
		t.Errorf("stored upstream_url retains the credential: %q", repo.UpstreamURL)
	}
	stored, err := e.db.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.UpstreamURL, "ghp_secrettoken") {
		t.Errorf("database row retains the credential: %q", stored.UpstreamURL)
	}
	// The gate's origin keeps the credential: that is what authenticates fetches.
	origin, err := gitConfigValue(t, e.p.RepoDir(repo.ID), "remote.origin.url")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(origin, "ghp_secrettoken") {
		t.Errorf("gate origin lost its credential: %q", origin)
	}
}

// TestEnsureIdentityStubHasNoRemotesRefsOrObjects pins the stub as an identity
// placeholder rather than a clone. A stub with an origin would let
// gate.RefreshRepoURLs read a URL from it and overwrite the config authority.
func TestEnsureIdentityStubHasNoRemotesRefsOrObjects(t *testing.T) {
	e := newConnectedEnv(t)
	repo, err := EnsureConnected(context.Background(), e.db, e.p, e.spec("acme-api"))
	if err != nil {
		t.Fatalf("EnsureConnected: %v", err)
	}
	stub := e.p.SourceDir(repo.ID)

	if out := mustRunGitOut(t, stub, "remote"); strings.TrimSpace(out) != "" {
		t.Errorf("stub has remotes:\n%s", out)
	}
	if out := mustRunGitOut(t, stub, "for-each-ref"); strings.TrimSpace(out) != "" {
		t.Errorf("stub has refs:\n%s", out)
	}
	// The pinned identity is the whole point of the stub existing.
	for key, want := range map[string]string{"user.name": "QA Bot", "user.email": "qa@example.test"} {
		got, err := gitConfigValue(t, stub, key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if strings.TrimSpace(got) != want {
			t.Errorf("stub %s = %q, want %q", key, got, want)
		}
	}
}

// TestEnsureIdentityStubFailsWhenNoCommitIdentityResolvable is finding #3.
//
// CopyLocalUserIdentity reads --local user.name/user.email and CONTINUES on
// empty, so an unpinned stub makes every run worktree silently fall back to the
// daemon's global git identity - which on a headless host often does not exist,
// surfacing as an opaque failure deep in a fix-commit path. Registration must
// refuse instead.
func TestEnsureIdentityStubFailsWhenNoCommitIdentityResolvable(t *testing.T) {
	e := newConnectedEnv(t)
	// Point git's global config at an empty file so no ambient identity leaks in.
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_SYSTEM", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	spec := e.spec("acme-api")
	spec.CommitName = ""
	spec.CommitEmail = ""

	repo, err := EnsureConnected(context.Background(), e.db, e.p, spec)
	if err == nil {
		t.Fatalf("EnsureConnected returned %+v with no resolvable commit identity; it must refuse", repo)
	}
	if !strings.Contains(err.Error(), "commit_name") && !strings.Contains(err.Error(), "identity") {
		t.Errorf("error %q does not explain what is missing or how to fix it", err)
	}
	// A failure before the insert must leave no record behind.
	repos, err := e.db.GetRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Errorf("a failed registration left %d repo rows behind", len(repos))
	}
}

// TestEnsureConnectedRefusesLocalRepoIDCollision pins that a 48-bit collision
// against a local repository's path hash is a visible refusal, never a silent
// overwrite of someone's working gate.
func TestEnsureConnectedRefusesLocalRepoIDCollision(t *testing.T) {
	e := newConnectedEnv(t)
	identity, err := RemoteIdentity(e.remote)
	if err != nil {
		t.Fatal(err)
	}
	id := ConnectedRepoID(identity)

	// Occupy that exact ID with a local repository.
	if _, err := e.db.InsertRepoWithIDAndFork(id, filepath.Join(t.TempDir(), "clone"), "https://example.test/other.git", "", "main"); err != nil {
		t.Fatal(err)
	}

	repo, err := EnsureConnected(context.Background(), e.db, e.p, e.spec("acme-api"))
	if err == nil {
		t.Fatalf("EnsureConnected returned %+v over a local repo's ID; it must refuse", repo)
	}
	if !strings.Contains(err.Error(), "collision") && !strings.Contains(err.Error(), "already") {
		t.Errorf("error %q does not name the collision", err)
	}
}

// TestEnsureConnectedTreatsASecondNameForOneRemoteAsARename documents a real
// limit of this layer rather than a wish.
//
// Seeing one spec at a time, "acme-api-again" pointing at an already-registered
// remote is indistinguishable from renaming "acme-api" to it, and renaming is
// explicitly supported: it is what preserves the gate and run history across a
// configuration edit. Refusing two names for one repository therefore requires a
// caller holding the WHOLE spec set, which is ReconcileConnected in S10.
func TestEnsureConnectedTreatsASecondNameForOneRemoteAsARename(t *testing.T) {
	e := newConnectedEnv(t)
	first, err := EnsureConnected(context.Background(), e.db, e.p, e.spec("acme-api"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureConnected(context.Background(), e.db, e.p, e.spec("acme-api-again"))
	if err != nil {
		t.Fatalf("a second name for one remote must be accepted as a rename here: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("rename produced a new ID %q, want %q; the gate and history would be stranded", second.ID, first.ID)
	}
	repos, err := e.db.GetRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Errorf("got %d repo rows, want one; a rename must not create a second registration", len(repos))
	}
}

// TestConnectedRepoIDIsStableAcrossTransports pins that an operator switching
// an entry from https to ssh, or rotating a token, keeps the same gate and run
// history rather than silently starting over.
func TestConnectedRepoIDIsStableAcrossTransports(t *testing.T) {
	ids := map[string]string{}
	for _, raw := range []string{
		"https://token@github.com/Acme/API.git",
		"git@github.com:acme/api.git",
		"ssh://git@github.com:2222/acme/api",
		"https://github.com/acme/api",
	} {
		identity, err := RemoteIdentity(raw)
		if err != nil {
			t.Fatalf("RemoteIdentity(%q): %v", raw, err)
		}
		ids[raw] = ConnectedRepoID(identity)
	}
	var first string
	for raw, id := range ids {
		if first == "" {
			first = id
			continue
		}
		if id != first {
			t.Errorf("%q produced ID %q, want %q; the identity is not transport-stable", raw, id, first)
		}
	}
}

// TestRemoteIdentityAcceptsCredentialBearingHTTPSRemote is finding #17:
// inspectRefreshRemote reports an ERROR for a userinfo-bearing https remote
// while still populating the identity, and an operator's URL routinely carries
// a token, so RemoteIdentity must key off the identity rather than the error.
func TestRemoteIdentityAcceptsCredentialBearingHTTPSRemote(t *testing.T) {
	identity, err := RemoteIdentity("https://ghp_token@github.com/acme/api.git")
	if err != nil {
		t.Fatalf("RemoteIdentity rejected a credential-bearing remote: %v", err)
	}
	if identity != "github.com/acme/api" {
		t.Errorf("identity = %q, want github.com/acme/api", identity)
	}
	if strings.Contains(identity, "ghp_token") {
		t.Errorf("identity retains the credential: %q", identity)
	}
}

func gitConfigValue(t *testing.T, dir, key string) (string, error) {
	t.Helper()
	return gitOut(dir, "config", "--get", key)
}

func mustRunGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOut(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}
