package pipeline

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// applyApprovalAction is the single place where an answered approval gate turns
// into pipeline state. Its contract is pinned here directly, so a second caller
// (an unattended policy) provably executes the same code as the human path
// rather than a second copy of the switch that can drift from it.

func newApprovalActionFixture(t *testing.T) (*Executor, *db.Run, *db.Repo, *db.StepResult, *StepContext) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	sr, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step result: %v", err)
	}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	sctx := &StepContext{Run: run, Repo: repo, DB: database, StepResultID: sr.ID}
	return exec, run, repo, sr, sctx
}

func newApprovalActionState(phaseStart *time.Time, nextTrigger *string) approvalActionState {
	return approvalActionState{
		stepName:    types.StepReview,
		findings:    `{"findings":[{"id":"f1","severity":"warning","description":"needs a look","action":"ask-user"}]}`,
		roundNum:    1,
		executionMS: 42,
		phaseStart:  phaseStart,
		nextTrigger: nextTrigger,
		writeLog:    func(string) {},
	}
}

func TestApplyApprovalActionApproveResolvesTheGateWithoutTouchingTheStep(t *testing.T) {
	exec, run, repo, sr, sctx := newApprovalActionFixture(t)
	phaseStart := time.Now().Add(-time.Hour)
	nextTrigger := "initial"

	result := exec.applyApprovalAction(approvalResponse{action: types.ActionApprove}, run, repo, sr, sctx,
		newApprovalActionState(&phaseStart, &nextTrigger))

	if result.continuation != approvalGateResolved {
		t.Fatalf("continuation = %v, want approvalGateResolved", result.continuation)
	}
	if result.err != nil {
		t.Fatalf("err = %v, want nil", result.err)
	}
	// The approval path resets the phase clock so the completion at the done
	// label adds no approval-wait time to the step's execution total.
	if time.Since(phaseStart) > time.Minute {
		t.Fatalf("phaseStart was not reset: %s elapsed", time.Since(phaseStart))
	}
	stored, err := exec.db.GetStepResult(sr.ID)
	if err != nil {
		t.Fatalf("get step result: %v", err)
	}
	if stored.Status != types.StepStatusPending {
		t.Fatalf("status = %q, want the step left untouched for the done label to complete", stored.Status)
	}
}

func TestApplyApprovalActionSkipFinishesTheStepAsSkipped(t *testing.T) {
	exec, run, repo, sr, sctx := newApprovalActionFixture(t)
	phaseStart := time.Now()
	nextTrigger := "initial"

	result := exec.applyApprovalAction(approvalResponse{action: types.ActionSkip}, run, repo, sr, sctx,
		newApprovalActionState(&phaseStart, &nextTrigger))

	if result.continuation != approvalStepFinished {
		t.Fatalf("continuation = %v, want approvalStepFinished", result.continuation)
	}
	if result.err != nil {
		t.Fatalf("err = %v, want nil", result.err)
	}
	if result.skipRemaining {
		t.Fatal("skipRemaining = true, want false: skipping one step never skips the rest")
	}
	stored, err := exec.db.GetStepResult(sr.ID)
	if err != nil {
		t.Fatalf("get step result: %v", err)
	}
	if stored.Status != types.StepStatusSkipped {
		t.Fatalf("status = %q, want %q", stored.Status, types.StepStatusSkipped)
	}
}

func TestApplyApprovalActionAbortFailsTheStepAndTheRun(t *testing.T) {
	exec, run, repo, sr, sctx := newApprovalActionFixture(t)
	phaseStart := time.Now()
	nextTrigger := "initial"

	result := exec.applyApprovalAction(approvalResponse{action: types.ActionAbort}, run, repo, sr, sctx,
		newApprovalActionState(&phaseStart, &nextTrigger))

	if result.continuation != approvalStepFinished {
		t.Fatalf("continuation = %v, want approvalStepFinished", result.continuation)
	}
	if result.err == nil {
		t.Fatal("err = nil, want the abort error that fails the run")
	}
	stored, err := exec.db.GetStepResult(sr.ID)
	if err != nil {
		t.Fatalf("get step result: %v", err)
	}
	if stored.Status != types.StepStatusFailed {
		t.Fatalf("status = %q, want %q", stored.Status, types.StepStatusFailed)
	}
}

func TestApplyApprovalActionFixArmsTheNextRoundAndLoops(t *testing.T) {
	exec, run, repo, sr, sctx := newApprovalActionFixture(t)
	phaseStart := time.Now().Add(-time.Hour)
	nextTrigger := "initial"
	state := newApprovalActionState(&phaseStart, &nextTrigger)

	round, err := exec.db.InsertStepRound(sr.ID, state.roundNum, "initial", &state.findings, nil, 0)
	if err != nil {
		t.Fatalf("insert step round: %v", err)
	}
	state.currentRoundID = round.ID

	result := exec.applyApprovalAction(
		approvalResponse{action: types.ActionFix, findingIDs: []string{"f1"}},
		run, repo, sr, sctx, state)

	if result.continuation != approvalContinueLoop {
		t.Fatalf("continuation = %v, want approvalContinueLoop", result.continuation)
	}
	if result.err != nil {
		t.Fatalf("err = %v, want nil", result.err)
	}
	if !sctx.Fixing {
		t.Fatal("sctx.Fixing = false, want the next round to run as a fix round")
	}
	if sctx.PreviousFindings == "" {
		t.Fatal("sctx.PreviousFindings is empty, want the selected findings handed to the fix round")
	}
	if nextTrigger != "auto_fix" {
		t.Fatalf("nextTrigger = %q, want %q", nextTrigger, "auto_fix")
	}
	// The fix round is execution, not approval wait, so the phase clock restarts.
	if time.Since(phaseStart) > time.Minute {
		t.Fatalf("phaseStart was not reset: %s elapsed", time.Since(phaseStart))
	}
	stored, err := exec.db.GetStepResult(sr.ID)
	if err != nil {
		t.Fatalf("get step result: %v", err)
	}
	if stored.Status != types.StepStatusFixing {
		t.Fatalf("status = %q, want %q", stored.Status, types.StepStatusFixing)
	}
	rounds, err := exec.db.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatalf("get rounds: %v", err)
	}
	if len(rounds) != 1 || rounds[0].SelectedFindingIDs == nil {
		t.Fatalf("selected finding ids were not recorded on the round: %+v", rounds)
	}
}

func TestApplyApprovalActionUnrecognizedActionRetriesTheStepUnchanged(t *testing.T) {
	exec, run, repo, sr, sctx := newApprovalActionFixture(t)
	phaseStart := time.Now()
	nextTrigger := "initial"

	result := exec.applyApprovalAction(approvalResponse{action: types.ApprovalAction("teleport")}, run, repo, sr, sctx,
		newApprovalActionState(&phaseStart, &nextTrigger))

	// Pre-existing behaviour: an action the switch does not recognize falls
	// through and the loop re-executes the step. Preserved verbatim here so the
	// refactor cannot quietly turn it into a failure or a completion.
	if result.continuation != approvalContinueLoop {
		t.Fatalf("continuation = %v, want approvalContinueLoop", result.continuation)
	}
	if result.err != nil {
		t.Fatalf("err = %v, want nil", result.err)
	}
	if sctx.Fixing {
		t.Fatal("sctx.Fixing = true, want an unrecognized action to arm nothing")
	}
	if nextTrigger != "initial" {
		t.Fatalf("nextTrigger = %q, want it left alone", nextTrigger)
	}
	stored, err := exec.db.GetStepResult(sr.ID)
	if err != nil {
		t.Fatalf("get step result: %v", err)
	}
	if stored.Status != types.StepStatusPending {
		t.Fatalf("status = %q, want the step row untouched", stored.Status)
	}
}
