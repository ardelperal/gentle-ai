package reviewassets

import (
	"strings"
	"testing"

	"github.com/gentleman-programming/gentle-ai/v4/internal/reviewtransaction"
)

// TestReplay1763AssembledPromptEmbedsConcreteDefectRule is the cross-package
// half of the #1763 bounded replay. It lives in `reviewassets` because the
// provider-bound prompt is assembled here (see `lens.go:ReviewerPrompt`),
// and the canonical direction of imports is
// `reviewassets -> reviewtransaction`. The reverse direction is forbidden
// (it would cycle), so the in-process half of the replay lives in
// `internal/reviewtransaction/replay_1763_lens_prompt_test.go`.
//
// Together, the two tests prove the rule is mounted end-to-end:
//  1. `LensMandate[LensReadability].focus` carries the concrete-defect rule.
//  2. The shell-capable `ReviewerPrompt("review-readability")` and the
//     no-shell `ClaudeReviewerPrompt("review-readability")` both embed the
//     rule verbatim in the `## Scope` section the provider sees.
//
// The fixture file `internal/reviewtransaction/replay_fixture_1763.go`
// introduces a new unexported camelCase type following the Effective Go
// convention. A well-configured lens will not flag it because the rule
// explicitly bans style-only findings.
func TestReplay1763AssembledPromptEmbedsConcreteDefectRule(t *testing.T) {
	// Cross-check the in-process half: the focus must come from the
	// canonical source, not be re-asserted independently here.
	_, focus, ok := reviewtransaction.LensMandate("review-readability")
	if !ok {
		t.Fatal("reviewtransaction.LensMandate(review-readability) returned ok=false; the runtime contract is not mounted")
	}

	const concreteDefectRule = "Report style only when it hides a concrete defect or makes the change unsafe to maintain."
	if !strings.Contains(focus, concreteDefectRule) {
		t.Fatalf("LensMandate(review-readability).focus is missing the concrete-defect rule (cross-package check).\nfocus = %q", focus)
	}

	// Shell-capable OpenCode transport.
	shellPrompt, ok := ReviewerPrompt("review-readability")
	if !ok {
		t.Fatal("ReviewerPrompt(review-readability) returned ok=false")
	}
	if !strings.Contains(shellPrompt, "## Scope") {
		t.Fatalf("shell-capable prompt does not carry a `## Scope` section; the rule cannot reach the provider")
	}
	if !strings.Contains(shellPrompt, concreteDefectRule) {
		t.Fatalf("shell-capable prompt is missing the concrete-defect rule in `## Scope`")
	}

	// No-shell Claude transport.
	claudePrompt, ok := ClaudeReviewerPrompt("review-readability")
	if !ok {
		t.Fatal("ClaudeReviewerPrompt(review-readability) returned ok=false")
	}
	if !strings.Contains(claudePrompt, concreteDefectRule) {
		t.Fatalf("no-shell Claude prompt is missing the concrete-defect rule")
	}

	// Generic no-shell runtime transport (the path opencode/codex/pi use).
	runtimePrompt, ok := RuntimeReviewerPrompt("review-readability", "the parent")
	if !ok {
		t.Fatal("RuntimeReviewerPrompt(review-readability) returned ok=false")
	}
	if !strings.Contains(runtimePrompt, concreteDefectRule) {
		t.Fatalf("runtime no-shell prompt is missing the concrete-defect rule")
	}
}
