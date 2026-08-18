package config

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRepoConfigCannotChangeQAPolicy pins the `qa:` block as operator-only,
// mirroring TestRepoConfigCannotChangeEvalCollection.
//
// The block governs this machine's scheduling, concurrency, and whether
// findings are published to a forge. A pushed branch that could set it would
// raise its own concurrency, shorten its own poll interval, or switch on forge
// commenting for the operator.
func TestRepoConfigCannotChangeQAPolicy(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(
		"qa:\n  enabled: true\n  max_concurrent: 2\n  comment: false\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte(
		"qa:\n  enabled: false\n  max_concurrent: 64\n  comment: true\n",
	))
	if err != nil {
		t.Fatalf("repo config with a qa block must load, ignoring the key: %v", err)
	}

	if !global.QA.Enabled {
		t.Error("QA.Enabled = false, want the operator's true")
	}
	if global.QA.MaxConcurrent != 2 {
		t.Errorf("QA.MaxConcurrent = %d, want the operator's 2", global.QA.MaxConcurrent)
	}
	if global.QA.Comment {
		t.Error("QA.Comment = true; the repository must not be able to switch on forge writes")
	}

	merged := Merge(global, repo)
	if !merged.QA.Enabled || merged.QA.MaxConcurrent != 2 || merged.QA.Comment {
		t.Fatalf("merged QA = %#v, want the operator's global values untouched by the repository", merged.QA)
	}
}

// TestQADefaultsAreOffAndConservative pins that a daemon with no qa block does
// nothing unattended. Enabling QA must be a deliberate act.
func TestQADefaultsAreOffAndConservative(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("log_level: debug\n"))
	if err != nil {
		t.Fatal(err)
	}
	if global.QA.Enabled {
		t.Error("QA.Enabled defaults to true; it must default off")
	}
	if global.QA.Comment {
		t.Error("QA.Comment defaults to true; a forge write must be opt-in")
	}
	if !global.QA.DefaultMode.WritesCode() {
		// Guard the guard: report must be the default, so assert the specific
		// mode rather than only that it does not write.
		if global.QA.DefaultMode != types.RunModeReport {
			t.Errorf("QA.DefaultMode = %q, want %q", global.QA.DefaultMode, types.RunModeReport)
		}
	} else {
		t.Errorf("QA.DefaultMode = %q, which writes code; the default must be read-only", global.QA.DefaultMode)
	}
	if global.QA.MaxConcurrent <= 0 {
		t.Errorf("QA.MaxConcurrent = %d, want a positive default", global.QA.MaxConcurrent)
	}
}

// TestQARejectsCodeWritingDefaultMode is the security rule of this spec.
//
// default_mode is the mode every watch inherits, and a watch fires with no
// human present. Unattended runs never write code, so a code-writing default is
// refused at parse time rather than filtered later: fix-pr remains reachable
// only through an explicit, consented `qa run --mode fix-pr --yes`.
func TestQARejectsCodeWritingDefaultMode(t *testing.T) {
	for _, mode := range types.AllRunModes() {
		t.Run(string(mode), func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte("qa:\n  default_mode: " + string(mode) + "\n"))
			if mode.WritesCode() {
				if err == nil {
					t.Fatalf("default_mode: %s was accepted; a code-writing default must be refused", mode)
				}
				if !strings.Contains(err.Error(), "default_mode") {
					t.Errorf("error %q does not name default_mode", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read-only default_mode %s rejected: %v", mode, err)
			}
			if global.QA.DefaultMode != mode {
				t.Errorf("DefaultMode = %q, want %q", global.QA.DefaultMode, mode)
			}
		})
	}
}

// TestQAParsesFullSpec pins every field round-tripping.
func TestQAParsesFullSpec(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`qa:
  enabled: true
  poll_interval: 15m
  nightly_at: "02:30"
  max_concurrent: 3
  max_total_runs: 5
  queue_depth: 32
  max_run_duration: 6h
  max_reports: 500
  default_mode: comment
  comment: true
`))
	if err != nil {
		t.Fatal(err)
	}
	qa := global.QA
	if !qa.Enabled || !qa.Comment {
		t.Errorf("Enabled=%v Comment=%v, want both true", qa.Enabled, qa.Comment)
	}
	if qa.PollInterval != 15*time.Minute {
		t.Errorf("PollInterval = %v, want 15m", qa.PollInterval)
	}
	if qa.MaxRunDuration != 6*time.Hour {
		t.Errorf("MaxRunDuration = %v, want 6h", qa.MaxRunDuration)
	}
	if qa.NightlyAt != "02:30" {
		t.Errorf("NightlyAt = %q, want 02:30", qa.NightlyAt)
	}
	if qa.MaxConcurrent != 3 || qa.MaxTotalRuns != 5 || qa.QueueDepth != 32 || qa.MaxReports != 500 {
		t.Errorf("counts = %#v", qa)
	}
	if qa.DefaultMode != types.RunModeComment {
		t.Errorf("DefaultMode = %q, want comment", qa.DefaultMode)
	}
}

// TestQAValidationRejectsUnusableValues covers the parse-time rules. Each would
// otherwise surface as a confusing runtime behaviour rather than a config error.
func TestQAValidationRejectsUnusableValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"unknown mode", "qa:\n  default_mode: audit\n", "default_mode"},
		{"zero max_concurrent", "qa:\n  max_concurrent: 0\n", "max_concurrent"},
		{"negative max_concurrent", "qa:\n  max_concurrent: -1\n", "max_concurrent"},
		{"negative max_total_runs", "qa:\n  max_total_runs: -1\n", "max_total_runs"},
		{"negative queue_depth", "qa:\n  queue_depth: -1\n", "queue_depth"},
		{"negative max_reports", "qa:\n  max_reports: -1\n", "max_reports"},
		{"unparseable poll_interval", "qa:\n  poll_interval: soon\n", "poll_interval"},
		{"zero poll_interval", "qa:\n  poll_interval: 0s\n", "poll_interval"},
		{"negative max_run_duration", "qa:\n  max_run_duration: -1h\n", "max_run_duration"},
		{"nightly_at not a time", "qa:\n  nightly_at: \"midnight\"\n", "nightly_at"},
		{"nightly_at out of range", "qa:\n  nightly_at: \"25:00\"\n", "nightly_at"},
		{"nightly_at missing minutes", "qa:\n  nightly_at: \"2\"\n", "nightly_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadGlobalFromBytes([]byte(tc.yaml)); err == nil {
				t.Fatalf("LoadGlobalFromBytes accepted %q; it must fail closed", tc.yaml)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestQAAcceptsValidNightlyAnchors guards against a time rule so strict it
// rejects ordinary values.
func TestQAAcceptsValidNightlyAnchors(t *testing.T) {
	for _, at := range []string{"00:00", "02:30", "9:05", "23:59"} {
		t.Run(at, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte("qa:\n  nightly_at: \"" + at + "\"\n"))
			if err != nil {
				t.Fatalf("valid nightly_at %q rejected: %v", at, err)
			}
			if global.QA.NightlyAt != at {
				t.Errorf("NightlyAt = %q, want %q", global.QA.NightlyAt, at)
			}
		})
	}
}

// TestQAEmptyNightlyAtIsAllowed pins the daily anchor as optional: an operator
// may want interval polling only.
func TestQAEmptyNightlyAtIsAllowed(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("qa:\n  enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if global.QA.NightlyAt != "" {
		t.Errorf("NightlyAt = %q, want empty when unset", global.QA.NightlyAt)
	}
}
