package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// GateRequest describes an approval gate a run has reached: which step parked,
// what it found, and where in the fix loop the gate sits.
type GateRequest struct {
	Step         types.StepName
	Findings     types.Findings
	FindingsJSON string
	// Fixing reports that the round which produced these findings was itself a
	// fix round, so the gate is a fix_review rather than an initial gate.
	Fixing bool
	// AutoFixAttempts is how many auto-fix rounds this step has already spent.
	AutoFixAttempts int
	// Round is the number of the round that produced these findings.
	Round int
	// Recovered reports that this gate was restored from a run that was parked
	// when the daemon stopped, rather than reached during normal execution.
	Recovered bool
}

// GateDecision is a policy's answer to a gate. Reason is a short explanation
// recorded beside the action; it is bounded when persisted.
type GateDecision struct {
	Action     types.ApprovalAction
	FindingIDs []string
	Reason     string
}

// GatePolicy answers approval gates for a run that has no human and no driving
// agent. Returning ok=false leaves the gate parked exactly as it is today, so a
// policy is purely additive to the author-side gate, which never installs one.
//
// The executor consults a policy ONLY for steps that do not implement
// ApprovalGateReconciler. A reconcilable gate (today: the CI step) already has
// an external source of truth that resolves it without a human, and answering it
// from a policy would destroy the very mechanism that makes CI babysitting
// unattended.
type GatePolicy interface {
	ResolveGate(GateRequest) (GateDecision, bool)
}

// SetGatePolicy installs the policy that answers this run's approval gates
// unattended. A nil policy - the default, and the only thing an author-side gate
// run ever has - leaves every gate blocking for a human.
func (e *Executor) SetGatePolicy(policy GatePolicy) {
	e.gatePolicy = policy
}

// resolveGateByPolicy asks the installed policy to answer a gate, and returns
// the answer as the same approvalResponse the human path produces so both apply
// through applyApprovalAction.
//
// Callers must invoke this BEFORE any park side effect: before
// ParkStepForApproval writes awaiting_agent_since, before e.waiting is set, and
// before the approval wait is entered. A policy-answered gate is therefore never
// observable as parked, can never be answered over IPC, is never seen by
// recoverableParkedRuns, and can never race claimGateReconciliation.
func (e *Executor) resolveGateByPolicy(ctx context.Context, step Step, req GateRequest, roundID string, log func(string)) (approvalResponse, bool) {
	if e.gatePolicy == nil {
		return approvalResponse{}, false
	}
	// Cancellation outranks unattended resolution. A cancelled run is being
	// stopped - by daemon shutdown, a supersede, an abort, or the watcher's
	// max_run_duration - and answering its gate would advance the pipeline into
	// the steps that push, open a PR, or watch CI. Declining hands the gate to
	// the wait path, which returns the cancellation cause immediately.
	if ctx != nil && ctx.Err() != nil {
		return approvalResponse{}, false
	}
	if _, reconcilable := step.(ApprovalGateReconciler); reconcilable {
		return approvalResponse{}, false
	}
	decision, ok := e.gatePolicy.ResolveGate(req)
	if !ok {
		return approvalResponse{}, false
	}
	if decision.Action == "" {
		// A policy that answers with no action has decided nothing. Treat it as
		// a decline rather than letting an empty action reach the switch.
		slog.Warn("gate policy returned an empty action; preserving the gate", "step", req.Step)
		return approvalResponse{}, false
	}
	if roundID != "" {
		if dbErr := e.db.SetStepRoundGateAction(roundID, string(decision.Action), db.GateActionSourcePolicy, decision.Reason); dbErr != nil {
			slog.Warn("failed to record unattended gate action", "step", req.Step, "round", req.Round, "error", dbErr)
		}
	}
	if log != nil {
		message := fmt.Sprintf("unattended gate: %s", decision.Action)
		if decision.Reason != "" {
			message = fmt.Sprintf("unattended gate: %s (%s)", decision.Action, decision.Reason)
		}
		log(message)
	}
	slog.Info("gate answered by policy", "step", req.Step, "action", decision.Action, "reason", decision.Reason, "recovered", req.Recovered)
	return approvalResponse{action: decision.Action, findingIDs: decision.FindingIDs}, true
}

// gateFindings parses a gate's findings for a policy. Unreadable findings are
// presented as none, which is what every existing consumer of findings JSON
// does: a gate whose findings cannot be read offers no IDs to select.
func gateFindings(raw string) types.Findings {
	parsed, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return types.Findings{}
	}
	return parsed
}
