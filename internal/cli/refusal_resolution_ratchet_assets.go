// This file is the asset-side walker for the by-design envelope layer. It
// walks one markdown document, parses every directive line, and -- this is
// the Drift #1 fix -- verifies each named verb against the dispatchable set
// returned by ReviewDispatchableReviewVerbs. A verb the CLI does not dispatch
// causes the entire parse to fail closed with an error pointing at the line.
//
// Why the dispatch check matters here but not in the Go-side ratchet:
// the Go-side ratchet (refusal_resolution_ratchet_test.go::TestEveryProduction
// RefusalNamesResolutionOrDeclaresByDesign) walks error-constructor messages,
// and TestEveryNamedReviewContinuationIsStructurallyReal already pins every
// `gentle-ai review <verb>` in source against the dispatchable set. The asset
// walker has no such safety net on the markdown side -- agents read these
// directives and may issue the named command -- so the dispatch check has to
// live in this walker.
package cli

import (
	"fmt"
	"strings"
)

// ParseMarkdownByDesignEnvelope walks one markdown document and returns every
// by-design envelope it finds. A "directive line" is any non-empty line that
// EITHER contains a `by-design:` marker or a `gentle-ai review ` continuation;
// prose-only lines are skipped silently.
//
// The parse fails closed (returns a non-nil error) when any directive names a
// verb that does not dispatch via ReviewDispatchableReviewVerbs -- the named
// verb resolver integration is the Drift #1 fix, and a non-dispatching verb is
// a hard violation: it would silently send an agent to run a command that
// does not exist.
//
// SDD-tree verbs (`gentle-ai sdd-continue`, `gentle-ai sdd-attempt`, etc.) are
// out of scope for the resolver per the Q1 resolution; lines naming only those
// verbs do not match the pre-filter and are skipped, never parsed. Adding a
// sibling SDD-tree extractor is a v2 follow-up if those directives ever need
// the same guarantee.
func ParseMarkdownByDesignEnvelope(source string) ([]ByDesignEnvelope, error) {
	dispatched, err := ReviewDispatchableReviewVerbs()
	if err != nil {
		return nil, fmt.Errorf("resolve dispatchable verbs: %w", err)
	}
	var envelopes []ByDesignEnvelope
	for lineNo, raw := range strings.Split(source, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !byDesignDirectivePreFilter(line) {
			continue
		}
		env, err := ParseByDesignEnvelope(line, lineNo+1)
		if err != nil {
			return nil, fmt.Errorf("parse markdown line %d: %w", lineNo+1, err)
		}
		if env.IsNamed() && !dispatched[env.Verb] {
			// refusal:by-design world-action: a non-dispatching verb in a directive is a fixture bug; repair the markdown, no command can fix it
			return nil, fmt.Errorf("line %d: directive names the review verb %q, but it is not in ReviewDispatchableReviewVerbs; a non-dispatching verb is a hard violation, not a satisfied named exit", lineNo+1, env.Verb)
		}
		envelopes = append(envelopes, env)
	}
	return envelopes, nil
}
