// Package tui implements the bubbletea-based Sprint 1 MVP for
// `gentle-ai skills tui`. The MVP shows two views: a Tiers view that
// counts skills per tier and lists custom tiers, and a Skills view that
// renders all discovered skills with their tiers and version. The two
// views are switched via the Tab key. Tier selection in the Tiers view
// filters the Skills view.
//
// This is intentionally the smallest interactive surface that proves the
// wiring: a richer editor (creating custom tiers, editing metadata.tiers
// per skill, sync trigger) is Sprint 2 and lives in a follow-up.
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/gentleman-programming/gentle-ai/v2/internal/components/skills/tiers"
	"github.com/gentleman-programming/gentle-ai/v2/internal/skillregistry"
)

// view identifies which pane is active.
type view int

const (
	viewTiers view = iota
	viewSkills
)

// model is the bubbletea model.
type model struct {
	all     []tiers.SkillEntry     // every skill (post defaulting)
	view    view                   // active pane
	cursor  int                    // cursor within the active list
	filter  string                 // tier filter applied to Skills view
	grouped map[string][]tiers.SkillEntry
	custom  []string               // sorted custom tier names
	width   int
	height  int
	quitting bool
	err     error
}

// New constructs a model from registry entries. Pass the raw entries;
// the model calls tiers.FromSkillEntries to apply the defaulting rules.
func New(entries []skillregistry.SkillEntry) *model {
	proj := tiers.FromSkillEntries(entries)
	return &model{
		all:     proj,
		view:    viewTiers,
		grouped: tiers.Group(proj),
		custom:  tiers.CustomTiers(proj),
	}
}

// Init satisfies tea.Model. No async startup required for the MVP.
func (m *model) Init() tea.Cmd { return nil }

// Update satisfies tea.Model.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit
		case "tab":
			if m.view == viewTiers {
				m.view = viewSkills
			} else {
				m.view = viewTiers
			}
			m.cursor = 0
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "j":
			if m.cursor < m.listLen()-1 {
				m.cursor++
			}
			return m, nil
		case "esc":
			m.filter = ""
			return m, nil
		case "a":
			// Select all (clear filter).
			m.filter = ""
			return m, nil
		default:
			// Single-key tier filter: 1-9 selects tier N from the canonical
			// list. Sprint 2 will replace this with text input. In bubbletea
			// v1+ printable key presses arrive as KeyMsg with Type == KeyRunes
			// and the rune in msg.Runes[0].
			if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
				r := msg.Runes[0]
				if r >= '1' && r <= '9' {
					i := int(r - '1')
					if i < len(tiers.WellKnownTiers) {
						m.filter = tiers.WellKnownTiers[i]
						m.view = viewSkills
						m.cursor = 0
						return m, nil
					}
				}
			}
			return m, nil
		}
	case errMsg:
		m.err = msg.err
		return m, tea.Quit
	}
	return m, nil
}

// errMsg is the only custom message the MVP needs; it carries an init
// failure (e.g. cwd resolution) up to Update so the user sees it on quit.
type errMsg struct{ err error }

// InitError returns a tea.Cmd that bubbles an initialization error to the
// model so the View can render it instead of a blank screen.
func InitError(err error) tea.Cmd {
	return func() tea.Msg { return errMsg{err: err} }
}

// listLen returns the row count for the active view (Tiers or Skills).
// The Skills view applies the active tier filter.
func (m *model) listLen() int {
	switch m.view {
	case viewTiers:
		// One row per canonical tier + one row per custom tier.
		return len(tiers.WellKnownTiers) + len(m.custom)
	case viewSkills:
		return len(m.filteredSkills())
	}
	return 0
}

func (m *model) filteredSkills() []tiers.SkillEntry {
	if m.filter == "" {
		return m.all
	}
	return tiers.Filter(m.all, []string{m.filter})
}

// tierIndexFromKey maps a single digit rune to a canonical tier index. It
// returns -1 for anything that is not a digit 1-9. Sprint 1 keys are
// numeric; Sprint 2 will add labelled shortcuts.
func tierIndexFromKey(s string) int {
	if len(s) != 1 || s[0] < '1' || s[0] > '9' {
		return -1
	}
	return int(s[0] - '1')
}

// View satisfies tea.Model.
func (m *model) View() string {
	if m.quitting {
		if m.err != nil {
			return errorStyle.Render(fmt.Sprintf("skills tui error: %v\n", m.err))
		}
		return "bye.\n"
	}
	if m.width == 0 {
		// First frame; wait for WindowSizeMsg.
		return ""
	}

	var header strings.Builder
	header.WriteString(titleStyle.Render("gentle-ai skills tui (Sprint 1 MVP)") + "\n")
	header.WriteString(tabStyle(viewTiers == m.view, "Tiers") + "  " + tabStyle(viewSkills == m.view, "Skills") + "\n")
	if m.filter != "" {
		header.WriteString(filterStyle.Render(fmt.Sprintf("filter: tier=%s  (esc / a to clear)", m.filter)) + "\n")
	} else {
		header.WriteString(filterStyle.Render("filter: none  (press 1-9 for tier, esc to clear)") + "\n")
	}
	header.WriteString(strings.Repeat("─", minInt(m.width, 80)) + "\n")

	var body string
	switch m.view {
	case viewTiers:
		body = m.renderTiers()
	case viewSkills:
		body = m.renderSkills()
	}

	return header.String() + body
}

func (m *model) renderTiers() string {
	var sb strings.Builder
	// One row per canonical tier; cursor marks the current selection.
	for i, t := range tiers.WellKnownTiers {
		count := len(m.grouped[t])
		row := fmt.Sprintf("%s  %3d skill(s)", t, count)
		sb.WriteString(rowStyle(i == m.cursor, false, row) + "\n")
	}
	for i, t := range m.custom {
		count := len(m.grouped[t])
		row := fmt.Sprintf("%s (custom)  %3d skill(s)", t, count)
		sb.WriteString(rowStyle(i+len(tiers.WellKnownTiers) == m.cursor, true, row) + "\n")
	}
	return sb.String()
}

func (m *model) renderSkills() string {
	var sb strings.Builder
	header := fmt.Sprintf("%-32s  %-8s  %-20s  %s", "NAME", "VERSION", "TIERS", "PATH")
	sb.WriteString(headerStyle.Render(header) + "\n")
	rows := m.filteredSkills()
	if len(rows) == 0 {
		sb.WriteString(emptyStyle.Render("(no skills match the current filter)") + "\n")
		return sb.String()
	}
	for i, e := range rows {
		line := fmt.Sprintf("%-32s  %-8s  %-20s  %s",
			truncate(e.Name, 32),
			e.Version,
			strings.Join(e.Tiers, ","),
			e.Path,
		)
		sb.WriteString(rowStyle(i == m.cursor, false, line) + "\n")
	}
	return sb.String()
}

// styles ----------------------------------------------------------------

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4")).
			PaddingBottom(1)

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4"))

	tabStyle = func(active bool, name string) string {
		if active {
			return lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#FFFFFF")).
				Background(lipgloss.Color("#7D56F4")).
				Padding(0, 1).
				Render(name)
		}
		return lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7D56F4")).
			Padding(0, 1).
			Render(name)
	}

	rowStyle = func(selected, custom bool, text string) string {
		s := lipgloss.NewStyle()
		if selected {
			s = s.Bold(true).Foreground(lipgloss.Color("#FFFFFF")).
				Background(lipgloss.Color("#3C3C3C"))
		} else if custom {
			s = s.Foreground(lipgloss.Color("#F78000")) // orange for custom tiers
		}
		return s.Render(text)
	}

	filterStyle = lipgloss.NewStyle().
			Italic(true).
			Foreground(lipgloss.Color("#888888"))

	emptyStyle = lipgloss.NewStyle().
			Italic(true).
			Foreground(lipgloss.Color("#888888"))

	errorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FF0000"))
)

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
