package branchsync

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// connectedFixture builds the shape a connected repository actually has: an
// identity stub with no remotes, no refs, and no objects, registered as the
// repo's working_path. It is deliberately not a clone - the objects live in the
// bare gate - so every author-side surface reached from here has nothing to
// operate on.
type connectedFixture struct {
	db   *db.DB
	repo *db.Repo
	stub string
	p    *paths.Paths
}

func newConnectedFixture(t *testing.T) *connectedFixture {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	const id = "aabbccddeeff"
	stub := p.SourceDir(id)
	mustRun(t, root, "init", stub)
	configureIdentity(t, stub)

	repo, err := database.InsertConnectedRepo(
		id, stub, "https://example.test/acme/api.git", "example.test/acme/api", "acme-api", "main",
	)
	if err != nil {
		t.Fatal(err)
	}
	return &connectedFixture{db: database, repo: repo, stub: stub, p: p}
}

func (f *connectedFixture) service() *Service {
	return &Service{DB: f.db, Repo: f.repo, WorkDir: f.stub, GateDir: f.p.RepoDir(f.repo.ID), Paths: f.p}
}

// TestBranchSyncOpenCurrentRefusesConnectedRepoFromStubCWD is the headline
// regression for the verified hole: OpenCurrent does its own GetRepoByPath and
// never consults cli.findRepo, so gating the CLI choke point alone would still
// hand back a live Service with Apply and Recover wired. It also disarms the TUI
// with no TUI change, because tui/app.go only uses the service when err == nil.
func TestBranchSyncOpenCurrentRefusesConnectedRepoFromStubCWD(t *testing.T) {
	f := newConnectedFixture(t)
	t.Setenv("NM_HOME", f.p.Root())
	t.Chdir(f.stub)

	service, closeFn, err := OpenCurrent()
	if err == nil {
		if closeFn != nil {
			closeFn()
		}
		t.Fatalf("OpenCurrent returned a live service %v for a connected repo; it must refuse", service)
	}
	if service != nil || closeFn != nil {
		t.Errorf("OpenCurrent returned service=%v closeFn!=nil=%v alongside an error; both must be nil", service, closeFn != nil)
	}
	assertConnectedRefusal(t, err.Error())
}

// TestBranchSyncInspectReportsNotApplicableWithoutNextAction pins that a
// directly constructed Service - the shape cli/status.go and cli/axi_drive.go
// both build - answers not_applicable rather than a state that could offer
// recover_custody or user_owned for a repo with no local branch.
func TestBranchSyncInspectReportsNotApplicableWithoutNextAction(t *testing.T) {
	t.Parallel()
	f := newConnectedFixture(t)

	for _, tc := range []struct {
		name  string
		state State
	}{
		{"InspectCached", f.service().InspectCached(t.Context())},
		{"Refresh", f.service().Refresh(t.Context())},
	} {
		if tc.state.State != StateNotApplicable {
			t.Errorf("%s: State = %q, want %q", tc.name, tc.state.State, StateNotApplicable)
		}
		if tc.state.NextAction != nil {
			t.Errorf("%s: NextAction = %+v, want nil for a repo with no local branch", tc.name, tc.state.NextAction)
		}
		if tc.state.Changed {
			t.Errorf("%s: Changed = true; a refusal changes nothing", tc.name)
		}
		if tc.state.Recovered {
			t.Errorf("%s: Recovered = true; nothing was recovered", tc.name)
		}
		assertConnectedRefusal(t, tc.state.Error)
	}
}

// TestCanApplyIsFalseForNotApplicableState pins that the refusal needs no new
// case in CanApply: it tests Safety, and a not-applicable state carries none, so
// the guard holds by construction rather than by a second copy of the rule.
func TestCanApplyIsFalseForNotApplicableState(t *testing.T) {
	t.Parallel()
	if CanApply(State{State: StateNotApplicable}) {
		t.Error("CanApply is true for a not-applicable state")
	}
}

// TestBranchSyncApplyAndRecoverRefuseConnectedRepoIndependently pins that the
// two worktree-mutating entry points assert for themselves instead of trusting
// derived state, and that a refusal touches no file and no ref.
func TestBranchSyncApplyAndRecoverRefuseConnectedRepoIndependently(t *testing.T) {
	t.Parallel()
	f := newConnectedFixture(t)

	for _, tc := range []struct {
		name  string
		state State
	}{
		{"Apply", f.service().Apply(t.Context())},
		{"Recover", f.service().Recover(t.Context(), false)},
		{"Recover --keep-local", f.service().Recover(t.Context(), true)},
	} {
		if tc.state.State != StateNotApplicable {
			t.Errorf("%s: State = %q, want %q", tc.name, tc.state.State, StateNotApplicable)
		}
		if tc.state.Changed {
			t.Errorf("%s: Changed = true; a connected repo must never be mutated", tc.name)
		}
		if tc.state.Recovered {
			t.Errorf("%s: Recovered = true; custody was never returned", tc.name)
		}
		assertConnectedRefusal(t, tc.state.Error)
	}

	// The stub must still have no refs: a refusal that created one would leave
	// the very local branch state the guard exists to deny.
	if out := mustRun(t, f.stub, "for-each-ref"); out != "" {
		t.Errorf("refs exist in the identity stub after refusals:\n%s", out)
	}
}

// assertConnectedRefusal keeps every layer's message recognizable and free of a
// URL, which for a connected repo may carry the operator's token.
func assertConnectedRefusal(t *testing.T, msg string) {
	t.Helper()
	if msg == "" {
		t.Error("refusal carries no explanation")
		return
	}
	if !strings.Contains(msg, "connected") {
		t.Errorf("refusal %q does not name the connected registration", msg)
	}
	for _, leak := range []string{"https://", "http://", "@"} {
		if strings.Contains(msg, leak) {
			t.Errorf("refusal %q leaks a URL fragment %q", msg, leak)
		}
	}
}
