package daemon

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGatePolicyForRunIsNilForEveryGateRun(t *testing.T) {
	// Every run the push path creates is a gate run, and a gate run's gates
	// block for the person who pushed. Nothing about a stored mode may change
	// that, including the fix-pr value a legacy row reads back as.
	for _, mode := range []string{"", string(types.RunModeFixPR), string(types.RunModeReport), string(types.RunModeComment), "nonsense"} {
		run := &db.Run{RunKind: string(types.RunKindGate), RunMode: mode}
		if got := gatePolicyForRun(run); got != nil {
			t.Errorf("gatePolicyForRun(gate run, mode=%q) = %T, want nil", mode, got)
		}
		legacy := &db.Run{RunMode: mode} // pre-QA row: run_kind is NULL
		if got := gatePolicyForRun(legacy); got != nil {
			t.Errorf("gatePolicyForRun(legacy row, mode=%q) = %T, want nil", mode, got)
		}
	}
}

func TestGatePolicyForRunInstallsAPolicyForQARuns(t *testing.T) {
	for _, mode := range types.AllRunModes() {
		run := &db.Run{RunKind: string(types.RunKindQA), RunMode: string(mode)}
		if got := gatePolicyForRun(run); got == nil {
			t.Errorf("gatePolicyForRun(qa run, mode=%q) = nil, want a policy", mode)
		}
	}
}

func TestGatePolicyForRunHandlesAMissingRun(t *testing.T) {
	if got := gatePolicyForRun(nil); got != nil {
		t.Errorf("gatePolicyForRun(nil) = %T, want nil", got)
	}
}
