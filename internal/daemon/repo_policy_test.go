package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestRepoPolicyKeyIsEmptyForLocalRepos pins the property that keeps every
// existing local-repository run byte-identical: a local repo yields an empty
// key, an empty key finds no operator entry, and no entry is the zero policy.
//
// Only connected repositories are declared in configuration, so this is the
// single place where "the floor never touches local repos" is decided.
func TestRepoPolicyKeyIsEmptyForLocalRepos(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo *db.Repo
		want string
	}{
		{"nil repo", nil, ""},
		{"local repo", &db.Repo{ID: "abc", SourceKind: db.SourceKindLocal}, ""},
		{"legacy repo with no source_kind", &db.Repo{ID: "abc"}, ""},
		{
			name: "local repo that somehow carries a name",
			repo: &db.Repo{ID: "abc", SourceKind: db.SourceKindLocal, SourceName: "acme-api"},
			want: "",
		},
		{
			name: "connected repo uses its operator-chosen name",
			repo: &db.Repo{ID: "abc", SourceKind: db.SourceKindConnected, SourceName: "acme-api"},
			want: "acme-api",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoPolicyKey(tc.repo); got != tc.want {
				t.Errorf("repoPolicyKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStartRunFailsClosedOnConnectedGateDrift pins the deliberate asymmetry at
// run start.
//
// A local repository whose URL refresh fails logs and continues: the fallback is
// the operator's own row about their own clone. A connected repository must FAIL
// the run, because a stale or drifted row may name a different repository and
// continuing could carve a worktree from a gate pointing somewhere this run was
// never authorized to touch.
func TestStartRunFailsClosedOnConnectedGateDrift(t *testing.T) {
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

	repo, err := gate.EnsureConnected(context.Background(), database, p, config.RepoSpec{
		Name: "acme-api", URL: "https://example.test/acme/api.git",
		DefaultBranch: "main", CommitName: "QA", CommitEmail: "qa@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Healthy binding passes.
	if err := gate.AssertConnectedGateURLBinding(context.Background(), p, stored); err != nil {
		t.Fatalf("freshly registered repo failed its binding check: %v", err)
	}

	// Drift the gate to a different repository; the run must refuse.
	if _, err := git.RunBare(context.Background(), p.RepoDir(repo.ID), "config", "remote.origin.url", "https://example.test/attacker/evil.git"); err != nil {
		t.Fatal(err)
	}
	err = gate.AssertConnectedGateURLBinding(context.Background(), p, stored)
	if err == nil {
		t.Fatal("binding check passed against a drifted gate; startRun would proceed")
	}
	if !strings.Contains(err.Error(), "acme-api") {
		t.Errorf("error %q does not name the repository", err)
	}
}
