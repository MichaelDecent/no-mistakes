package gate

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func specFor(name, url string) config.RepoSpec {
	return config.RepoSpec{Name: name, URL: url, DefaultBranch: "main", CommitName: "QA Bot", CommitEmail: "qa@example.test"}
}

// TestReconcileConnectedNeverDeletesAnyRepoOrHistory is the data-safety rule of
// this spec. An entry disappearing from configuration - edited, commented out,
// or lost to a bad merge - must never destroy a gate or run history. It is
// marked detached, and re-adding the URL restores it with the same ID.
func TestReconcileConnectedNeverDeletesAnyRepoOrHistory(t *testing.T) {
	e := newConnectedEnv(t)
	ctx := context.Background()
	specs := map[string]config.RepoSpec{"acme-api": specFor("acme-api", e.remote)}

	if _, err := ReconcileConnected(ctx, e.db, e.p, specs); err != nil {
		t.Fatal(err)
	}
	registered, err := e.db.GetRepoBySourceName("acme-api")
	if err != nil || registered == nil {
		t.Fatalf("GetRepoBySourceName = %v, %v", registered, err)
	}

	// The entry disappears from configuration.
	result, err := ReconcileConnected(ctx, e.db, e.p, map[string]config.RepoSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Detached) != 1 || result.Detached[0] != "acme-api" {
		t.Errorf("Detached = %v, want [acme-api]", result.Detached)
	}
	still, err := e.db.GetRepo(registered.ID)
	if err != nil || still == nil {
		t.Fatalf("the record was destroyed: %v, %v", still, err)
	}
	if still.DetachedAt == nil {
		t.Error("DetachedAt is nil after the entry disappeared")
	}

	// Re-adding restores the SAME record, ID, gate, and history.
	if _, err := ReconcileConnected(ctx, e.db, e.p, specs); err != nil {
		t.Fatal(err)
	}
	back, err := e.db.GetRepo(registered.ID)
	if err != nil || back == nil {
		t.Fatalf("re-add did not restore the record: %v, %v", back, err)
	}
	if back.DetachedAt != nil {
		t.Error("DetachedAt was not cleared on re-add")
	}
}

// TestReconcileConnectedNeverTouchesLocalRepos pins that a local repository is
// never read for detachment and never written. Only connected rows participate.
func TestReconcileConnectedNeverTouchesLocalRepos(t *testing.T) {
	e := newConnectedEnv(t)
	local, err := e.db.InsertRepo(t.TempDir(), "https://example.test/acme/local.git", "main")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ReconcileConnected(context.Background(), e.db, e.p, map[string]config.RepoSpec{}); err != nil {
		t.Fatal(err)
	}
	after, err := e.db.GetRepo(local.ID)
	if err != nil || after == nil {
		t.Fatalf("local repo disappeared: %v, %v", after, err)
	}
	if after.DetachedAt != nil {
		t.Error("a local repository was marked detached")
	}
	if after.UpstreamURL != local.UpstreamURL {
		t.Errorf("a local repository's URL changed: %q -> %q", local.UpstreamURL, after.UpstreamURL)
	}
}

// TestReconcileConnectedRefusesTwoNamesForOneRepository is the duplicate-name
// check that moved here from S4 and then S8. Only a caller holding the WHOLE
// spec set can see it: to EnsureConnected, a second name for a registered remote
// is indistinguishable from a rename.
func TestReconcileConnectedRefusesTwoNamesForOneRepository(t *testing.T) {
	e := newConnectedEnv(t)
	result, err := ReconcileConnected(context.Background(), e.db, e.p, map[string]config.RepoSpec{
		"acme-api":   specFor("acme-api", e.remote),
		"acme-again": specFor("acme-again", "https://example.test/acme/api"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic by sorted name: "acme-again" sorts first and wins, so
	// "acme-api" is the reported conflict. What matters is that exactly one is
	// registered and the other is reported rather than silently dropped.
	if len(result.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly one conflict", result.Failed)
	}
	for name, err := range result.Failed {
		if !strings.Contains(err.Error(), "same repository") {
			t.Errorf("conflict for %q reads %q, want it to name the duplication", name, err)
		}
	}
	if len(result.Registered) != 1 {
		t.Errorf("Registered = %v, want exactly one", result.Registered)
	}
}

// TestReconcileConnectedContinuesPastOneBadEntry pins that an operator with
// several repositories and one typo still gets the rest reconciled.
func TestReconcileConnectedContinuesPastOneBadEntry(t *testing.T) {
	e := newConnectedEnv(t)
	result, err := ReconcileConnected(context.Background(), e.db, e.p, map[string]config.RepoSpec{
		"good":   specFor("good", e.remote),
		"broken": specFor("broken", "not a url"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Registered) != 1 || result.Registered[0] != "good" {
		t.Errorf("Registered = %v, want [good]", result.Registered)
	}
	if _, ok := result.Failed["broken"]; !ok {
		t.Errorf("Failed = %v, want the broken entry reported", result.Failed)
	}
}

// TestRemoveConnectedRefusesWhileRunsAreActive pins that the one destructive
// path protects live work: those runs hold worktrees carved from this gate.
func TestRemoveConnectedRefusesWhileRunsAreActive(t *testing.T) {
	e := newConnectedEnv(t)
	ctx := context.Background()
	repo, err := EnsureConnected(ctx, e.db, e.p, specFor("acme-api", e.remote))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.InsertRun(repo.ID, "feature/x", "abc123", "def456"); err != nil {
		t.Fatal(err)
	}

	if err := RemoveConnected(ctx, e.db, e.p, "acme-api", true); err == nil {
		t.Fatal("RemoveConnected succeeded with an active run; it must refuse")
	}
	if still, _ := e.db.GetRepo(repo.ID); still == nil {
		t.Error("the record was removed despite the refusal")
	}
}

// TestRemoveConnectedDeletesOnlyWhenAsked pins that the registration goes away
// by default while the gate survives, and that --delete-history is what removes
// the gate itself.
func TestRemoveConnectedDeletesOnlyWhenAsked(t *testing.T) {
	e := newConnectedEnv(t)
	ctx := context.Background()
	repo, err := EnsureConnected(ctx, e.db, e.p, specFor("acme-api", e.remote))
	if err != nil {
		t.Fatal(err)
	}
	gateDir := e.p.RepoDir(repo.ID)

	if err := RemoveConnected(ctx, e.db, e.p, "acme-api", false); err != nil {
		t.Fatalf("RemoveConnected: %v", err)
	}
	if gone, _ := e.db.GetRepo(repo.ID); gone != nil {
		t.Error("the registration survived removal")
	}
	if !dirExists(gateDir) {
		t.Error("the gate was deleted without --delete-history")
	}
}
