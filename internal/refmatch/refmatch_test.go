package refmatch

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMatchDoesNotFallBackToBasename is the reason this package exists rather
// than reusing the pipeline's ignore-pattern matcher, whose path semantics treat
// a slash-free pattern as a basename match. Applied to refs that would make a
// watch for "main" also sweep every feature branch whose last segment is "main",
// spending an agent invocation on each.
func TestMatchDoesNotFallBackToBasename(t *testing.T) {
	cases := []struct {
		ref     string
		pattern string
	}{
		{"refs/heads/feature/main", "main"},
		{"refs/heads/team/a/main", "main"},
		{"refs/heads/release/1.0", "1.0"},
		{"refs/heads/a/b/c", "c"},
	}
	for _, tc := range cases {
		if Match(tc.ref, tc.pattern) {
			t.Errorf("Match(%q, %q) = true; refs are hierarchical names, not paths, so there must be no basename fallback", tc.ref, tc.pattern)
		}
	}
}

// TestMatchSingleStarDoesNotCrossSlash pins the refspec-shaped distinction
// between "*" and "**". Getting this backwards would silently widen every
// operator pattern to whole subtrees.
func TestMatchSingleStarDoesNotCrossSlash(t *testing.T) {
	if !Match("refs/heads/release/1.0", "release/*") {
		t.Error(`Match("release/1.0", "release/*") = false, want true`)
	}
	if Match("refs/heads/release/1.0/hotfix", "release/*") {
		t.Error(`Match("release/1.0/hotfix", "release/*") = true; a single star must not cross a slash`)
	}
	if !Match("refs/heads/release/1.0/hotfix", "release/**") {
		t.Error(`Match("release/1.0/hotfix", "release/**") = false, want true`)
	}
}

// TestDoubleStarSubtreeExcludesThePrefixItself pins that "release/**" describes
// branches INSIDE release/, so a branch literally named "release" is a different
// ref and is not matched.
func TestDoubleStarSubtreeExcludesThePrefixItself(t *testing.T) {
	if Match("refs/heads/release", "release/**") {
		t.Error(`Match("release", "release/**") = true; the bare prefix is a different ref`)
	}
	if !Match("refs/heads/release/x", "release/**") {
		t.Error(`Match("release/x", "release/**") = false, want true`)
	}
}

func TestMatchAcceptsEitherSideFullyQualified(t *testing.T) {
	for _, ref := range []string{"main", "refs/heads/main"} {
		for _, pattern := range []string{"main", "refs/heads/main"} {
			if !Match(ref, pattern) {
				t.Errorf("Match(%q, %q) = false, want true", ref, pattern)
			}
		}
	}
}

func TestMatchTable(t *testing.T) {
	cases := []struct {
		ref     string
		pattern string
		want    bool
	}{
		{"main", "main", true},
		{"main", "mai", false},
		{"main", "main2", false},
		{"main", "*", true},
		{"feature/x", "*", false},
		{"feature/x", "**", true},
		{"main", "**", true},
		{"a/b/c/d", "**", true},
		{"feature/x", "feature/*", true},
		{"feature/x/y", "feature/*", false},
		{"feature/x/y", "feature/**", true},
		{"hotfix/x", "feature/**", false},
		{"release/1.0", "release/1.*", true},
		{"release/2.0", "release/1.*", false},
		{"", "main", false},
		{"main", "", false},
	}
	for _, tc := range cases {
		if got := Match(tc.ref, tc.pattern); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.ref, tc.pattern, got, tc.want)
		}
	}
}

func TestMatchAnyRequiresOnlyOnePattern(t *testing.T) {
	patterns := []string{"main", "release/*"}
	if !MatchAny("refs/heads/main", patterns) {
		t.Error("MatchAny did not match the first pattern")
	}
	if !MatchAny("refs/heads/release/1.0", patterns) {
		t.Error("MatchAny did not match the second pattern")
	}
	if MatchAny("refs/heads/feature/x", patterns) {
		t.Error("MatchAny matched a ref no pattern covers")
	}
	if MatchAny("refs/heads/main", nil) {
		t.Error("MatchAny with no patterns matched")
	}
}

func TestValidatePatternRejectsUnusableInput(t *testing.T) {
	bad := []string{
		"",
		" ",
		" main",
		"main ",
		"/main",
		"main/",
		"a//b",
		"a/./b",
		"a/../b",
		"refs/tags/v1",
		"refs/remotes/origin/main",
		"a**b",
		"**/main",
		"a/**/b",
		"[unterminated",
		"main\tx",
	}
	for _, pattern := range bad {
		if err := ValidatePattern(pattern); err == nil {
			t.Errorf("ValidatePattern(%q) = nil; a typo must fail at registration, not silently match nothing every sweep", pattern)
		}
	}
}

func TestValidatePatternAcceptsRealPatterns(t *testing.T) {
	good := []string{
		"main",
		"refs/heads/main",
		"*",
		"**",
		"release/*",
		"release/**",
		"refs/heads/release/**",
		"feature/JIRA-*",
		"v1.[0-9]",
	}
	for _, pattern := range good {
		if err := ValidatePattern(pattern); err != nil {
			t.Errorf("ValidatePattern(%q) = %v, want nil", pattern, err)
		}
	}
}

// TestValidatedPatternsNeverPanicOrError pins that anything ValidatePattern
// accepts is safe to hand to Match, so a registered watch can never blow up mid
// sweep.
func TestValidatedPatternsNeverPanicOrError(t *testing.T) {
	patterns := []string{"main", "*", "**", "release/*", "release/**", "v1.[0-9]"}
	refs := []string{"main", "release/1.0", "release/1.0/hotfix", "v1.2", "a/b/c/d/e", ""}
	for _, pattern := range patterns {
		if err := ValidatePattern(pattern); err != nil {
			t.Fatalf("fixture pattern %q is invalid: %v", pattern, err)
		}
		for _, ref := range refs {
			_ = Match(ref, pattern)
		}
	}
}

// TestMatchDivergesFromFilepathMatchOnBasenames documents the concrete
// difference from path-style matching, so the distinction is visible in the test
// output rather than only in a comment.
func TestMatchDivergesFromFilepathMatchOnBasenames(t *testing.T) {
	const ref = "feature/main"
	const pattern = "main"

	pathStyle, err := filepath.Match(pattern, filepath.Base(ref))
	if err != nil {
		t.Fatalf("filepath.Match: %v", err)
	}
	if !pathStyle {
		t.Fatal("fixture is wrong: path-style basename matching should match here")
	}
	if Match(ref, pattern) {
		t.Errorf("Match(%q, %q) agrees with path-style basename matching; it must not", ref, pattern)
	}
}

func TestTrimHeadsIsIdempotentAndTrimsSpace(t *testing.T) {
	for _, in := range []string{"main", "refs/heads/main", "  refs/heads/main  ", " main "} {
		if got := TrimHeads(in); got != "main" {
			t.Errorf("TrimHeads(%q) = %q, want %q", in, got, "main")
		}
	}
	// Only one prefix is stripped, so a branch genuinely named
	// "refs/heads/main" nested under refs/heads/ is not over-trimmed.
	if got := TrimHeads("refs/heads/refs/heads/main"); got != "refs/heads/main" {
		t.Errorf("TrimHeads stripped more than one prefix: %q", got)
	}
	if strings.TrimSpace(TrimHeads("  ")) != "" {
		t.Error("TrimHeads of blank input is not empty")
	}
}
