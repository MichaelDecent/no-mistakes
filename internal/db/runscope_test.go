package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestInsertRunWithIntentLeavesTheScopeColumnsUntouched(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/scope-gate", "https://example.com/repo.git", "main")
	run, err := d.InsertRunWithIntent(repo.ID, "feature", "head", "base", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The gate path must keep writing exactly the columns it always wrote, so a
	// gate run's row is byte-identical to what it was before QA existed.
	if got.RunKind != "" || got.RunMode != "" || got.SkippedSteps != nil {
		t.Fatalf("gate run row = kind %q mode %q skipped %v, want all unset", got.RunKind, got.RunMode, got.SkippedSteps)
	}
	if len(got.SkippedStepNames()) != 0 {
		t.Fatalf("SkippedStepNames() = %v, want none", got.SkippedStepNames())
	}
}

func TestInsertRunWithScopeRecordsKindModeAndSkipDeclaration(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/scope-qa", "https://example.com/repo.git", "main")
	skips := types.SkipSetFor(types.RunKindQA, types.RunModeReport)
	run, err := d.InsertRunWithScope(repo.ID, "main", "head", "base", nil, RunScope{
		Kind:         types.RunKindQA,
		Mode:         types.RunModeReport,
		SkippedSteps: skips,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunKind != string(types.RunKindQA) || got.RunMode != string(types.RunModeReport) {
		t.Fatalf("row = kind %q mode %q, want qa/report", got.RunKind, got.RunMode)
	}
	// The declaration is recorded at run start, before any step executed, so it
	// survives a crash and can be reinstalled on recovery.
	names := got.SkippedStepNames()
	if len(names) != len(skips) {
		t.Fatalf("SkippedStepNames() = %v, want %v", names, skips)
	}
	for i := range skips {
		if names[i] != skips[i] {
			t.Fatalf("SkippedStepNames() = %v, want %v", names, skips)
		}
	}
}

func TestInsertRunWithScopeStoresNoSkipDeclarationWhenThereIsNone(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/tmp/scope-fixpr", "https://example.com/repo.git", "main")
	run, err := d.InsertRunWithScope(repo.ID, "main", "head", "base", nil, RunScope{
		Kind: types.RunKindQA,
		Mode: types.RunModeFixPR,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// An empty declaration is NULL, not "[]": "this run skips nothing" and "this
	// run never declared anything" must not be forced to look different.
	if got.SkippedSteps != nil {
		t.Fatalf("skipped_steps = %v, want NULL for a run that skips nothing", *got.SkippedSteps)
	}
}

func TestSkippedStepNamesIgnoresUnreadableDeclarations(t *testing.T) {
	garbage := "not json"
	run := &Run{SkippedSteps: &garbage}
	// A declaration that cannot be read must degrade to "skips nothing", which
	// runs the FULL pipeline. Degrading toward more validation is the only safe
	// direction for a value that reached the database from an unknown writer.
	if got := run.SkippedStepNames(); got != nil {
		t.Fatalf("SkippedStepNames() = %v, want nil for an unreadable declaration", got)
	}
}

func TestCommitDerivedIntentIsNeverAuthoritative(t *testing.T) {
	// A QA run's intent comes from the branch's own commit messages, which are
	// contributor-authored text, not an operator contract. It must never be
	// framed as authoritative acceptance criteria.
	if IsAuthoritativeRunIntentSource(RunIntentSourceCommits) {
		t.Fatalf("%q is authoritative, want it treated as a low-confidence hint", RunIntentSourceCommits)
	}
	if RunIntentSourceCommits == RunIntentSourceAgent || RunIntentSourceCommits == RunIntentSourceRerun {
		t.Fatal("the commits source must be a distinct value")
	}
}
