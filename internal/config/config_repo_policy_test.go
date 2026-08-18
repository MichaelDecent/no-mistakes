package config

import "testing"

// TestOperatorPolicyCannotClearTrustedDisableProjectSettings is the security
// rule of this spec: the operator floor is monotone. It may only ADD the
// restriction, never remove one a repository set for itself on its own trusted
// default branch.
func TestOperatorPolicyCannotClearTrustedDisableProjectSettings(t *testing.T) {
	global := DefaultGlobalConfig()
	repo := &RepoConfig{DisableProjectSettings: true}

	for _, floor := range []RepoPolicy{{DisableProjectSettings: false}, {DisableProjectSettings: true}} {
		merged := MergeWithRepoPolicy(global, repo, floor)
		if !merged.DisableProjectSettings {
			t.Errorf("floor %+v cleared the repository's own trusted true", floor)
		}
	}
}

// TestOperatorPolicyRaisesButNeverLowers pins the full truth table of
// effective = trustedRepoValue OR operatorFloor.
func TestOperatorPolicyRaisesButNeverLowers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		repo  bool
		floor bool
		want  bool
	}{
		{"neither set", false, false, false},
		{"floor raises it", false, true, true},
		{"repo sets it alone", true, false, true},
		{"both set", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := MergeWithRepoPolicy(
				DefaultGlobalConfig(),
				&RepoConfig{DisableProjectSettings: tc.repo},
				RepoPolicy{DisableProjectSettings: tc.floor},
			)
			if merged.DisableProjectSettings != tc.want {
				t.Errorf("DisableProjectSettings = %v, want %v", merged.DisableProjectSettings, tc.want)
			}
		})
	}
}

// TestMergeEqualsMergeWithEmptyRepoPolicy pins that Merge is preserved exactly,
// which is what keeps every existing local-repository call site unchanged.
func TestMergeEqualsMergeWithEmptyRepoPolicy(t *testing.T) {
	for _, repo := range []*RepoConfig{
		{DisableProjectSettings: false},
		{DisableProjectSettings: true},
	} {
		plain := Merge(DefaultGlobalConfig(), repo)
		withEmpty := MergeWithRepoPolicy(DefaultGlobalConfig(), repo, RepoPolicy{})
		if plain.DisableProjectSettings != withEmpty.DisableProjectSettings {
			t.Errorf("Merge and MergeWithRepoPolicy(.., RepoPolicy{}) disagree for repo=%+v", repo)
		}
	}
}

// TestRepoPolicyForDefaultsConnectedReposToDisabled pins the connected default.
//
// The operator cannot edit a watched repository, so its AGENTS.md or CLAUDE.md
// is untrusted input to the agent that will run against it. The floor is
// therefore on by default for a connected repository, and off for anything the
// operator has not declared - which is every local repository.
func TestRepoPolicyForDefaultsConnectedReposToDisabled(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(
		"repos:\n  acme-api:\n    url: git@github.com:acme/api.git\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if got := global.RepoPolicyFor("acme-api"); !got.DisableProjectSettings {
		t.Error("a declared connected repo does not default to the floor being on")
	}
	// A local repository has no entry, and an empty key is how local repos are
	// looked up, so both must resolve to an empty (no-op) policy.
	for _, name := range []string{"", "not-declared"} {
		if got := global.RepoPolicyFor(name); got.DisableProjectSettings {
			t.Errorf("RepoPolicyFor(%q) turned the floor on for an undeclared repository", name)
		}
	}
}

// TestAllowProjectInstructionsLowersOnlyTheConnectedDefault pins the escape
// hatch an operator needs when their only agent has no verified suppression
// knob. It lowers the connected DEFAULT and nothing else - a repository's own
// trusted true still wins, which the first test above covers.
func TestAllowProjectInstructionsLowersOnlyTheConnectedDefault(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(
		"repos:\n  acme-api:\n    url: git@github.com:acme/api.git\n    allow_project_instructions: true\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if global.RepoPolicyFor("acme-api").DisableProjectSettings {
		t.Error("allow_project_instructions did not lower the connected default")
	}
	// It is an opt-out of the DEFAULT, never of the repository's own choice.
	merged := MergeWithRepoPolicy(
		global,
		&RepoConfig{DisableProjectSettings: true},
		global.RepoPolicyFor("acme-api"),
	)
	if !merged.DisableProjectSettings {
		t.Error("allow_project_instructions cleared the repository's own trusted true")
	}
}

// TestDisableProjectSettingsSpecRaisesTheFloor pins the other direction: an
// operator may turn the floor on explicitly for a repository.
func TestDisableProjectSettingsSpecRaisesTheFloor(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(
		"repos:\n  acme-api:\n    url: git@github.com:acme/api.git\n    disable_project_settings: true\n    allow_project_instructions: true\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	// An explicit disable_project_settings wins over allow_project_instructions:
	// the operator asked for the restriction by name.
	if !global.RepoPolicyFor("acme-api").DisableProjectSettings {
		t.Error("an explicit disable_project_settings was overridden by allow_project_instructions")
	}
}
