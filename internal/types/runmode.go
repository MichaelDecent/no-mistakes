package types

// Run kind and mode describe a run's declared scope: which mechanism started it
// and what it is allowed to do. They exist because the product has two scopes.
//
// The GATE is the author-side pre-push gate: a person pushes a branch on
// purpose, and that push is the consent boundary authorizing the run to
// validate, apply reviewable fixes, push the branch, and raise a PR.
//
// QA is operator-configured validation of repositories connected by remote URL.
// Its consent boundary is the operator's declarative configuration, which is
// narrower: an unattended QA run never writes code.
//
// Keeping the two apart as a stored RunKind - rather than as a flag on a shared
// path - is what lets the gate's guarantees stay untouched while QA grows.

// RunKind is the mechanism that started a run and the scope it was started under.
type RunKind string

const (
	// RunKindGate is the author-side pre-push gate. Every run created before
	// this column existed was one, which is why NormalizeRunKind resolves an
	// empty or unrecognized value here.
	RunKindGate RunKind = "gate"
	// RunKindQA is operator-configured validation of a connected repository.
	RunKindQA RunKind = "qa"
)

// RunMode is what a run is permitted to do with what it finds.
type RunMode string

const (
	// RunModeFixPR is the full pipeline: validate, fix, push, open a PR, watch
	// CI. It is the only mode that writes code, so it is the only mode a run
	// may be started in while a person is present to consent to it.
	RunModeFixPR RunMode = "fix-pr"
	// RunModeComment validates read-only and publishes findings to the forge as
	// a comment on an existing PR.
	RunModeComment RunMode = "comment"
	// RunModeReport validates read-only and publishes findings locally only.
	RunModeReport RunMode = "report"
)

// AllRunModes returns every declared mode. SkipSetFor is required to name each
// one explicitly, so adding a mode without deciding its step set fails a test
// rather than silently inheriting the default branch.
func AllRunModes() []RunMode {
	return []RunMode{RunModeFixPR, RunModeComment, RunModeReport}
}

// NormalizeRunKind resolves a stored value to a kind. Anything unrecognized -
// including the empty string a row written before the column existed reads back
// as - resolves to RunKindGate, because that is what those runs actually were.
func NormalizeRunKind(s string) RunKind {
	if RunKind(s) == RunKindQA {
		return RunKindQA
	}
	return RunKindGate
}

// NormalizeRunMode resolves a stored value to a mode, defaulting to
// RunModeFixPR. That default is deliberate and is not a fail-open: every run
// created before QA existed pushed its branch and opened a PR, so fix-pr is the
// honest reading of a missing value on a historical row. It is safe because the
// mode alone never authorizes anything - a QA run is only ever started in a
// code-writing mode by an explicit, consented request (see RunMode.WritesCode).
func NormalizeRunMode(s string) RunMode {
	switch RunMode(s) {
	case RunModeFixPR:
		return RunModeFixPR
	case RunModeComment:
		return RunModeComment
	case RunModeReport:
		return RunModeReport
	default:
		return RunModeFixPR
	}
}

// ValidRunMode reports whether s is exactly one of the declared modes. It is
// the parse-time check for operator configuration and CLI flags, and unlike
// NormalizeRunMode it does not substitute a default: a typo in a config file
// must fail loudly rather than quietly select fix-pr.
func ValidRunMode(s string) bool {
	for _, mode := range AllRunModes() {
		if RunMode(s) == mode {
			return true
		}
	}
	return false
}

// WritesCode reports whether a run in this mode may modify a repository - apply
// fixes, commit, push, or open a PR. It is a whitelist of one so a mode added
// later is non-writing until someone deliberately lists it here.
func (m RunMode) WritesCode() bool {
	return m == RunModeFixPR
}

// SkipSetFor returns the steps a run of this kind and mode declares it will not
// run.
//
// It returns nil for RunKindGate for EVERY mode. This is the structural half of
// the VISION.md rule that "a person may explicitly skip steps for one run; a
// standing rule may never skip them on anyone's behalf": a gate run's step set
// is unreachable from configuration, and its only skips remain the explicit
// per-run --skip / no-mistakes.skip= a person passes. The other half is that
// the push handler never consults a repository's QA mode at all, so a repo
// cannot be configured such that pushing to it produces a weaker gate.
//
// Skipping is a step STATUS, not a shorter step list: the executor still
// inserts all nine step_results rows and still runs the full step slice, so
// "the pipeline always has all nine steps" stays literally true and the
// recovered-run positional check keeps working.
//
// An unrecognized mode returns nil, so it runs the FULL pipeline. Degrading
// toward more validation rather than less is the only safe direction for a
// value that reached the database from an unknown writer.
func SkipSetFor(kind RunKind, mode RunMode) []StepName {
	if kind != RunKindQA {
		return nil
	}
	switch mode {
	case RunModeFixPR:
		// The full pipeline, identical to a gate run.
		return nil
	case RunModeReport, RunModeComment:
		// The two read-only modes skip exactly the same steps and differ only
		// in whether findings are published to the forge afterwards.
		//
		// document: it edits files and commits them, advancing the run head.
		// Those commits have nowhere to go when nothing is pushed. The cost is
		// that read-only modes produce no documentation findings, which is a
		// deliberate accepted trade; the fix is a read-only mode inside the
		// document step, not un-skipping it here.
		//
		// push, pr, ci: every step that writes a branch, creates or updates a
		// PR, or babysits checks for days. Removing them is what makes these
		// modes read-only against the remote, and it makes a stray write
		// structurally impossible rather than merely unlikely.
		//
		// Deliberately NOT skipped: rebase, which never pushes (it only fetches
		// and rewrites the disposable worktree) and whose absence would make
		// every sweep review against a stale base; intent, which is supplied
		// from the branch's own commits at run creation so review keeps the
		// "what was this trying to do" axis without reading anyone's
		// transcripts; and review, test, and lint, which are the validation
		// these modes exist to perform.
		return []StepName{StepDocument, StepPush, StepPR, StepCI}
	default:
		return nil
	}
}
