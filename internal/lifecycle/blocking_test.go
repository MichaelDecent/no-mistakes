package lifecycle

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestBlockingRunsProtectsWorkAndReleasesReadOnlyQA is the whole rule.
//
// The failure this prevents is subtle: a nightly sweep that blocks
// `daemon restart` most nights teaches operators to always pass --force, which
// removes the protection for gate runs that genuinely hold uncommitted work.
func TestBlockingRunsProtectsWorkAndReleasesReadOnlyQA(t *testing.T) {
	for _, tc := range []struct {
		name  string
		run   *db.Run
		block bool
	}{
		{"gate run", &db.Run{ID: "1", RunKind: string(types.RunKindGate)}, true},
		{"legacy run with no kind", &db.Run{ID: "2"}, true},
		{"unknown kind fails safe", &db.Run{ID: "3", RunKind: "something-new"}, true},
		{"qa fix-pr writes code", &db.Run{ID: "4", RunKind: string(types.RunKindQA), RunMode: string(types.RunModeFixPR)}, true},
		{"qa with no mode defaults to fix-pr", &db.Run{ID: "5", RunKind: string(types.RunKindQA)}, true},
		{"qa report", &db.Run{ID: "6", RunKind: string(types.RunKindQA), RunMode: string(types.RunModeReport)}, false},
		{"qa comment", &db.Run{ID: "7", RunKind: string(types.RunKindQA), RunMode: string(types.RunModeComment)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := []*db.Run{tc.run}
			blocking := BlockingRuns(runs)
			cancellable := CancellableRuns(runs)
			if tc.block && len(blocking) != 1 {
				t.Errorf("run does not block a destructive lifecycle operation but must")
			}
			if !tc.block && len(blocking) != 0 {
				t.Errorf("run blocks but is read-only QA that the next sweep re-derives")
			}
			// The two must partition the input exactly, or a run is either
			// double-counted or silently dropped.
			if len(blocking)+len(cancellable) != 1 {
				t.Errorf("blocking=%d cancellable=%d, want them to partition the input", len(blocking), len(cancellable))
			}
		})
	}
}

// TestCancellableRunListNamesWhatItDiscards pins that discarded work is always
// reported: silently cancelling a run is how an operator loses trust in a guard.
func TestCancellableRunListNamesWhatItDiscards(t *testing.T) {
	runs := []*db.Run{{ID: "run-abc", Status: types.RunRunning, Branch: "main", RunKind: string(types.RunKindQA), RunMode: string(types.RunModeReport)}}
	out := CancellableRunList(CancellableRuns(runs))
	for _, want := range []string{"run-abc", "main", "re-derives"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered list %q does not mention %q", out, want)
		}
	}
	if CancellableRunList(nil) != "" {
		t.Error("an empty list must render nothing")
	}
}
