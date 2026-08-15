package config

import (
	"strings"
	"testing"
)

// TestRepoConfigCannotDeclareConnectedRepos pins the `repos:` block as
// operator-only, mirroring TestRepoConfigCannotChangeEvalCollection.
//
// The block names repositories this daemon will fetch, provision gates for, and
// run agents against. A pushed branch declaring one would make a repository
// register itself, so parseRepoConfig must load and ignore the key while only
// config.yaml can declare it.
func TestRepoConfigCannotDeclareConnectedRepos(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(
		"repos:\n  acme-api:\n    url: git@github.com:acme/api.git\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte(
		"repos:\n  attacker:\n    url: git@github.com:attacker/evil.git\n",
	))
	if err != nil {
		t.Fatalf("repo config with a repos block must load, ignoring the key: %v", err)
	}

	if len(global.Repos) != 1 {
		t.Fatalf("global.Repos = %#v, want exactly the operator's one entry", global.Repos)
	}
	if _, ok := global.Repos["acme-api"]; !ok {
		t.Errorf("global.Repos is missing the operator's entry: %#v", global.Repos)
	}
	if _, ok := global.Repos["attacker"]; ok {
		t.Error("the repository's entry reached the global configuration")
	}

	// Merge must not carry the block onto the per-run Config at all: a run acts
	// on one repository and never needs the operator's whole inventory.
	merged := Merge(global, repo)
	if merged == nil {
		t.Fatal("Merge returned nil")
	}
}

// TestGlobalReposParseFullSpec pins every field round-tripping, so a later
// change that drops one is caught rather than silently ignoring operator input.
func TestGlobalReposParseFullSpec(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`repos:
  acme-api:
    url: git@github.com:acme/api.git
    credential_env: ACME_TOKEN
    default_branch: develop
    commit_name: QA Bot
    commit_email: qa@example.com
    disable_project_settings: true
`))
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := global.Repos["acme-api"]
	if !ok {
		t.Fatalf("Repos = %#v, missing acme-api", global.Repos)
	}
	for _, tc := range []struct{ field, got, want string }{
		{"URL", spec.URL, "git@github.com:acme/api.git"},
		{"CredentialEnv", spec.CredentialEnv, "ACME_TOKEN"},
		{"DefaultBranch", spec.DefaultBranch, "develop"},
		{"CommitName", spec.CommitName, "QA Bot"},
		{"CommitEmail", spec.CommitEmail, "qa@example.com"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
	if !spec.DisableProjectSettings {
		t.Error("DisableProjectSettings = false, want true")
	}
}

// TestGlobalReposValidationRejectsUnsafeEntries covers the parse-time rules.
// A name reaches a filesystem path through the gate and stub directories, so
// path-escaping names are rejected outright rather than sanitized.
func TestGlobalReposValidationRejectsUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "empty name",
			yaml: "repos:\n  \"\":\n    url: git@github.com:acme/api.git\n",
			want: "name",
		},
		{
			name: "dot name cannot reach a path",
			yaml: "repos:\n  \".\":\n    url: git@github.com:acme/api.git\n",
			want: "name",
		},
		{
			name: "dotdot name cannot reach a path",
			yaml: "repos:\n  \"..\":\n    url: git@github.com:acme/api.git\n",
			want: "name",
		},
		{
			name: "separator in name",
			yaml: "repos:\n  \"acme/api\":\n    url: git@github.com:acme/api.git\n",
			want: "name",
		},
		{
			name: "uppercase name",
			yaml: "repos:\n  Acme:\n    url: git@github.com:acme/api.git\n",
			want: "name",
		},
		{
			name: "name starting with punctuation",
			yaml: "repos:\n  \"-acme\":\n    url: git@github.com:acme/api.git\n",
			want: "name",
		},
		{
			name: "missing url",
			yaml: "repos:\n  acme-api:\n    default_branch: main\n",
			want: "url",
		},
		{
			name: "url with a control character",
			yaml: "repos:\n  acme-api:\n    url: \"git@github.com:acme/api.git\\u0007\"\n",
			want: "url",
		},
		{
			name: "fork_url is out of scope for connected repos",
			yaml: "repos:\n  acme-api:\n    url: git@github.com:acme/api.git\n    fork_url: git@github.com:me/api.git\n",
			want: "fork_url",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("LoadGlobalFromBytes accepted %q; it must fail closed", tc.yaml)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestGlobalReposAcceptsValidNames guards against a charset rule so strict it
// rejects ordinary repository names.
func TestGlobalReposAcceptsValidNames(t *testing.T) {
	for _, name := range []string{"api", "acme-api", "acme_api", "acme.api", "api2", "0api"} {
		t.Run(name, func(t *testing.T) {
			yaml := "repos:\n  " + name + ":\n    url: git@github.com:acme/api.git\n"
			global, err := LoadGlobalFromBytes([]byte(yaml))
			if err != nil {
				t.Fatalf("valid name %q rejected: %v", name, err)
			}
			if _, ok := global.Repos[name]; !ok {
				t.Errorf("Repos = %#v, missing %q", global.Repos, name)
			}
		})
	}
}

// TestGlobalReposAbsentByDefault pins that not declaring the block leaves no
// entries, so a daemon with no operator configuration connects to nothing.
func TestGlobalReposAbsentByDefault(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("log_level: debug\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(global.Repos) != 0 {
		t.Errorf("Repos = %#v, want empty when the block is absent", global.Repos)
	}
}

// TestGlobalReposCredentialInURLIsRedactedFromSourceYAML extends the S2
// guarantee to the block that makes operators most likely to paste a token.
func TestGlobalReposCredentialInURLIsRedactedFromSourceYAML(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(
		"repos:\n  acme-api:\n    url: https://ghp_secrettoken@github.com/acme/api.git\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(global.SourceYAML), "ghp_secrettoken") {
		t.Errorf("SourceYAML retains the credential:\n%s", global.SourceYAML)
	}
	// The parsed value keeps the credential: it is what authenticates the fetch.
	// Only the persisted provenance bytes are redacted.
	if !strings.Contains(global.Repos["acme-api"].URL, "ghp_secrettoken") {
		t.Errorf("parsed URL lost its credential: %q", global.Repos["acme-api"].URL)
	}
}
