package config

import (
	"strings"
	"testing"
)

// TestStoredProvenanceBytesNeverContainUserinfo is the end-to-end version of the
// credential-spill regression, asserted on the exact byte slices the executor
// hands to the database rather than only on the config field.
//
// The spill path is: config.yaml bytes -> GlobalConfig.SourceYAML ->
// EnableEvalProvenance -> Config.ReplayGlobalYAML and Config.ReplayRepoYAML ->
// InsertReviewStepRoundWithProvenance(globalConfigYAML, repoConfigYAML) ->
// step_rounds, and from there onto disk via eval capture. Both provenance and
// auto-capture default on, and this runs on EVERY review round, so a credential
// that survives to this point is written repeatedly and durably.
//
// This test lives in internal/config rather than internal/db because config is
// where the redaction is owned and where a regression would be introduced; the db
// layer stores whatever bytes it is given by design.
func TestStoredProvenanceBytesNeverContainUserinfo(t *testing.T) {
	const token = "ghp_provenance_secret"
	rawGlobal := "log_level: info\n" +
		"acpx_path: https://" + token + "@example.test/acpx\n"

	global, err := LoadGlobalFromBytes([]byte(rawGlobal))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}

	// The repository half of provenance is re-marshalled from the parsed struct
	// rather than copied from bytes, so it can only carry a credential if one
	// reached a RepoConfig field. Cover it anyway: this is the pair of slices
	// that actually gets persisted together.
	repo := &RepoConfig{}
	merged := Merge(global, repo)
	if err := merged.EnableEvalProvenance(global, repo); err != nil {
		t.Fatalf("EnableEvalProvenance: %v", err)
	}

	for name, payload := range map[string][]byte{
		"global_config_yaml": merged.ReplayGlobalYAML,
		"repo_config_yaml":   merged.ReplayRepoYAML,
	} {
		if strings.Contains(string(payload), token) {
			t.Errorf("%s carries the credential and would be written to step_rounds on every review round:\n%s", name, payload)
		}
		// Redaction replaces userinfo with the literal "redacted" rather than
		// deleting it, matching how every other redaction site in the project
		// renders a sanitized URL. So userinfo may still be present - it just
		// must be the placeholder and never the original secret.
		if strings.Contains(string(payload), "@example.test") && !strings.Contains(string(payload), "redacted@example.test") {
			t.Errorf("%s has userinfo that is not the redaction placeholder:\n%s", name, payload)
		}
	}

	// Provenance must remain useful after redaction: the host has to survive, or
	// a captured case can no longer say what it was configured against.
	if !strings.Contains(string(merged.ReplayGlobalYAML), "example.test") {
		t.Errorf("redaction removed the host from provenance:\n%s", merged.ReplayGlobalYAML)
	}
	if !merged.CaptureEvalProvenance {
		t.Error("CaptureEvalProvenance = false after EnableEvalProvenance")
	}
}

// TestProvenanceIsUnchangedForCredentialFreeConfig pins that the redaction does
// not perturb provenance for the ordinary case, so existing captured cases and
// newly captured ones stay byte-comparable.
func TestProvenanceIsUnchangedForCredentialFreeConfig(t *testing.T) {
	raw := "log_level: info\nsession_reuse: true\n"
	global, err := LoadGlobalFromBytes([]byte(raw))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	repo := &RepoConfig{}
	merged := Merge(global, repo)
	if err := merged.EnableEvalProvenance(global, repo); err != nil {
		t.Fatalf("EnableEvalProvenance: %v", err)
	}
	if string(merged.ReplayGlobalYAML) != raw {
		t.Errorf("provenance bytes changed for credential-free config.\n got: %q\nwant: %q", merged.ReplayGlobalYAML, raw)
	}
}
