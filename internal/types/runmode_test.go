package types

import "testing"

// TestSkipSetForNeverSkipsAnythingForAGateRun is the structural expression of
// the VISION.md rule that "a standing rule may never skip [steps] on anyone's
// behalf": configuration selects a mode, and a gate run's step set must be
// unreachable from that selection no matter which mode is stored on the run.
func TestSkipSetForNeverSkipsAnythingForAGateRun(t *testing.T) {
	for _, mode := range []RunMode{RunModeFixPR, RunModeComment, RunModeReport, RunMode("nonsense"), RunMode("")} {
		if got := SkipSetFor(RunKindGate, mode); got != nil {
			t.Errorf("SkipSetFor(gate, %q) = %v, want nil: configuration must never reach a gate run's step set", mode, got)
		}
	}
}

// TestSkipSetForIsExhaustiveOverModes fails when a new RunMode is added without
// a declared skip set, so a mode can never silently inherit the default branch.
func TestSkipSetForIsExhaustiveOverModes(t *testing.T) {
	declared := map[RunMode][]StepName{
		RunModeFixPR:   nil,
		RunModeReport:  {StepDocument, StepPush, StepPR, StepCI},
		RunModeComment: {StepDocument, StepPush, StepPR, StepCI},
	}
	if len(declared) != len(AllRunModes()) {
		t.Fatalf("AllRunModes() has %d entries but this test declares %d; a new mode needs a reviewed skip set", len(AllRunModes()), len(declared))
	}
	for _, mode := range AllRunModes() {
		want, ok := declared[mode]
		if !ok {
			t.Fatalf("mode %q has no declared skip set in this test", mode)
		}
		got := SkipSetFor(RunKindQA, mode)
		if !sameSteps(got, want) {
			t.Errorf("SkipSetFor(qa, %q) = %v, want %v", mode, got, want)
		}
	}
}

// TestReportAndCommentSkipSetsAreIdentical pins that the two read-only modes
// differ only in publication. If they ever diverge, the difference must be a
// deliberate reviewed change here rather than a drift in one branch.
func TestReportAndCommentSkipSetsAreIdentical(t *testing.T) {
	report := SkipSetFor(RunKindQA, RunModeReport)
	comment := SkipSetFor(RunKindQA, RunModeComment)
	if !sameSteps(report, comment) {
		t.Errorf("report skips %v but comment skips %v; the modes must differ only in publication", report, comment)
	}
}

// TestSkipSetForUnknownModeDegradesToTheFullPipeline pins the fail-safe
// direction: an unrecognized stored mode must run MORE validation, never less.
func TestSkipSetForUnknownModeDegradesToTheFullPipeline(t *testing.T) {
	if got := SkipSetFor(RunKindQA, RunMode("something-new")); got != nil {
		t.Errorf("SkipSetFor(qa, unknown) = %v, want nil so an unknown mode runs the full pipeline", got)
	}
}

// TestReadOnlyModesSkipEveryDeliveryStep pins the property that actually makes
// report and comment read-only against the remote: no step that writes a branch
// or a PR may run.
func TestReadOnlyModesSkipEveryDeliveryStep(t *testing.T) {
	for _, mode := range []RunMode{RunModeReport, RunModeComment} {
		skipped := make(map[StepName]bool)
		for _, s := range SkipSetFor(RunKindQA, mode) {
			skipped[s] = true
		}
		for _, delivery := range []StepName{StepPush, StepPR, StepCI} {
			if !skipped[delivery] {
				t.Errorf("mode %q does not skip %q; a read-only mode must not run any delivery step", mode, delivery)
			}
		}
	}
}

// TestReadOnlyModesStillValidateAgainstAFreshBase pins that rebase and intent
// are deliberately NOT skipped. rebase never pushes (it only fetches and
// rewrites the disposable worktree), and skipping it would make every sweep
// review against a stale base and re-report already-fixed issues. intent is
// supplied from commits at run creation rather than skipped, so review keeps
// the "what was this trying to do" axis.
func TestReadOnlyModesStillValidateAgainstAFreshBase(t *testing.T) {
	for _, mode := range []RunMode{RunModeReport, RunModeComment} {
		for _, s := range SkipSetFor(RunKindQA, mode) {
			if s == StepRebase {
				t.Errorf("mode %q skips rebase; that would validate against a stale base", mode)
			}
			if s == StepIntent {
				t.Errorf("mode %q skips intent; supply a commit-derived intent instead", mode)
			}
			if s == StepReview || s == StepTest || s == StepLint {
				t.Errorf("mode %q skips %q, which is the validation the mode exists to perform", mode, s)
			}
		}
	}
}

func TestNormalizeRunKindTreatsEveryLegacyValueAsAGateRun(t *testing.T) {
	for _, in := range []string{"", "gate", "nonsense", "QA"} {
		got := NormalizeRunKind(in)
		if in == "qa" {
			continue
		}
		if in != "qa" && got != RunKindGate {
			t.Errorf("NormalizeRunKind(%q) = %q, want %q: a row written before this column existed was an author gate", in, got, RunKindGate)
		}
	}
	if got := NormalizeRunKind("qa"); got != RunKindQA {
		t.Errorf("NormalizeRunKind(\"qa\") = %q, want %q", got, RunKindQA)
	}
}

func TestNormalizeRunModeDefaultsToFixPRSoHistoricalRunsKeepTheirMeaning(t *testing.T) {
	if got := NormalizeRunMode(""); got != RunModeFixPR {
		t.Errorf("NormalizeRunMode(\"\") = %q, want %q: every run before QA existed pushed and opened a PR", got, RunModeFixPR)
	}
	if got := NormalizeRunMode("nonsense"); got != RunModeFixPR {
		t.Errorf("NormalizeRunMode(\"nonsense\") = %q, want %q", got, RunModeFixPR)
	}
	for _, mode := range AllRunModes() {
		if got := NormalizeRunMode(string(mode)); got != mode {
			t.Errorf("NormalizeRunMode(%q) = %q, want round-trip", mode, got)
		}
	}
}

func TestValidRunModeAcceptsOnlyDeclaredModes(t *testing.T) {
	for _, mode := range AllRunModes() {
		if !ValidRunMode(string(mode)) {
			t.Errorf("ValidRunMode(%q) = false, want true", mode)
		}
	}
	for _, bad := range []string{"", "gate", "fixpr", "FIX-PR", "report "} {
		if ValidRunMode(bad) {
			t.Errorf("ValidRunMode(%q) = true, want false", bad)
		}
	}
}

// TestWritesCodeIsTrueOnlyForFixPR pins the predicate the daemon uses to decide
// whether a run may mutate a repository, so a new mode cannot become
// code-writing by accident.
func TestWritesCodeIsTrueOnlyForFixPR(t *testing.T) {
	if !RunModeFixPR.WritesCode() {
		t.Error("RunModeFixPR.WritesCode() = false, want true")
	}
	for _, mode := range []RunMode{RunModeReport, RunModeComment} {
		if mode.WritesCode() {
			t.Errorf("%q.WritesCode() = true, want false", mode)
		}
	}
	if RunMode("something-new").WritesCode() {
		t.Error("an unknown mode must not be treated as code-writing")
	}
}

func sameSteps(a, b []StepName) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
