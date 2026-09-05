package tiers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gentleman-programming/gentle-ai/v2/internal/skillregistry"
)

func mkEntry(name string, tiers []string) skillregistry.SkillEntry {
	return skillregistry.SkillEntry{
		Name:        name,
		Path:        "/skills/" + name + "/SKILL.md",
		Description: "test",
		Tiers:       tiers,
	}
}

func TestFromSkillEntryDefaultsToUniversal(t *testing.T) {
	got := FromSkillEntry(mkEntry("foo", nil))
	want := []string{DefaultTier}
	if !reflect.DeepEqual(got.Tiers, want) {
		t.Fatalf("Tiers = %v, want %v", got.Tiers, want)
	}
}

func TestFromSkillEntryPreservesExplicitEmpty(t *testing.T) {
	// An explicit empty list is a valid signal that the skill author
	// intends no tier assignment (rare but possible). We still default to
	// `universal` because the filter logic needs a non-empty set; the
	// contract here is "Tiers must be non-empty after FromSkillEntry".
	got := FromSkillEntry(mkEntry("foo", []string{}))
	if len(got.Tiers) == 0 {
		t.Fatalf("Tiers must default to non-empty; got empty")
	}
}

func TestNormalizeTrimsDedupesSorts(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil in", nil, nil},
		{"empty in", []string{}, nil},
		{"trim and dedupe case-insensitive (preserves first-case)", []string{"VBA", " vba ", "web", "Web"}, []string{"VBA", "web"}},
		{"drop empty", []string{"", "vba", "  ", "web"}, []string{"vba", "web"}},
		{"sort", []string{"web", "universal", "vba"}, []string{"universal", "vba", "web"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Normalize(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsWellKnown(t *testing.T) {
	for _, w := range WellKnownTiers {
		if !IsWellKnown(w) {
			t.Errorf("IsWellKnown(%q) = false, want true", w)
		}
		if !IsWellKnown(strings.ToUpper(w)) {
			t.Errorf("IsWellKnown(%q) = false, want true (case-insensitive)", strings.ToUpper(w))
		}
	}
	if IsWellKnown("custom-tier") {
		t.Errorf("IsWellKnown(\"custom-tier\") = true, want false")
	}
	if IsWellKnown("") {
		t.Errorf("IsWellKnown(\"\") = true, want false")
	}
}

func TestFilterByTier(t *testing.T) {
	all := []skillregistry.SkillEntry{
		mkEntry("vba-event-tracer", []string{"vba", "runtime"}),
		mkEntry("react-19", []string{"web"}),
		mkEntry("documentation-alan-style", []string{"universal"}),
		mkEntry("access-vba-tdd-loop", []string{"vba"}),
		mkEntry("legacy-no-tiers", nil),
	}
	proj := FromSkillEntries(all)

	cases := []struct {
		name    string
		wanted  []string
		wantLen int
		wantNames []string
	}{
		{"vba tier returns 2 vba + 1 legacy-defaults-to-universal", []string{"vba"}, 2, []string{"vba-event-tracer", "access-vba-tdd-loop"}},
		{"web tier returns 1", []string{"web"}, 1, []string{"react-19"}},
		{"universal tier returns 2", []string{"universal"}, 2, []string{"documentation-alan-style", "legacy-no-tiers"}},
		{"multiple tiers are union", []string{"vba", "web"}, 3, []string{"vba-event-tracer", "react-19", "access-vba-tdd-loop"}},
		{"unknown tier returns empty", []string{"nosuchtier"}, 0, nil},
		{"empty filter returns all", nil, 5, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Filter(proj, tc.wanted)
			if len(got) != tc.wantLen {
				t.Fatalf("Filter(%v) returned %d entries, want %d:\n%v", tc.wanted, len(got), tc.wantLen, got)
			}
			if tc.wantNames != nil {
				gotNames := make([]string, len(got))
				for i, e := range got {
					gotNames[i] = e.Name
				}
				if !reflect.DeepEqual(gotNames, tc.wantNames) {
					t.Fatalf("names = %v, want %v", gotNames, tc.wantNames)
				}
			}
		})
	}
}

func TestGroup(t *testing.T) {
	all := []skillregistry.SkillEntry{
		mkEntry("vba-event-tracer", []string{"vba", "runtime"}),
		mkEntry("react-19", []string{"web"}),
		mkEntry("documentation-alan-style", []string{"universal"}),
		mkEntry("access-vba-tdd-loop", []string{"vba"}),
		mkEntry("legacy-no-tiers", nil),
	}
	proj := FromSkillEntries(all)
	got := Group(proj)

	cases := []struct {
		tier     string
		wantLen  int
		wantHasNames []string
	}{
		{"universal", 2, []string{"documentation-alan-style", "legacy-no-tiers"}},
		{"vba", 2, []string{"vba-event-tracer", "access-vba-tdd-loop"}},
		{"web", 1, []string{"react-19"}},
		{"runtime", 1, []string{"vba-event-tracer"}},
		{"nonexistent", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			group := got[tc.tier]
			if len(group) != tc.wantLen {
				t.Fatalf("len(%s group) = %d, want %d", tc.tier, len(group), tc.wantLen)
			}
			if tc.wantHasNames != nil {
				gotNames := make([]string, len(group))
				for i, e := range group {
					gotNames[i] = e.Name
				}
				if !reflect.DeepEqual(gotNames, tc.wantHasNames) {
					t.Fatalf("names in %s = %v, want %v", tc.tier, gotNames, tc.wantHasNames)
				}
			}
		})
	}
}

func TestCustomTiers(t *testing.T) {
	all := []skillregistry.SkillEntry{
		mkEntry("vba-x", []string{"vba", "experimental"}),
		mkEntry("web-x", []string{"web", "experimental"}),
		mkEntry("plain", []string{"custom-only"}),
		mkEntry("well-known", []string{"vba"}),
	}
	proj := FromSkillEntries(all)
	got := CustomTiers(proj)
	want := []string{"custom-only", "experimental"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CustomTiers = %v, want %v", got, want)
	}
}

func TestParseFrontmatterTiers(t *testing.T) {
	// Round-trip via the production parser to ensure the tiers slice
	// produced by parseFrontmatter feeds correctly into Filter. Lives in
	// internal/skillregistry/registry_test.go for access to the unexported
	// parseFrontmatter function; here we only re-test the public-facing
	// defaulting in FromSkillEntry.
	all := []skillregistry.SkillEntry{
		mkEntry("inline-list", nil),
		mkEntry("scalar", nil),
	}
	proj := FromSkillEntries(all)
	for _, p := range proj {
		if len(p.Tiers) != 1 || p.Tiers[0] != DefaultTier {
			t.Fatalf("default tier fallback failed for %q: got %v", p.Name, p.Tiers)
		}
	}
}
