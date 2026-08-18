package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// bareGateWithBranches provisions a bare gate and pushes the given branches into
// it, which is exactly how a watcher's gate comes to hold refs.
func bareGateWithBranches(t *testing.T, branches ...string) (string, string) {
	t.Helper()
	ctx := context.Background()
	work := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	run(t, work, "git", "remote", "add", "gate", bare)
	for _, branch := range branches {
		run(t, work, "git", "push", "gate", "HEAD:refs/heads/"+branch)
	}
	return work, bare
}

func TestListBranchRefsReturnsEveryBranchWithItsCommit(t *testing.T) {
	ctx := context.Background()
	// Nested paths are exercised here, but note git itself forbids
	// "release/1.0" and "release/1.0/hotfix" coexisting: a loose ref file
	// cannot also be a directory. So the nested case uses a distinct parent.
	work, bare := bareGateWithBranches(t, "main", "release/1.0", "release/2.0/hotfix", "feature/x")

	refs, err := ListBranchRefs(ctx, bare)
	if err != nil {
		t.Fatalf("ListBranchRefs: %v", err)
	}

	head := run(t, work, "git", "rev-parse", "HEAD")
	want := []string{
		"refs/heads/main",
		"refs/heads/release/1.0",
		"refs/heads/release/2.0/hotfix",
		"refs/heads/feature/x",
	}
	for _, name := range want {
		got, ok := refs[name]
		if !ok {
			t.Errorf("ListBranchRefs missing %q; got %v", name, refs)
			continue
		}
		if got != head {
			t.Errorf("%s = %q, want %q", name, got, head)
		}
	}
	if len(refs) != len(want) {
		t.Errorf("ListBranchRefs returned %d refs, want %d: %v", len(refs), len(want), refs)
	}
}

// TestListBranchRefsOnBareGateUnderSafeBareRepositoryExplicit pins the reason
// this routes through RunBare. Agent harnesses and hardened CI inject
// safe.bareRepository=explicit, which forbids cwd-based discovery of a bare
// repository, so a helper relying on -C or cmd.Dir would fail there.
func TestListBranchRefsOnBareGateUnderSafeBareRepositoryExplicit(t *testing.T) {
	ctx := context.Background()
	_, bare := bareGateWithBranches(t, "main")

	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")

	refs, err := ListBranchRefs(ctx, bare)
	if err != nil {
		t.Fatalf("ListBranchRefs under safe.bareRepository=explicit: %v", err)
	}
	if _, ok := refs["refs/heads/main"]; !ok {
		t.Errorf("missing refs/heads/main from %v", refs)
	}
}

// TestListBranchRefsIsEmptyForAFreshBareRepo pins that a gate provisioned but
// never fetched yields no work rather than an error, so a watcher's first pass
// over a brand new registration is a clean no-op.
func TestListBranchRefsIsEmptyForAFreshBareRepo(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "fresh.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	refs, err := ListBranchRefs(ctx, bare)
	if err != nil {
		t.Fatalf("ListBranchRefs on a fresh bare repo: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("ListBranchRefs = %v, want empty", refs)
	}
}

// TestListBranchRefsDoesNotDiscoverAnAncestorRepository is the fail-closed
// property, and the reason RunBare is used instead of Run. A malformed directory
// must produce an error, never the branches of whatever repository happens to sit
// above it on disk - which would make a watcher validate refs from an unrelated
// project under the wrong repository's registration.
func TestListBranchRefsDoesNotDiscoverAnAncestorRepository(t *testing.T) {
	ctx := context.Background()
	work := initTestRepo(t)
	run(t, work, "git", "branch", "ancestor-only-branch")

	nested := filepath.Join(work, "not-a-git-dir")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	refs, err := ListBranchRefs(ctx, nested)
	if err == nil {
		if _, leaked := refs["refs/heads/ancestor-only-branch"]; leaked {
			t.Fatalf("ListBranchRefs walked up to an ancestor repository and returned its branches: %v", refs)
		}
		t.Fatalf("ListBranchRefs on a non-repository returned %v, want an error", refs)
	}
}

// TestListBranchRefsIgnoresTagsAndRemoteTrackingRefs pins the scope: only local
// branches are watchable, so a tag or a remote-tracking ref must never appear as
// a branch to validate.
func TestListBranchRefsIgnoresTagsAndRemoteTrackingRefs(t *testing.T) {
	ctx := context.Background()
	_, bare := bareGateWithBranches(t, "main")
	head, err := RunBare(ctx, bare, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("resolve gate head: %v", err)
	}
	if _, err := RunBare(ctx, bare, "tag", "v1.0.0", head); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if _, err := RunBare(ctx, bare, "update-ref", "refs/remotes/origin/main", head); err != nil {
		t.Fatalf("create remote-tracking ref: %v", err)
	}

	refs, err := ListBranchRefs(ctx, bare)
	if err != nil {
		t.Fatalf("ListBranchRefs: %v", err)
	}
	for name := range refs {
		if name == "refs/tags/v1.0.0" || name == "refs/remotes/origin/main" {
			t.Errorf("ListBranchRefs returned non-branch ref %q", name)
		}
	}
	if len(refs) != 1 {
		t.Errorf("ListBranchRefs returned %d refs, want only refs/heads/main: %v", len(refs), refs)
	}
}

// TestListBranchRefsRejectsAnEmptyDirectory pins that an empty argument fails
// rather than silently running git against the process working directory.
func TestListBranchRefsRejectsAnEmptyDirectory(t *testing.T) {
	if _, err := ListBranchRefs(context.Background(), ""); err == nil {
		t.Fatal("ListBranchRefs(\"\") = nil error; an empty gate path must fail closed")
	}
}
