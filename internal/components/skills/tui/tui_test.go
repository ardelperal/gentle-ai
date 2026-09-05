package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gentleman-programming/gentle-ai/v2/internal/skillregistry"
)

func mk(name string, tiers []string) skillregistry.SkillEntry {
	return skillregistry.SkillEntry{
		Name:        name,
		Path:        "/skills/" + name + "/SKILL.md",
		Description: "test",
		Tiers:       tiers,
	}
}

func TestNewDefaultsLegacyEntriesToUniversal(t *testing.T) {
	// Only entries with empty/nil Tiers get the universal default; entries
	// with explicit tiers keep them.
	all := []skillregistry.SkillEntry{
		mk("legacy-nil", nil),
		mk("legacy-empty", []string{}),
		mk("vba-x", []string{"vba"}),
	}
	m := New(all)
	if len(m.all) != 3 {
		t.Fatalf("len(all) = %d, want 3", len(m.all))
	}
	// legacy-nil and legacy-empty default to universal; vba-x keeps its tier.
	want := map[string][]string{
		"legacy-nil":   {"universal"},
		"legacy-empty": {"universal"},
		"vba-x":        {"vba"},
	}
	for _, e := range m.all {
		got, ok := want[e.Name]
		if !ok {
			t.Fatalf("unexpected entry %q", e.Name)
		}
		if len(e.Tiers) != len(got) || e.Tiers[0] != got[0] {
			t.Fatalf("entry %q: tiers = %v, want %v", e.Name, e.Tiers, got)
		}
	}
}

func TestNewComputesCustomTiers(t *testing.T) {
	all := []skillregistry.SkillEntry{
		mk("x", []string{"experimental"}),
		mk("y", []string{"vba", "experimental"}),
		mk("z", []string{"web"}),
	}
	m := New(all)
	if len(m.custom) != 1 || m.custom[0] != "experimental" {
		t.Fatalf("custom = %v, want [experimental]", m.custom)
	}
}

func TestTabSwitchesView(t *testing.T) {
	m := New(nil)
	if m.view != viewTiers {
		t.Fatalf("initial view = %v, want viewTiers", m.view)
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m2 := updated.(*model)
	if m2.view != viewSkills {
		t.Fatalf("after Tab view = %v, want viewSkills", m2.view)
	}
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyTab})
	m3 := updated.(*model)
	if m3.view != viewTiers {
		t.Fatalf("after second Tab view = %v, want viewTiers", m3.view)
	}
}

func TestDigitSelectsTier(t *testing.T) {
	all := []skillregistry.SkillEntry{
		mk("a", []string{"universal"}),
		mk("b", []string{"vba"}),
		mk("c", []string{"web"}),
		mk("d", []string{"runtime"}),
	}
	m := New(all)

	// Pressing '2' should select the vba tier. In bubbletea v1+, KeyMsg uses
	// `Type` for the KeyType and `Runes` (slice) for printable chars.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m2 := updated.(*model)
	if m2.filter != "vba" {
		t.Fatalf("after '2' filter = %q, want vba", m2.filter)
	}
	if m2.view != viewSkills {
		t.Fatalf("after digit view = %v, want viewSkills", m2.view)
	}
	if got := len(m2.filteredSkills()); got != 1 {
		t.Fatalf("vba skills count = %d, want 1", got)
	}

	// 'a' clears the filter.
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m3 := updated.(*model)
	if m3.filter != "" {
		t.Fatalf("after 'a' filter = %q, want empty", m3.filter)
	}
	if got := len(m3.filteredSkills()); got != 4 {
		t.Fatalf("cleared filter skill count = %d, want 4", got)
	}
}

func TestQuitReturnsQuitCmd(t *testing.T) {
	m := New(nil)
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := updated.(*model)
	if !m2.quitting {
		t.Fatal("expected quitting=true after 'q'")
	}
	if cmd == nil {
		t.Fatal("expected non-nil tea.Quit cmd")
	}
}

func TestViewRendersHeaderAndAtLeastOneRow(t *testing.T) {
	all := []skillregistry.SkillEntry{
		mk("a", []string{"vba"}),
		mk("b", []string{"web"}),
	}
	m := New(all)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m2 := updated.(*model)
	out := m2.View()
	for _, want := range []string{"Tiers", "Skills", "filter:", "universal"} {
		if !strings.Contains(out, want) {
			t.Errorf("View output missing %q\n---\n%s", want, out)
		}
	}
}

func TestListLenAccountsForCustomTiers(t *testing.T) {
	all := []skillregistry.SkillEntry{
		mk("a", []string{"vba"}),
		mk("b", []string{"experimental"}),
	}
	m := New(all)
	// Tiers view: 4 canonical + 1 custom = 5 rows
	if got := m.listLen(); got != 5 {
		t.Fatalf("Tiers listLen = %d, want 5", got)
	}
}

func TestViewRendersTiersAndSkills(t *testing.T) {
	all := []skillregistry.SkillEntry{
		mk("alpha", []string{"vba"}),
		mk("beta", []string{"web"}),
		mk("gamma", []string{"experimental"}),
		mk("delta", nil),
	}
	m := New(all)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m2 := updated.(*model)

	tiersView := m2.View()
	// Tiers view lists canonical + custom tiers with skill counts; it does
	// NOT list individual skill names.
	for _, want := range []string{"universal", "vba", "web", "runtime", "experimental", "(custom)", "skill(s)"} {
		if !strings.Contains(tiersView, want) {
			t.Errorf("Tiers view missing %q\n---\n%s", want, tiersView)
		}
	}

	// Switch to Skills view.
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyTab})
	m3 := updated.(*model)
	skillsView := m3.View()
	for _, want := range []string{"alpha", "beta", "gamma", "delta", "NAME", "TIERS", "VERSION", "PATH"} {
		if !strings.Contains(skillsView, want) {
			t.Errorf("Skills view missing %q\n---\n%s", want, skillsView)
		}
	}
}

func TestFilterShowsOnlyMatchingSkills(t *testing.T) {
	all := []skillregistry.SkillEntry{
		mk("vba-1", []string{"vba"}),
		mk("vba-2", []string{"vba", "runtime"}),
		mk("web-1", []string{"web"}),
		mk("universal-1", []string{"universal"}),
	}
	m := New(all)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	updated, _ = updated.(*model).Update(tea.KeyMsg{Type: tea.KeyTab}) // Skills view
	updated, _ = updated.(*model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}}) // tier=vba
	m2 := updated.(*model)
	view := m2.View()
	for _, want := range []string{"vba-1", "vba-2"} {
		if !strings.Contains(view, want) {
			t.Errorf("vba-filtered view missing %q", want)
		}
	}
	if strings.Contains(view, "web-1") || strings.Contains(view, "universal-1") {
		t.Errorf("vba-filtered view should not contain web-1 or universal-1")
	}
}
