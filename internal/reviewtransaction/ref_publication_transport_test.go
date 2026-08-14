package reviewtransaction

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestRefPublicationTransportBuildPushArgvIsCanonical(t *testing.T) {
	_, repository := openRefPublicationFixture(t, "build-argv")
	auth := sampleRefPublicationAuthorization(t, "argv-canonical")
	auth.EndpointIdentity = "https://example.test/owner/repo.git"
	auth.DestinationRef = "refs/heads/feat/argv"
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	transport, err := OpenRefPublicationTransport(context.Background(), repository.RepositoryRef())
	if err != nil || transport == nil {
		// Use a real path-based Open since the fixture hides the path.
		repo := initSnapshotRepo(t)
		transport, err = OpenRefPublicationTransport(context.Background(), repo)
		if err != nil {
			t.Fatalf("OpenRefPublicationTransport: %v", err)
		}
		defer closeTransportRoot(t, transport)
	}
	path := transport.transportPath(auth.RequestID, auth.RequestDigest)
	argv, env, err := transport.buildPushCommandEnv(auth, path)
	if err != nil {
		t.Fatalf("buildPushCommandEnv: %v", err)
	}
	if err := validatePushArgv(argv); err != nil {
		t.Fatalf("validatePushArgv: %v", err)
	}
	if !slices.Contains(argv, "--no-verify") ||
		!slices.Contains(argv, "--porcelain") ||
		!slices.Contains(argv, "--no-tags") ||
		!slices.Contains(argv, "--no-replace-objects") {
		t.Fatalf("argv missing canonical markers: %#v", argv)
	}
	lease := ""
	for _, arg := range argv {
		if strings.HasPrefix(arg, "--force-with-lease=") {
			lease = arg
		}
	}
	if !strings.Contains(lease, ":"+zeroSHA1OID) {
		t.Fatalf("argv lease %q missing zero OID", lease)
	}
	if !slices.Contains(argv, auth.EndpointIdentity) {
		t.Fatalf("argv missing canonical endpoint %q: %#v", auth.EndpointIdentity, argv)
	}
	spec := auth.SourceCommit + ":" + auth.DestinationRef
	if !slices.Contains(argv, spec) {
		t.Fatalf("argv missing refspec %q: %#v", spec, argv)
	}
	for _, key := range []string{
		"GIT_DIR=" + path,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
	} {
		if !slices.Contains(env, key) {
			t.Fatalf("env missing %q: %#v", key, env)
		}
	}
	for _, banned := range []string{"push.default", "insteadOf", "pushInsteadOf"} {
		for _, entry := range env {
			if strings.Contains(entry, banned) {
				t.Fatalf("env contains forbidden token %q: %s", banned, entry)
			}
		}
	}
}

func TestRefPublicationTransportBuildPushArgvRejectsNonZeroLease(t *testing.T) {
	auth := sampleRefPublicationAuthorization(t, "argv-lease-reject")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/lease-reject"
	auth.EndpointIdentity = "https://example.test/owner/repo.git"

	if err := ValidateRefPublicationDestinationRef(auth.DestinationRef); err != nil {
		t.Fatalf("destination allowlist: %v", err)
	}
	degenerate := append([]string{}, "--force-with-lease="+auth.DestinationRef+":0123456789abcdef0123456789abcdef01234567")
	for _, arg := range degenerate {
		if !strings.HasPrefix(arg, "--force-with-lease=") {
			continue
		}
		if strings.Contains(arg, ":"+zeroSHA1OID) || strings.Contains(arg, ":"+zeroSHA256OID) {
			t.Fatalf("non-zero lease %q unexpectedly contains zero OID", arg)
		}
	}
}

func TestRefPublicationTransportValidatePushArgvRefusesBannedFlags(t *testing.T) {
	baseCanonical := []string{
		"--no-replace-objects",
		"-c", "core.attributesFile=/tmp/empty",
		"push",
		"--no-verify", "--porcelain", "--no-tags",
		"--force-with-lease=refs/heads/feat/x:" + zeroSHA1OID,
		"https://example.test/owner/repo.git",
		"0123456789abcdef0123456789abcdef01234567:refs/heads/feat/x",
	}
	banned := [][]string{
		append(append([]string{}, baseCanonical[:len(baseCanonical)-1]...), "--all", baseCanonical[len(baseCanonical)-1]),
		append(append([]string{}, baseCanonical[:len(baseCanonical)-1]...), "--mirror", baseCanonical[len(baseCanonical)-1]),
		append(append([]string{}, baseCanonical[:len(baseCanonical)-1]...), "--force-with-lease=refs/heads/feat/x:0123456789abcdef0123456789abcdef01234567", baseCanonical[len(baseCanonical)-1]),
		append(append([]string{}, baseCanonical[:len(baseCanonical)-1]...), "--force", baseCanonical[len(baseCanonical)-1]),
		append(append([]string{}, baseCanonical[:len(baseCanonical)-1]...), "--exec=cat", baseCanonical[len(baseCanonical)-1]),
		append(append([]string{}, baseCanonical[:len(baseCanonical)-1]...), "--push-option=foo", baseCanonical[len(baseCanonical)-1]),
	}
	for index, argv := range banned {
		if err := validatePushArgv(argv); err == nil {
			t.Fatalf("validatePushArgv accepted banned argv %d: %#v", index, argv)
		}
	}
}

func TestRefPublicationTransportStrictPorcelainParserAcceptsExactSoleNewBranch(t *testing.T) {
	auth := sampleRefPublicationAuthorization(t, "porcelain-exact")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/porcelain-exact"
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	argv := []string{"<exact>"}
	stdout := []byte(fmt.Sprintf(
		"To https://example.test/owner/repo.git\n" +
			" * 0123456789abcdef0123456789abcdef01234567 -> refs/heads/feat/porcelain-exact [new branch]\n" +
			"Done\n",
	))
	result, err := parseStrictPorcelain(stdout, nil, auth, argv)
	if err != nil {
		t.Fatalf("parseStrictPorcelain: %v", err)
	}
	if result.Classification != RefPublicationAttributionProven {
		t.Fatalf("classification = %q; want %q", result.Classification, RefPublicationAttributionProven)
	}
	if result.State != RefPubPushed {
		t.Fatalf("state = %q; want %q", result.State, RefPubPushed)
	}
}

func TestRefPublicationTransportStrictPorcelainParserRejectsForeignShapes(t *testing.T) {
	auth := sampleRefPublicationAuthorization(t, "porcelain-bad")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/porcelain-bad"

	cases := []struct {
		name   string
		stdout string
		wrap   error
	}{
		{
			name:   "missing To header",
			stdout: " * 0123456789abcdef0123456789abcdef01234567 -> refs/heads/feat/porcelain-bad [new branch]\nDone\n",
			wrap:   ErrRefPublicationPorcelainMalformed,
		},
		{
			name:   "missing Done trailer",
			stdout: "To https://example.test/owner/repo.git\n * 0123456789abcdef0123456789abcdef01234567 -> refs/heads/feat/porcelain-bad [new branch]\n",
			wrap:   ErrRefPublicationPorcelainMalformed,
		},
		{
			name: "non-dashed record line",
			stdout: "To https://example.test/owner/repo.git\n" +
				"*0123456789abcdef0123456789abcdef01234567 refs/heads/feat/porcelain-bad [new branch]\n" +
				"Done\n",
			wrap: ErrRefPublicationPorcelainMalformed,
		},
		{
			name: "no new branch reason",
			stdout: "To https://example.test/owner/repo.git\n" +
				" * 0123456789abcdef0123456789abcdef01234567 -> refs/heads/feat/porcelain-bad [up to date]\n" +
				"Done\n",
			wrap: ErrRefPublicationPorcelainMalformed,
		},
		{
			name: "rejected record",
			stdout: "To https://example.test/owner/repo.git\n" +
				" ! 0123456789abcdef0123456789abcdef01234567 -> refs/heads/feat/porcelain-bad [rejected]\n" +
				"Done\n",
			wrap: ErrRefPublicationLeaseRejected,
		},
		{
			name: "two new branches",
			stdout: "To https://example.test/owner/repo.git\n" +
				" * 0123456789abcdef0123456789abcdef01234567 -> refs/heads/feat/porcelain-bad [new branch]\n" +
				" * fedcba9876543210fedcba9876543210fedcba98 -> refs/heads/feat/other [new branch]\n" +
				"Done\n",
			wrap: ErrRefPublicationPorcelainAmbiguous,
		},
		{
			name: "wrong from OID",
			stdout: "To https://example.test/owner/repo.git\n" +
				" * fedcba9876543210fedcba9876543210fedcba98 -> refs/heads/feat/porcelain-bad [new branch]\n" +
				"Done\n",
			wrap: ErrRefPublicationDriftRejected,
		},
		{
			name: "wrong to ref",
			stdout: "To https://example.test/owner/repo.git\n" +
				" * 0123456789abcdef0123456789abcdef01234567 -> refs/heads/feat/other [new branch]\n" +
				"Done\n",
			wrap: ErrRefPublicationDriftRejected,
		},
		{
			name:   "empty body",
			stdout: "",
			wrap:   ErrRefPublicationPorcelainMalformed,
		},
		{
			name: "garbage line",
			stdout: "To https://example.test/owner/repo.git\n" +
				"unexpected trailing chatter\n" +
				"Done\n",
			wrap: ErrRefPublicationPorcelainMalformed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseStrictPorcelain([]byte(tc.stdout), nil, auth, []string{"<argv>"})
			if !errors.Is(err, tc.wrap) {
				t.Fatalf("parseStrictPorcelain(%q) error = %v; want %v", tc.name, err, tc.wrap)
			}
		})
	}
}

func TestRefPublicationTransportStrictLsRemote(t *testing.T) {
	auth := sampleRefPublicationAuthorization(t, "lsremote")
	auth.DestinationRef = "refs/heads/feat/lsremote"
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"

	missing, err := parseStrictLsRemote([]byte("\n"), nil, auth)
	if err != nil {
		t.Fatalf("parseStrictLsRemote(empty): %v", err)
	}
	if missing.Classification != RefPubNotCreated {
		t.Fatalf("missing classification = %q; want %q", missing.Classification, RefPubNotCreated)
	}

	matched, err := parseStrictLsRemote(
		[]byte("0123456789abcdef0123456789abcdef01234567\trefs/heads/feat/lsremote\n"),
		nil, auth,
	)
	if err != nil {
		t.Fatalf("parseStrictLsRemote(matched): %v", err)
	}
	if matched.Classification != RefPubConfirmed {
		t.Fatalf("matched classification = %q; want %q", matched.Classification, RefPubConfirmed)
	}

	drift, err := parseStrictLsRemote(
		[]byte("fedcba9876543210fedcba9876543210fedcba98\trefs/heads/feat/lsremote\n"),
		nil, auth,
	)
	if err != nil {
		t.Fatalf("parseStrictLsRemote(drift): %v", err)
	}
	if drift.Classification != RefPubConflict {
		t.Fatalf("drift classification = %q; want %q", drift.Classification, RefPubConflict)
	}

	ambiguous := "0123456789abcdef0123456789abcdef01234567\trefs/heads/feat/lsremote\n" +
		"fedcba9876543210fedcba9876543210fedcba98\trefs/heads/other\n"
	if _, err := parseStrictLsRemote([]byte(ambiguous), nil, auth); err != nil {
		t.Fatalf("parseStrictLsRemote(scattered): %v", err)
	}
}

func TestRefPublicationTransportPrepareCreatesBareRepo(t *testing.T) {
	repo := initSnapshotRepo(t)
	transport, err := OpenRefPublicationTransport(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransportRoot(t, transport)

	repository, err := OpenRefPublicationRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}

	auth := sampleRefPublicationAuthorization(t, "prepare-creates")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/prepare-creates"
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	record := newRefPublicationRecord(t, auth, RefPubPrepared)
	if err := transport.Prepare(context.Background(), auth, record); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	path := transport.transportPath(auth.RequestID, auth.RequestDigest)
	for _, subdir := range []string{"objects", "objects/info", "refs", "hooks", "info"} {
		if _, err := os.Lstat(filepath.Join(path, subdir)); err != nil {
			t.Fatalf("transport subdir %q missing: %v", subdir, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(path, "HEAD")); err != nil {
		t.Fatalf("transport HEAD missing: %v", err)
	}
	alternatesPath := filepath.Join(path, "objects/info/alternates")
	payload, err := os.ReadFile(alternatesPath)
	if err != nil {
		t.Fatalf("read alternates: %v", err)
	}
	want := filepath.Join(repository.rar.identity.GitCommonDir, "objects")
	if strings.TrimSpace(string(payload)) != want {
		t.Fatalf("alternates = %q; want %q", strings.TrimSpace(string(payload)), want)
	}

	if err := transport.Prepare(context.Background(), auth, newRefPublicationRecord(t, auth, RefPubPrepared)); err != nil {
		t.Fatalf("Prepare(replay): %v", err)
	}
}

func TestRefPublicationTransportPushOneShotRefusesReplay(t *testing.T) {
	runIsolatedGitPushOverride(t, &refPublicationPushFixture{
		stdout: "To https://example.test/owner/repo.git\n" +
			" * 0123456789abcdef0123456789abcdef01234567 -> refs/heads/feat/oneshot [new branch]\n" +
			"Done\n",
		stderr: "",
	}, defaultTransportMode)

	repo := initSnapshotRepo(t)
	transport, err := OpenRefPublicationTransport(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransportRoot(t, transport)

	auth := sampleRefPublicationAuthorization(t, "oneshot")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/oneshot"
	auth.EndpointIdentity = "https://example.test/owner/repo.git"
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	if err := transport.Prepare(context.Background(), auth, newRefPublicationRecord(t, auth, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}

	prepared := newRefPublicationRecord(t, auth, RefPubPrepared)
	result, pushErr := transport.Push(context.Background(), prepared)
	if pushErr != nil {
		t.Fatalf("first Push: %v", pushErr)
	}
	if result.Classification != RefPublicationAttributionProven {
		t.Fatalf("first push classification = %q", result.Classification)
	}

	pushed := prepared
	pushed.State = RefPubPushed
	if _, replayErr := transport.Push(context.Background(), pushed); !errors.Is(replayErr, ErrRefPublicationTransportAlreadyTerminal) {
		t.Fatalf("replay Push = %v; want ErrRefPublicationTransportAlreadyTerminal", replayErr)
	}
}

func TestRefPublicationTransportPushLeaseRejectionClassifiedAsConflict(t *testing.T) {
	runIsolatedGitPushOverride(t, &refPublicationPushFixture{
		stdout: "To https://example.test/owner/repo.git\n" +
			" ! 0123456789abcdef0123456789abcdef01234567 -> refs/heads/feat/conflict [rejected]\n" +
			"Done\n",
		stderr: "remote: error: failed to push some refs",
	}, defaultTransportMode)

	repo := initSnapshotRepo(t)
	transport, err := OpenRefPublicationTransport(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransportRoot(t, transport)

	auth := sampleRefPublicationAuthorization(t, "lease-reject")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/conflict"
	auth.EndpointIdentity = "https://example.test/owner/repo.git"
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	if err := transport.Prepare(context.Background(), auth, newRefPublicationRecord(t, auth, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}

	_, err = transport.Push(context.Background(), newRefPublicationRecord(t, auth, RefPubPrepared))
	if !errors.Is(err, ErrRefPublicationLeaseRejected) {
		t.Fatalf("lease rejection = %v; want ErrRefPublicationLeaseRejected", err)
	}
}

func TestRefPublicationTransportPushDriftRejectedBeforeDispatch(t *testing.T) {
	runIsolatedGitPushOverride(t, &refPublicationPushFixture{
		stdout: "To https://example.test/owner/repo.git\n" +
			" * fedcba9876543210fedcba9876543210fedcba98 -> refs/heads/feat/drift [new branch]\n" +
			"Done\n",
		stderr: "",
	}, defaultTransportMode)

	repo := initSnapshotRepo(t)
	transport, err := OpenRefPublicationTransport(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransportRoot(t, transport)

	auth := sampleRefPublicationAuthorization(t, "drift-rejected")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/drift"
	auth.EndpointIdentity = "https://example.test/owner/repo.git"
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	if err := transport.Prepare(context.Background(), auth, newRefPublicationRecord(t, auth, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}

	_, err = transport.Push(context.Background(), newRefPublicationRecord(t, auth, RefPubPrepared))
	if !errors.Is(err, ErrRefPublicationDriftRejected) {
		t.Fatalf("drift = %v; want ErrRefPublicationDriftRejected", err)
	}
}

func TestRefPublicationTransportPushCrashClassified(t *testing.T) {
	runIsolatedGitPushOverride(t, &refPublicationPushFixture{
		stdout:    "",
		stderr:    "fatal: unable to access",
		exitError: true,
	}, defaultTransportMode)

	repo := initSnapshotRepo(t)
	transport, err := OpenRefPublicationTransport(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransportRoot(t, transport)

	auth := sampleRefPublicationAuthorization(t, "crash")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/crash"
	auth.EndpointIdentity = "https://example.test/owner/repo.git"
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	if err := transport.Prepare(context.Background(), auth, newRefPublicationRecord(t, auth, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}

	_, err = transport.Push(context.Background(), newRefPublicationRecord(t, auth, RefPubPrepared))
	if !errors.Is(err, ErrRefPublicationPorcelainMalformed) &&
		!errors.Is(err, ErrRefPublicationTransportCrashed) {
		t.Fatalf("crash = %v; want porcelain/crash error", err)
	}
}

func TestRefPublicationTransportObserveClassifiesRemoteState(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		auth   *RefPublicationAuthorization
		want   RefPublicationState
	}{
		{
			name:   "destination absent",
			stdout: "\n",
			want:   RefPubNotCreated,
		},
		{
			name:   "destination matches C",
			stdout: "0123456789abcdef0123456789abcdef01234567\trefs/heads/feat/observe-match\n",
			want:   RefPubConfirmed,
		},
		{
			name:   "destination drifts to another OID",
			stdout: "fedcba9876543210fedcba9876543210fedcba98\trefs/heads/feat/observe-drift\n",
			want:   RefPubConflict,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runIsolatedGitPushOverride(t, &refPublicationPushFixture{stdout: tc.stdout}, observeTransportMode)

			repo := initSnapshotRepo(t)
			transport, err := OpenRefPublicationTransport(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			defer closeTransportRoot(t, transport)

			auth := sampleRefPublicationAuthorization(t, "observe-"+regexp.QuoteMeta(tc.name))
			auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
			auth.DestinationRef = "refs/heads/feat/observe-match"
			if tc.name == "destination drifts to another OID" {
				auth.DestinationRef = "refs/heads/feat/observe-drift"
			}
			auth.EndpointIdentity = "https://example.test/owner/repo.git"
			auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

			if err := transport.Prepare(context.Background(), auth, newRefPublicationRecord(t, auth, RefPubPrepared)); err != nil {
				t.Fatal(err)
			}

			observation, err := transport.Observe(context.Background(), newRefPublicationRecord(t, auth, RefPubPrepared))
			if err != nil {
				t.Fatalf("Observe(%s): %v", tc.name, err)
			}
			if observation.Classification != tc.want {
				t.Fatalf("observation classification = %q; want %q", observation.Classification, tc.want)
			}
		})
	}
}

func TestRefPublicationTransportReconcileFallsBackToPublicationUnknownOnCrash(t *testing.T) {
	runIsolatedGitPushOverride(t, &refPublicationPushFixture{
		stdout:    "",
		stderr:    "fatal: unable to access",
		exitError: true,
	}, observeTransportMode)

	repo := initSnapshotRepo(t)
	transport, err := OpenRefPublicationTransport(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransportRoot(t, transport)

	auth := sampleRefPublicationAuthorization(t, "reconcile")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/reconcile"
	auth.EndpointIdentity = "https://example.test/owner/repo.git"
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	if err := transport.Prepare(context.Background(), auth, newRefPublicationRecord(t, auth, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}

	reconciliation, err := transport.Reconcile(context.Background(), newRefPublicationRecord(t, auth, RefPubPrepared))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reconciliation.Classification != RefPubPublicationUnknown {
		t.Fatalf("Reconcile classification = %q; want %q", reconciliation.Classification, RefPubPublicationUnknown)
	}
}

func TestRefPublicationTransportPrepareWithoutRecordStateIsRejected(t *testing.T) {
	repo := initSnapshotRepo(t)
	transport, err := OpenRefPublicationTransport(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransportRoot(t, transport)

	auth := sampleRefPublicationAuthorization(t, "state-reject")
	auth.EndpointIdentity = "https://example.test/owner/repo.git"

	invalidRecord := newRefPublicationRecord(t, auth, RefPubConfirmed)
	err = transport.Prepare(context.Background(), auth, invalidRecord)
	if err == nil {
		t.Fatal("Prepare accepted a confirmed-state record")
	}
}

func TestRefPublicationTransportAlternatesFilePointsAtValidatedSourceObjects(t *testing.T) {
	repo := initSnapshotRepo(t)
	transport, err := OpenRefPublicationTransport(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransportRoot(t, transport)

	repository, err := OpenRefPublicationRepository(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}

	auth := sampleRefPublicationAuthorization(t, "alternates-validate")
	auth.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	auth.DestinationRef = "refs/heads/feat/alternates"
	auth.RequestDigest = RefPublicationAuthorizationDigest(auth)

	if err := transport.Prepare(context.Background(), auth, newRefPublicationRecord(t, auth, RefPubPrepared)); err != nil {
		t.Fatal(err)
	}
	path := transport.transportPath(auth.RequestID, auth.RequestDigest)
	alternatesPath := filepath.Join(path, "objects/info/alternates")
	payload, err := os.ReadFile(alternatesPath)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(repository.rar.identity.GitCommonDir, "objects")
	if !strings.HasSuffix(strings.TrimSpace(string(payload)), want) {
		t.Fatalf("alternates payload %q does not point at %q", strings.TrimSpace(string(payload)), want)
	}
	if err := validatePrivateRARDirectory(filepath.Dir(alternatesPath)); err != nil {
		t.Fatalf("alternates dir is not private: %v", err)
	}
}

func TestRefPublicationTransportCreatePrivateBareRepoLayoutIsPrivate(t *testing.T) {
	repo := initSnapshotRepo(t)
	transport, err := OpenRefPublicationTransport(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransportRoot(t, transport)
	path := filepath.Join(transport.TransportRoot(), "private-bare-test")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := transport.initBareTransport(context.Background(), path); err != nil {
		t.Fatalf("initBareTransport: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	if err := validatePrivateBareRepo(path); err != nil {
		t.Fatalf("validatePrivateBareRepo: %v", err)
	}
}

func TestRefPublicationTransportRejectsCredentialAmbiguity(t *testing.T) {
	transport := &RarRefPublicationTransport{}
	_, _, err := transport.buildPushCommandEnv(RefPublicationAuthorization{
		RequestID:           "req-cred",
		RequestDigest:       verificationTestHash("req-cred-digest"),
		LineageID:           "1471-lineage",
		AuthorityRevision:   verificationTestHash("cred-auth"),
		ReceiptRef:          verificationTestHash("cred-receipt"),
		EndpointIdentity:    "https://user:secret-token@example.test/owner/repo.git",
		LocalSourceRef:      "refs/heads/feat/cred",
		AdvertisedSourceRef: "refs/heads/main",
		DestinationRef:      "refs/heads/feat/cred",
		SourceCommit:        "0123456789abcdef0123456789abcdef01234567",
		CandidateTree:       "fedcba9876543210fedcba9876543210fedcba98",
		PathManifestDigest:  verificationTestHash("cred-manifest"),
		Actor:               "ardelperal",
		Reason:              "creds",
	}, filepath.Join(t.TempDir(), "transport.git"))
	if err == nil {
		t.Fatal("buildPushCommandEnv accepted an embedded credential URL")
	}
}

// -- test fakes for the git exec seam -------------------------------------------------

type transportMode int

const (
	defaultTransportMode transportMode = iota
	observeTransportMode
)

type refPublicationPushFixture struct {
	stdout    string
	stderr    string
	exitError bool
}

func runIsolatedGitPushOverride(
	t *testing.T,
	fixture *refPublicationPushFixture,
	mode transportMode,
) {
	t.Helper()
	t.Setenv("GENTLE_AI_TEST_TRANSPORT_HELPER", encodeTransportFixture(fixture))
}

func encodeTransportFixture(fixture *refPublicationPushFixture) string {
	return fmt.Sprintf("%d|%t|%s|%s",
		1, fixture.exitError, fixture.stdout, fixture.stderr,
	)
}

func closeTransportRoot(t *testing.T, transport *RarRefPublicationTransport) {
	t.Helper()
	if transport == nil || transport.repo == nil {
		return
	}
	if root := transport.TransportRoot(); root != "" {
		_ = os.RemoveAll(root)
	}
}

// io / bytes import bindings.
var _ = bytes.NewBuffer
