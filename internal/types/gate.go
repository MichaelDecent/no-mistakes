package types

// ResolveConvergingGate is the single owner of the rule that answers an approval
// gate so a run converges instead of looping.
//
// The rule: a gate carrying actionable findings is fixed with every identified
// finding selected, EXCEPT once this step has already been fixed once (or is
// already showing a fix for review), when it is approved instead. Gates with no
// findings, only informational "no-op" findings, or actionable findings that
// carry no IDs (which a fix would resolve to zero selections) are approved.
//
// The exception is the loop guard, and it is the whole reason this is one
// function rather than an inline conditional. Without it, a finding that no fix
// can clear cycles review -> fix -> fix_review -> fix indefinitely, spending a
// full agent invocation per turn.
//
// This lives in internal/types, with no dependencies, because two callers need
// it and VISION.md requires that "when two mechanisms would judge the same
// question, they are reconciled into one, never stacked on top of each other":
//
//   - internal/cli, for `axi run --yes` and the TUI's yolo mode, where an
//     attended driver answers gates over IPC.
//   - the daemon's unattended QA gate policy, which answers the same gates
//     in-process for a run that has no driver.
//
// A second copy in the daemon would be a second mechanism judging the same
// question, and the two would drift the first time either was adjusted.
func ResolveConvergingGate(findings Findings, alreadyFixed, inFixReview bool) (ApprovalAction, []string) {
	if alreadyFixed || inFixReview {
		return ActionApprove, nil
	}
	if !HasActionableFindings(findings) {
		return ActionApprove, nil
	}
	ids := make([]string, 0, len(findings.Items))
	for _, f := range findings.Items {
		if f.ID != "" {
			ids = append(ids, f.ID)
		}
	}
	if len(ids) == 0 {
		return ActionApprove, nil
	}
	return ActionFix, ids
}
