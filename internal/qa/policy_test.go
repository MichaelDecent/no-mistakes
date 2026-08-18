package qa

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const blockingFindings = `{"findings":[{"id":"rev-1","severity":"warning","description":"needs a human","action":"ask-user"},{"id":"rev-2","severity":"error","description":"fix me","action":"auto-fix"}],"summary":"2 issues"}`

func mustParse(t *testing.T, raw string) types.Findings {
	t.Helper()
	parsed, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	return parsed
}

func TestPolicyForIsNeverInstalledForGateRuns(t *testing.T) {
	// The author-side gate blocks for a human in every mode. A run that is not
	// a QA run must never acquire a policy, whatever mode its row carries.
	for _, mode := range types.AllRunModes() {
		if got := PolicyFor(types.RunKindGate, mode); got != nil {
			t.Errorf("PolicyFor(gate, %s) = %T, want nil", mode, got)
		}
	}
	if got := PolicyFor(types.RunKindGate, types.RunMode("something-else")); got != nil {
		t.Errorf("PolicyFor(gate, unknown) = %T, want nil", got)
	}
}

func TestPolicyForIsExhaustiveOverQAModes(t *testing.T) {
	// Fails when a mode is added without deciding how its gates are answered.
	want := map[types.RunMode]string{
		types.RunModeReport:  "*qa.readOnlyGatePolicy",
		types.RunModeComment: "*qa.readOnlyGatePolicy",
		types.RunModeFixPR:   "*qa.convergingGatePolicy",
	}
	for _, mode := range types.AllRunModes() {
		expected, declared := want[mode]
		if !declared {
			t.Fatalf("mode %q has no declared gate policy; decide it deliberately", mode)
		}
		got := PolicyFor(types.RunKindQA, mode)
		if got == nil {
			t.Fatalf("PolicyFor(qa, %s) = nil, want %s", mode, expected)
		}
		if name := fmt.Sprintf("%T", got); name != expected {
			t.Errorf("PolicyFor(qa, %s) = %s, want %s", mode, name, expected)
		}
	}
}

func TestPolicyForUnknownQAModeParksInsteadOfGuessing(t *testing.T) {
	// Unreachable in practice - NormalizeRunMode collapses unknown values
	// before a run starts - but the fail-safe direction matters: no policy
	// means the gate parks, which can never write code or approve anything.
	if got := PolicyFor(types.RunKindQA, types.RunMode("fix-everything")); got != nil {
		t.Errorf("PolicyFor(qa, unknown) = %T, want nil", got)
	}
}

func TestReadOnlyPolicyApprovesEveryGateAndSelectsNothing(t *testing.T) {
	policy := PolicyFor(types.RunKindQA, types.RunModeReport)
	cases := []struct {
		name string
		req  pipeline.GateRequest
	}{
		{"blocking findings", pipeline.GateRequest{Step: types.StepReview, Findings: mustParse(t, blockingFindings), FindingsJSON: blockingFindings, Round: 1}},
		{"no findings", pipeline.GateRequest{Step: types.StepTest, Round: 1}},
		{"fix review", pipeline.GateRequest{Step: types.StepReview, Findings: mustParse(t, blockingFindings), FindingsJSON: blockingFindings, Fixing: true, Round: 2}},
		{"recovered", pipeline.GateRequest{Step: types.StepLint, Recovered: true, Round: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision, ok := policy.ResolveGate(tc.req)
			if !ok {
				t.Fatal("ResolveGate declined, want a read-only run to answer every gate it is asked")
			}
			// Approve, never fix (a fix round writes code), never skip (skipped
			// erases the fact the step ran), never abort (that would end the run
			// at the first blocking finding and truncate the report).
			if decision.Action != types.ActionApprove {
				t.Errorf("Action = %q, want %q", decision.Action, types.ActionApprove)
			}
			if len(decision.FindingIDs) != 0 {
				t.Errorf("FindingIDs = %v, want none", decision.FindingIDs)
			}
			if decision.Reason == "" {
				t.Error("Reason is empty, want the recorded gate action to explain itself")
			}
		})
	}
}

func TestConvergingPolicyFixesOnceThenApproves(t *testing.T) {
	policy := PolicyFor(types.RunKindQA, types.RunModeFixPR)

	first, ok := policy.ResolveGate(pipeline.GateRequest{
		Step:         types.StepReview,
		Findings:     mustParse(t, blockingFindings),
		FindingsJSON: blockingFindings,
		Round:        1,
	})
	if !ok || first.Action != types.ActionFix {
		t.Fatalf("first gate = %q (ok=%v), want %q", first.Action, ok, types.ActionFix)
	}
	if len(first.FindingIDs) != 2 {
		t.Fatalf("FindingIDs = %v, want both findings selected", first.FindingIDs)
	}

	// The fix round's own gate (fix_review) approves, which is the loop guard:
	// a finding no fix can clear would otherwise cycle fix -> fix_review -> fix.
	second, ok := policy.ResolveGate(pipeline.GateRequest{
		Step:         types.StepReview,
		Findings:     mustParse(t, blockingFindings),
		FindingsJSON: blockingFindings,
		Fixing:       true,
		Round:        2,
	})
	if !ok || second.Action != types.ActionApprove {
		t.Fatalf("fix-review gate = %q (ok=%v), want %q", second.Action, ok, types.ActionApprove)
	}

	// Same rule when the step already spent an auto-fix round before parking.
	third, ok := policy.ResolveGate(pipeline.GateRequest{
		Step:            types.StepReview,
		Findings:        mustParse(t, blockingFindings),
		FindingsJSON:    blockingFindings,
		AutoFixAttempts: 1,
		Round:           2,
	})
	if !ok || third.Action != types.ActionApprove {
		t.Fatalf("already-auto-fixed gate = %q (ok=%v), want %q", third.Action, ok, types.ActionApprove)
	}
}

func TestConvergingPolicyDelegatesToTypesResolveConvergingGate(t *testing.T) {
	// Drift check: the daemon's unattended policy and `axi run --yes` must
	// answer the same gate identically, which holds only while both delegate to
	// the one owner of the rule.
	policy := PolicyFor(types.RunKindQA, types.RunModeFixPR)
	raws := []string{
		blockingFindings,
		`{"findings":[],"summary":"clean"}`,
		`{"findings":[{"severity":"warning","description":"no id","action":"auto-fix"}]}`,
		`{"findings":[{"id":"n-1","severity":"info","description":"nothing to do","action":"no-op"}]}`,
	}
	for _, raw := range raws {
		for _, fixing := range []bool{false, true} {
			for _, alreadyFixed := range []bool{false, true} {
				parsed := mustParse(t, raw)
				attempts := 0
				if alreadyFixed {
					attempts = 1
				}
				decision, ok := policy.ResolveGate(pipeline.GateRequest{
					Step:            types.StepReview,
					Findings:        parsed,
					FindingsJSON:    raw,
					Fixing:          fixing,
					AutoFixAttempts: attempts,
				})
				if !ok {
					t.Fatalf("ResolveGate declined for %q", raw)
				}
				wantAction, wantIDs := types.ResolveConvergingGate(parsed, alreadyFixed, fixing)
				if decision.Action != wantAction {
					t.Errorf("action for %q (fixing=%v fixed=%v) = %q, want %q", raw, fixing, alreadyFixed, decision.Action, wantAction)
				}
				if len(decision.FindingIDs) != len(wantIDs) {
					t.Errorf("ids for %q = %v, want %v", raw, decision.FindingIDs, wantIDs)
				}
			}
		}
	}
}

func TestConvergingPolicyApprovesUnreadableFindings(t *testing.T) {
	// Matching the CLI driver: findings that cannot be read offer no IDs to
	// select, so a fix would resolve to zero selections and loop.
	policy := PolicyFor(types.RunKindQA, types.RunModeFixPR)
	decision, ok := policy.ResolveGate(pipeline.GateRequest{
		Step:         types.StepReview,
		FindingsJSON: "{not json",
	})
	if !ok || decision.Action != types.ActionApprove {
		t.Fatalf("unreadable findings = %q (ok=%v), want %q", decision.Action, ok, types.ActionApprove)
	}
}

// --- integration with the real executor ---

type gatingStep struct {
	name     types.StepName
	findings string
	calls    int
}

func (s *gatingStep) Name() types.StepName { return s.name }

func (s *gatingStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls++
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: s.findings}, nil
}

func setupRun(t *testing.T) (*db.DB, *paths.Paths, *db.Run, *db.Repo) {
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
	repo, err := database.InsertRepoWithID("qarepo", "/tmp/qa-repo", "https://github.com/acme/api", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "main", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	return database, p, run, repo
}

func TestReportModeRunNeverSetsAwaitingAgentSince(t *testing.T) {
	database, p, run, repo := setupRun(t)
	step := &gatingStep{name: types.StepReview, findings: blockingFindings}
	exec := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{step}, nil)
	exec.SetGatePolicy(PolicyFor(types.RunKindQA, types.RunModeReport))

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AwaitingAgentSince != nil {
		t.Fatalf("AwaitingAgentSince = %v, want nil: an unattended report run has no agent to wait for", got.AwaitingAgentSince)
	}
	if got.ParkedMS != 0 {
		t.Fatalf("ParkedMS = %d, want 0", got.ParkedMS)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("status = %q, want %q", got.Status, types.RunCompleted)
	}
	if step.calls != 1 {
		t.Fatalf("step ran %d times, want 1: a read-only run never starts a fix round", step.calls)
	}
}

func TestReportModeApproveKeepsStepFindingsAndRoundIntact(t *testing.T) {
	database, p, run, repo := setupRun(t)
	step := &gatingStep{name: types.StepReview, findings: blockingFindings}
	exec := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{step}, nil)
	exec.SetGatePolicy(PolicyFor(types.RunKindQA, types.RunModeReport))

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("got %d steps, want 1", len(steps))
	}
	// The verdict a report derives later is built from these rows. Approving
	// unattended must leave them exactly as the step produced them.
	if steps[0].Status != types.StepStatusCompleted {
		t.Errorf("step status = %q, want %q", steps[0].Status, types.StepStatusCompleted)
	}
	if steps[0].FindingsJSON == nil || *steps[0].FindingsJSON == "" {
		t.Fatal("step findings were dropped")
	}
	parsed, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatalf("parse stored findings: %v", err)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("stored %d findings, want 2", len(parsed.Items))
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 {
		t.Fatalf("got %d rounds, want 1", len(rounds))
	}
	if rounds[0].FindingsJSON == nil || *rounds[0].FindingsJSON == "" {
		t.Fatal("round findings were dropped")
	}
	if rounds[0].SelectedFindingIDs != nil {
		t.Errorf("selected_finding_ids = %v, want none: nothing was selected for a fix", rounds[0].SelectedFindingIDs)
	}
	if rounds[0].GateActionSource == nil || *rounds[0].GateActionSource != db.GateActionSourcePolicy {
		t.Errorf("gate_action_source = %v, want %q", rounds[0].GateActionSource, db.GateActionSourcePolicy)
	}
}

// autoFixableGateStep mimics the rebase step's conflict outcome: an approval
// gate that also declares itself auto-fixable.
type autoFixableGateStep struct {
	name     types.StepName
	findings string
	calls    int
	fixing   int
}

func (s *autoFixableGateStep) Name() types.StepName { return s.name }

func (s *autoFixableGateStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls++
	if sctx.Fixing {
		s.fixing++
		return &pipeline.StepOutcome{}, nil
	}
	return &pipeline.StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: s.findings}, nil
}

// headRecordingStep records the run head the steps after the gate observe.
type headRecordingStep struct {
	name types.StepName
	head string
	ran  bool
}

func (s *headRecordingStep) Name() types.StepName { return s.name }

func (s *headRecordingStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.ran = true
	s.head = sctx.Run.HeadSHA
	return &pipeline.StepOutcome{}, nil
}

func TestReportModeApprovesRebaseConflictAndValidatesUnrebasedHead(t *testing.T) {
	database, p, run, repo := setupRun(t)
	conflict := `{"findings":[{"severity":"warning","file":"shared.txt","description":"merge conflict rebasing onto origin/feature"}],"summary":"conflict rebasing onto origin/feature"}`
	rebase := &autoFixableGateStep{name: types.StepRebase, findings: conflict}
	review := &headRecordingStep{name: types.StepReview}

	// A read-only QA run carries no auto-fix budget (the daemon zeroes it), so a
	// rebase conflict reaches the gate instead of being handed to a fix agent.
	exec := pipeline.NewExecutor(database, p, &config.Config{}, nil, []pipeline.Step{rebase, review}, nil)
	exec.SetGatePolicy(PolicyFor(types.RunKindQA, types.RunModeReport))

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if rebase.fixing != 0 {
		t.Errorf("rebase ran %d fix rounds, want 0: a read-only run never resolves conflicts", rebase.fixing)
	}
	if rebase.calls != 1 {
		t.Errorf("rebase step ran %d times, want 1", rebase.calls)
	}
	// The conflict is recorded and validation continues on the unrebased head,
	// rather than the run stopping at the first thing it could not rebase.
	if !review.ran {
		t.Fatal("review never ran: the approved conflict gate must not end the run")
	}
	if review.head != run.HeadSHA {
		t.Errorf("review saw head %q, want the unrebased head %q", review.head, run.HeadSHA)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].Status != types.StepStatusCompleted {
		t.Errorf("rebase status = %q, want %q", steps[0].Status, types.StepStatusCompleted)
	}
	if steps[0].FindingsJSON == nil || !strings.Contains(*steps[0].FindingsJSON, "merge conflict") {
		t.Error("the conflict finding was not preserved for the report")
	}
	finished, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", finished.Status, types.RunCompleted)
	}
}
