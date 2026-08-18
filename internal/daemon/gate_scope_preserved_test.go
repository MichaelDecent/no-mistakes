package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPushReceivedIgnoresRepoQAModeAndRunsAllNineSteps is the gate-path
// regression guard for the whole QA feature. A push is the author's consent
// boundary, so no QA setting - the operator's global qa block, a repository's own
// .no-mistakes.yaml, or a mode configured anywhere - may narrow what a push runs.
func TestPushReceivedIgnoresRepoQAModeAndRunsAllNineSteps(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// An operator qa block that would narrow a QA run to the read-only step set,
	// plus a repository declaring its own qa policy (which config loading ignores
	// entirely).
	if err := os.WriteFile(p.ConfigFile(), []byte("qa:\n  enabled: true\n  default_mode: report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	// The upstream the gate mirrors. A run needs the trusted default branch to
	// be fetchable from the worktree's inherited origin, which is exactly how a
	// production gate is provisioned.
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	qaGit(t, t.TempDir(), "init", "--bare", upstream)
	clone := t.TempDir()
	qaGit(t, clone, "clone", upstream, ".")
	qaGit(t, clone, "config", "user.name", "Test")
	qaGit(t, clone, "config", "user.email", "test@test.com")
	qaGit(t, clone, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A repository declaring its own qa policy: config loading ignores the block
	// entirely, so a repository can never weaken the gate that validates it.
	if err := os.WriteFile(filepath.Join(clone, ".no-mistakes.yaml"), []byte("qa:\n  default_mode: report\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qaGit(t, clone, "add", ".")
	qaGit(t, clone, "commit", "-m", "base commit")
	qaGit(t, clone, "push", "-u", "origin", "main")
	baseSHA := qaGit(t, clone, "rev-parse", "HEAD")

	repoID := "gaterepo0001"
	gateDir := p.RepoDir(repoID)
	if err := os.MkdirAll(filepath.Dir(gateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	qaGit(t, t.TempDir(), "clone", "--bare", upstream, gateDir)

	qaGit(t, clone, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(clone, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qaGit(t, clone, "add", ".")
	qaGit(t, clone, "commit", "-m", "add the widget cache")
	qaGit(t, clone, "remote", "add", "gate", gateDir)
	qaGit(t, clone, "push", "gate", "feature")
	headSHA := qaGit(t, clone, "rev-parse", "HEAD")

	if _, err := database.InsertRepoWithID(repoID, clone, upstream, "main"); err != nil {
		t.Fatal(err)
	}

	// One fake step per real pipeline step, so "all nine" is observable.
	names := types.AllSteps()
	recorded := make([]*recordingStep, 0, len(names))
	mgr := NewRunManager(database, p, func() []pipeline.Step {
		built := make([]pipeline.Step, 0, len(names))
		recorded = recorded[:0]
		for _, name := range names {
			step := &recordingStep{name: name}
			recorded = append(recorded, step)
			built = append(built, step)
		}
		return built
	})

	runID, err := mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate: gateDir,
		Ref:  "refs/heads/feature",
		New:  headSHA,
		Old:  baseSHA,
	})
	if err != nil {
		t.Fatalf("HandlePushReceived: %v", err)
	}
	waitForRunToFinish(t, database, runID)

	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if types.NormalizeRunKind(run.RunKind) != types.RunKindGate {
		t.Errorf("kind = %q, want a gate run", run.RunKind)
	}
	if run.RunKind != "" || run.RunMode != "" {
		t.Errorf("row = kind %q mode %q, want both unwritten for a push", run.RunKind, run.RunMode)
	}
	if len(run.SkippedStepNames()) != 0 {
		t.Fatalf("skipped_steps = %v, want a push to declare no skips", run.SkippedStepNames())
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != len(names) {
		t.Fatalf("got %d step rows, want %d", len(steps), len(names))
	}
	for _, step := range steps {
		if step.Status == types.StepStatusSkipped {
			t.Errorf("step %s was skipped in a push-triggered run", step.StepName)
		}
	}
	for _, step := range recorded {
		if !step.ran {
			t.Errorf("step %s never executed", step.name)
		}
	}
}
