package reviewtransaction

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRefPublicationRepositorySaveAndLoadRoundTrip(t *testing.T) {
	_, repository := openRefPublicationFixture(t, "save-load")

	auth := sampleRefPublicationAuthorization(t, "request-save-load")
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	record := RefPublicationRecord{
		Schema:        RefPublicationRecordSchema,
		RequestID:     auth.RequestID,
		RequestDigest: auth.RequestDigest,
		State:         RefPubPrepared,
		Payload:       []byte(auth.Payload()),
		UpdatedAt:     "2026-08-13T19:53:36Z",
	}

	persisted, err := repository.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save(prepared): %v", err)
	}
	if persisted.RequestDigest != auth.RequestDigest {
		t.Fatalf("Save rewrote request_digest: got %q want %q", persisted.RequestDigest, auth.RequestDigest)
	}
	if persisted.Payload == nil {
		t.Fatal("Save persisted a nil payload")
	}

	loaded, err := repository.Load(context.Background(), record.RequestID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.RequestID != record.RequestID ||
		loaded.State != RefPubPrepared ||
		loaded.RequestDigest != record.RequestDigest {
		t.Fatalf("loaded record = %#v", loaded)
	}

	for _, path := range []string{
		filepath.Join(repository.root, "objects", record.RequestID+".json"),
		filepath.Join(repository.root, "by-request", record.RequestID+".json"),
		filepath.Join(repository.root, "by-endpoint-destination",
			hashPathComponent(auth.EndpointIdentity),
			pathSafeRef(auth.DestinationRef),
			record.RequestID+".json"),
	} {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("slot path %q = %#v, %v", path, info, statErr)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("slot path %q mode = %o", path, info.Mode().Perm())
		}
	}

	persisted, err = repository.Save(context.Background(), persisted)
	if err != nil {
		t.Fatalf("Save(replay prepared): %v", err)
	}
	if persisted.State != RefPubPrepared {
		t.Fatalf("exact replay changed state to %q", persisted.State)
	}

	pushed := persisted
	pushed.State = RefPubPushed
	pushed, err = repository.Save(context.Background(), pushed)
	if err != nil {
		t.Fatalf("Save(pushed): %v", err)
	}
	if pushed.State != RefPubPushed {
		t.Fatalf("Save(pushed) state = %q", pushed.State)
	}
}

func TestRefPublicationRepositoryRejectsReplayWithDifferentRequestDigest(t *testing.T) {
	_, repository := openRefPublicationFixture(t, "replay-mismatch")

	first := sampleRefPublicationAuthorization(t, "request-replay-1")
	first.RequestDigest = RefPublicationAuthorizationDigest(first)
	firstRecord := newRefPublicationRecord(t, first, RefPubPrepared)
	if _, err := repository.Save(context.Background(), firstRecord); err != nil {
		t.Fatalf("Save(first prepared): %v", err)
	}

	second := first
	second.SourceCommit = "fedcba9876543210fedcba9876543210fedcba98"
	second.RequestDigest = RefPublicationAuthorizationDigest(second)
	secondRecord := newRefPublicationRecord(t, second, RefPubPrepared)
	_, err := repository.Save(context.Background(), secondRecord)
	if !errors.Is(err, ErrRefPublicationReplayMismatch) {
		t.Fatalf("Save(replay) error = %v; want ErrRefPublicationReplayMismatch", err)
	}
}

func TestRefPublicationRepositoryMarkTerminalRejectsNonPushedState(t *testing.T) {
	_, repository := openRefPublicationFixture(t, "mark-terminal")

	auth := sampleRefPublicationAuthorization(t, "request-mark-terminal")
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)
	record := newRefPublicationRecord(t, auth, RefPubPrepared)
	if _, err := repository.Save(context.Background(), record); err != nil {
		t.Fatalf("Save(prepared): %v", err)
	}

	_, err := repository.MarkTerminal(context.Background(), record.RequestID,
		RefPubConfirmed, RefPublicationAttributionProven, verificationTestHash("result-stub"))
	if !errors.Is(err, ErrRefPublicationNotPrepared) {
		t.Fatalf("MarkTerminal from prepared = %v; want ErrRefPublicationNotPrepared", err)
	}

	if _, err := repository.Save(context.Background(), newRefPublicationRecord(t, auth, RefPubPushed)); err != nil {
		t.Fatalf("Save(pushed): %v", err)
	}

	_, err = repository.MarkTerminal(context.Background(), record.RequestID,
		RefPubConfirmed, RefPublicationAttributionProven, "sha256:"+t.Name()+"="+verificationTestHash("result"))
	if err != nil {
		t.Fatalf("MarkTerminal from pushed: %v", err)
	}

	loaded, err := repository.Load(context.Background(), record.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != RefPubConfirmed ||
		loaded.Attribution != RefPublicationAttributionProven ||
		loaded.ResultRef == "" {
		t.Fatalf("loaded terminal record = %#v", loaded)
	}

	_, err = repository.MarkTerminal(context.Background(), record.RequestID,
		RefPubConflict, RefPublicationAttributionUnproven, "")
	if !errors.Is(err, ErrRefPublicationNotPrepared) {
		t.Fatalf("MarkTerminal after confirmed = %v; want ErrRefPublicationNotPrepared", err)
	}
}

func TestRefPublicationRepositoryMarkTerminalRejectsAttributionMismatch(t *testing.T) {
	_, repository := openRefPublicationFixture(t, "attribution-mismatch")

	auth := sampleRefPublicationAuthorization(t, "request-attribution")
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)
	if _, err := repository.Save(context.Background(), newRefPublicationRecord(t, auth, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Save(context.Background(), newRefPublicationRecord(t, auth, RefPubPushed)); err != nil {
		t.Fatal(err)
	}

	if _, err := repository.MarkTerminal(context.Background(), auth.RequestID,
		RefPubConfirmed, RefPublicationAttributionUnproven, ""); err == nil {
		t.Fatal("MarkTerminal accepted confirmed verdict with unproven attribution")
	}
	if _, err := repository.MarkTerminal(context.Background(), auth.RequestID,
		RefPubConflict, RefPublicationAttributionProven, "result-ref"); err == nil {
		t.Fatal("MarkTerminal accepted non-confirmed state with proven attribution")
	}
}

func TestRefPublicationRepositoryListByEndpointDestinationFiltersTerminalAndForeign(t *testing.T) {
	_, repository := openRefPublicationFixture(t, "list")

	primary := sampleRefPublicationAuthorization(t, "request-primary")
	primary.RequestDigest = RefPublicationAuthorizationDigest(primary)
	if _, err := repository.Save(context.Background(), newRefPublicationRecord(t, primary, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}

	foreign := sampleRefPublicationAuthorization(t, "request-foreign")
	foreign.EndpointIdentity = verificationTestHash("foreign-endpoint")
	foreign.DestinationRef = "refs/heads/feat/other"
	foreign.RequestDigest = RefPublicationAuthorizationDigest(foreign)
	if _, err := repository.Save(context.Background(), newRefPublicationRecord(t, foreign, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}

	records, err := repository.ListByEndpointDestination(
		context.Background(), primary.EndpointIdentity, primary.DestinationRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RequestID != primary.RequestID {
		t.Fatalf("list mismatch = %#v", records)
	}

	if _, err := repository.Save(context.Background(), newRefPublicationRecord(t, primary, RefPubPushed)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.MarkTerminal(context.Background(), primary.RequestID,
		RefPubConfirmed, RefPublicationAttributionProven, verificationTestHash("primary-result")); err != nil {
		t.Fatal(err)
	}

	records, err = repository.ListByEndpointDestination(
		context.Background(), primary.EndpointIdentity, primary.DestinationRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("terminal records leaked into list = %#v", records)
	}

	replacement := sampleRefPublicationAuthorization(t, "request-replacement")
	replacement.RequestDigest = RefPublicationAuthorizationDigest(replacement)
	if _, err := repository.Save(context.Background(), newRefPublicationRecord(t, replacement, RefPubPrepared)); err != nil {
		t.Fatalf("re-prepared after terminal = %v", err)
	}
}

func TestRefPublicationRepositoryAllocatesOneActiveRequestPerEndpointDestination(t *testing.T) {
	_, repository := openRefPublicationFixture(t, "allocation")

	first := sampleRefPublicationAuthorization(t, "request-allocate-1")
	first.RequestDigest = RefPublicationAuthorizationDigest(first)
	if _, err := repository.Save(context.Background(), newRefPublicationRecord(t, first, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}

	second := sampleRefPublicationAuthorization(t, "request-allocate-2")
	second.LocalSourceRef = "refs/heads/feat/other-integration"
	second.AdvertisedSourceRef = "refs/heads/main"
	second.DestinationRef = first.DestinationRef
	second.EndpointIdentity = first.EndpointIdentity
	second.RequestDigest = RefPublicationAuthorizationDigest(second)
	_, err := repository.Save(context.Background(), newRefPublicationRecord(t, second, RefPubPrepared))
	if !errors.Is(err, ErrRefPublicationAllocationContested) {
		t.Fatalf("contested Save error = %v; want ErrRefPublicationAllocationContested", err)
	}
}

func TestRefPublicationRepositoryLoadReturnsUnknownForMissingRequest(t *testing.T) {
	repo, repository := openRefPublicationFixture(t, "load-missing")

	_, err := repository.Load(context.Background(), "never-persisted-request")
	if !errors.Is(err, ErrRefPublicationUnknownRequestID) {
		t.Fatalf("Load(missing) error = %v; want ErrRefPublicationUnknownRequestID", err)
	}
	if _, err := os.Lstat(filepath.Join(repo, "never-persisted-request")); err == nil {
		t.Fatal("unknown request id listing reported a path inside the source repo")
	}
}

func openRefPublicationFixture(t *testing.T, name string) (string, *RarRefPublicationRepository) {
	t.Helper()
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "tracked.txt", "ref-publication "+name+"\n")
	repository, err := OpenRefPublicationRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, repository
}

func newRefPublicationRecord(
	t *testing.T,
	auth RefPublicationAuthorization,
	state RefPublicationState,
) RefPublicationRecord {
	t.Helper()
	if auth.RequestDigest == "" {
		auth.RequestDigest = RefPublicationAuthorizationDigest(auth)
	}
	return RefPublicationRecord{
		Schema:        RefPublicationRecordSchema,
		RequestID:     auth.RequestID,
		RequestDigest: auth.RequestDigest,
		State:         state,
		Payload:       []byte(auth.Payload()),
		UpdatedAt:     "2026-08-13T19:53:36Z",
	}
}
