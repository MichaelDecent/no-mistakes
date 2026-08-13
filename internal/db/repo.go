package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// Source kinds distinguish how a repository came to be registered.
const (
	// SourceKindLocal is a repository registered by `no-mistakes init` inside
	// the operator's own working clone. Every repo recorded before connected
	// repositories existed is one, which is why the column defaults to it.
	SourceKindLocal = "local"
	// SourceKindConnected is a repository registered from the operator's
	// declarative configuration by remote URL, with no working clone. Its
	// working_path is a daemon-owned identity stub, not a checkout.
	SourceKindConnected = "connected"
)

// repoColumns is the single authoritative read list for the repos table. Every
// query that scans a Repo uses it together with scanRepo, so a column added to
// the table is added in exactly one place rather than in four diverging SELECTs.
// Nullable columns are coalesced so a row written before they existed scans into
// a plain string rather than failing.
const repoColumns = `id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, created_at, COALESCE(source_kind, 'local'), COALESCE(source_identity, ''), COALESCE(source_name, ''), detached_at`

// rowScanner is the intersection of *sql.Row and *sql.Rows that scanRepo needs,
// so one scan helper serves both single-row and iterating queries.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRepo(s rowScanner) (*Repo, error) {
	r := &Repo{}
	if err := s.Scan(
		&r.ID, &r.WorkingPath, &r.UpstreamURL, &r.ForkURL, &r.DefaultBranch, &r.CreatedAt,
		&r.SourceKind, &r.SourceIdentity, &r.SourceName, &r.DetachedAt,
	); err != nil {
		return nil, err
	}
	return r, nil
}

// Repo represents a registered repository.
type Repo struct {
	ID            string
	WorkingPath   string
	UpstreamURL   string
	ForkURL       string
	DefaultBranch string
	CreatedAt     int64

	// SourceKind is SourceKindLocal or SourceKindConnected. It reads back as
	// SourceKindLocal for every repository registered before connected
	// repositories existed, so author-side behaviour is unchanged by default.
	SourceKind string
	// SourceIdentity is the canonical credential-free "<host>/<path>" identity
	// of a connected repository's remote, and empty for a local one. The repo ID
	// is derived from it, so it is stable across transports and credential
	// rotation.
	SourceIdentity string
	// SourceName is the operator-chosen handle for a connected repository: the
	// CLI selector and display label. It is deliberately not part of the ID, so
	// renaming an entry preserves the gate and every run row.
	SourceName string
	// DetachedAt is set when a connected repository is no longer listed in the
	// operator's configuration. Its gate and run history are deliberately kept,
	// so re-adding the same URL reuses this record. Nil for a listed connected
	// repository and for every local one, and never backfilled.
	DetachedAt *int64

	// URLsVerified is run-scoped, in-memory evidence that the URL fields were
	// just validated against the working clone. It is never persisted.
	URLsVerified bool `json:"-"`
}

// Connected reports whether this repository is daemon-owned and registered by
// remote URL rather than by `init` in a working clone.
//
// It is the single predicate every author-side guard reads. A connected
// repository has no operator checkout, so branch synchronization, custody
// recovery, cwd-based repo resolution, eject, and the local-default-branch
// bundling check are all meaningless (and in some cases actively wrong) for one.
func (r *Repo) Connected() bool {
	return r != nil && r.SourceKind == SourceKindConnected
}

// PushURL returns the remote URL that should receive branch updates.
func (r *Repo) PushURL() string {
	if r == nil {
		return ""
	}
	if strings.TrimSpace(r.ForkURL) != "" {
		return r.ForkURL
	}
	return r.UpstreamURL
}

// InsertRepoWithID creates a new repo record with a caller-provided ID.
func (d *DB) InsertRepoWithID(id, workingPath, upstreamURL, defaultBranch string) (*Repo, error) {
	return d.InsertRepoWithIDAndFork(id, workingPath, upstreamURL, "", defaultBranch)
}

// InsertRepoWithIDAndFork creates a repo record with an optional fork push URL.
func (d *DB) InsertRepoWithIDAndFork(id, workingPath, upstreamURL, forkURL, defaultBranch string) (*Repo, error) {
	r := &Repo{
		ID:            id,
		WorkingPath:   workingPath,
		UpstreamURL:   upstreamURL,
		ForkURL:       strings.TrimSpace(forkURL),
		DefaultBranch: defaultBranch,
		CreatedAt:     now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, fork_url, default_branch, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkingPath, r.UpstreamURL, nullableString(r.ForkURL), r.DefaultBranch, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert repo: %w", err)
	}
	return r, nil
}

// InsertRepo creates a new repo record and returns it with a generated ID.
func (d *DB) InsertRepo(workingPath, upstreamURL, defaultBranch string) (*Repo, error) {
	return d.InsertRepoWithFork(workingPath, upstreamURL, "", defaultBranch)
}

// InsertRepoWithFork creates a new repo record with an optional fork push URL.
func (d *DB) InsertRepoWithFork(workingPath, upstreamURL, forkURL, defaultBranch string) (*Repo, error) {
	r := &Repo{
		ID:            newID(),
		WorkingPath:   workingPath,
		UpstreamURL:   upstreamURL,
		ForkURL:       strings.TrimSpace(forkURL),
		DefaultBranch: defaultBranch,
		CreatedAt:     now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, fork_url, default_branch, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkingPath, r.UpstreamURL, nullableString(r.ForkURL), r.DefaultBranch, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert repo: %w", err)
	}
	return r, nil
}

// GetRepos returns every authoritative repository record ordered by ID.
func (d *DB) GetRepos() ([]*Repo, error) {
	rows, err := d.sql.Query(`SELECT ` + repoColumns + ` FROM repos ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("get repos: %w", err)
	}
	defer rows.Close()

	var repos []*Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repos: %w", err)
	}
	return repos, nil
}

// GetRepo returns a repo by ID.
func (d *DB) GetRepo(id string) (*Repo, error) {
	r, err := scanRepo(d.sql.QueryRow(`SELECT `+repoColumns+` FROM repos WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	return r, nil
}

// GetRepoByPath returns a repo by its working path.
func (d *DB) GetRepoByPath(workingPath string) (*Repo, error) {
	r, err := scanRepo(d.sql.QueryRow(`SELECT `+repoColumns+` FROM repos WHERE working_path = ?`, workingPath))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo by path: %w", err)
	}
	return r, nil
}

// GetRepoBySourceIdentity returns the connected repository registered for a
// canonical remote identity, or nil when none is. The unique index on
// source_identity guarantees at most one.
func (d *DB) GetRepoBySourceIdentity(identity string) (*Repo, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil, nil
	}
	r, err := scanRepo(d.sql.QueryRow(`SELECT `+repoColumns+` FROM repos WHERE source_identity = ?`, identity))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo by source identity: %w", err)
	}
	return r, nil
}

// GetRepoBySourceName returns the connected repository registered under an
// operator-chosen handle, or nil when none is.
func (d *DB) GetRepoBySourceName(name string) (*Repo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	r, err := scanRepo(d.sql.QueryRow(`SELECT `+repoColumns+` FROM repos WHERE source_name = ?`, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo by source name: %w", err)
	}
	return r, nil
}

// InsertConnectedRepo registers a repository connected by remote URL.
//
// workingPath is a daemon-owned identity stub, NOT a clone: the objects live in
// the bare gate. The stub exists so a connected repository has a unique
// daemon-owned working_path satisfying the table's NOT NULL UNIQUE constraint
// without a table rebuild, and so there is somewhere to pin the commit identity
// each run worktree copies.
//
// upstreamURL must already be redacted by the caller. The full credentialled URL
// belongs only on the bare gate's own origin remote, from which the pipeline
// recovers it at run time.
func (d *DB) InsertConnectedRepo(id, workingPath, redactedUpstreamURL, sourceIdentity, sourceName, defaultBranch string) (*Repo, error) {
	r := &Repo{
		ID:             id,
		WorkingPath:    workingPath,
		UpstreamURL:    redactedUpstreamURL,
		DefaultBranch:  defaultBranch,
		CreatedAt:      now(),
		SourceKind:     SourceKindConnected,
		SourceIdentity: strings.TrimSpace(sourceIdentity),
		SourceName:     strings.TrimSpace(sourceName),
	}
	_, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, fork_url, default_branch, created_at, source_kind, source_identity, source_name)
		 VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkingPath, r.UpstreamURL, r.DefaultBranch, r.CreatedAt,
		r.SourceKind, r.SourceIdentity, r.SourceName,
	)
	if err != nil {
		return nil, fmt.Errorf("insert connected repo: %w", err)
	}
	return r, nil
}

// UpdateConnectedRepoSpec refreshes the mutable, non-URL fields of a connected
// repository and clears any detached marker, so re-adding a previously removed
// entry reuses this record along with its gate and run history.
//
// It deliberately does not touch upstream_url or fork_url: those carry the
// redaction invariant that the stored URL must match the gate remote's redaction,
// and they are owned by the URL refresh path alone.
func (d *DB) UpdateConnectedRepoSpec(id, sourceName, defaultBranch string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET source_name = ?, default_branch = ?, detached_at = NULL WHERE id = ?`,
		strings.TrimSpace(sourceName), defaultBranch, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update connected repo spec: %w", err)
	}
	return d.GetRepo(id)
}

// SetRepoDetachedAt marks a connected repository as no longer listed in the
// operator's configuration, or clears the marker when at is nil.
//
// Detaching is deliberately not deletion. Reconciliation must never destroy a
// gate or run history just because a configuration entry was edited or removed,
// so it records the fact and stops scheduling work; removal is an explicit,
// separate operator command.
func (d *DB) SetRepoDetachedAt(id string, at *int64) error {
	var value any
	if at != nil {
		value = *at
	}
	_, err := d.sql.Exec(`UPDATE repos SET detached_at = ? WHERE id = ?`, value, id)
	if err != nil {
		return fmt.Errorf("set repo detached_at: %w", err)
	}
	return nil
}

// ReplaceRepoURLs atomically replaces both registered repository URLs and
// returns the committed record. A failure leaves the exact prior registration
// intact.
func (d *DB) ReplaceRepoURLs(id, upstreamURL, forkURL string) (*Repo, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin repo URL replacement: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE repos SET upstream_url = ?, fork_url = ? WHERE id = ?`,
		upstreamURL, nullableString(forkURL), id,
	)
	if err != nil {
		return nil, fmt.Errorf("replace repo URLs: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("replace repo URLs rows affected: %w", err)
	} else if affected != 1 {
		return nil, fmt.Errorf("replace repo URLs: repository not found")
	}

	r, err := scanRepo(tx.QueryRow(`SELECT `+repoColumns+` FROM repos WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("read replaced repo URLs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit repo URL replacement: %w", err)
	}
	return r, nil
}

// UpdateRepoMetadata refreshes mutable repository metadata while preserving the
// stable repo ID, created_at timestamp, and any existing fork push URL.
func (d *DB) UpdateRepoMetadata(id, upstreamURL, defaultBranch string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET upstream_url = ?, default_branch = ? WHERE id = ?`,
		upstreamURL, defaultBranch, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo metadata: %w", err)
	}
	return d.GetRepo(id)
}

// UpdateRepoMetadataWithFork refreshes repo metadata and explicitly sets the
// optional fork push URL.
func (d *DB) UpdateRepoMetadataWithFork(id, upstreamURL, forkURL, defaultBranch string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET upstream_url = ?, fork_url = ?, default_branch = ? WHERE id = ?`,
		upstreamURL, nullableString(forkURL), defaultBranch, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo metadata: %w", err)
	}
	return d.GetRepo(id)
}

// UpdateRepoForkURL sets or clears the optional fork push URL.
func (d *DB) UpdateRepoForkURL(id, forkURL string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET fork_url = ? WHERE id = ?`,
		nullableString(forkURL), id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo fork URL: %w", err)
	}
	return d.GetRepo(id)
}

// UpdateRepoWorkingPath moves a repo record to a new working path, preserving
// the repo ID (and with it the gate and run history) when the working
// directory is renamed or moved on disk.
func (d *DB) UpdateRepoWorkingPath(id, workingPath string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET working_path = ? WHERE id = ?`,
		workingPath, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo working path: %w", err)
	}
	return d.GetRepo(id)
}

// DeleteRepo deletes a repo by ID (cascade deletes runs and steps).
func (d *DB) DeleteRepo(id string) error {
	_, err := d.sql.Exec(`DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	return nil
}

func nullableString(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}
