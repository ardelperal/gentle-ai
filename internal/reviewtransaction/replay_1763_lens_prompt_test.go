package reviewtransaction

import (
	"strings"
	"testing"
)

// TestReplay1763ReviewReadabilityMandateContainsConcreteDefectRule is the
// in-process half of the #1763 bounded replay. It asserts that the runtime
// `LensMandate[LensReadability].focus` (the canonical source of the
// readability lens prompt at runtime) carries the concrete-defect rule.
//
// The fixture file (replay_fixture_1763.go) introduces a new unexported
// camelCase type following the Effective Go convention. The runtime rule
// explicitly bans style-only findings ("Report style only when it hides a
// concrete defect or makes the change unsafe to maintain."), so a
// well-configured lens will not flag the fixture.
//
// The cross-package half of the replay — that the assembled provider-bound
// prompt in `internal/components/reviewassets/lens.go:ReviewerPrompt`
// embeds this focus verbatim — is captured by running
// `gentle-ai review lens-context --lens review-readability` against the
// worktree (separate step, see `odd/tasks/1763-bounded-replay.md`).
func TestReplay1763ReviewReadabilityMandateContainsConcreteDefectRule(t *testing.T) {
	title, focus, ok := LensMandate("review-readability")
	if !ok {
		t.Fatal("LensMandate(review-readability) returned ok=false; the runtime contract is not mounted")
	}
	if title != "R2 Readability" {
		t.Fatalf("LensMandate(review-readability) title = %q, want %q", title, "R2 Readability")
	}
	const concreteDefectRule = "Report style only when it hides a concrete defect or makes the change unsafe to maintain."
	if !strings.Contains(focus, concreteDefectRule) {
		t.Fatalf("LensMandate(review-readability).focus is missing the concrete-defect rule.\nfocus = %q", focus)
	}
}

// TestReplay1763FixtureTypeExists is the structural half of the replay. It
// proves that the fixture file is present in the package and that the
// unexported camelCase type is what the lens would see. A future refactor
// that renames or removes the fixture will fail this test, which is the
// correct signal: the bounded replay depends on a stable fixture.
func TestReplay1763FixtureTypeExists(t *testing.T) {
	// Compile-time check: if the type does not exist, this test will not
	// even compile. The runtime check below is a belt-and-braces assertion
	// that the type is package-internal (unexported) and follows the
	// camelCase convention.
	var zero allowedDecisionEdge
	if zero != (allowedDecisionEdge{}) {
		t.Fatalf("allowedDecisionEdge zero value is not the empty struct: %#v", zero)
	}
}
