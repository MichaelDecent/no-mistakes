package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// recordingPolicy answers every gate with a fixed decision and records what it
// was asked. onResolve, when set, runs while the executor is inside the policy
// call, which is where reentrancy claims about the gate can be checked.
type recordingPolicy struct {
	decision  GateDecision
	answer    bool
	requests  atomic.Int64
	last      atomic.Pointer[GateRequest]
	onResolve func()
}

func (p *recordingPolicy) ResolveGate(req GateRequest) (GateDecision, bool) {
	p.requests.Add(1)
	stored := req
	p.last.Store(&stored)
	if p.onResolve != nil {
		p.onResolve()
	}
	return p.decision, p.answer
}

func approvingPolicy() *recordingPolicy {
	return &recordingPolicy{
		decision: GateDecision{Action: types.ActionApprove, Reason: "read-only run"},
		answer:   true,
	}
}

const blockingReviewFindings = `{"findings":[{"id":"rev-1","severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`

func TestGatePolicyNilPreservesBlockingGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepReview, blockingReviewFindings)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	// No policy installed: the gate parks for a human exactly as it always has.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get parked run: %v", err)
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("AwaitingAgentSince = nil, want the run parked at the gate")
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("respond: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestGatePolicyResolvedGateIsNeverObservableAsParked(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepReview, blockingReviewFindings)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	policy := approvingPolicy()
	exec.SetGatePolicy(policy)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if policy.requests.Load() != 1 {
		t.Fatalf("policy consulted %d times, want 1", policy.requests.Load())
	}
	finished, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	// A policy-resolved gate is answered before any park side effect, so the
	// parked marker is never written and no parked time is ever accumulated.
	if finished.AwaitingAgentSince != nil {
		t.Fatalf("AwaitingAgentSince = %v, want nil: a policy-resolved gate never parks", finished.AwaitingAgentSince)
	}
	if finished.ParkedMS != 0 {
		t.Fatalf("ParkedMS = %d, want 0", finished.ParkedMS)
	}
	if finished.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", finished.Status, types.RunCompleted)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("step status = %q, want %q", steps[0].Status, types.StepStatusCompleted)
	}
	// Approving unattended must not erase what the step found: the verdict a
	// report derives later reads these findings, not the step status.
	if steps[0].FindingsJSON == nil || *steps[0].FindingsJSON == "" {
		t.Fatal("step findings were dropped by the unattended approval")
	}
	if steps[0].DurationMS == nil || *steps[0].DurationMS < 0 {
		t.Fatalf("step duration = %v, want the execution time preserved", steps[0].DurationMS)
	}
}

func TestGatePolicyRequestDescribesTheGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepReview, blockingReviewFindings)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	policy := approvingPolicy()
	exec.SetGatePolicy(policy)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	req := policy.last.Load()
	if req == nil {
		t.Fatal("policy was never consulted")
	}
	if req.Step != types.StepReview {
		t.Errorf("Step = %q, want %q", req.Step, types.StepReview)
	}
	if req.FindingsJSON == "" || len(req.Findings.Items) != 1 || req.Findings.Items[0].ID != "rev-1" {
		t.Errorf("Findings = %+v / %q, want the gate's parsed findings", req.Findings, req.FindingsJSON)
	}
	if req.Fixing {
		t.Error("Fixing = true, want false on an initial gate")
	}
	if req.Round != 1 {
		t.Errorf("Round = %d, want 1", req.Round)
	}
	if req.Recovered {
		t.Error("Recovered = true, want false outside crash recovery")
	}
}

func TestGatePolicyRecordsItsDecisionOnTheRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepReview, blockingReviewFindings)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGatePolicy(&recordingPolicy{
		decision: GateDecision{Action: types.ActionApprove, Reason: "read-only QA run"},
		answer:   true,
	})

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if len(rounds) != 1 {
		t.Fatalf("got %d rounds, want 1", len(rounds))
	}
	got := rounds[0]
	if got.GateAction == nil || *got.GateAction != string(types.ActionApprove) {
		t.Errorf("gate_action = %v, want %q", got.GateAction, types.ActionApprove)
	}
	if got.GateActionSource == nil || *got.GateActionSource != db.GateActionSourcePolicy {
		t.Errorf("gate_action_source = %v, want %q", got.GateActionSource, db.GateActionSourcePolicy)
	}
	if got.GateActionReason == nil || *got.GateActionReason != "read-only QA run" {
		t.Errorf("gate_action_reason = %v, want the policy's reason", got.GateActionReason)
	}
}

func TestGatePolicyIsNotConsultedForAReconcilableGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(10*time.Millisecond, time.Second)
	policy := approvingPolicy()
	exec.SetGatePolicy(policy)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	// A reconcilable gate has its own external source of truth. Answering it
	// from a policy would destroy the mechanism that resolves it, so the gate
	// parks and waits for reconciliation instead.
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
	if policy.requests.Load() != 0 {
		t.Fatalf("policy consulted %d times for a reconcilable gate, want 0", policy.requests.Load())
	}
	step.resolved.Store(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
	if policy.requests.Load() != 0 {
		t.Fatalf("policy consulted %d times, want 0 for a gate resolved by reconciliation", policy.requests.Load())
	}
}

func TestGatePolicyDeclinedDecisionStillParks(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepReview, blockingReviewFindings)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	// A policy that declines to answer is exactly as blocking as no policy:
	// ok=false must leave the gate parked, never approve by omission.
	policy := &recordingPolicy{answer: false}
	exec.SetGatePolicy(policy)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get parked run: %v", err)
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("AwaitingAgentSince = nil, want a declined decision to leave the gate parked")
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("respond: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestGatePolicyDoesNotResolveAGateAfterCancellation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// The run is cancelled while the step is still working, so the gate is
	// reached with cancellation already pending.
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			cancel(errCancelledForTest)
			return &StepOutcome{NeedsApproval: true, Findings: blockingReviewFindings}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step, newPassStep(types.StepTest)}, nil)
	policy := approvingPolicy()
	exec.SetGatePolicy(policy)

	err := exec.Execute(ctx, run, repo, t.TempDir())
	if err == nil {
		t.Fatal("execute returned nil, want the cancellation to end the run")
	}
	// Approving here would advance a run that is being stopped into the steps
	// that push, open a PR, and watch CI.
	if policy.requests.Load() != 0 {
		t.Fatalf("policy consulted %d times after cancellation, want 0", policy.requests.Load())
	}
	steps, dbErr := database.GetStepsByRun(run.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if steps[1].Status == types.StepStatusCompleted {
		t.Fatal("the next step ran after cancellation")
	}
}

var errCancelledForTest = errors.New("cancelled for test")

func TestPolicyResolvedGateRejectsIPCRespond(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepReview, blockingReviewFindings)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	var respondErr atomic.Pointer[string]
	policy := approvingPolicy()
	// Called from inside the policy, so this observes the executor at the exact
	// moment the gate is being answered unattended. There is no window in which
	// an IPC responder can answer the same gate.
	policy.onResolve = func() {
		err := exec.Respond(types.StepReview, types.ActionAbort, nil)
		msg := "<nil>"
		if err != nil {
			msg = err.Error()
		}
		respondErr.Store(&msg)
	}
	exec.SetGatePolicy(policy)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := respondErr.Load()
	if got == nil {
		t.Fatal("policy was never consulted")
	}
	if *got != "no step awaiting approval" {
		t.Fatalf("Respond during a policy-resolved gate = %q, want it rejected", *got)
	}
	finished, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if finished.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want the policy's approval to stand", finished.Status)
	}
}

func TestGatePolicyFixDecisionRunsAFixRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	calls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			if calls == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: blockingReviewFindings}, nil
			}
			if !sctx.Fixing {
				t.Error("second round is not a fix round")
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGatePolicy(&recordingPolicy{
		decision: GateDecision{Action: types.ActionFix, FindingIDs: []string{"rev-1"}, Reason: "converging"},
		answer:   true,
	})

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if calls != 2 {
		t.Fatalf("step ran %d times, want 2 (initial gate then fix round)", calls)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if len(rounds) != 2 {
		t.Fatalf("got %d rounds, want 2", len(rounds))
	}
	if rounds[0].SelectedFindingIDs == nil {
		t.Error("the policy's selected finding ids were not recorded on the round it answered")
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != db.RoundSelectionSourcePolicy {
		t.Errorf("selection_source = %v, want %q: a policy fix is not a user fix", rounds[0].SelectionSource, db.RoundSelectionSourcePolicy)
	}
}

func TestResumePolicyResolvesARecoveredGateWithoutWaiting(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := blockingReviewFindings
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "cafe1234", 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	policy := approvingPolicy()
	exec.SetGatePolicy(policy)

	if err := exec.Resume(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("resume: %v", err)
	}

	req := policy.last.Load()
	if req == nil {
		t.Fatal("policy was not consulted for the recovered gate")
	}
	if !req.Recovered {
		t.Error("Recovered = false, want the policy told this gate came from crash recovery")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The run was already parked when the daemon died, so recovery must clear
	// the marker it inherited rather than leave the run reading as parked.
	if got.AwaitingAgentSince != nil {
		t.Fatalf("AwaitingAgentSince = %v, want the inherited park marker cleared", got.AwaitingAgentSince)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", got.Status, types.RunCompleted)
	}
}
