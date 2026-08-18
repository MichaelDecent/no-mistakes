package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitOut runs git and returns stdout, letting a caller distinguish "the command
// failed" from "the value is empty" - which config --get needs.
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// TestEjectRefusesConnectedRepo pins that eject - which exists to remove the
// gate remote from a developer's clone - refuses a connected repository, and
// that its gate and record survive.
//
// The two cases exercise DIFFERENT guards, which is why they are separate. A
// stub lives under SourcesDir, so refuseDaemonOwnedRoot rejects it before the
// repository is even looked up. The out-of-tree case is the only one that
// reaches the repo.Connected() check, and it is what keeps that check honest
// rather than dead code shadowed by the root guard.
func TestEjectRefusesConnectedRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		// workingPath returns the path registered as the repo's working_path,
		// which is also the directory eject is invoked from.
		workingPath func(t *testing.T, p *paths.Paths, root, id string) string
		wantHint    string
	}{
		{
			name:        "identity stub is refused as daemon-owned before lookup",
			workingPath: func(_ *testing.T, p *paths.Paths, _, id string) string { return p.SourceDir(id) },
			wantHint:    "no-mistakes owns",
		},
		{
			name: "connected repo outside the daemon tree is refused by the connected guard",
			workingPath: func(_ *testing.T, _ *paths.Paths, root, _ string) string {
				return filepath.Join(root, "elsewhere")
			},
			wantHint: "repos remove",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			p := paths.WithRoot(filepath.Join(root, "state"))
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			const id = "aabbccddeeff"
			workDir := tc.workingPath(t, p, root, id)
			mustRunGit(t, root, "init", workDir)
			// EnsureConnected stores the symlink-resolved stub path, and Eject
			// resolves before it looks a repository up. A row holding an
			// unresolved path would be found by neither, so the guard under test
			// would never be reached - which is exactly what happened on the
			// macOS leg, where every temp dir is a symlink.
			storedPath := workDir
			if resolved, resolveErr := filepath.EvalSymlinks(workDir); resolveErr == nil {
				storedPath = resolved
			}
			if _, err := database.InsertConnectedRepo(
				id, storedPath, "https://example.test/acme/api.git", "example.test/acme/api", "acme-api", "main",
			); err != nil {
				t.Fatal(err)
			}

			repo, err := Eject(context.Background(), database, p, workDir)
			if err == nil {
				t.Fatalf("Eject returned %+v for a connected repo; it must refuse", repo)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Errorf("error %q does not contain %q, so a different guard fired than this case is pinning", err, tc.wantHint)
			}
			for _, leak := range []string{"https://", "http://"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q leaks a URL fragment %q", err, leak)
				}
			}

			// The record must survive: eject is not a removal path for connected repos.
			if still, getErr := database.GetRepo(id); getErr != nil || still == nil {
				t.Fatalf("GetRepo after refused Eject = %v, %v; the record must survive", still, getErr)
			}
		})
	}
}

// TestInitRefusesRootsUnderDaemonOwnedDirectories closes a pre-existing gap:
// nothing stopped `no-mistakes init` inside a bare gate, a run worktree, or a
// connected repository's identity stub. Initializing there would register
// daemon-owned state as a developer checkout and let author-side commands
// mutate it.
func TestInitRefusesRootsUnderDaemonOwnedDirectories(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  func(p *paths.Paths) string
	}{
		{"identity stub under sources", func(p *paths.Paths) string { return p.SourceDir("aabbccddeeff") }},
		{"bare gate under repos", func(p *paths.Paths) string { return p.RepoDir("aabbccddeeff") }},
		{"run worktree under worktrees", func(p *paths.Paths) string { return p.WorktreeDir("aabbccddeeff", "run1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			p := paths.WithRoot(root)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			dir := tc.dir(p)
			mustRunGit(t, root, "init", dir)

			repo, created, err := Init(context.Background(), database, p, dir)
			if err == nil {
				t.Fatalf("Init returned repo=%+v created=%v inside %s; it must refuse", repo, created, dir)
			}
			if !strings.Contains(err.Error(), "no-mistakes") {
				t.Errorf("error %q does not explain that the directory is daemon-owned", err)
			}
		})
	}
}

// TestInitStillAcceptsAnOrdinaryCheckout is the regression half: the new root
// guard must not reject a normal developer clone that merely lives somewhere
// else on disk.
func TestInitStillAcceptsAnOrdinaryCheckout(t *testing.T) {
	root := t.TempDir()
	p := paths.WithRoot(filepath.Join(root, "state"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	clone := filepath.Join(root, "operator")
	mustRunGit(t, root, "init", clone)
	mustRunGit(t, clone, "remote", "add", "origin", "https://example.test/acme/api.git")

	repo, created, err := Init(context.Background(), database, p, clone)
	if err != nil {
		t.Fatalf("Init on an ordinary checkout: %v", err)
	}
	if !created || repo == nil {
		t.Fatalf("Init created=%v repo=%v, want a fresh gate", created, repo)
	}
	if repo.Connected() {
		t.Error("an ordinary checkout registered as connected")
	}
}

// removeGate deletes a repository's bare gate so a gate-side write fails while
// the database stays healthy.
func removeGate(t *testing.T, bareDir string) {
	t.Helper()
	if err := os.RemoveAll(bareDir); err != nil {
		t.Fatal(err)
	}
}
