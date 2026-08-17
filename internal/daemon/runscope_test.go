package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestEffectiveSkipStepsNeverNarrowsAGateRun(t *testing.T) {
	explicit := []types.StepName{types.StepTest}
	for _, mode := range types.AllRunModes() {
		got := effectiveSkipSteps(runScope{kind: types.RunKindGate, mode: mode}, explicit)
		if len(got) != 1 || got[0] != types.StepTest {
			t.Errorf("effectiveSkipSteps(gate, %s) = %v, want only the explicit per-run skip", mode, got)
		}
	}
}

func TestEffectiveSkipStepsUnionsTheModeDeclarationWithExplicitSkips(t *testing.T) {
	got := effectiveSkipSteps(runScope{kind: types.RunKindQA, mode: types.RunModeReport}, []types.StepName{types.StepTest, types.StepPush})
	want := map[types.StepName]bool{
		types.StepTest:     true, // explicit
		types.StepDocument: true, // declared by the mode
		types.StepPush:     true, // both, and listed once
		types.StepPR:       true,
		types.StepCI:       true,
	}
	if len(got) != len(want) {
		t.Fatalf("effectiveSkipSteps = %v, want %d distinct steps", got, len(want))
	}
	seen := map[types.StepName]bool{}
	for _, step := range got {
		if seen[step] {
			t.Fatalf("effectiveSkipSteps = %v, want no duplicates", got)
		}
		seen[step] = true
		if !want[step] {
			t.Errorf("effectiveSkipSteps included %q unexpectedly", step)
		}
	}
}

func TestEffectiveSkipStepsLeavesAFixPRQARunFull(t *testing.T) {
	if got := effectiveSkipSteps(runScope{kind: types.RunKindQA, mode: types.RunModeFixPR}, nil); len(got) != 0 {
		t.Errorf("effectiveSkipSteps(qa, fix-pr) = %v, want nothing skipped", got)
	}
}

func TestReadOnlyQARunNeverGetsAnAutoFixBudget(t *testing.T) {
	// Auto-fix rounds edit files and commit them. In a read-only run those
	// commits can never be pushed, and a repaired test would be reported as a
	// passing test - the report must describe the code as it actually is.
	for _, mode := range []types.RunMode{types.RunModeReport, types.RunModeComment} {
		cfg := &config.Config{AutoFix: config.AutoFix{Lint: 3, Test: 3, Document: 3, CI: 3, Rebase: 3, Review: 1}}
		got := applyRunScopeToConfig(cfg, runScope{kind: types.RunKindQA, mode: mode})
		for _, step := range []types.StepName{types.StepLint, types.StepTest, types.StepDocument, types.StepCI, types.StepRebase, types.StepReview} {
			if limit := got.AutoFixLimit(step); limit != 0 {
				t.Errorf("mode %s: AutoFixLimit(%s) = %d, want 0", mode, step, limit)
			}
		}
	}
}

func TestFixPRQARunKeepsItsAutoFixBudget(t *testing.T) {
	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 3, Test: 2}}
	got := applyRunScopeToConfig(cfg, runScope{kind: types.RunKindQA, mode: types.RunModeFixPR})
	if got.AutoFixLimit(types.StepLint) != 3 || got.AutoFixLimit(types.StepTest) != 2 {
		t.Fatalf("fix-pr budgets = lint %d test %d, want them untouched", got.AutoFixLimit(types.StepLint), got.AutoFixLimit(types.StepTest))
	}
}

func TestGateRunConfigIsNeverNarrowedByScope(t *testing.T) {
	for _, mode := range types.AllRunModes() {
		cfg := &config.Config{AutoFix: config.AutoFix{Lint: 3, Test: 3}}
		got := applyRunScopeToConfig(cfg, runScope{kind: types.RunKindGate, mode: mode})
		if got.AutoFixLimit(types.StepLint) != 3 || got.AutoFixLimit(types.StepTest) != 3 {
			t.Fatalf("gate run with mode %s lost its auto-fix budget", mode)
		}
	}
}

func TestRunScopeFromRunReadsLegacyRowsAsGateRuns(t *testing.T) {
	if got := runScopeFromRun(&db.Run{}); got.kind != types.RunKindGate {
		t.Errorf("runScopeFromRun(legacy row).kind = %q, want gate", got.kind)
	}
	if got := runScopeFromRun(nil); got.kind != types.RunKindGate {
		t.Errorf("runScopeFromRun(nil).kind = %q, want gate", got.kind)
	}
	got := runScopeFromRun(&db.Run{RunKind: string(types.RunKindQA), RunMode: string(types.RunModeComment)})
	if got.kind != types.RunKindQA || got.mode != types.RunModeComment {
		t.Errorf("runScopeFromRun(qa/comment) = %+v", got)
	}
}

func TestDBScopeKeepsAGateRunRowUnchanged(t *testing.T) {
	got := gateRunScope().dbScope([]types.StepName{types.StepTest})
	if got.Kind != "" || got.Mode != "" || got.SkippedSteps != nil {
		t.Fatalf("gate dbScope = %+v, want every column unset", got)
	}
}

// --- commit-derived intent ---

func initIntentRepo(t *testing.T) (dir, baseSHA, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base commit")
	baseSHA = run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "add the widget cache")
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "cover the cache with tests")
	headSHA = run("rev-parse", "HEAD")
	return dir, baseSHA, headSHA
}

func TestCommitDerivedIntentSummarizesTheBranchsOwnCommits(t *testing.T) {
	dir, baseSHA, headSHA := initIntentRepo(t)
	got := commitDerivedIntent(context.Background(), dir, baseSHA, headSHA)
	if !strings.Contains(got, "add the widget cache") || !strings.Contains(got, "cover the cache with tests") {
		t.Fatalf("commitDerivedIntent = %q, want both branch commit subjects", got)
	}
	if strings.Contains(got, "base commit") {
		t.Errorf("commitDerivedIntent = %q, want only commits the branch added", got)
	}
}

func TestCommitDerivedIntentFallsBackToTheHeadCommit(t *testing.T) {
	dir, _, headSHA := initIntentRepo(t)
	// No usable range - an unknown base is normal for a freshly observed
	// branch - so the head commit alone still describes it.
	got := commitDerivedIntent(context.Background(), dir, "", headSHA)
	if got != "cover the cache with tests" {
		t.Fatalf("commitDerivedIntent = %q, want the head commit subject", got)
	}
}

func TestCommitDerivedIntentIsEmptyWhenNothingCanBeRead(t *testing.T) {
	if got := commitDerivedIntent(context.Background(), t.TempDir(), "", "deadbeef"); got != "" {
		t.Fatalf("commitDerivedIntent = %q, want empty: derivation is best effort", got)
	}
	if got := commitDerivedIntent(context.Background(), t.TempDir(), "", ""); got != "" {
		t.Fatalf("commitDerivedIntent with no head = %q, want empty", got)
	}
}

func TestCommitDerivedIntentIsBounded(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "base")
	baseCmd := exec.Command("git", "rev-parse", "HEAD")
	baseCmd.Dir = dir
	baseOut, err := baseCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(baseOut))
	subject := strings.Repeat("x", 120)
	for i := 0; i < 30; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Repeat("y", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-m", subject)
	}
	headCmd := exec.Command("git", "rev-parse", "HEAD")
	headCmd.Dir = dir
	headOut, err := headCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := commitDerivedIntent(context.Background(), dir, baseSHA, strings.TrimSpace(string(headOut)))
	// A branch with hundreds of commits must not become a prompt section of the
	// same size.
	if len(got) > maxCommitIntentBytes+3 {
		t.Fatalf("commitDerivedIntent is %d bytes, want at most %d", len(got), maxCommitIntentBytes+3)
	}
}
