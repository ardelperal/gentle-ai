package screens

import (
	"strings"

	"github.com/gentleman-programming/gentle-ai/v2/internal/tui/styles"
)

// ModelSelectionGuidance describes the capabilities, reasoning range, and
// tradeoffs a user should consider when assigning a model to a given SDD,
// Judgment Day, or review role. Guidance is capability-based — it never
// pins to specific model brands or transient model IDs, and reasoning-effort
// guidance is kept separate from Fast / service-tier guidance.
type ModelSelectionGuidance struct {
	// Purpose is a one-line summary of what the role does.
	Purpose string

	// Priorities is a newline-separated list of capabilities to prefer
	// when selecting a model for the role. Rendered as bullet points
	// when the expanded guidance panel is visible.
	Priorities string

	// Reasoning is the suggested reasoning-effort range. Reasoning is
	// separate from Fast mode — see FastMode below.
	Reasoning string

	// FastMode describes when to enable Fast / service-tier processing.
	// Empty means Fast mode is not recommended for the role. Fast mode
	// changes processing speed and pricing only; it does not replace
	// the chosen reasoning effort.
	FastMode string

	// Tradeoffs lists quality / speed / cost / context considerations.
	// Rendered only when the expanded guidance panel is visible.
	Tradeoffs string
}

// modelSelectionGuidance maps every configurable agent role to capability-
// based guidance. Roles not present in the map fall back to generic
// implementation- or review-agent guidance (see guidanceFor).
//
// The guidance describes what to look for in a model, not which model to
// pick, so the map stays valid as provider catalogs, pricing, and IDs
// change. The Fast / service-tier note is intentionally separate from
// the reasoning-effort note because the two settings control different
// dimensions of model behaviour.
var modelSelectionGuidance = map[string]ModelSelectionGuidance{}

// guidanceFor returns the guidance entry for a given role. Unknown roles
// fall back to a generic guidance that describes the implementation /
// review responsibilities without claiming any specific SDD or JD phase.
func guidanceFor(role string) ModelSelectionGuidance {
	if g, ok := modelSelectionGuidance[role]; ok {
		return g
	}
	if strings.HasPrefix(role, "jd-") {
		return ModelSelectionGuidance{
			Purpose:    "Judgment Day sub-agent that participates in adversarial review.",
			Priorities: "Strong reasoning\nDefect detection\nIndependence from the implementation model",
			Reasoning:  "High reasoning effort.",
			Tradeoffs:  "Prefer review quality over generation speed.",
		}
	}
	if strings.HasPrefix(role, "review-") {
		return ModelSelectionGuidance{
			Purpose:    "Native bounded-review sub-agent.",
			Priorities: "Reasoning depth\nDefect detection\nSpecific, actionable findings",
			Reasoning:  "Medium to high reasoning effort.",
			Tradeoffs:  "Cite evidence; prefer careful review over speed.",
		}
	}
	return ModelSelectionGuidance{
		Purpose:    "SDD sub-agent that implements approved tasks.",
		Priorities: "Coding and tool-use reliability\nSufficient context\nMedium reasoning for non-trivial work",
		Reasoning:  "Medium reasoning.",
		FastMode:   "Use when lower latency justifies the additional cost.",
		Tradeoffs:  "Code correctness matters more than cleverness.",
	}
}

// renderCompactGuidance renders the single-line summary shown by default
// below the help line. The "i / ?" hint invites the user to expand the
// full guidance panel.
func renderCompactGuidance(role string) string {
	g := guidanceFor(role)
	if g.Purpose == "" {
		return ""
	}
	return styles.SubtextStyle.Render("Role: "+role+" — "+g.Purpose) +
		"\n" +
		styles.HelpStyle.Render("i or ?: show full guidance") +
		"\n"
}

// renderExpandedGuidance renders the multi-line capability panel. The
// panel always ends with a hint that the user can collapse it again.
func renderExpandedGuidance(role string) string {
	g := guidanceFor(role)
	if g.Purpose == "" {
		return renderCompactGuidance(role)
	}

	var b strings.Builder
	b.WriteString(styles.SubtextStyle.Render("Role: " + role))
	b.WriteString("\n")
	b.WriteString(styles.SelectedStyle.Render(g.Purpose))
	b.WriteString("\n\n")

	b.WriteString(styles.SubtextStyle.Render("Consider:"))
	b.WriteString("\n")
	for _, line := range strings.Split(g.Priorities, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(styles.SubtextStyle.Render("  • " + line))
		b.WriteString("\n")
	}

	if g.Reasoning != "" {
		b.WriteString("\n")
		b.WriteString(styles.SubtextStyle.Render("Reasoning: " + g.Reasoning))
		b.WriteString("\n")
	}

	if g.FastMode != "" {
		b.WriteString(styles.SubtextStyle.Render("Fast mode: " + g.FastMode))
		b.WriteString("\n")
		b.WriteString(styles.SubtextStyle.Render("Fast changes processing speed and pricing."))
		b.WriteString("\n")
		b.WriteString(styles.SubtextStyle.Render("It does not replace the selected reasoning effort."))
		b.WriteString("\n")
	}

	if g.Tradeoffs != "" {
		b.WriteString("\n")
		b.WriteString(styles.SubtextStyle.Render("Tradeoffs:"))
		b.WriteString("\n")
		for _, line := range strings.Split(g.Tradeoffs, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			b.WriteString(styles.SubtextStyle.Render("  - " + line))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(styles.HelpStyle.Render("i or ?: hide guidance"))
	b.WriteString("\n")
	return b.String()
}

// ToggleModelPickerGuidance flips the GuidanceExpanded flag when the user
// presses the i or ? key. Returns true if the key was handled so the
// caller can short-circuit normal navigation.
func ToggleModelPickerGuidance(state *ModelPickerState, key string) bool {
	if state == nil {
		return false
	}
	if key != "i" && key != "?" {
		return false
	}
	state.GuidanceExpanded = !state.GuidanceExpanded
	return true
}
