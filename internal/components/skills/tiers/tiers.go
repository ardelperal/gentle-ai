// Package tiers classifies SKILL.md files into project-type buckets so
// contributors can scope which skills propagate into a given repo. A tier
// is a string label declared in the `metadata.tiers:` frontmatter field of
// a SKILL.md. Examples:
//
//	---
//	name: vba-event-tracer
//	metadata:
//	  tiers: [vba, runtime]
//	---
//
// Skills without a `metadata.tiers` field default to `universal`, so legacy
// skills continue to work without changes.
//
// The four canonical tiers are defined in WellKnownTiers. Custom tiers are
// allowed (the filter and listing machinery is string-keyed) but the TUI
// and CLI surface them as "custom" so the operator knows they are not in
// the canonical set.
package tiers

import (
	"sort"
	"strings"

	"github.com/gentleman-programming/gentle-ai/v2/internal/skillregistry"
)

// WellKnownTiers is the ordered list of canonical tier names. The first
// entry is the implicit default for skills without metadata.tiers.
var WellKnownTiers = []string{
	"universal", // applies to every project (style, AI-slop, governance)
	"vba",       // Microsoft Access / VBA runtime surfaces
	"web",       // browser/TS/web frontends
	"runtime",   // cross-runtime tooling, language agnostic
}

// DefaultTier is the tier assigned to skills whose frontmatter omits
// `metadata.tiers`. Centralizing the constant keeps the filter behavior
// explicit and lets the TUI render legacy entries consistently.
const DefaultTier = "universal"

// SkillEntry is the tier-aware projection of skillregistry.SkillEntry.
// Keeping a separate type here avoids dragging the registry import into the
// TUI / CLI when only tier fields are needed.
type SkillEntry struct {
	Name        string
	Path        string
	Description string
	Author      string
	Version     string
	Tiers       []string
}

// FromSkillEntry projects a skillregistry.SkillEntry into the tier-aware
// type. It defaults missing tiers to []string{DefaultTier} so the filter
// and grouping logic can treat every skill uniformly.
func FromSkillEntry(e skillregistry.SkillEntry) SkillEntry {
	tiers := e.Tiers
	if len(tiers) == 0 {
		tiers = []string{DefaultTier}
	}
	return SkillEntry{
		Name:        e.Name,
		Path:        e.Path,
		Description: e.Description,
		Author:      e.Author,
		Version:     e.Version,
		Tiers:       tiers,
	}
}

// FromSkillEntries converts a slice of registry entries in one call.
func FromSkillEntries(entries []skillregistry.SkillEntry) []SkillEntry {
	out := make([]SkillEntry, len(entries))
	for i, e := range entries {
		out[i] = FromSkillEntry(e)
	}
	return out
}

// Normalize trims, dedupes (case-insensitive), and sorts tier strings. It
// returns a fresh slice and never mutates the input.
func Normalize(tiers []string) []string {
	if len(tiers) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tiers))
	out := make([]string, 0, len(tiers))
	for _, t := range tiers {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		key := strings.ToLower(t)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// IsWellKnown reports whether a tier name is one of the canonical tiers
// (case-insensitive). Custom tiers return false; the TUI uses this to
// badge them differently from canonical tiers.
func IsWellKnown(tier string) bool {
	t := strings.ToLower(strings.TrimSpace(tier))
	for _, w := range WellKnownTiers {
		if strings.EqualFold(w, t) {
			return true
		}
	}
	return false
}

// Filter returns the subset of entries whose `Tiers` field intersects the
// `wantedTiers` list (case-insensitive). An empty `wantedTiers` slice
// returns the input unchanged (no filter applied). Skills whose `Tiers`
// field is empty are treated as `[]string{DefaultTier}` so they survive a
// `wantedTiers = ["universal"]` query.
func Filter(entries []SkillEntry, wantedTiers []string) []SkillEntry {
	if len(wantedTiers) == 0 || len(entries) == 0 {
		return entries
	}
	wanted := make(map[string]struct{}, len(wantedTiers))
	for _, w := range wantedTiers {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		wanted[w] = struct{}{}
	}
	if len(wanted) == 0 {
		return entries
	}
	out := make([]SkillEntry, 0, len(entries))
	for _, e := range entries {
		for _, t := range e.Tiers {
			if _, ok := wanted[strings.ToLower(strings.TrimSpace(t))]; ok {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// Group buckets entries by their (lowercased) tier name. A skill with
// multiple tiers appears once per tier. Entries with empty Tiers fall
// into the DefaultTier bucket.
func Group(entries []SkillEntry) map[string][]SkillEntry {
	out := make(map[string][]SkillEntry, len(WellKnownTiers)+2)
	for _, e := range entries {
		tiers := e.Tiers
		if len(tiers) == 0 {
			tiers = []string{DefaultTier}
		}
		for _, t := range tiers {
			key := strings.ToLower(strings.TrimSpace(t))
			if key == "" {
				continue
			}
			out[key] = append(out[key], e)
		}
	}
	return out
}

// CustomTiers returns the tier names present in `entries` that are NOT in
// WellKnownTiers. The result is sorted. Operators use this to discover
// drift in the tier taxonomy without leaving the TUI.
func CustomTiers(entries []SkillEntry) []string {
	seen := make(map[string]struct{})
	for _, e := range entries {
		for _, t := range e.Tiers {
			if IsWellKnown(t) {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(t))
			if key == "" {
				continue
			}
			seen[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
