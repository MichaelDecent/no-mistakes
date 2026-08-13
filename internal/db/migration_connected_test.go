package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// legacyReposSchema is the repos table as it shipped before connected
// repositories existed. The QA migration has to land on top of exactly this.
const legacyReposSchema = `
CREATE TABLE IF NOT EXISTS repos (
    id             TEXT PRIMARY KEY,
    working_path   TEXT NOT NULL UNIQUE,
    upstream_url   TEXT NOT NULL,
    fork_url       TEXT,
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
    id         TEXT PRIMARY KEY,
    repo_id    TEXT NOT NULL,
    branch     TEXT NOT NULL,
    head_sha   TEXT NOT NULL,
    base_sha   TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
`

// openLegacyThenMigrate builds a database at the pre-QA schema, inserts a repo
// row through raw SQL so it carries no new columns at all, then reopens it
// through Open so the real migration path runs against it.
func openLegacyThenMigrate(t *testing.T) (string, *DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := raw.Exec(legacyReposSchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at) VALUES (?, ?, ?, ?, ?)`,
		"aabbccddeeff", "/home/dev/my-repo", "https://example.test/acme/api.git", "main", 1,
	); err != nil {
		t.Fatalf("insert legacy repo: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("Open on legacy database: %v", err)
	}
	t.Cleanup(func() { migrated.Close() })
	return path, migrated
}

// TestUniqueSourceIdentityIndexAppliesToUpgradedDatabases pins the migration
// ORDERING. schemaSQL's CREATE TABLE IF NOT EXISTS is a no-op on an existing
// database, so a unique index on source_identity placed there would run before
// the column existed and Open would hard-fail for every installed user - the
// migration loop tolerates only "duplicate column name". The index therefore has
// to be the LAST entry in migrationStatements, after the ADD COLUMN lines.
func TestUniqueSourceIdentityIndexAppliesToUpgradedDatabases(t *testing.T) {
	_, d := openLegacyThenMigrate(t)

	var indexSQL string
	err := d.sql.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_repos_source_identity'`,
	).Scan(&indexSQL)
	if err != nil {
		t.Fatalf("unique source_identity index missing after upgrade: %v", err)
	}
	if indexSQL == "" {
		t.Fatal("index exists but has no SQL definition")
	}

	// The index must actually be unique, not merely present.
	if _, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at, source_kind, source_identity)
		 VALUES ('111111111111', '/a', 'u', 'main', 1, 'connected', 'example.test/acme/api')`,
	); err != nil {
		t.Fatalf("first connected insert: %v", err)
	}
	if _, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at, source_kind, source_identity)
		 VALUES ('222222222222', '/b', 'u', 'main', 1, 'connected', 'example.test/acme/api')`,
	); err == nil {
		t.Fatal("two repos share one source_identity; the unique index is not enforcing")
	}
}

// TestOpenIsIdempotentAcrossRepeatedMigrations pins that a second and third
// Open of an already-migrated database succeeds. Without a version table, that
// property rests entirely on IF NOT EXISTS plus the duplicate-column tolerance,
// so it is worth asserting directly rather than assuming.
func TestOpenIsIdempotentAcrossRepeatedMigrations(t *testing.T) {
	path, first := openLegacyThenMigrate(t)
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	for i := 0; i < 3; i++ {
		again, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d on migrated database: %v", i+2, err)
		}
		if err := again.Close(); err != nil {
			t.Fatalf("close #%d: %v", i+2, err)
		}
	}
}

// TestUpgradedRepoRowReadsBackAsLocal pins the backward-compatibility contract:
// a repo registered before connected repositories existed must read back as a
// local repo, so every author-side code path keeps its current behaviour.
func TestUpgradedRepoRowReadsBackAsLocal(t *testing.T) {
	_, d := openLegacyThenMigrate(t)

	repo, err := d.GetRepo("aabbccddeeff")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo == nil {
		t.Fatal("legacy repo row disappeared after migration")
	}
	if repo.SourceKind != SourceKindLocal {
		t.Errorf("SourceKind = %q, want %q", repo.SourceKind, SourceKindLocal)
	}
	if repo.Connected() {
		t.Error("Connected() = true for a repo registered by init; every author-side guard would wrongly refuse it")
	}
	if repo.SourceIdentity != "" || repo.SourceName != "" {
		t.Errorf("SourceIdentity=%q SourceName=%q, want empty for a legacy row", repo.SourceIdentity, repo.SourceName)
	}
	if repo.DetachedAt != nil {
		t.Error("DetachedAt is non-nil for a legacy row; it must never be backfilled")
	}
	if repo.WorkingPath != "/home/dev/my-repo" {
		t.Errorf("WorkingPath = %q, want the original path", repo.WorkingPath)
	}
}

// TestGetReposReadsConnectedColumnsForBothKinds pins that the listing path -
// which the repos CLI and reconciliation both use - carries the new columns for
// a connected row and safe zero values for a local one.
func TestGetReposReadsConnectedColumnsForBothKinds(t *testing.T) {
	_, d := openLegacyThenMigrate(t)

	if _, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at, source_kind, source_identity, source_name)
		 VALUES ('333333333333', '/nm/sources/333333333333', 'https://example.test/acme/web.git', 'trunk', 2, 'connected', 'example.test/acme/web', 'acme-web')`,
	); err != nil {
		t.Fatalf("insert connected repo: %v", err)
	}

	repos, err := d.GetRepos()
	if err != nil {
		t.Fatalf("GetRepos: %v", err)
	}
	byID := make(map[string]*Repo, len(repos))
	for _, r := range repos {
		byID[r.ID] = r
	}

	local, ok := byID["aabbccddeeff"]
	if !ok {
		t.Fatal("local repo missing from GetRepos")
	}
	if local.Connected() {
		t.Error("local repo reports Connected()")
	}

	connected, ok := byID["333333333333"]
	if !ok {
		t.Fatal("connected repo missing from GetRepos")
	}
	if !connected.Connected() {
		t.Error("connected repo does not report Connected()")
	}
	if connected.SourceIdentity != "example.test/acme/web" {
		t.Errorf("SourceIdentity = %q", connected.SourceIdentity)
	}
	if connected.SourceName != "acme-web" {
		t.Errorf("SourceName = %q", connected.SourceName)
	}
}
