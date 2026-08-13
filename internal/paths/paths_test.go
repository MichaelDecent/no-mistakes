package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithRoot(t *testing.T) {
	root := filepath.Join("tmp", "nm-test")
	p := WithRoot(root)

	if got := p.Root(); got != root {
		t.Errorf("Root() = %q, want %q", got, root)
	}
	if got := p.DB(); got != filepath.Join(root, "state.sqlite") {
		t.Errorf("DB() = %q, want %q", got, filepath.Join(root, "state.sqlite"))
	}
	if got := p.Socket(); got != filepath.Join(root, "socket") {
		t.Errorf("Socket() = %q, want %q", got, filepath.Join(root, "socket"))
	}
	if got := p.PIDFile(); got != filepath.Join(root, "daemon.pid") {
		t.Errorf("PIDFile() = %q, want %q", got, filepath.Join(root, "daemon.pid"))
	}
	if got := p.ConfigFile(); got != filepath.Join(root, "config.yaml") {
		t.Errorf("ConfigFile() = %q, want %q", got, filepath.Join(root, "config.yaml"))
	}
}

func TestRepoPaths(t *testing.T) {
	root := filepath.Join("tmp", "nm-test")
	p := WithRoot(root)

	if got := p.ReposDir(); got != filepath.Join(root, "repos") {
		t.Errorf("ReposDir() = %q", got)
	}
	if got := p.RepoDir("abc123"); got != filepath.Join(root, "repos", "abc123.git") {
		t.Errorf("RepoDir() = %q", got)
	}
}

func TestWorktreePaths(t *testing.T) {
	root := filepath.Join("tmp", "nm-test")
	p := WithRoot(root)

	if got := p.WorktreesDir(); got != filepath.Join(root, "worktrees") {
		t.Errorf("WorktreesDir() = %q", got)
	}
	if got := p.WorktreeDir("repo1", "run1"); got != filepath.Join(root, "worktrees", "repo1", "run1") {
		t.Errorf("WorktreeDir() = %q", got)
	}
}

func TestLogPaths(t *testing.T) {
	root := filepath.Join("tmp", "nm-test")
	p := WithRoot(root)

	if got := p.LogsDir(); got != filepath.Join(root, "logs") {
		t.Errorf("LogsDir() = %q", got)
	}
	if got := p.RunLogDir("run1"); got != filepath.Join(root, "logs", "run1") {
		t.Errorf("RunLogDir() = %q", got)
	}
	if got := p.DaemonLog(); got != filepath.Join(root, "logs", "daemon.log") {
		t.Errorf("DaemonLog() = %q", got)
	}
	if got := p.DaemonBootstrapLog(); got != filepath.Join(root, "logs", "daemon-bootstrap.log") {
		t.Errorf("DaemonBootstrapLog() = %q", got)
	}
	if got := p.ManagedServerLog(); got != filepath.Join(root, "logs", "managed-server.log") {
		t.Errorf("ManagedServerLog() = %q", got)
	}
}

func TestNewWithEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NM_HOME", dir)

	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if p.Root() != dir {
		t.Errorf("Root() = %q, want %q", p.Root(), dir)
	}
}

func TestNewRejectsDefaultRootInTests(t *testing.T) {
	t.Setenv("NM_HOME", "")
	t.Setenv("NO_MISTAKES_ALLOW_DEFAULT_ROOT_IN_TESTS", "")

	_, err := New()
	if err == nil {
		t.Fatal("New() should reject the default root under go test")
	}
}

func TestNewDefault(t *testing.T) {
	t.Setenv("NM_HOME", "")
	t.Setenv("NO_MISTAKES_ALLOW_DEFAULT_ROOT_IN_TESTS", "1")

	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".no-mistakes")
	if p.Root() != want {
		t.Errorf("Root() = %q, want %q", p.Root(), want)
	}
}

func TestEnsureDirs(t *testing.T) {
	dir := t.TempDir()
	p := WithRoot(filepath.Join(dir, "nm"))

	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{p.Root(), p.ReposDir(), p.SourcesDir(), p.WorktreesDir(), p.LogsDir(), p.ServerPIDsDir()} {
		info, err := os.Stat(d)
		if err != nil {
			t.Errorf("expected dir %q to exist: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %q to be a directory", d)
		}
	}
}

// TestEnsureDirsDoesNotCreateQAReportsDir pins the same rule EvalDir follows:
// disabling a feature must leave no state behind, so the reports directory is
// created by the reporter that writes into it and not by startup.
func TestEnsureDirsDoesNotCreateQAReportsDir(t *testing.T) {
	dir := t.TempDir()
	p := WithRoot(filepath.Join(dir, "nm"))

	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.QAReportsDir(), p.EvalDir()} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("EnsureDirs created %q (stat err = %v); it must be created on first use instead", d, err)
		}
	}
}

// TestConnectedRepoPathsAreDistinctFromTheGate pins that a connected
// repository's identity stub and its bare gate are different directories. If
// SourceDir ever collapsed onto RepoDir, the URL refresh path would read the
// gate's origin - which carries the full credentialled URL by design - and
// persist it into the database.
func TestConnectedRepoPathsAreDistinctFromTheGate(t *testing.T) {
	p := WithRoot(filepath.Join(t.TempDir(), "nm"))
	const repoID = "aabbccddeeff"

	source := p.SourceDir(repoID)
	gate := p.RepoDir(repoID)

	if source == gate {
		t.Fatal("SourceDir equals RepoDir; the identity stub must never be the gate")
	}
	if filepath.Dir(source) != p.SourcesDir() {
		t.Errorf("SourceDir parent = %q, want %q", filepath.Dir(source), p.SourcesDir())
	}
	if filepath.Dir(gate) != p.ReposDir() {
		t.Errorf("RepoDir parent = %q, want %q", filepath.Dir(gate), p.ReposDir())
	}
	// The stub must not sit under ReposDir, or the gate-context classifier would
	// have to reason about whether it is a gate.
	if strings.HasPrefix(source, p.ReposDir()+string(filepath.Separator)) {
		t.Errorf("SourceDir %q is under ReposDir; the gate classifier scans that directory", source)
	}
	if p.QARunReportDir("run-1") != filepath.Join(p.QAReportsDir(), "run-1") {
		t.Errorf("QARunReportDir = %q, want it under QAReportsDir", p.QARunReportDir("run-1"))
	}
}
