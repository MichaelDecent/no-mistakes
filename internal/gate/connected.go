package gate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
)

// connectedIDDomain separates the connected-repository ID namespace from the
// working-clone one. Local repo IDs are sha256 of an absolute filesystem path;
// connected IDs are sha256 of a remote identity. Without a domain prefix the two
// would be each other's preimages, and a crafted path or URL could be made to
// collide deliberately rather than only by 48-bit accident.
const connectedIDDomain = "no-mistakes/connected/v1\x00"

// RemoteIdentity returns the canonical, credential-free identity of a remote
// URL: "<host>/<path>", lowercased, with userinfo and port stripped and one
// trailing ".git" removed.
//
// It is the stable identity of a connected repository across transports and
// credential rotation, so these all resolve to "github.com/acme/api":
//
//	https://token@github.com/Acme/API.git
//	git@github.com:acme/api.git
//	ssh://git@github.com:2222/acme/api
//
// The implementation delegates to inspectRefreshRemote, which already performs
// exactly this canonicalization for the URL-refresh path, so there is one
// normalizer rather than two that could disagree about what counts as the same
// repository.
//
// It keys off a non-empty identity rather than a nil error on purpose.
// inspectRefreshRemote reports "credential-bearing remote" as an ERROR while
// still populating the identity, because for its own caller a credential in a
// discovered clone remote is a reason to refuse. Here the opposite is true: an
// operator's configured URL routinely carries a token, and that URL is exactly
// what we must be able to identify.
func RemoteIdentity(raw string) (string, error) {
	info, err := inspectRefreshRemote(raw)
	if strings.TrimSpace(info.identity) != "" {
		return info.identity, nil
	}
	if err == nil {
		err = fmt.Errorf("invalid remote")
	}
	return "", err
}

// ConnectedRepoID derives the stable repository ID for a canonical remote
// identity.
//
// The result has the same shape as a working-clone repo ID - 12 lowercase hex
// characters - so every existing consumer keeps working unchanged: the gate
// directory is still "<id>.git", startup gate migration still recognizes it, the
// post-receive hook still resolves a repo from the gate path, and the gate-context
// classifier still trims the ".git" suffix.
//
// The ID is derived from the remote identity and NOT from the operator-chosen
// name, for two reasons. A name could contain characters that escape the repos
// directory when joined into a path. And deriving from identity means renaming a
// configuration entry preserves the gate and every run row, while repointing an
// entry at a genuinely different repository correctly produces a different ID.
func ConnectedRepoID(identity string) string {
	h := sha256.Sum256([]byte(connectedIDDomain + strings.TrimSpace(identity)))
	return fmt.Sprintf("%x", h[:6])
}

// ProvisionConnectedGate creates or repairs the bare gate for a repository
// connected by remote URL.
//
// It is provisionGate without the working-clone half: there is no developer
// checkout to attach a "no-mistakes" remote to. Everything else is identical, so
// startup gate migration, the gate-config stamp, and the gate-context classifier
// see exactly one gate shape. See provisionGateCore for why both receive hooks
// are still installed on a gate nothing is ever pushed to.
//
// upstreamURL may carry a credential. It is written only to the gate's own origin
// remote, which is where the pipeline recovers it from at run time; callers must
// persist a redacted copy to the database instead.
func ProvisionConnectedGate(ctx context.Context, bareDir, upstreamURL string) error {
	return provisionGateCore(ctx, bareDir, upstreamURL)
}
