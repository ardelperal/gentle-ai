// Package reviewtransaction replay fixture for issue #1763.
//
// This file is a deterministic bounded-replay probe for the review-readability
// lens. It deliberately introduces a new unexported type that follows the
// Effective Go convention: a single lowercase first letter for non-exported
// identifiers (https://go.dev/doc/effective_go#names). The Go language
// specification and Effective Go both mandate this, and the package's
// established convention is already camelCase for unexported types.
//
// The replay asserts that the review-readability lens does NOT flag this
// type as a naming-convention violation. The 2026-10-03 comment on issue
// #1763 framed the close-with-evidence argument on the rule that the lens
// runtime must already carry; this fixture is the smallest possible
// candidate tree to exercise that rule.
package reviewtransaction

// allowedDecisionEdge is a deliberately-canonical unexported type used by the
// #1763 bounded replay. It is named in camelCase per the Go language
// specification and Effective Go because it is package-internal: it does not
// appear in any exported API surface. A runtime that flags this name as a
// "naming-convention violation" is reproducing the false positive that issue
// #1763 reported on PR #1696's `allowedDecisionEdge`.
//
// The fields here are intentionally inert: a struct with no logic, no
// methods, and no callers. The replay is purely about whether the lens
// picks on the name; if it does, the issue is still open; if it does not,
// the issue is closed.
type allowedDecisionEdge struct {
	fromCompactState string
	toCompactState   string
	reasonCode       string
}
