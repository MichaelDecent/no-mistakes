// Package refmatch matches Git branch refs against operator-configured glob
// patterns.
//
// It exists as its own package, rather than reusing the pipeline's
// ignore-pattern matcher, because the two answer different questions. That
// matcher has PATH semantics: a pattern containing no slash matches the
// BASENAME of a path, so "config.go" usefully matches "internal/db/config.go".
// Applied to refs the same rule is silently wrong and dangerous - "main" would
// match "refs/heads/feature/main", so a watch an operator intended for one
// branch would sweep every branch named main in any namespace, spending an agent
// invocation on each.
//
// Refs are hierarchical names, not file paths, so there is deliberately no
// basename fallback here. A pattern anchors at the start of the ref name.
package refmatch

import (
	"fmt"
	"path"
	"strings"
	"unicode"
)

const headsPrefix = "refs/heads/"

// Match reports whether a branch ref matches pattern.
//
// Both the ref and the pattern may carry a leading "refs/heads/", which is
// stripped before comparison, so "main" and "refs/heads/main" are the same
// pattern and match the same refs.
//
// Semantics, deliberately small and shaped like a Git refspec:
//
//	"main"       matches main, and nothing else
//	"release/*"  matches release/1.0 but NOT release/1.0/hotfix
//	"release/**" matches release/1.0 AND release/1.0/hotfix
//	"*"          matches every top-level branch
//	"**"         matches every branch
//
// The single star needs no special handling: path.Match already refuses to let
// "*" cross a slash. Only "**" needs explicit branches.
func Match(ref, pattern string) bool {
	ref = TrimHeads(ref)
	pattern = TrimHeads(pattern)
	if ref == "" || pattern == "" {
		return false
	}

	// "**" alone matches every branch at any depth.
	if pattern == "**" {
		return true
	}

	// A "/**" suffix matches the subtree: the prefix itself is deliberately NOT
	// matched, because "release/**" describes branches inside release/, and a
	// branch literally named "release" is a different ref.
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(ref, strings.TrimSuffix(pattern, "**"))
	}

	// A malformed pattern matches nothing rather than erroring: ValidatePattern
	// is the place a bad pattern is reported, at registration time.
	ok, err := path.Match(pattern, ref)
	return err == nil && ok
}

// MatchAny reports whether ref matches at least one pattern.
func MatchAny(ref string, patterns []string) bool {
	for _, pattern := range patterns {
		if Match(ref, pattern) {
			return true
		}
	}
	return false
}

// TrimHeads removes a leading "refs/heads/" so callers can pass either a full
// ref name or a bare branch name.
func TrimHeads(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), headsPrefix)
}

// ValidatePattern rejects a pattern that cannot match anything, or that names
// something other than a branch.
//
// This runs when a watch is registered, not when a sweep runs, so a typo fails
// the command the operator just typed instead of silently matching zero refs
// every night for weeks.
func ValidatePattern(pattern string) error {
	raw := strings.TrimSpace(pattern)
	if raw == "" {
		return fmt.Errorf("branch pattern is empty")
	}
	if raw != pattern {
		return fmt.Errorf("branch pattern %q has leading or trailing whitespace", pattern)
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return fmt.Errorf("branch pattern contains a control character")
		}
	}
	// Only branches are watchable, so a fully-qualified ref outside refs/heads/
	// is a mistake worth naming rather than quietly never matching.
	if strings.HasPrefix(raw, "refs/") && !strings.HasPrefix(raw, headsPrefix) {
		return fmt.Errorf("branch pattern %q is not under %s; only branches can be watched", raw, headsPrefix)
	}

	trimmed := TrimHeads(raw)
	if trimmed == "" {
		return fmt.Errorf("branch pattern %q names no branch", raw)
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasSuffix(trimmed, "/") {
		return fmt.Errorf("branch pattern %q has a leading or trailing slash", raw)
	}
	if strings.Contains(trimmed, "//") {
		return fmt.Errorf("branch pattern %q has an empty path segment", raw)
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("branch pattern %q has a %q segment", raw, segment)
		}
		// "**" is only meaningful as a whole segment. Something like "a**b"
		// would be read by path.Match as two consecutive single stars, which
		// silently means something narrower than the operator intended.
		if strings.Contains(segment, "**") && segment != "**" {
			return fmt.Errorf("branch pattern %q uses ** inside a path segment; ** must be a whole segment", raw)
		}
	}
	if trimmed != "**" && strings.Contains(trimmed, "**") && !strings.HasSuffix(trimmed, "/**") {
		return fmt.Errorf("branch pattern %q uses ** in the middle; ** is only supported as a trailing segment", raw)
	}
	// Reject a pattern path.Match itself cannot parse, such as an unterminated
	// character class.
	if _, err := path.Match(trimmed, "probe"); err != nil {
		return fmt.Errorf("branch pattern %q is malformed: %w", raw, err)
	}
	return nil
}
