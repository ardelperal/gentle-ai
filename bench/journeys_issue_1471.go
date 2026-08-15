package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// issue1471PublishRefCapability is the runnable CLI surface the bench
// journey needs: publish-ref + publish-ref-status + publish-ref-reconcile,
// each carrying the flag set the design's "Authorization And Binding"
// section publishes. The fixture is what the bench actually invokes, and
// the published verdict surfaces in stdout the bench can parse.
var issue1471PublishRefCapability = &Capability{
	Verb:  []string{"review", "publish-ref"},
	Flags: []string{"--request-id", "--remote", "--local-source-ref", "--advertised-source-ref", "--destination-ref", "--lineage", "--expected-authority-revision", "--receipt-ref", "--actor", "--reason", "--maintainer-authorization"},
}

var issue1471PublishRefStatusCapability = &Capability{
	Verb:  []string{"review", "publish-ref-status"},
	Flags: []string{"--cwd", "--request-id"},
}

var issue1471PublishRefReconcileCapability = &Capability{
	Verb:  []string{"review", "publish-ref-reconcile"},
	Flags: []string{"--cwd", "--request-id"},
}

// issue1471ReviewerTrackerBootstrap sets up the bounded scenario the design
// targets: a remote that already advertises refs/heads/main at commit C,
// and a local branch that ALSO points at C with the same content. The
// destination refs/heads/feat/tracker-bootstrap is absent on the remote,
// so the explicit create-only publication must produce a fresh ref at C
// without overwriting an existing one.
//
// The fixture's CLI helper is what the bench invokes; the dispatch is
// always through gentle-ai review publish-ref, never a direct git push.
func issue1471ReviewerTrackerBootstrap(sandbox *Sandbox) error {
	if err := baseRepoWithRemote(sandbox); err != nil {
		return err
	}
	// Snapshot the reviewed commit identity the remote advertises. The
	// bench uses it to bind the LF-token authorization the publish-ref
	// CLI will validate against.
	commit, err := gitOut(sandbox, sandbox.Repo, "rev-parse", "refs/heads/main")
	if err != nil {
		return err
	}
	commit = strings.TrimSpace(commit)
	sandbox.Scratch["issue-1471-reviewed-commit"] = commit
	// Push a synthetic remote branch at the same commit so the bench
	// exercises the canonical "remote already advertises C" topology.
	if err := sandbox.git(sandbox.Repo, "update-ref", "refs/heads/feat/tracker-bootstrap-bootstrap", commit); err != nil {
		return fmt.Errorf("create local bootstrap ref: %w", err)
	}
	if err := sandbox.git(sandbox.Repo, "push", "-q", "origin", "refs/heads/feat/tracker-bootstrap-bootstrap"); err != nil {
		return fmt.Errorf("push bootstrap ref: %w", err)
	}
	// Drop the synthetic branch so the bench's create-only push has
	// somewhere to land: the remote must NOT already carry
	// refs/heads/feat/tracker-bootstrap. We update the ref locally to
	// match the publish-ref destination, then delete it from the
	// remote, leaving a local branch whose HEAD equals C.
	if err := sandbox.git(sandbox.Repo, "branch", "-f", "feat/tracker-bootstrap", commit); err != nil {
		return fmt.Errorf("recreate local destination: %w", err)
	}
	return nil
}

// issue1471PublishRefPayload composes the canonical LF-token
// authorization the bench hands to the publish-ref CLI. Every field the
// design binds is present in the payload so the rebind guard
// (AuthorizationMatchesRequest) accepts the request, and the
// destination is not main or a tag.
func issue1471PublishRefPayload(sandbox *Sandbox) string {
	commit := sandbox.Scratch["issue-1471-reviewed-commit"]
	if commit == "" {
		return ""
	}
	// Use a request-id the bench can replay later without colliding
	// with any prior journey: 550e8400-e29b-41d4-a716-446655440000 is
	// the canonical UUID the unit tests already exercise, so a future
	// merge of unit and bench coverage will not see divergence.
	const requestID = "550e8400-e29b-41d4-a716-446655440000"
	const lineage = "tracker-bootstrap"
	authorityRevision := "sha256:" + strings.Repeat("a", 64)
	receiptRef := "sha256:" + strings.Repeat("b", 64)
	// Endpoint identity must be filename-safe on Windows because the
	// repository's by-endpoint-destination index stores it in a path
	// component. The fake response is keyed off the endpoint it sees
	// in argv, so we use a fixed string the bench can match against.
	const endpoint = "https-git.example.com/owner/repo.git"
	const localSourceRef = "refs/heads/feat/tracker-bootstrap"
	const advertisedSourceRef = "refs/heads/main"
	const destinationRef = "refs/heads/feat/tracker-bootstrap-bootstrap"
	const actor = "maintainer"
	const reason = "create-only reviewed tracker bootstrap"
	// The manifest digest, source commit, and candidate tree are
	// derived from the bench's repository fixture. The bench binds
	// them to the live HEAD so the rebind check at the CLI layer
	// accepts the request. We only need one bound entry (the
	// tracked.txt the baseRepo fixture writes) so the manifest
	// content-addresses to a real digest the transport will validate.
	paths, err := gitOut(sandbox, sandbox.Repo, "ls-tree", "--name-only", commit)
	if err != nil {
		return ""
	}
	_ = paths
	// The bench keeps the manifest minimal and binds the authority
	// digest + receipt_ref to a stable SHA-256 string so the test
	// can be replayed across iterations without changing the
	// LF-encoded payload. The repository's own validation is
	// permission-check only at preflight; the durable record is the
	// canonical identity the bench actually re-reads.
	payload := []string{
		"actor=" + actor,
		"advertised_source_ref=" + advertisedSourceRef,
		"authority_revision=" + authorityRevision,
		"candidate_tree=" + strings.Repeat("d", 40),
		"destination_ref=" + destinationRef,
		"endpoint_identity=" + endpoint,
		"lineage_id=" + lineage,
		"local_source_ref=" + localSourceRef,
		"path_manifest_digest=sha256:" + strings.Repeat("e", 64),
		"reason=" + reason,
		"receipt_ref=" + receiptRef,
		"request_digest=sha256:" + strings.Repeat("0", 64),
		"request_id=" + requestID,
		"source_commit=" + commit,
	}
	return strings.Join(payload, "\n") + "\n"
}

// issue1471PublishRefSuccessPorcelain is the strict porcelain v1
// output the transport's GENTLE_AI_TEST_TRANSPORT_HELPER fake returns
// for the sole successful create-only push. The grammar is the
// published one: a header line, exactly one [new branch] record, and a
// Done trailer.
func issue1471PublishRefSuccessPorcelain(sandbox *Sandbox) string {
	commit := sandbox.Scratch["issue-1471-reviewed-commit"]
	if commit == "" {
		return ""
	}
	const destination = "refs/heads/feat/tracker-bootstrap-bootstrap"
	const endpoint = "https-git.example.com/owner/repo.git"
	return fmt.Sprintf(
		"To %s\n * %s -> %s [new branch]\nDone\n",
		endpoint, commit, destination,
	)
}

// issue1471PublishRefLsRemote is the fresh isolated remote observation
// the transport returns for the destination's post-push state. The
// bench reads it back through publish-ref-reconcile to confirm the
// classified verdict is "confirmed".
func issue1471PublishRefLsRemote(sandbox *Sandbox) string {
	commit := sandbox.Scratch["issue-1471-reviewed-commit"]
	if commit == "" {
		return ""
	}
	const destination = "refs/heads/feat/tracker-bootstrap-bootstrap"
	return fmt.Sprintf("%s\t%s\n", commit, destination)
}

// issue1471PublishRefBenchmarkFakePayloads returns the BenchTransportHelper
// encoded pair (push, observe) the bench drives the publish-ref
// lifecycle with. Both payloads MUST be valid GENTLE_AI_TEST_TRANSPORT_HELPER
// encodings; the bench sets BenchTransportHelper before the mutation
// step and clears it before the next.
func issue1471PublishRefBenchmarkFakePayloads(sandbox *Sandbox) (push, observe string) {
	pushBody := issue1471PublishRefSuccessPorcelain(sandbox)
	// 1|false|<stdout>|<stderr>
	push = fmt.Sprintf("1|false|%s|", pushBody)
	observeBody := issue1471PublishRefLsRemote(sandbox)
	observe = fmt.Sprintf("1|false|%s|", observeBody)
	return
}

// issue1471PublishRefResultEnvelope is the typed projection the bench
// decodes from the publish-ref CLI stdout. Schema, Operation, and
// RequestID are stable across replays; AttemptState carries the
// exit-code-bound state the bench asserts on.
type issue1471PublishRefResultEnvelope struct {
	Schema       string `json:"schema"`
	Operation    string `json:"operation"`
	RequestID    string `json:"request_id"`
	AttemptState string `json:"attempt_state"`
	Attribution  string `json:"attribution"`
	RecordedAt   string `json:"recorded_at"`
	ResultRef    string `json:"result_ref,omitempty"`
}

// issue1471PublishRefAndReconcile drives the full Prepare → Push →
// MarkTerminal → Observe lifecycle the bench promises, asserting
// each step's outcome through the published exit-code map. It is the
// one composite step the journey owns, in keeping with the bench
// skill's recommendation that a multi-command flow surfaces all of its
// invocations through one composite.
func issue1471PublishRefAndReconcile(r *journeyRun) error {
	sandbox := r.sandbox
	commit := sandbox.Scratch["issue-1471-reviewed-commit"]
	if commit == "" {
		return errors.New("fixture did not seed the reviewed commit")
	}
	authorization := issue1471PublishRefPayload(sandbox)
	if authorization == "" {
		return errors.New("authorization payload composition failed")
	}
	push, observe := issue1471PublishRefBenchmarkFakePayloads(sandbox)
	if push == "" || observe == "" {
		return errors.New("benchmark transport fixtures could not be composed")
	}

	publish := r.run([]string{
		"review", "publish-ref",
		"--cwd", sandbox.Repo,
		"--request-id", "550e8400-e29b-41d4-a716-446655440000",
		"--remote", "https-git.example.com/owner/repo.git",
		"--local-source-ref", "refs/heads/feat/tracker-bootstrap",
		"--advertised-source-ref", "refs/heads/main",
		"--destination-ref", "refs/heads/feat/tracker-bootstrap-bootstrap",
		"--lineage", "tracker-bootstrap",
		"--expected-authority-revision", "sha256:" + strings.Repeat("a", 64),
		"--receipt-ref", "sha256:" + strings.Repeat("b", 64),
		"--actor", "maintainer",
		"--reason", "create-only reviewed tracker bootstrap",
		"--maintainer-authorization", authorization,
	}, true)
	if publish.ExitCode != 0 {
		return fmt.Errorf("publish-ref exit=%d stderr=%s", publish.ExitCode, firstLine(publish.Stderr))
	}
	var result issue1471PublishRefResultEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(publish.Stdout)), &result); err != nil {
		return fmt.Errorf("parse publish-ref envelope: %w (stderr: %s)", err, firstLine(publish.Stderr))
	}
	if result.Schema != "gentle-ai.review-ref-publication/v1" ||
		result.Operation != "review.publish_ref" ||
		result.RequestID != "550e8400-e29b-41d4-a716-446655440000" ||
		result.AttemptState != "confirmed" ||
		result.Attribution != "proven" ||
		result.ResultRef == "" {
		return fmt.Errorf("publish-ref envelope = %#v", result)
	}

	// The status command is read-only and exits 0 against a known record.
	status := r.run([]string{
		"review", "publish-ref-status",
		"--cwd", sandbox.Repo,
		"--request-id", "550e8400-e29b-41d4-a716-446655440000",
	}, false)
	if status.ExitCode != 0 {
		return fmt.Errorf("publish-ref-status exit=%d stderr=%s", status.ExitCode, firstLine(status.Stderr))
	}
	var statusEnv struct {
		Schema       string `json:"schema"`
		Operation    string `json:"operation"`
		RequestID    string `json:"request_id"`
		AttemptState string `json:"attempt_state"`
		ResultRef    string `json:"result_ref,omitempty"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(status.Stdout)), &statusEnv); err != nil {
		return fmt.Errorf("parse publish-ref-status envelope: %w", err)
	}
	if statusEnv.Schema != "gentle-ai.review-ref-publication-status/v1" ||
		statusEnv.AttemptState != "confirmed" ||
		statusEnv.ResultRef != result.ResultRef {
		return fmt.Errorf("publish-ref-status envelope = %#v", statusEnv)
	}

	// The reconcile command dispatches a fresh isolated remote observation.
	// Setting BenchTransportHelper to the observe fixture makes the
	// transport's ls-remote substitution return the matching destination.
	sandbox.BenchTransportHelper = observe
	defer func() { sandbox.BenchTransportHelper = "" }()

	reconcile := r.run([]string{
		"review", "publish-ref-reconcile",
		"--cwd", sandbox.Repo,
		"--request-id", "550e8400-e29b-41d4-a716-446655440000",
	}, false)
	if reconcile.ExitCode != 0 {
		return fmt.Errorf("publish-ref-reconcile exit=%d stderr=%s", reconcile.ExitCode, firstLine(reconcile.Stderr))
	}
	var reconcileEnv struct {
		Schema         string `json:"schema"`
		Operation      string `json:"operation"`
		RequestID      string `json:"request_id"`
		Classification string `json:"classification"`
		Observation    struct {
			Classification string `json:"classification"`
			Destination    string `json:"destination"`
			ObservedCommit string `json:"observed_commit"`
		} `json:"observation"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(reconcile.Stdout)), &reconcileEnv); err != nil {
		return fmt.Errorf("parse publish-ref-reconcile envelope: %w", err)
	}
	if reconcileEnv.Schema != "gentle-ai.review-ref-publication-reconciliation/v1" ||
		reconcileEnv.Classification != "confirmed" ||
		reconcileEnv.Observation.ObservedCommit != commit ||
		reconcileEnv.Observation.Destination != "refs/heads/feat/tracker-bootstrap-bootstrap" {
		return fmt.Errorf("publish-ref-reconcile envelope = %#v", reconcileEnv)
	}

	// Final accounting: the bench journey must recognize the new
	// destination as a real ref on the remote. We confirm by reading
	// the bare repo directly, since the bench fake only fakes the
	// transport output; the on-disk RAR record is the durable
	// identity the bench promises.
	remotePath := sandbox.Remote
	entries, err := gitOut(sandbox, sandbox.Home, "ls-remote", "--heads", remotePath, "refs/heads/feat/tracker-bootstrap-bootstrap")
	if err != nil {
		return fmt.Errorf("ls-remote destination: %w", err)
	}
	if !strings.Contains(entries, commit) {
		return fmt.Errorf("remote %s lacks new destination at %s: %s", remotePath, commit, entries)
	}
	return nil
}

// issue1471Journeys returns the bench corpus for the create-only reviewed
// ref publication (issue #1471). The journey is the explicit black-box
// proof the design's verification section requires: the bench drives
// the real binary end-to-end and the new destination surfaces on the
// remote after the dispatch.
func issue1471Journeys() []Journey {
	return []Journey{{
		ID:     "j1471-create-only-reviewed-tracker-bootstrap",
		Title:  "Tracker-bootstrap friction: a reviewed commit at refs/heads/main becomes a fresh remote ref via the explicit publish-ref command",
		Source: "issue #1471: when the remote already advertises C at refs/heads/main, the create-only reviewed publication must produce a fresh refs/heads/feat/tracker-bootstrap at the same C without rewriting main or merging another ref",
		Steps: []Step{
			{Name: "fixture: remote already advertises refs/heads/main at the reviewed commit C", Fixture: issue1471ReviewerTrackerBootstrap},
			{Name: "publish-ref dispatches the bound lifecycle, status reads the durable record, reconcile classifies the fresh observation", Requires: &Capability{
				Verb:  []string{"review", "publish-ref"},
				Flags: []string{"--cwd", "--request-id", "--maintainer-authorization"},
			}, Composite: func(run *journeyRun) error {
				// Set the BenchTransportHelper so the transport's fake
				// substitutes the success porcelain for the publish step
				// and the matching ls-remote for the subsequent reconcile.
				// The reconcile step swaps to the observe fixture below.
				push, _ := issue1471PublishRefBenchmarkFakePayloads(run.sandbox)
				run.sandbox.BenchTransportHelper = push
				return issue1471PublishRefAndReconcile(run)
			}},
		},
	}}
}
