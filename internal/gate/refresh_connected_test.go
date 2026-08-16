package gate

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// TestRefreshRepoURLsRefusesConnectedRepos pins the guard at the top of the
// clone-discovery refresh path.
//
// RefreshRepoURLs runs on EVERY startRun. For a local repository it reads the
// developer clone's origin, which is the right authority. For a connected
// repository the "clone" is a refless identity stub with no remotes, and the
// only other thing on disk holding a URL is the bare gate's origin - which by
// design carries the FULL credentialled URL. Letting this path run would read
// that credential and persist it through ReplaceRepoURLs, destroying the
// redaction invariant in a single run.
func TestRefreshRepoURLsRefusesConnectedRepos(t *testing.T) {
	e := newConnectedEnv(t)
	repo, err := EnsureConnected(context.Background(), e.db, e.p, e.spec("acme-api"))
	if err != nil {
		t.Fatal(err)
	}

	refreshed, changed, err := RefreshRepoURLs(context.Background(), e.db, repo)
	if err == nil {
		t.Fatalf("RefreshRepoURLs returned %+v changed=%v for a connected repo; it must refuse", refreshed, changed)
	}
	if changed {
		t.Error("changed = true on a refusal")
	}
	if got := ReasonForRefreshFailure(err); got != RefreshConfigMismatch {
		t.Errorf("reason = %q, want %q", got, RefreshConfigMismatch)
	}
	// The bounded reason must never carry a URL: that is the whole point of the
	// RefreshFailureReason type.
	if strings.Contains(err.Error(), "example.test") || strings.Contains(err.Error(), "http") {
		t.Errorf("error %q leaks a URL", err)
	}

	// The stored row must be untouched by the refusal.
	stored, err := e.db.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UpstreamURL != repo.UpstreamURL {
		t.Errorf("upstream_url changed on a refused refresh: %q -> %q", repo.UpstreamURL, stored.UpstreamURL)
	}
}

// TestRefreshConnectedRepoURLsWritesRedactedRowBeforeGateRemote pins invariant
// C1 and the one safe write order.
//
// C1: safeurl.Redact(<gate origin>) == repos.upstream_url for every connected
// repository. resolveUpstreamURL trusts the worktree origin only when its
// redaction matches the stored row, so C1 is what keeps credential recovery
// working at run time.
//
// The order must be redacted row FIRST, then the gate remote. The reverse leaves
// a window in which the gate points at the new repository while the row still
// names the old one, and a fallback resolving the old URL could push this run's
// branch to the PREVIOUS repository.
func TestRefreshConnectedRepoURLsWritesRedactedRowBeforeGateRemote(t *testing.T) {
	e := newConnectedEnv(t)
	repo, err := EnsureConnected(context.Background(), e.db, e.p, e.spec("acme-api"))
	if err != nil {
		t.Fatal(err)
	}

	const rotated = "https://ghp_rotated@example.test/acme/api.git"
	updated, changed, err := RefreshConnectedRepoURLs(context.Background(), e.db, e.p, repo, rotated)
	if err != nil {
		t.Fatalf("RefreshConnectedRepoURLs: %v", err)
	}
	if !changed {
		t.Error("changed = false after rotating the credential")
	}

	// The row is redacted.
	if strings.Contains(updated.UpstreamURL, "ghp_rotated") {
		t.Errorf("returned row retains the credential: %q", updated.UpstreamURL)
	}
	stored, err := e.db.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.UpstreamURL, "ghp_rotated") {
		t.Errorf("database row retains the credential: %q", stored.UpstreamURL)
	}

	// The gate origin carries the credential, and C1 holds between the two.
	origin, err := gitConfigValue(t, e.p.RepoDir(repo.ID), "remote.origin.url")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(origin, "ghp_rotated") {
		t.Errorf("gate origin was not updated: %q", origin)
	}
	if safeurl.Redact(origin) != stored.UpstreamURL {
		t.Errorf("C1 violated: Redact(gate origin)=%q != upstream_url=%q", safeurl.Redact(origin), stored.UpstreamURL)
	}
}

// TestRefreshConnectedRepoURLsRollsBackTheRowWhenTheGateWriteFails pins that a
// half-applied rotation is never left behind. If the gate remote cannot be
// written, the row must go back to naming what the gate still points at, or C1
// is broken in the other direction.
func TestRefreshConnectedRepoURLsRollsBackTheRowWhenTheGateWriteFails(t *testing.T) {
	e := newConnectedEnv(t)
	repo, err := EnsureConnected(context.Background(), e.db, e.p, e.spec("acme-api"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := e.db.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Removing the gate makes the remote write fail while the database is fine.
	removeGate(t, e.p.RepoDir(repo.ID))

	if _, _, err := RefreshConnectedRepoURLs(context.Background(), e.db, e.p, repo, "https://ghp_new@example.test/acme/api.git"); err == nil {
		t.Fatal("RefreshConnectedRepoURLs succeeded with no gate; it must fail")
	}

	after, err := e.db.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UpstreamURL != before.UpstreamURL {
		t.Errorf("row was left rotated after the gate write failed: %q -> %q", before.UpstreamURL, after.UpstreamURL)
	}
}

// TestAssertConnectedGateURLBindingDetectsDrift pins the run-time check. A
// connected run must refuse before carving a worktree when the gate and the row
// disagree, because that is exactly the state in which a push could reach the
// wrong repository.
func TestAssertConnectedGateURLBindingDetectsDrift(t *testing.T) {
	e := newConnectedEnv(t)
	repo, err := EnsureConnected(context.Background(), e.db, e.p, e.spec("acme-api"))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := e.db.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := AssertConnectedGateURLBinding(context.Background(), e.p, stored); err != nil {
		t.Fatalf("binding assertion failed on a freshly registered repo: %v", err)
	}

	// Drift the gate origin to a different repository.
	mustRunGit(t, e.p.RepoDir(repo.ID), "--git-dir", e.p.RepoDir(repo.ID), "config", "remote.origin.url", "https://example.test/attacker/evil.git")

	err = AssertConnectedGateURLBinding(context.Background(), e.p, stored)
	if err == nil {
		t.Fatal("binding assertion passed with a drifted gate origin; it must refuse")
	}
	for _, leak := range []string{"attacker", "example.test", "http"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("binding error %q leaks URL content %q", err, leak)
		}
	}
}

// TestAssertConnectedGateURLBindingIgnoresLocalRepos pins that the assertion is
// a no-op for a local repository, whose stored URL is deliberately NOT redacted
// and whose gate origin is managed by the local init path.
func TestAssertConnectedGateURLBindingIgnoresLocalRepos(t *testing.T) {
	e := newConnectedEnv(t)
	local, err := e.db.InsertRepo(t.TempDir(), "https://example.test/acme/local.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertConnectedGateURLBinding(context.Background(), e.p, local); err != nil {
		t.Errorf("assertion must be a no-op for a local repo, got: %v", err)
	}
}
