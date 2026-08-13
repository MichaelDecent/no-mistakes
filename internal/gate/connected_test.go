package gate

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// TestRemoteIdentityAcceptsCredentialBearingHTTPSRemote pins the subtlety that
// makes this wrapper necessary. inspectRefreshRemote reports a credential-bearing
// https remote as an ERROR while still populating the identity, because for the
// URL-refresh path a credential in a discovered clone remote is a reason to
// refuse. An operator's configured URL routinely carries a token, so keying off
// the error instead of the identity would make the common case unregisterable.
func TestRemoteIdentityAcceptsCredentialBearingHTTPSRemote(t *testing.T) {
	got, err := RemoteIdentity("https://token@github.com/acme/api.git")
	if err != nil {
		t.Fatalf("RemoteIdentity with an embedded token = %v; an operator URL routinely carries one", err)
	}
	if got != "github.com/acme/api" {
		t.Errorf("identity = %q, want %q (credential must not appear)", got, "github.com/acme/api")
	}

	withPassword, err := RemoteIdentity("https://user:pass@github.com/acme/api.git")
	if err != nil {
		t.Fatalf("RemoteIdentity with user:pass = %v", err)
	}
	if withPassword != "github.com/acme/api" {
		t.Errorf("identity = %q, want the credential stripped", withPassword)
	}
}

// TestConnectedRepoIDIsStableAcrossTransports pins that the same repository
// reached over ssh, https, or scp-style syntax - with or without a credential, a
// port, a trailing .git, or different casing - is ONE registration with one gate
// and one run history.
func TestConnectedRepoIDIsStableAcrossTransports(t *testing.T) {
	equivalent := []string{
		"git@github.com:acme/api.git",
		"git@github.com:acme/api",
		"https://github.com/acme/api.git",
		"https://github.com/acme/api",
		"https://token@github.com/acme/api.git",
		"ssh://git@github.com/acme/api.git",
		"ssh://git@github.com:2222/acme/api.git",
		"https://github.com/Acme/API.git",
	}

	want, err := RemoteIdentity(equivalent[0])
	if err != nil {
		t.Fatalf("RemoteIdentity(%q): %v", equivalent[0], err)
	}
	wantID := ConnectedRepoID(want)

	for _, raw := range equivalent {
		identity, err := RemoteIdentity(raw)
		if err != nil {
			t.Errorf("RemoteIdentity(%q) = %v", raw, err)
			continue
		}
		if identity != want {
			t.Errorf("RemoteIdentity(%q) = %q, want %q", raw, identity, want)
		}
		if got := ConnectedRepoID(identity); got != wantID {
			t.Errorf("ConnectedRepoID for %q = %q, want %q", raw, got, wantID)
		}
	}

	// A genuinely different repository must not collide.
	other, err := RemoteIdentity("git@github.com:acme/web.git")
	if err != nil {
		t.Fatalf("RemoteIdentity for other repo: %v", err)
	}
	if ConnectedRepoID(other) == wantID {
		t.Error("two different repositories share one connected repo ID")
	}
}

// TestConnectedRepoIDHasTheSameShapeAsAWorkingCloneID pins the compatibility
// contract. The ID becomes the "<id>.git" gate directory name, so anything other
// than 12 lowercase hex characters would break startup gate migration, the
// post-receive hook's repo resolution, and the gate-context classifier.
func TestConnectedRepoIDHasTheSameShapeAsAWorkingCloneID(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-f]{12}$`)

	connected := ConnectedRepoID("github.com/acme/api")
	if !shape.MatchString(connected) {
		t.Errorf("ConnectedRepoID = %q, want 12 lowercase hex characters", connected)
	}
	local := repoID("/home/dev/my-repo")
	if !shape.MatchString(local) {
		t.Fatalf("repoID = %q; fixture assumption about the local shape is wrong", local)
	}
	if len(connected) != len(local) {
		t.Errorf("connected ID length %d != local ID length %d", len(connected), len(local))
	}
}

// TestConnectedRepoIDIsDomainSeparatedFromPathIDs pins that the two ID
// namespaces cannot be each other's preimages: hashing the same string through
// both functions must not produce the same ID, so a crafted path or URL cannot be
// made to collide deliberately.
func TestConnectedRepoIDIsDomainSeparatedFromPathIDs(t *testing.T) {
	const shared = "github.com/acme/api"
	if ConnectedRepoID(shared) == repoID(shared) {
		t.Error("connected and local repo IDs collide for the same input; the domain prefix is not applied")
	}
}

func TestRemoteIdentityRejectsUnusableRemotes(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"not a url",
		"https://github.com",        // no owner/repo
		"https://github.com/acme",   // only one path segment
		"ftp://github.com/acme/api", // unsupported scheme
		"https://github.com/acme/../etc",
		"https://github.com/acme/api?x=1",
		"https://github.com/acme/api#frag",
	}
	for _, raw := range bad {
		if got, err := RemoteIdentity(raw); err == nil {
			t.Errorf("RemoteIdentity(%q) = %q, nil; want an error", raw, got)
		}
	}
}

// TestProvisionConnectedGateProducesTheSameGateShapeAsInit is the invariant that
// keeps startup gate migration and the gate-config stamp working unchanged: a
// connected gate must be indistinguishable from an init-created one, including
// both receive hooks.
func TestProvisionConnectedGateProducesTheSameGateShapeAsInit(t *testing.T) {
	ctx := context.Background()
	bareDir := filepath.Join(t.TempDir(), "aabbccddeeff.git")

	if err := ProvisionConnectedGate(ctx, bareDir, "https://example.test/acme/api.git"); err != nil {
		t.Fatalf("ProvisionConnectedGate: %v", err)
	}

	if !git.LooksLikeBareRepository(bareDir) {
		t.Fatal("ProvisionConnectedGate did not produce a bare repository")
	}

	// The pre-receive admission guard is what refuses a push from inside a
	// pipeline step. A connected gate is more exposed than a local one, so it
	// must never ship without it.
	for _, hook := range []string{"pre-receive", "post-receive"} {
		path := filepath.Join(bareDir, "hooks", hook)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s hook missing from a connected gate: %v", hook, err)
		}
	}

	// The gate config stamp is what stops startup migration from re-running six
	// git subprocesses per gate on every daemon restart.
	if !git.GateConfigCurrent(bareDir) {
		t.Error("connected gate is not stamped as current; startup migration would rewrite it on every restart")
	}

	// Push options must be advertised so skip/intent push options keep working
	// for a deliberate human push into a connected gate.
	out, err := git.RunBare(ctx, bareDir, "config", "--get", "receive.advertisePushOptions")
	if err != nil || out != "true" {
		t.Errorf("receive.advertisePushOptions = %q (err %v), want true", out, err)
	}

	// The credentialled URL belongs on the gate's origin, which is where the
	// pipeline recovers it from at run time.
	origin, err := git.GetRemoteURL(ctx, bareDir, "origin")
	if err != nil {
		t.Fatalf("gate origin: %v", err)
	}
	if origin != "https://example.test/acme/api.git" {
		t.Errorf("gate origin = %q, want the configured upstream", origin)
	}

	// Idempotent: provisioning again is the repair path.
	if err := ProvisionConnectedGate(ctx, bareDir, "https://example.test/acme/api.git"); err != nil {
		t.Fatalf("second ProvisionConnectedGate: %v", err)
	}
}
