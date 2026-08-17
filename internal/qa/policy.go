// Package qa holds the operator-side QA scope: validation of repositories
// connected by remote URL, run by configuration rather than by a person pushing
// a branch. Its consent boundary is the operator's declarative configuration,
// which is narrower than the author gate's - see VISION.md and
// docs/src/content/docs/concepts/qa.md.
package qa

import (
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PolicyFor returns the gate policy that answers a run's approval gates when
// nobody is there to answer them, or nil to leave every gate blocking.
//
// It returns nil for RunKindGate in EVERY mode, and that is the load-bearing
// half of this function: the author-side gate parks for a human because a person
// pushed the branch and is there to decide. A run started by that push must
// never acquire a policy, whatever mode value its row happens to carry.
//
// A QA run has no such person. Its gates are answered in-process:
//
//   - report and comment validate read-only, so their gates are approved and
//     the findings are published afterwards from the persisted rows;
//   - fix-pr is the one mode that writes code, so its gates converge through
//     the same rule an attended `axi run --yes` uses.
//
// An unrecognized mode gets nil - no policy - so its gates park rather than
// resolve on a guess. That stalls the run into the watcher's max_run_duration
// instead of approving or fixing anything, which is the only fail-safe direction
// for a value that reached the database from an unknown writer. It is unreachable
// in practice because types.NormalizeRunMode resolves a stored value to a
// declared mode before a run is ever started.
func PolicyFor(kind types.RunKind, mode types.RunMode) pipeline.GatePolicy {
	if kind != types.RunKindQA {
		return nil
	}
	switch mode {
	case types.RunModeReport, types.RunModeComment:
		return &readOnlyGatePolicy{}
	case types.RunModeFixPR:
		return &convergingGatePolicy{}
	default:
		return nil
	}
}

// readOnlyGatePolicy approves every gate it is asked about.
//
// Approve, and deliberately none of the alternatives:
//
//   - not fix: a fix round runs an agent that edits files and commits them, and
//     a read-only run has nowhere to put those commits;
//   - not skip: "skipped" erases the fact that the step ran and produced
//     findings, and every consumer reads it as "not checked";
//   - not abort: that would fail the run at its first blocking finding, so a
//     report would cover review and nothing after it.
//
// Approving loses nothing: the findings stay on the step and its round, and the
// run's verdict is derived from those persisted findings, never from the step
// status.
type readOnlyGatePolicy struct{}

func (p *readOnlyGatePolicy) ResolveGate(req pipeline.GateRequest) (pipeline.GateDecision, bool) {
	return pipeline.GateDecision{
		Action: types.ActionApprove,
		Reason: "read-only QA run: findings recorded, no fix round",
	}, true
}

// convergingGatePolicy answers a code-writing QA run's gates by delegating to
// types.ResolveConvergingGate, the single owner of the fix-once-then-approve
// rule that `axi run --yes` and the TUI's yolo mode also use. A second copy of
// that judgement here would drift the first time either was adjusted.
//
// The fix-once shape is the loop guard: without it, a finding that no fix can
// clear cycles review -> fix -> fix_review -> fix forever, spending a full agent
// invocation per turn.
type convergingGatePolicy struct{}

func (p *convergingGatePolicy) ResolveGate(req pipeline.GateRequest) (pipeline.GateDecision, bool) {
	// A gate reached after any fix round for this step counts as already fixed.
	// Both signals are passed through because they answer different questions:
	// Fixing says this gate IS a fix review, AutoFixAttempts says a fix was
	// already spent before the gate was reached.
	action, ids := types.ResolveConvergingGate(req.Findings, req.AutoFixAttempts > 0, req.Fixing)
	reason := "converging QA gate"
	if action == types.ActionFix {
		reason = "converging QA gate: fixing every identified finding once"
	}
	return pipeline.GateDecision{Action: action, FindingIDs: ids, Reason: reason}, true
}
