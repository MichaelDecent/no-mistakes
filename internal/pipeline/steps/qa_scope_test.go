package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestQARunNeverScansLocalTranscripts(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Intent.Enabled = true
	sctx.Run.RunKind = string(types.RunKindQA)
	sctx.Run.RunMode = string(types.RunModeReport)
	commitIntent := "add the widget cache"
	sctx.Run.Intent = &commitIntent

	logged := []string{}
	sctx.Log = func(text string) { logged = append(logged, text) }

	// runIntent is the transcript-reading pipeline. A QA run must not reach it:
	// the operator's local sessions are about their own work, and the connected
	// repository has no working clone on this machine at all.
	step := &IntentStep{}
	scanned := false
	step.runIntent = func(context.Context, *pipeline.StepContext) (*intent.Result, error) {
		scanned = true
		return nil, nil
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if scanned {
		t.Fatal("the intent step scanned local transcripts for a QA run")
	}
	if outcome.Skipped {
		t.Error("outcome = skipped, want completed: the commit-derived intent is present and used")
	}
	if !strings.Contains(strings.Join(logged, "\n"), "commits") {
		t.Errorf("log = %q, want it to say where the intent came from", logged)
	}
}

func TestQARunWithoutCommitIntentStillNeverScansTranscripts(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Intent.Enabled = true
	sctx.Run.RunKind = string(types.RunKindQA)
	sctx.Run.RunMode = string(types.RunModeReport)
	sctx.Run.Intent = nil

	step := &IntentStep{}
	scanned := false
	step.runIntent = func(context.Context, *pipeline.StepContext) (*intent.Result, error) {
		scanned = true
		return nil, nil
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// A branch with no derivable commit intent is not a licence to fall back to
	// reading transcripts: there is nothing here that describes it.
	if scanned {
		t.Fatal("the intent step scanned local transcripts for a QA run with no intent")
	}
	if !outcome.Skipped {
		t.Error("outcome = completed, want skipped: nothing was inferred")
	}
}

func TestSkippedDocumentStepFallsBackToLintOwnAgentPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"unused import","action":"ask-user"}],"summary":"lint findings"}`)}, nil
		},
	}

	// A read-only QA run skips the document step, so the combined
	// document+lint housekeeping pass never runs and stashes nothing. Lint must
	// then pay for its own agent pass rather than silently reporting nothing.
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	if _, stashed := sctx.Shared.TakeHousekeepingLint(); stashed {
		t.Fatal("the fixture already carries a housekeeping stash")
	}

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if calls != 1 {
		t.Fatalf("agent called %d times, want 1: the lint duty is never silently dropped", calls)
	}
	if !strings.Contains(outcome.Findings, "unused import") {
		t.Errorf("findings = %q, want the agent pass's own findings", outcome.Findings)
	}
}
