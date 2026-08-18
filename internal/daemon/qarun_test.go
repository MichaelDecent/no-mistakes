package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// resolvedTestPath mirrors what registration records: git reports a
// symlink-resolved root, and every path-keyed lookup resolves before it asks.
func resolvedTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

func qaGit(t *testing.T, dir string, args ...string) string {
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

// connectedFixture builds what a registered connected repository looks like on
// disk: a bare "remote" holding the branches, and a daemon-owned bare gate whose
// origin points at it. The row is inserted directly, the way S3's guard tests
// do, so these tests need neither a reachable forge nor a scheme-qualified URL.
type connectedFixture struct {
	mgr      *RunManager
	database *db.DB
	paths    *paths.Paths
	repo     *db.Repo
	upstream string
	mainSHA  string
	headSHA  string
}

func newConnectedFixture(t *testing.T, stepFactory StepFactory) *connectedFixture {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	// The "remote": a bare repo with main plus a feature branch.
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	qaGit(t, t.TempDir(), "init", "--bare", upstream)
	work := t.TempDir()
	qaGit(t, work, "clone", upstream, ".")
	qaGit(t, work, "config", "user.name", "Test")
	qaGit(t, work, "config", "user.email", "test@test.com")
	qaGit(t, work, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qaGit(t, work, "add", ".")
	qaGit(t, work, "commit", "-m", "base commit")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qaGit(t, work, "commit", "-am", "second commit on main")
	qaGit(t, work, "push", "-u", "origin", "main")
	mainSHA := qaGit(t, work, "rev-parse", "HEAD")
	qaGit(t, work, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qaGit(t, work, "add", ".")
	qaGit(t, work, "commit", "-m", "add the widget cache")
	qaGit(t, work, "push", "origin", "feature")
	headSHA := qaGit(t, work, "rev-parse", "HEAD")

	repoID := "connected0001"
	stub := p.SourceDir(repoID)
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	qaGit(t, filepath.Dir(stub), "init", stub)
	qaGit(t, stub, "config", "--local", "user.name", "QA Bot")
	qaGit(t, stub, "config", "--local", "user.email", "qa@example.com")
	// EnsureConnected stores the symlink-resolved stub path, so the fixture
	// stores the same thing: an unresolved path stops matching wherever the
	// platform aliases directories (macOS /var, Windows 8.3 short names).
	repo, err := database.InsertConnectedRepo(repoID, resolvedTestPath(t, stub), upstream, "example.com/acme/api", "acme-api", "main")
	if err != nil {
		t.Fatal(err)
	}

	// The gate: bare, with origin pointing at the remote, exactly as
	// ProvisionConnectedGate leaves it.
	gateDir := p.RepoDir(repo.ID)
	if err := os.MkdirAll(filepath.Dir(gateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	qaGit(t, t.TempDir(), "init", "--bare", gateDir)
	qaGit(t, gateDir, "remote", "add", "origin", upstream)

	return &connectedFixture{
		mgr:      NewRunManager(database, p, stepFactory),
		database: database,
		paths:    p,
		repo:     repo,
		upstream: upstream,
		mainSHA:  mainSHA,
		headSHA:  headSHA,
	}
}

func TestStartQARunRefusesALocalRepository(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/acme/api", "main")
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewRunManager(database, p, nil)

	_, err = mgr.StartQARun(context.Background(), QARunRequest{RepoID: repo.ID, Mode: types.RunModeReport})
	if err == nil {
		t.Fatal("StartQARun accepted a local repository")
	}
	// QA never operates on someone's own checkout: a run there would take the
	// branch lock on a branch the author is working on and show up in their own
	// status.
	if !strings.Contains(err.Error(), "connected") {
		t.Fatalf("error = %v, want it to name the connected-repository requirement", err)
	}
}

func TestStartQARunRefusesADetachedRepository(t *testing.T) {
	f := newConnectedFixture(t, nil)
	detachedAt := time.Now().Unix()
	if err := f.database.SetRepoDetachedAt(f.repo.ID, &detachedAt); err != nil {
		t.Fatal(err)
	}
	reloaded, err := f.database.GetRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = reloaded
	_, err = f.mgr.StartQARun(context.Background(), QARunRequest{RepoID: f.repo.ID, Mode: types.RunModeReport})
	if err == nil || !strings.Contains(err.Error(), "detached") {
		t.Fatalf("error = %v, want a refusal naming the detached registration", err)
	}
}

func TestStartQARunRefusesAModeItCannotHonor(t *testing.T) {
	f := newConnectedFixture(t, nil)
	for _, mode := range []types.RunMode{"", "fix-everything", "REPORT"} {
		if _, err := f.mgr.StartQARun(context.Background(), QARunRequest{RepoID: f.repo.ID, Mode: mode}); err == nil {
			t.Errorf("StartQARun accepted mode %q", mode)
		}
	}
}

func TestStartQARunRefusesAnUnknownRepository(t *testing.T) {
	f := newConnectedFixture(t, nil)
	if _, err := f.mgr.StartQARun(context.Background(), QARunRequest{RepoID: "nope", Mode: types.RunModeReport}); err == nil {
		t.Fatal("StartQARun accepted an unknown repository")
	}
}

func TestPrepareQARunFetchesTheBranchIntoTheGate(t *testing.T) {
	f := newConnectedFixture(t, nil)
	prepared, err := f.mgr.prepareQARun(context.Background(), f.repo, QARunRequest{
		RepoID: f.repo.ID,
		Branch: "feature",
		Mode:   types.RunModeReport,
	})
	if err != nil {
		t.Fatalf("prepareQARun: %v", err)
	}
	if prepared.branch != "feature" {
		t.Errorf("branch = %q, want feature", prepared.branch)
	}
	if prepared.headSHA != f.headSHA {
		t.Errorf("headSHA = %q, want the remote's branch head %q", prepared.headSHA, f.headSHA)
	}
	// A connected gate receives no pushes, so the run's commits can only be
	// there if the branch was fetched first.
	inGate := qaGit(t, f.paths.RepoDir(f.repo.ID), "rev-parse", "refs/heads/feature")
	if inGate != f.headSHA {
		t.Errorf("gate refs/heads/feature = %q, want %q", inGate, f.headSHA)
	}
	// The base is the merge-base with the default branch, so review sees the
	// branch's own work rather than the whole repository.
	if prepared.baseSHA != f.mainSHA {
		t.Errorf("baseSHA = %q, want the merge-base with main %q", prepared.baseSHA, f.mainSHA)
	}
}

func TestPrepareQARunDefaultsToTheRepositorysDefaultBranch(t *testing.T) {
	f := newConnectedFixture(t, nil)
	prepared, err := f.mgr.prepareQARun(context.Background(), f.repo, QARunRequest{RepoID: f.repo.ID, Mode: types.RunModeReport})
	if err != nil {
		t.Fatalf("prepareQARun: %v", err)
	}
	if prepared.branch != "main" {
		t.Fatalf("branch = %q, want the repository default branch", prepared.branch)
	}
	if prepared.headSHA != f.mainSHA {
		t.Fatalf("headSHA = %q, want %q", prepared.headSHA, f.mainSHA)
	}
	// On the default branch there is no merge-base to fall back on, so the base
	// is the previous commit: the newest change is what gets validated.
	if prepared.baseSHA == "" || prepared.baseSHA == prepared.headSHA {
		t.Fatalf("baseSHA = %q, want a base the newest commit can be diffed against", prepared.baseSHA)
	}
}

func TestPrepareQARunHonorsAnExplicitBase(t *testing.T) {
	f := newConnectedFixture(t, nil)
	// The watcher supplies the head it last validated, so a sweep reviews only
	// what arrived since.
	prepared, err := f.mgr.prepareQARun(context.Background(), f.repo, QARunRequest{
		RepoID:  f.repo.ID,
		Branch:  "feature",
		Mode:    types.RunModeReport,
		BaseSHA: f.mainSHA,
	})
	if err != nil {
		t.Fatalf("prepareQARun: %v", err)
	}
	if prepared.baseSHA != f.mainSHA {
		t.Fatalf("baseSHA = %q, want the caller's base %q", prepared.baseSHA, f.mainSHA)
	}
}

func TestPrepareQARunFailsOnAnUnknownBranch(t *testing.T) {
	f := newConnectedFixture(t, nil)
	_, err := f.mgr.prepareQARun(context.Background(), f.repo, QARunRequest{
		RepoID: f.repo.ID,
		Branch: "no-such-branch",
		Mode:   types.RunModeReport,
	})
	if err == nil {
		t.Fatal("prepareQARun accepted a branch the remote does not have")
	}
}

func TestPrepareQARunFailsWhenTheGateOriginDoesNotMatchTheRegistration(t *testing.T) {
	f := newConnectedFixture(t, nil)
	// Invariant C1: a drifted gate may name a DIFFERENT repository, so nothing
	// may be fetched from it or run against it.
	qaGit(t, f.paths.RepoDir(f.repo.ID), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "elsewhere.git"))
	_, err := f.mgr.prepareQARun(context.Background(), f.repo, QARunRequest{RepoID: f.repo.ID, Branch: "main", Mode: types.RunModeReport})
	if err == nil {
		t.Fatal("prepareQARun ran against a gate whose origin does not match its registration")
	}
	if !strings.Contains(err.Error(), "registration") {
		t.Fatalf("error = %v, want the binding refusal", err)
	}
}

// --- StartQARun end to end, with fake steps ---

type recordingStep struct {
	name     types.StepName
	findings string
	ran      bool
	head     string
	intent   string
	source   string
}

func (s *recordingStep) Name() types.StepName { return s.name }

func (s *recordingStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.ran = true
	s.head = sctx.Run.HeadSHA
	s.intent = sctx.UserIntent
	s.source = sctx.IntentSource
	return &pipeline.StepOutcome{NeedsApproval: s.findings != "", Findings: s.findings}, nil
}

func TestStartQARunRecordsItsScopeFindingsAndCommitIntent(t *testing.T) {
	t.Setenv("NM_DEMO", "1") // a no-op agent; the steps here are fakes anyway
	review := &recordingStep{
		name:     types.StepReview,
		findings: `{"findings":[{"id":"rev-1","severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`,
	}
	push := &recordingStep{name: types.StepPush}
	f := newConnectedFixture(t, func() []pipeline.Step {
		return []pipeline.Step{&recordingStep{name: types.StepIntent}, review, &recordingStep{name: types.StepDocument}, push}
	})

	runID, err := f.mgr.StartQARun(context.Background(), QARunRequest{
		RepoID: f.repo.ID,
		Branch: "feature",
		Mode:   types.RunModeReport,
	})
	if err != nil {
		t.Fatalf("StartQARun: %v", err)
	}
	waitForRunToFinish(t, f.database, runID)

	run, err := f.database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.RunKind != string(types.RunKindQA) || run.RunMode != string(types.RunModeReport) {
		t.Errorf("row = kind %q mode %q, want qa/report", run.RunKind, run.RunMode)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("status = %q, want %q (error: %v)", run.Status, types.RunCompleted, run.Error)
	}
	if run.AwaitingAgentSince != nil {
		t.Error("an unattended report run parked at a gate")
	}
	// The declared skip set is recorded and honored: document and push never ran.
	declared := map[types.StepName]bool{}
	for _, step := range run.SkippedStepNames() {
		declared[step] = true
	}
	if !declared[types.StepDocument] || !declared[types.StepPush] {
		t.Errorf("skipped_steps = %v, want the report mode's declaration", run.SkippedStepNames())
	}
	if push.ran {
		t.Error("the push step ran in a report-mode run")
	}
	if !review.ran {
		t.Fatal("the review step never ran")
	}
	if review.head != f.headSHA {
		t.Errorf("review saw head %q, want %q", review.head, f.headSHA)
	}
	// The intent came from the branch's commits and is non-authoritative.
	if !strings.Contains(review.intent, "add the widget cache") {
		t.Errorf("review intent = %q, want the branch's commit subject", review.intent)
	}
	if review.source != db.RunIntentSourceCommits {
		t.Errorf("intent source = %q, want %q", review.source, db.RunIntentSourceCommits)
	}
	// The findings the report is built from survived the unattended approval.
	steps, err := f.database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName != types.StepReview {
			continue
		}
		if step.Status != types.StepStatusCompleted {
			t.Errorf("review status = %q, want %q", step.Status, types.StepStatusCompleted)
		}
		if step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, "rev-1") {
			t.Error("review findings are not in the database")
		}
	}
}

func TestStartQARunDeclinesWhileAGateRunHoldsTheBranch(t *testing.T) {
	f := newConnectedFixture(t, nil)
	// A gate run is the author's push, which is newer truth about the branch and
	// has a person waiting on it.
	gateRun, err := f.database.InsertRun(f.repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.database.UpdateRunStatus(gateRun.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	_, err = f.mgr.StartQARun(context.Background(), QARunRequest{RepoID: f.repo.ID, Branch: "feature", Mode: types.RunModeReport})
	if err == nil {
		t.Fatal("a QA run superseded an active gate run")
	}
	if !strings.Contains(err.Error(), gateRun.ID) {
		t.Fatalf("error = %v, want it to name the gate run it declined to", err)
	}
	active, err := f.database.GetRun(gateRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != types.RunRunning {
		t.Fatalf("gate run status = %q, want it untouched", active.Status)
	}
}

func waitForRunToFinish(t *testing.T, database *db.DB, runID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		run, err := database.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Status != types.RunPending && run.Status != types.RunRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish within the timeout", runID)
}

// autoFixableStep reports findings the executor would normally hand to an
// auto-fix round.
type autoFixableStep struct {
	name  types.StepName
	calls int
	fixes int
}

func (s *autoFixableStep) Name() types.StepName { return s.name }

func (s *autoFixableStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls++
	if sctx.Fixing {
		s.fixes++
		return &pipeline.StepOutcome{}, nil
	}
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   true,
		Findings:      `{"findings":[{"id":"lint-1","severity":"warning","description":"unused import","action":"auto-fix"}],"summary":"1 issue"}`,
	}, nil
}

func TestReportModeNeverStartsAFixRound(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	// The shipped auto_fix defaults give lint three fix rounds. A read-only run
	// must still not run one: those commits could never be pushed, and a report
	// describing code the pipeline had just repaired would be a lie.
	lint := &autoFixableStep{name: types.StepLint}
	f := newConnectedFixture(t, func() []pipeline.Step {
		return []pipeline.Step{lint}
	})

	runID, err := f.mgr.StartQARun(context.Background(), QARunRequest{
		RepoID: f.repo.ID,
		Branch: "feature",
		Mode:   types.RunModeReport,
	})
	if err != nil {
		t.Fatalf("StartQARun: %v", err)
	}
	waitForRunToFinish(t, f.database, runID)

	if lint.fixes != 0 {
		t.Errorf("lint ran %d fix rounds, want 0", lint.fixes)
	}
	if lint.calls != 1 {
		t.Errorf("lint ran %d times, want exactly 1", lint.calls)
	}
	run, err := f.database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("status = %q, want %q (error: %v)", run.Status, types.RunCompleted, run.Error)
	}
	steps, err := f.database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := f.database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 {
		t.Fatalf("got %d rounds, want 1: the gate was approved, not fixed", len(rounds))
	}
	// The finding is preserved with the gate action that resolved it, which is
	// what the report reads.
	if steps[0].FindingsJSON == nil || !strings.Contains(*steps[0].FindingsJSON, "lint-1") {
		t.Error("the finding is not in the database")
	}
	if rounds[0].GateAction == nil || *rounds[0].GateAction != string(types.ActionApprove) {
		t.Errorf("gate_action = %v, want approve", rounds[0].GateAction)
	}
	if rounds[0].GateActionSource == nil || *rounds[0].GateActionSource != db.GateActionSourcePolicy {
		t.Errorf("gate_action_source = %v, want %q", rounds[0].GateActionSource, db.GateActionSourcePolicy)
	}
}
