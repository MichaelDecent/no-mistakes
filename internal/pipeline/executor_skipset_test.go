package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestValidateRecoveredRunAcceptsQARunWithSkippedSteps(t *testing.T) {
	database, _, run, _ := setupTest(t)
	// The real shape of a parked read-only QA run: document was skipped by the
	// run's declaration, the lint gate parked, and the steps after it are still
	// pending. Recovery validation must accept a skipped step ahead of the gate.
	steps := []Step{
		newPassStep(types.StepReview),
		newPassStep(types.StepDocument),
		newApprovalStep(types.StepLint, blockingReviewFindings),
		newPassStep(types.StepPush),
	}
	findings := blockingReviewFindings
	for _, step := range steps {
		sr, err := database.InsertStepResult(run.ID, step.Name())
		if err != nil {
			t.Fatal(err)
		}
		switch step.Name() {
		case types.StepReview:
			if err := database.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		case types.StepDocument:
			if err := database.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
				t.Fatal(err)
			}
		case types.StepLint:
			if err := database.StartStep(sr.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepFindings(sr.ID, findings); err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 10); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatusWithDuration(sr.ID, types.StepStatusAwaitingApproval, 10); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := ValidateRecoveredRun(database, reloaded, steps); err != nil {
		t.Fatalf("ValidateRecoveredRun rejected a QA run carrying skipped steps: %v", err)
	}
}

func TestRecoveredRunRemainderStillHonorsItsSkipSet(t *testing.T) {
	database, p, run, repo := setupTest(t)
	pushStep := newPassStep(types.StepPush)
	prStep := newPassStep(types.StepPR)
	steps := []Step{
		newApprovalStep(types.StepReview, blockingReviewFindings),
		newPassStep(types.StepTest),
		pushStep,
		prStep,
	}

	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	findings := blockingReviewFindings
	for _, step := range steps {
		sr, err := database.InsertStepResult(run.ID, step.Name())
		if err != nil {
			t.Fatal(err)
		}
		if step.Name() != types.StepReview {
			continue
		}
		if err := database.StartStep(sr.ID); err != nil {
			t.Fatal(err)
		}
		if err := database.SetStepFindings(sr.ID, findings); err != nil {
			t.Fatal(err)
		}
		if _, err := database.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, "cafe1234", 10); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateStepStatusWithDuration(sr.ID, types.StepStatusAwaitingApproval, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	// The declaration a read-only run started with, reinstalled by recovery.
	exec.SetSkippedSteps([]types.StepName{types.StepPush, types.StepPR})
	exec.SetGatePolicy(approvingPolicy())

	if err := exec.Resume(context.Background(), reloaded, repo, t.TempDir()); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// Without the skip check on the recovered remainder, a report-mode run
	// resumed after a crash pushes its branch and opens a PR.
	if pushStep.callCount() != 0 {
		t.Errorf("push step ran %d times after recovery, want 0", pushStep.callCount())
	}
	if prStep.callCount() != 0 {
		t.Errorf("pr step ran %d times after recovery, want 0", prStep.callCount())
	}
	stepRows, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range stepRows {
		switch row.StepName {
		case types.StepPush, types.StepPR:
			if row.Status != types.StepStatusSkipped {
				t.Errorf("%s = %q, want %q", row.StepName, row.Status, types.StepStatusSkipped)
			}
		case types.StepTest:
			if row.Status != types.StepStatusCompleted {
				t.Errorf("%s = %q, want %q: a skip declaration must not stop the steps it does not name", row.StepName, row.Status, types.StepStatusCompleted)
			}
		}
	}
	finished, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", finished.Status, types.RunCompleted)
	}
}

func TestRunSkippedStepsColumnMatchesSkippedStepResults(t *testing.T) {
	database, p, _, repo := setupTest(t)
	declared := types.SkipSetFor(types.RunKindQA, types.RunModeReport)
	run, err := database.InsertRunWithScope(repo.ID, "main", "head", "base", nil, db.RunScope{
		Kind:         types.RunKindQA,
		Mode:         types.RunModeReport,
		SkippedSteps: declared,
	})
	if err != nil {
		t.Fatal(err)
	}

	steps := []Step{
		newPassStep(types.StepReview),
		newPassStep(types.StepTest),
		newPassStep(types.StepDocument),
		newPassStep(types.StepLint),
		newPassStep(types.StepPush),
		newPassStep(types.StepPR),
		newPassStep(types.StepCI),
	}
	exec := NewExecutor(database, p, nil, nil, steps, nil)
	// The executor is configured from the same declaration the row recorded, so
	// the row and the step results cannot disagree about what this run ran.
	exec.SetSkippedSteps(run.SkippedStepNames())

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	rows, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[types.StepName]types.StepStatus{}
	for _, row := range rows {
		statuses[row.StepName] = row.Status
	}
	for _, step := range declared {
		if statuses[step] != types.StepStatusSkipped {
			t.Errorf("declared skip %s = %q, want %q", step, statuses[step], types.StepStatusSkipped)
		}
	}
	// And nothing outside the declaration was skipped: a read-only run still
	// validates. This is the direction that would break silently if the two ever
	// drifted apart.
	declaredSet := map[types.StepName]bool{}
	for _, step := range declared {
		declaredSet[step] = true
	}
	for name, status := range statuses {
		if !declaredSet[name] && status == types.StepStatusSkipped {
			t.Errorf("step %s was skipped but never declared", name)
		}
	}
}
