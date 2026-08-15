package daemon

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// TestRepoPolicyKeyIsEmptyForLocalRepos pins the property that keeps every
// existing local-repository run byte-identical: a local repo yields an empty
// key, an empty key finds no operator entry, and no entry is the zero policy.
//
// Only connected repositories are declared in configuration, so this is the
// single place where "the floor never touches local repos" is decided.
func TestRepoPolicyKeyIsEmptyForLocalRepos(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo *db.Repo
		want string
	}{
		{"nil repo", nil, ""},
		{"local repo", &db.Repo{ID: "abc", SourceKind: db.SourceKindLocal}, ""},
		{"legacy repo with no source_kind", &db.Repo{ID: "abc"}, ""},
		{
			name: "local repo that somehow carries a name",
			repo: &db.Repo{ID: "abc", SourceKind: db.SourceKindLocal, SourceName: "acme-api"},
			want: "",
		},
		{
			name: "connected repo uses its operator-chosen name",
			repo: &db.Repo{ID: "abc", SourceKind: db.SourceKindConnected, SourceName: "acme-api"},
			want: "acme-api",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoPolicyKey(tc.repo); got != tc.want {
				t.Errorf("repoPolicyKey() = %q, want %q", got, tc.want)
			}
		})
	}
}
