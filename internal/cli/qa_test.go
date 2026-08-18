package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// qaTestHome isolates NM_HOME and returns an open DB for it, so a test can
// register repositories the CLI will resolve.
func qaTestHome(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return p, database
}

func insertTestConnectedRepo(t *testing.T, p *paths.Paths, database *db.DB, id, name string) *db.Repo {
	t.Helper()
	stub := p.SourceDir(id)
	mustGit(t, p.Root(), "init", stub)
	mustGit(t, stub, "config", "user.email", "qa@example.test")
	mustGit(t, stub, "config", "user.name", "QA")
	// The row stores what registration stores: the resolved path git reports.
	repo, err := database.InsertConnectedRepo(id, resolvedRepoPath(t, stub), "https://example.test/acme/"+name+".git", "example.test/acme/"+name, name, "main")
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func runQARun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newQARunCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestQARunRequiresARepoSelector(t *testing.T) {
	// Nothing about the current directory can select a connected repository, so
	// the command asks rather than guessing - and it asks before any daemon is
	// started.
	_, err := runQARun(t, "--mode", "report")
	if err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("error = %v, want the missing-selector refusal", err)
	}
}

func TestQARunRejectsAModeItCannotHonor(t *testing.T) {
	_, err := runQARun(t, "--repo", "acme-api", "--mode", "fix-everything")
	if err == nil || !strings.Contains(err.Error(), "--mode") {
		t.Fatalf("error = %v, want the mode refusal", err)
	}
}

func TestQARunFixPRRequiresExplicitConsent(t *testing.T) {
	p, database := qaTestHome(t)
	insertTestConnectedRepo(t, p, database, "aabbccddeeff", "acme-api")

	out, err := runQARun(t, "--repo", "acme-api", "--branch", "feature", "--mode", "fix-pr")
	if err == nil {
		t.Fatal("a code-writing QA run started without consent")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %v, want it to name the consent flag", err)
	}
	// The notice states the bounded scope being consented to: what it may write,
	// where it pushes, and what it opens a PR against.
	for _, want := range []string{"writes code", "feature", "acme-api", "pull request"} {
		if !strings.Contains(out, want) {
			t.Errorf("consent notice %q is missing %q", out, want)
		}
	}
	// A URL can carry a token, so the notice must never print one.
	if strings.Contains(out, "https://") {
		t.Errorf("consent notice leaked a URL: %q", out)
	}
	// Nothing was started: no daemon, no run.
	runs, err := database.GetRunsByRepo("aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("got %d runs, want none", len(runs))
	}
	if _, statErr := os.Stat(p.Socket()); statErr == nil {
		t.Error("the daemon socket exists: consent was checked after starting the daemon")
	}
}

func TestQARunRefusesALocalRepository(t *testing.T) {
	_, database := qaTestHome(t)
	if _, err := database.InsertRepoWithID("112233445566", t.TempDir(), "https://example.test/acme/local.git", "main"); err != nil {
		t.Fatal(err)
	}

	_, err := runQARun(t, "--repo", "112233445566", "--mode", "report")
	if err == nil || !strings.Contains(err.Error(), "local repository") {
		t.Fatalf("error = %v, want QA to refuse a local repository", err)
	}
}

// --- resolveRepo ---

func TestResolveRepoBySelector(t *testing.T) {
	p, database := qaTestHome(t)
	api := insertTestConnectedRepo(t, p, database, "aabbccddeeff", "acme-api")
	web := insertTestConnectedRepo(t, p, database, "aabbcc001122", "acme-web")

	for _, tc := range []struct {
		name     string
		selector string
		wantID   string
	}{
		{"by name", "acme-api", api.ID},
		{"by full id", web.ID, web.ID},
		{"by unique id prefix", "aabbccdd", api.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, err := resolveRepo(database, tc.selector)
			if err != nil {
				t.Fatalf("resolveRepo(%q): %v", tc.selector, err)
			}
			if repo.ID != tc.wantID {
				t.Fatalf("resolveRepo(%q) = %s, want %s", tc.selector, repo.ID, tc.wantID)
			}
		})
	}
}

func TestResolveRepoRefusesAnAmbiguousPrefix(t *testing.T) {
	p, database := qaTestHome(t)
	insertTestConnectedRepo(t, p, database, "aabbccddeeff", "acme-api")
	insertTestConnectedRepo(t, p, database, "aabbcc001122", "acme-web")

	// "aabbcc" matches both. Picking one would run against a repository the
	// operator did not name.
	_, err := resolveRepo(database, "aabbcc")
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("error = %v, want an ambiguity refusal", err)
	}
	if !strings.Contains(err.Error(), "acme-api") || !strings.Contains(err.Error(), "acme-web") {
		t.Errorf("error = %v, want it to name the candidates", err)
	}
}

func TestResolveRepoRefusesAnUnknownSelector(t *testing.T) {
	_, database := qaTestHome(t)
	if _, err := resolveRepo(database, "nope"); err == nil {
		t.Fatal("resolveRepo accepted an unknown selector")
	}
}

func TestResolveRepoWithNoSelectorKeepsTheCwdBehaviour(t *testing.T) {
	p, database := qaTestHome(t)
	repo := insertTestConnectedRepo(t, p, database, "aabbccddeeff", "acme-api")
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo.WorkingPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(dir) })

	// An empty selector still asks "which repository is this directory", and a
	// connected repository has no answer to that - the S3 refusal stands.
	if _, err := resolveRepo(database, ""); err == nil || !strings.Contains(err.Error(), "connected") {
		t.Fatalf("error = %v, want the cwd-based connected refusal", err)
	}
}

func TestRunsAcceptsAnExplicitConnectedRepoSelector(t *testing.T) {
	p, database := qaTestHome(t)
	repo := insertTestConnectedRepo(t, p, database, "aabbccddeeff", "acme-api")
	run, err := database.InsertRun(repo.ID, "main", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}

	cmd := newRunsCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--repo", "acme-api"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("runs --repo: %v", err)
	}
	// An operator needs to see what a QA run did; reading rows involves no
	// checkout, so an explicitly named connected repository is answerable here.
	rendered := out.String()
	if !strings.Contains(rendered, run.Branch) || !strings.Contains(rendered, "pending") {
		t.Fatalf("runs output %q does not describe the run", rendered)
	}
}
