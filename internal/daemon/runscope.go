package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// runScope is the kind and mode a run is created under. The zero value is not
// used: every caller states its scope, and gateRunScope is the name of the one
// the push path always has.
type runScope struct {
	kind types.RunKind
	mode types.RunMode
}

// gateRunScope is the author-side gate: a person pushed a branch, so the run may
// do everything the pipeline can do, and nothing about configuration may narrow
// or widen it.
func gateRunScope() runScope {
	return runScope{kind: types.RunKindGate, mode: types.RunModeFixPR}
}

func (s runScope) isQA() bool { return s.kind == types.RunKindQA }

// runScopeFromRun reads a run's declared scope back from its row. It goes
// through the normalizers, which own what a NULL or unrecognized stored value
// means, so nothing here develops its own opinion: every row written before QA
// existed reads back as the gate run it was.
func runScopeFromRun(run *db.Run) runScope {
	if run == nil {
		return gateRunScope()
	}
	return runScope{
		kind: types.NormalizeRunKind(run.RunKind),
		mode: types.NormalizeRunMode(run.RunMode),
	}
}

// dbScope converts the scope into the columns a run row records, including the
// skip declaration the run is started with.
func (s runScope) dbScope(skips []types.StepName) db.RunScope {
	if s.kind == types.RunKindGate {
		// A gate run records nothing new, so its row stays exactly what it was
		// before QA existed. Its skips are the explicit per-run ones a person
		// passed, which have always lived only on the executor.
		return db.RunScope{}
	}
	return db.RunScope{Kind: s.kind, Mode: s.mode, SkippedSteps: skips}
}

// effectiveSkipSteps combines the explicit per-run skips a caller passed with
// the steps this scope declares it will not run.
//
// The union direction matters: a mode's declaration can only ADD skips to what
// the caller asked for, and for a gate run SkipSetFor contributes nothing at
// all, so a standing rule can never skip a step on an author's behalf.
func effectiveSkipSteps(scope runScope, explicit []types.StepName) []types.StepName {
	declared := types.SkipSetFor(scope.kind, scope.mode)
	if len(declared) == 0 {
		return explicit
	}
	seen := make(map[types.StepName]bool, len(explicit)+len(declared))
	combined := make([]types.StepName, 0, len(explicit)+len(declared))
	for _, step := range append(append([]types.StepName{}, explicit...), declared...) {
		if seen[step] {
			continue
		}
		seen[step] = true
		combined = append(combined, step)
	}
	return combined
}

// applyRunScopeToConfig narrows a run's configuration to what its scope permits.
//
// A read-only QA run has every auto-fix budget zeroed. Auto-fix rounds run an
// agent that edits files and commits them, and in a read-only run those commits
// can never be pushed - the same reason the document step is skipped. Worse than
// wasteful, they would make the report dishonest: a test the pipeline repaired
// in a disposable worktree reads as a test that passes, and the operator would
// be told the code is fine when it is not.
//
// A fix-pr QA run keeps its budgets. It writes code by design, a person asked
// for that specific run, and its fixes reach a PR.
//
// The gate path is untouched: gate runs return the config unchanged.
func applyRunScopeToConfig(cfg *config.Config, scope runScope) *config.Config {
	if cfg == nil || !scope.isQA() || scope.mode.WritesCode() {
		return cfg
	}
	cfg.AutoFix = config.AutoFix{}
	return cfg
}

// maxCommitIntentCommits bounds how many commit subjects a derived intent
// summarizes, and maxCommitIntentBytes bounds the stored text. A branch with
// four hundred commits must not turn into a four-hundred-line prompt section.
const (
	maxCommitIntentCommits = 20
	maxCommitIntentBytes   = 1000
)

// commitDerivedIntent summarizes what a branch was trying to do from its own
// commit subjects, for a run whose author is not present to state it.
//
// It is best effort by design: an unreadable range or an empty result yields "",
// and the run proceeds with no intent rather than failing. The text is
// contributor-authored, which is why it is stored as a non-authoritative
// RunIntentSourceCommits hint and still passes through the prompt layer's
// adversarial stripping and secret redaction like any other untrusted input.
func commitDerivedIntent(ctx context.Context, gitDir, baseSHA, headSHA string) string {
	if headSHA == "" {
		return ""
	}
	subjects := ""
	if baseSHA != "" && !git.IsZeroSHA(baseSHA) && baseSHA != headSHA {
		subjects, _ = git.Run(ctx, gitDir, "log", "--no-merges", "--format=%s",
			fmt.Sprintf("--max-count=%d", maxCommitIntentCommits), baseSHA+".."+headSHA)
	}
	if strings.TrimSpace(subjects) == "" {
		// No usable range: fall back to the head commit alone, which is still a
		// better description of the branch than nothing.
		subjects, _ = git.Run(ctx, gitDir, "log", "--no-merges", "--format=%s", "--max-count=1", headSHA)
	}
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(subjects), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	summary := strings.Join(lines, "; ")
	if len(summary) > maxCommitIntentBytes {
		summary = summary[:maxCommitIntentBytes] + "..."
	}
	return summary
}
