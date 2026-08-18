package config

import (
	"strings"
	"testing"
)

// TestGlobalSourceYAMLRedactsCredentials is the regression for a credential
// spill this feature would otherwise introduce.
//
// GlobalConfig.SourceYAML holds the bytes of config.yaml. EnableEvalProvenance
// copies them into Config.ReplayGlobalYAML, the executor writes that into
// step_rounds.global_config_yaml on EVERY review round, and eval capture copies
// cases onto disk under the application root. Both eval.capture_provenance and
// eval.auto_capture default to true, so the path is live by default.
//
// Connecting repositories by remote URL makes it normal for config.yaml to name
// a URL, and operators embed tokens in URLs. Without redaction here, one token in
// config.yaml is persisted verbatim into SQLite and the on-disk corpus, over and
// over. The token must never reach SourceYAML in the first place.
func TestGlobalSourceYAMLRedactsCredentials(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		secret string
	}{
		{
			name:   "https token in a repo url",
			yaml:   "log_level: info\nrepos:\n  acme-api:\n    url: https://ghp_supersecrettoken@github.com/acme/api.git\n",
			secret: "ghp_supersecrettoken",
		},
		{
			name:   "user and password",
			yaml:   "repos:\n  acme:\n    url: https://michael:hunter2@github.com/acme/api.git\n",
			secret: "hunter2",
		},
		{
			name:   "quoted scalar",
			yaml:   "repos:\n  acme:\n    url: \"https://ghp_quoted@github.com/acme/api.git\"\n",
			secret: "ghp_quoted",
		},
		{
			name:   "single quoted scalar",
			yaml:   "repos:\n  acme:\n    url: 'https://ghp_single@github.com/acme/api.git'\n",
			secret: "ghp_single",
		},
		{
			name:   "fork url",
			yaml:   "repos:\n  acme:\n    fork_url: https://ghp_forktoken@github.com/me/api.git\n",
			secret: "ghp_forktoken",
		},
		{
			name:   "credential outside any repos block",
			yaml:   "acpx_path: https://ghp_elsewhere@example.test/tool\n",
			secret: "ghp_elsewhere",
		},
		{
			name:   "ssh url carrying a password",
			yaml:   "repos:\n  acme:\n    url: ssh://michael:sshsecret@github.com/acme/api.git\n",
			secret: "sshsecret",
		},
		{
			name:   "several credentials in one document",
			yaml:   "repos:\n  a:\n    url: https://tok_one@github.com/a/a.git\n  b:\n    url: https://tok_two@github.com/b/b.git\n",
			secret: "tok_one",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := string(RedactConfigSource([]byte(tc.yaml)))
			if strings.Contains(stored, tc.secret) {
				t.Errorf("secret %q survives into stored config bytes:\n%s", tc.secret, stored)
			}
			// The host and path must survive, or provenance loses the ability to
			// say which repository a captured round belonged to.
			if strings.Contains(tc.yaml, "github.com") && !strings.Contains(stored, "github.com") {
				t.Errorf("redaction removed the host as well as the credential:\n%s", stored)
			}
		})
	}

	// Every secret in the multi-credential document must go, not just the first.
	multi := "repos:\n  a:\n    url: https://tok_one@github.com/a/a.git\n  b:\n    url: https://tok_two@github.com/b/b.git\n"
	stored := string(RedactConfigSource([]byte(multi)))
	for _, secret := range []string{"tok_one", "tok_two"} {
		if strings.Contains(stored, secret) {
			t.Errorf("secret %q survived a multi-credential document:\n%s", secret, stored)
		}
	}
}

// TestRedactConfigSourceLeavesCredentialFreeConfigByteIdentical pins that
// redaction is a no-op for the overwhelmingly common case. Rewriting a document
// that contains no credential would churn provenance bytes for every user and
// make a real redaction harder to notice in a diff.
func TestRedactConfigSourceLeavesCredentialFreeConfigByteIdentical(t *testing.T) {
	inputs := []string{
		"",
		"{}\n",
		"agent: claude\nlog_level: debug\n",
		// A credential-free URL keeps its userinfo-less form exactly.
		"repos:\n  acme:\n    url: https://github.com/acme/api.git\n",
		// An ssh remote whose user is the conventional, non-secret "git".
		"repos:\n  acme:\n    url: git@github.com:acme/api.git\n",
		// Comments and formatting must survive untouched.
		"# operator notes\nagent: codex   # trailing comment\n\nlog_level: info\n",
		// A bare email address is not userinfo. Connected repositories carry a
		// commit identity, so an "@" in the document is ordinary and must not
		// trigger a rewrite.
		"repos:\n  acme:\n    commit_email: qa@example.com\n",
		// The hard case: an email on a later line must not be swallowed as the
		// userinfo of a credential-free URL on an earlier one.
		"repos:\n  acme:\n    url: https://github.com/acme/api.git\n    commit_email: qa@example.com\n",
		defaultConfigYAML,
	}
	for _, in := range inputs {
		if got := string(RedactConfigSource([]byte(in))); got != in {
			t.Errorf("RedactConfigSource rewrote credential-free config.\n got: %q\nwant: %q", got, in)
		}
	}
}

// TestRedactConfigSourcePreservesParseability pins that a redacted document is
// still loadable. Eval capture calls LoadGlobalFromBytes on the stored bytes to
// validate them, so redaction that produced unparseable YAML would turn every
// capture into a failure instead of a clean skip.
func TestRedactConfigSourcePreservesParseability(t *testing.T) {
	raw := "log_level: info\nsession_reuse: false\nci_timeout: 48h\n"
	redacted := RedactConfigSource([]byte(raw))
	cfg, err := LoadGlobalFromBytes(redacted)
	if err != nil {
		t.Fatalf("redacted config no longer parses: %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.SessionReuse {
		t.Error("SessionReuse = true, want false: redaction changed a parsed value")
	}
}

// TestLoadGlobalFromBytesStoresRedactedSourceYAML pins the wiring, not just the
// helper: the redaction must happen where SourceYAML is assigned, so no caller
// can obtain unredacted bytes.
func TestLoadGlobalFromBytesStoresRedactedSourceYAML(t *testing.T) {
	raw := "log_level: info\nacpx_path: https://ghp_wired@example.test/acpx\n"
	cfg, err := LoadGlobalFromBytes([]byte(raw))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if strings.Contains(string(cfg.SourceYAML), "ghp_wired") {
		t.Fatalf("SourceYAML carries the credential:\n%s", cfg.SourceYAML)
	}
	if !strings.Contains(string(cfg.SourceYAML), "example.test") {
		t.Errorf("SourceYAML lost the host along with the credential:\n%s", cfg.SourceYAML)
	}
}

// TestReplayGlobalYAMLNeverCarriesUserinfo follows the value all the way to the
// field the executor persists, which is the byte sequence that actually lands in
// step_rounds.global_config_yaml and in the on-disk eval corpus.
func TestReplayGlobalYAMLNeverCarriesUserinfo(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("log_level: info\nacpx_path: https://ghp_replay@example.test/acpx\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	repo := &RepoConfig{}
	merged := Merge(global, repo)
	if err := merged.EnableEvalProvenance(global, repo); err != nil {
		t.Fatalf("EnableEvalProvenance: %v", err)
	}
	if strings.Contains(string(merged.ReplayGlobalYAML), "ghp_replay") {
		t.Fatalf("ReplayGlobalYAML carries the credential; it is written to step_rounds on every review round:\n%s", merged.ReplayGlobalYAML)
	}
}
