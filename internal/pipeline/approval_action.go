package pipeline

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// approvalContinuation tells executeStep's fix loop what an applied approval
// action means for the loop it was answered in.
type approvalContinuation int

const (
	// approvalContinueLoop re-executes the step: a fix round, or an action the
	// switch does not recognize, which is retried exactly as it always was.
	approvalContinueLoop approvalContinuation = iota
	// approvalGateResolved leaves the loop so the step completes at the done
	// label with the execution timing already accumulated.
	approvalGateResolved
	// approvalStepFinished means the action already wrote the step's terminal
	// state, so executeStep returns the carried values directly.
	approvalStepFinished
)

// approvalActionState is the executeStep loop state that applying an approval
// action reads. The two pointer fields are the only state it writes back into
// the loop; sctx carries the rest.
type approvalActionState struct {
	stepName       types.StepName
	findings       string
	roundNum       int
	executionMS    int64
	finalExitCode  int
	logPath        string
	currentRoundID string
	writeLog       func(string)

	// phaseStart restarts the execution clock when the gate resolves, so the
	// approval wait is never billed as step execution time.
	phaseStart *time.Time
	// nextTrigger labels the round the loop is about to run.
	nextTrigger *string
}

// approvalActionResult is what executeStep's loop does next, plus the values it
// returns when the action already finished the step.
type approvalActionResult struct {
	continuation  approvalContinuation
	skipRemaining bool
	err           error
}

// applyApprovalAction turns an answered approval gate into pipeline state: it
// is the single owner of what approve, skip, abort, and fix mean.
//
// It is deliberately one function rather than a switch inlined at its caller.
// A second copy of these cases is how skip/fix/abort semantics drift apart
// between the paths that reach them, and the only defence against that is for
// every path to execute this exact code.
func (e *Executor) applyApprovalAction(
	response approvalResponse,
	run *db.Run,
	repo *db.Repo,
	sr *db.StepResult,
	sctx *StepContext,
	state approvalActionState,
) approvalActionResult {
	switch response.action {
	case types.ActionApprove:
		// Approved - execution already frozen in executionMS, reset phaseStart
		// so the done label computes no additional elapsed.
		*state.phaseStart = time.Now()
		return approvalActionResult{continuation: approvalGateResolved}

	case types.ActionSkip:
		// Skip - mark step skipped and return (not an error)
		if err := e.db.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, state.finalExitCode, state.executionMS, state.logPath); err != nil {
			return approvalActionResult{
				continuation: approvalStepFinished,
				err:          fmt.Errorf("complete step %s (skip): %w", state.stepName, err),
			}
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, state.stepName, string(types.StepStatusSkipped), "", "", &state.executionMS)
		return approvalActionResult{continuation: approvalStepFinished}

	case types.ActionAbort:
		if dbErr := e.db.FailStep(sr.ID, "aborted by user", state.executionMS); dbErr != nil {
			slog.Warn("failed to mark step as failed in db", "step", state.stepName, "error", dbErr)
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, state.stepName, string(types.StepStatusFailed), "", "aborted by user", &state.executionMS)
		return approvalActionResult{
			continuation: approvalStepFinished,
			err:          fmt.Errorf("step %s: aborted by user", state.stepName),
		}

	case types.ActionFix:
		telemetry.Track("fix", e.fixTelemetryFields("user", state.stepName, selectedFindingCount(state.findings, response.findingIDs), 0))
		// Fix - mark step as fixing, resume execution timer, re-execute.
		*state.phaseStart = time.Now()
		selectedCount := selectedFindingCount(state.findings, response.findingIDs)
		state.writeLog(fmt.Sprintf("user-fix round starting after round %d (%d %s selected)", state.roundNum, selectedCount, pluralize(selectedCount, "finding", "findings")))
		if dbErr := e.db.UpdateStepStatus(sr.ID, types.StepStatusFixing); dbErr != nil {
			slog.Warn("failed to update step status in db", "step", state.stepName, "status", "fixing", "error", dbErr)
		}
		sctx.Fixing = true
		selectedFindings := filterFindingsJSON(state.findings, response.findingIDs)
		mergedFindings := mergeUserOverridesJSON(selectedFindings, response.instructions, response.addedFindings)
		sctx.PreviousFindings = mergedFindings
		*state.nextTrigger = "auto_fix"
		if state.currentRoundID != "" {
			allSelectedIDs := combineSelectedFindingIDs(response.findingIDs, mergedFindings)
			if idsJSON := marshalFindingIDs(allSelectedIDs); idsJSON != "" {
				if dbErr := e.db.SetStepRoundSelection(state.currentRoundID, &idsJSON, db.RoundSelectionSourceUser); dbErr != nil {
					slog.Warn("failed to record selected finding ids", "step", state.stepName, "round", state.roundNum, "error", dbErr)
				}
			}
			if mergedFindings != "" && mergedFindings != selectedFindings {
				merged := mergedFindings
				if dbErr := e.db.SetStepRoundUserFindings(state.currentRoundID, &merged); dbErr != nil {
					slog.Warn("failed to record user findings", "step", state.stepName, "round", state.roundNum, "error", dbErr)
				}
			}
		}
		e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, state.stepName, string(types.StepStatusFixing), "", "", nil)
		slog.Info("step fix requested, re-executing", "step", state.stepName)
		return approvalActionResult{continuation: approvalContinueLoop}
	}

	// An action the switch does not recognize arms nothing and touches nothing:
	// the loop re-executes the step, which is what it has always done.
	return approvalActionResult{continuation: approvalContinueLoop}
}
