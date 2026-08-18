package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// serveTestIPC registers the real handler set on a socket and returns a client.
func serveTestIPC(t *testing.T, mgr *RunManager, database *db.DB) *ipc.Client {
	t.Helper()
	// A short temp dir keeps the socket path inside the platform limit.
	tmpDir, err := os.MkdirTemp("", "qaipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	socket := filepath.Join(tmpDir, "d.sock")
	srv := ipc.NewServer()
	registerHandlers(srv, mgr, database, func() {})
	if err := srv.Listen(socket); err != nil {
		t.Fatalf("listen: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeReady() }()
	t.Cleanup(func() {
		srv.Close()
		<-served
	})
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestQAMutationsRefuseNestedGateContext(t *testing.T) {
	f := newConnectedFixture(t, nil)
	client := serveTestIPC(t, f.mgr, f.database)

	// A gate step running inside a pipeline must not be able to start QA work:
	// the outer executor owns the run, and recursive pipelines are what
	// internal/gatecontext exists to refuse.
	original := inspectGateContext
	inspectGateContext = func(context.Context, *db.DB, *paths.Paths, string, bool, bool) (gatecontext.Result, error) {
		return gatecontext.Result{Nested: true, RunID: "run-outer", Phase: types.StepReview}, nil
	}
	t.Cleanup(func() { inspectGateContext = original })

	var result ipc.QARunResult
	err := client.Call(ipc.MethodQARun, &ipc.QARunParams{RepoID: f.repo.ID, Branch: "main", Mode: types.RunModeReport}, &result)
	if err == nil {
		t.Fatal("qa_run started a run from inside a gate step")
	}
	if !strings.Contains(err.Error(), "refusing pipeline control") {
		t.Fatalf("error = %v, want the nested refusal", err)
	}
	runs, dbErr := f.database.GetRunsByRepo(f.repo.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if len(runs) != 0 {
		t.Fatalf("got %d runs, want none: the refusal must precede run creation", len(runs))
	}
}

func TestQARunIsRegisteredAsAnIPCMethod(t *testing.T) {
	f := newConnectedFixture(t, nil)
	client := serveTestIPC(t, f.mgr, f.database)

	// An unknown repository is a handler-level refusal, which proves the method
	// is dispatched rather than missing.
	var result ipc.QARunResult
	err := client.Call(ipc.MethodQARun, &ipc.QARunParams{RepoID: "nope", Mode: types.RunModeReport}, &result)
	if err == nil {
		t.Fatal("qa_run accepted an unknown repository")
	}
	if strings.Contains(err.Error(), "method not found") {
		t.Fatalf("error = %v, want the method registered", err)
	}
}
