package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestFindRepoRefusesConnectedRepo pins the CLI choke point. findRepo resolves
// the repository for sync, runs, attach, rerun, status, and axi, so refusing
// here makes every cwd-based author surface inert for a connected repository in
// one place.
//
// Both successful return paths are covered: the direct GetRepoByPath hit, and
// the main-worktree fallback a linked worktree takes.
func TestFindRepoRefusesConnectedRepo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		linked  bool
		wantErr string
	}{
		{name: "direct path", wantErr: "connected"},
		{name: "via main worktree fallback", linked: true, wantErr: "connected"},
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

			const id = "aabbccddeeff"
			stub := p.SourceDir(id)
			mustGit(t, root, "init", stub)
			mustGit(t, stub, "config", "user.email", "qa@example.test")
			mustGit(t, stub, "config", "user.name", "QA")
			if _, err := database.InsertConnectedRepo(
				id, stub, "https://example.test/acme/api.git", "example.test/acme/api", "acme-api", "main",
			); err != nil {
				t.Fatal(err)
			}

			cwd := stub
			if tc.linked {
				// A commit is required before a linked worktree can be added.
				mustWriteFile(t, filepath.Join(stub, "seed.txt"), "seed\n")
				mustGit(t, stub, "add", "seed.txt")
				mustGit(t, stub, "commit", "-m", "seed")
				linked := filepath.Join(root, "linked")
				mustGit(t, stub, "worktree", "add", "-b", "feature/x", linked)
				cwd = linked
			}
			t.Chdir(cwd)

			repo, err := findRepo(database)
			if err == nil {
				t.Fatalf("findRepo returned %+v for a connected repo; it must refuse", repo)
			}
			if repo != nil {
				t.Errorf("findRepo returned repo=%+v alongside an error; it must be nil", repo)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			// The operator's own token can live in a connected repo's URL.
			for _, leak := range []string{"https://", "http://"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q leaks a URL fragment %q", err, leak)
				}
			}
		})
	}
}

// TestFindRepoStillResolvesLocalRepos is the regression half: a local
// repository's source_kind is 'local', so the new guard must be a no-op for it.
func TestFindRepoStillResolvesLocalRepos(t *testing.T) {
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

	clone := filepath.Join(root, "operator")
	mustGit(t, root, "init", clone)
	inserted, err := database.InsertRepo(clone, "https://example.test/acme/api.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(clone)

	repo, err := findRepo(database)
	if err != nil {
		t.Fatalf("findRepo on a local repo: %v", err)
	}
	if repo == nil || repo.ID != inserted.ID {
		t.Fatalf("findRepo = %+v, want the inserted local repo %s", repo, inserted.ID)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReposRemoveRequiresYesForDeleteHistory pins that the one irreversible
// path takes an explicit confirmation, not just a flag. Deleting a gate
// destroys run history that cannot be recovered.
func TestReposRemoveRequiresYesForDeleteHistory(t *testing.T) {
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	const id = "aabbccddeeff"
	if _, err := database.InsertConnectedRepo(
		id, p.SourceDir(id), "https://example.test/acme/api.git", "example.test/acme/api", "acme-api", "main",
	); err != nil {
		t.Fatal(err)
	}
	database.Close()
	t.Setenv("NM_HOME", root)

	cmd := newReposCmd()
	cmd.SetArgs([]string{"remove", "acme-api", "--delete-history"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err = cmd.Execute()
	if err == nil {
		t.Fatal("repos remove --delete-history succeeded without --yes; it must refuse")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error %q does not tell the operator how to confirm", err)
	}

	// The record must survive the refusal.
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if still, _ := reopened.GetRepo(id); still == nil {
		t.Error("the registration was removed despite the refusal")
	}
}
