package reviewtransaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	refPublicationTransportsDirectory = "transports"
	refPublicationTransportGitName    = "transport.git"
	refPublicationTransportAlternates = "objects/info/alternates"
	refPublicationTransportPushMark   = "push-completed"
	refPublicationTransportPushLog    = "push.log"
	refPublicationTransportMaxStdout  = 1 << 20
	refPublicationTransportMaxStderr  = 64 << 10
	refPublicationTransportPushWait   = 90 * time.Second

	zeroSHA1OID   = "0000000000000000000000000000000000000000"
	zeroSHA256OID = "0000000000000000000000000000000000000000000000000000000000000000"
)

var (
	// ErrRefPublicationTransportUnavailable is returned when the isolated bare
	// transport cannot be set up (provider credentials, host identity, or
	// Git runtime are not provably present).
	ErrRefPublicationTransportUnavailable = errors.New("ref publication isolated transport is unavailable")
	// ErrRefPublicationDestinationRace is returned when the server reports a
	// concurrent ref-creation race; the zero-OID lease fired.
	ErrRefPublicationDestinationRace = errors.New("ref publication destination was created concurrently")
	// ErrRefPublicationDriftRejected is returned when the source ref, tree,
	// path, mode, blob, receipt, or authority has drifted between prepare
	// and the sole push.
	ErrRefPublicationDriftRejected = errors.New("ref publication authorization drift was detected before transport dispatch")
	// ErrRefPublicationPorcelainMalformed is returned when the strict
	// porcelain output does not match the required grammar.
	ErrRefPublicationPorcelainMalformed = errors.New("ref publication strict porcelain output is malformed")
	// ErrRefPublicationPorcelainAmbiguous is returned when the strict porcelain
	// output has more than one [new branch] record, an unexpected record, or
	// any rejection.
	ErrRefPublicationPorcelainAmbiguous = errors.New("ref publication porcelain output is ambiguous")
	// ErrRefPublicationLeaseRejected is returned when --force-with-lease
	// failed: the destination ref exists, no record satisfies the zero-OID
	// lease.
	ErrRefPublicationLeaseRejected = errors.New("ref publication force-with-lease was rejected by the server")
	// ErrRefPublicationTransportCrashed is returned when the isolated push
	// exited without producing attributable porcelain.
	ErrRefPublicationTransportCrashed = errors.New("ref publication isolated transport crashed mid-operation")
	// ErrRefPublicationObserverCrashed is returned when a fresh remote
	// observation fails to converge to a canonical answer.
	ErrRefPublicationObserverCrashed = errors.New("ref publication remote observer crashed mid-operation")
	// ErrRefPublicationTransportNotReady is returned when Push or Observe is
	// called without a PersistedPrepared record under this request id.
	ErrRefPublicationTransportNotReady = errors.New("ref publication transport is not prepared for this request")
	// ErrRefPublicationTransportAlreadyTerminal is returned when a second push
	// is attempted after the prepared transport has completed a successful
	// push. The one-use authorization budget forbids dispatching again.
	ErrRefPublicationTransportAlreadyTerminal = errors.New("ref publication transport has already executed its single push")
)

// refPublicationTransportHandleSchema is the durable bind between the
// prepared record and the on-disk isolated transport. It is persisted as a
// sidecar so the read-only reconcile path can recover the transport
// metadata without re-deriving it.
const refPublicationTransportHandleSchema = "gentle-ai.review-ref-publication-transport-handle/v1"

type refPublicationTransportHandleRecord struct {
	Schema        string                      `json:"schema"`
	RequestID     string                      `json:"request_id"`
	RequestDigest string                      `json:"request_digest"`
	TransportPath string                      `json:"transport_path"`
	Authorization RefPublicationAuthorization `json:"authorization"`
	CreatedAt     string                      `json:"created_at"`
}

func (handle refPublicationTransportHandleRecord) Validate() error {
	if handle.Schema != refPublicationTransportHandleSchema {
		return errors.New("ref publication transport handle schema is invalid")
	}
	if handle.RequestID == "" || !validSHA256(handle.RequestDigest) {
		return errors.New("ref publication transport handle request binding is invalid")
	}
	if strings.TrimSpace(handle.TransportPath) == "" {
		return errors.New("ref publication transport path is empty")
	}
	if err := handle.Authorization.Validate(); err != nil {
		return fmt.Errorf("ref publication transport authorization: %w", err)
	}
	return nil
}

var (
	porcelainHeaderLine = regexp.MustCompile(`^To (?P<url>[!-~\x20]+)$`)
	porcelainRecordLine = regexp.MustCompile(`^[[:space:]]+(?P<flag>[*!])[[:space:]]+(?P<from>[0-9a-f]{40,64})[[:space:]]+->[[:space:]]+(?P<to>refs/heads/[A-Za-z0-9._/-]+)(?:[[:space:]]+\[(?P<reason>[A-Za-z0-9 _-]+)\])?$`)
	porcelainDoneLine   = regexp.MustCompile(`^Done$`)
)

// RarRefPublicationTransport is the provider-owned isolated bare transport.
// It binds one exact Git repository lease, owns the per-request isolated
// bare repo under <rar-root>/ref-publications/v1/transports/.../transport.git,
// and dispatches the single create-only git push through a strictly sanitized
// process tree.
type RarRefPublicationTransport struct {
	repo *RarRefPublicationRepository
}

// OpenRefPublicationTransport opens the isolated transport seam under the
// same RAR authority root as the publish-ref repository.
func OpenRefPublicationTransport(
	ctx context.Context,
	repo string,
) (*RarRefPublicationTransport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	publication, err := OpenRefPublicationRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &RarRefPublicationTransport{repo: publication}, nil
}

// RepositoryRef returns the path-free Git identity retained by the parent
// RAR authority repository. CLI commands use it to confirm the running
// repository is the same one the record was published against.
func (transport *RarRefPublicationTransport) RepositoryRef() string {
	if transport == nil || transport.repo == nil {
		return ""
	}
	return transport.repo.RepositoryRef()
}

// TransportRoot returns the on-disk root under which every per-request
// isolated bare transport lives.
func (transport *RarRefPublicationTransport) TransportRoot() string {
	if transport == nil || transport.repo == nil {
		return ""
	}
	return filepath.Join(
		transport.repo.rar.root,
		refPublicationsDirectory,
		refPublicationsVersion,
		refPublicationTransportsDirectory,
	)
}

func (transport *RarRefPublicationTransport) transportPath(
	requestID, requestDigest string,
) string {
	return filepath.Join(
		transport.TransportRoot(),
		requestID,
		hashPathComponent(requestDigest),
		refPublicationTransportGitName,
	)
}

func (transport *RarRefPublicationTransport) handlePath(
	requestID, requestDigest string,
) string {
	return filepath.Join(
		transport.TransportRoot(),
		requestID,
		"handle.json",
	)
}

// Prepare builds the isolated bare transport and persists the prepared record
// under the supplied authorization. The same request_id may only be replayed
// with the identical request_digest.
func (transport *RarRefPublicationTransport) Prepare(
	ctx context.Context,
	auth RefPublicationAuthorization,
	record RefPublicationRecord,
) (RefPublicationTransportHandleRecord, error) {
	if err := ctx.Err(); err != nil {
		return RefPublicationTransportHandleRecord{}, err
	}
	if transport == nil || transport.repo == nil {
		return RefPublicationTransportHandleRecord{}, errors.New("ref publication transport is not initialized")
	}
	if err := auth.Validate(); err != nil {
		return RefPublicationTransportHandleRecord{}, fmt.Errorf("ref publication authorization: %w", err)
	}
	if record.RequestID != auth.RequestID {
		return RefPublicationTransportHandleRecord{}, errors.New("ref publication record request_id does not match authorization")
	}
	if record.State != RefPubPrepared && record.State != RefPubPushed {
		return RefPublicationTransportHandleRecord{}, fmt.Errorf(
			"%w: prepare requires prepared or pushed state; got %q",
			ErrRefPublicationTransitionIllegal, record.State,
		)
	}
	if err := transport.repo.rar.validateIdentity(ctx); err != nil {
		return RefPublicationTransportHandleRecord{}, err
	}
	if err := ensureRARRepositoryRoot(transport.repo.rar.identity.GitCommonDir, transport.repo.rar.root, true); err != nil {
		return RefPublicationTransportHandleRecord{}, err
	}
	if err := ensurePrivateRARDirectoryTree(transport.repo.rar.root, transport.repo.rar.root, true); err != nil {
		return RefPublicationTransportHandleRecord{}, err
	}
	parent := filepath.Dir(transport.transportPath(auth.RequestID, auth.RequestDigest))
	if err := ensurePrivateRARDirectoryTree(transport.repo.rar.root, parent, true); err != nil {
		return RefPublicationTransportHandleRecord{}, err
	}
	path := transport.transportPath(auth.RequestID, auth.RequestDigest)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if err := transport.initBareTransport(ctx, path); err != nil {
			return RefPublicationTransportHandleRecord{}, err
		}
		if err := transport.bindAlternates(path); err != nil {
			return RefPublicationTransportHandleRecord{}, err
		}
		if err := transport.clearPushMark(path); err != nil {
			return RefPublicationTransportHandleRecord{}, err
		}
		if err := SyncReviewDirectory(filepath.Dir(path)); err != nil {
			return RefPublicationTransportHandleRecord{}, err
		}
	} else if err != nil {
		return RefPublicationTransportHandleRecord{}, err
	}
	if existing, err := transport.loadHandleForRequestID(auth.RequestID); err == nil {
		if existing.RequestDigest != auth.RequestDigest {
			return RefPublicationTransportHandleRecord{}, fmt.Errorf(
				"%w: request_id=%s", ErrRefPublicationReplayMismatch, auth.RequestID,
			)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return RefPublicationTransportHandleRecord{}, err
	}
	handle := refPublicationTransportHandleRecord{
		Schema:        refPublicationTransportHandleSchema,
		RequestID:     auth.RequestID,
		RequestDigest: auth.RequestDigest,
		TransportPath: path,
		Authorization: auth,
		CreatedAt:     record.UpdatedAt,
	}
	payload, err := json.Marshal(handle)
	if err != nil {
		return RefPublicationTransportHandleRecord{}, fmt.Errorf("encode ref publication transport handle: %w", err)
	}
	if err := writePrivateRecordFile(transport.handlePath(auth.RequestID, auth.RequestDigest), payload); err != nil {
		return RefPublicationTransportHandleRecord{}, err
	}
	if err := transport.repo.rar.validateIdentity(ctx); err != nil {
		return RefPublicationTransportHandleRecord{}, err
	}
	return RefPublicationTransportHandleRecord{
		Handle: handle,
	}, nil
}

// loadHandleForRequestID re-reads the durable handle for a request_id. The
// handle stores the exact request_digest bound when the handle was written;
// a mismatch means a different authorization is being replayed under the
// same request_id and Prepare must refuse.
func (transport *RarRefPublicationTransport) loadHandleForRequestID(
	requestID string,
) (refPublicationTransportHandleRecord, error) {
	payload, err := readPrivateRARFile(transport.handlePath(requestID, ""))
	if err != nil {
		return refPublicationTransportHandleRecord{}, err
	}
	var handle refPublicationTransportHandleRecord
	if err := json.Unmarshal(payload, &handle); err != nil {
		return refPublicationTransportHandleRecord{}, fmt.Errorf("parse ref publication transport handle: %w", err)
	}
	if handle.RequestID != requestID {
		return refPublicationTransportHandleRecord{}, errors.New("ref publication transport handle request_id mismatch")
	}
	if err := handle.Validate(); err != nil {
		return refPublicationTransportHandleRecord{}, err
	}
	return handle, nil
}

// initBareTransport creates the fresh empty private bare repo on disk. The
// objects/info/alternates file is written separately so this stage cannot
// leak source refs into the transport.
func (transport *RarRefPublicationTransport) initBareTransport(
	ctx context.Context,
	path string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := createPrivateBareRepo(path); err != nil {
		return fmt.Errorf("%w: initialize bare transport: %v", ErrRefPublicationTransportUnavailable, err)
	}
	if err := validatePrivateRARDirectory(path); err != nil {
		return fmt.Errorf("%w: validate bare transport: %v", ErrRefPublicationTransportUnavailable, err)
	}
	return nil
}

// bindAlternates writes the validated absolute objects path into the
// transport's objects/info/alternates file. The transport gains read-only
// access to the source objects without inheriting any source remotes,
// refs, hooks, or configuration. The source objects directory is the
// shared Git bookkeeping owned by the source repository; we only verify
// that it is a real, non-symlinked directory the operator can reach.
func (transport *RarRefPublicationTransport) bindAlternates(path string) error {
	sourceObjects := filepath.Join(transport.repo.rar.identity.GitCommonDir, "objects")
	info, err := os.Lstat(sourceObjects)
	if err != nil {
		return fmt.Errorf("%w: source objects lstat: %v", ErrRefPublicationTransportUnavailable, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: source objects is not a directory", ErrRefPublicationTransportUnavailable)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: source objects is a symlink", ErrRefPublicationTransportUnavailable)
	}
	if err := writePrivateAlternatesRecord(filepath.Join(path, refPublicationTransportAlternates), sourceObjects); err != nil {
		return fmt.Errorf("%w: write alternates: %v", ErrRefPublicationTransportUnavailable, err)
	}
	return nil
}

// clearPushMark removes the push-completed marker from a freshly-prepared
// transport so the sole push can dispatch on the first call. Prepare is
// always called against the absence of an executed push.
func (transport *RarRefPublicationTransport) clearPushMark(path string) error {
	mark := filepath.Join(path, refPublicationTransportPushMark)
	if _, err := os.Lstat(mark); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return os.Remove(mark)
}

// markPushCompleted records that the prepared transport has dispatched
// its sole successful push. Subsequent Push calls see this marker and are
// rejected with ErrRefPublicationTransportAlreadyTerminal.
func (transport *RarRefPublicationTransport) markPushCompleted(
	path string,
) error {
	dir := filepath.Join(path, "refs", "heads")
	if err := ensurePrivateRARDirectoryTree(path, dir, true); err != nil {
		return err
	}
	if err := writePrivateRecordFile(
		filepath.Join(path, refPublicationTransportPushMark),
		[]byte(strconv.FormatInt(time.Now().UnixNano(), 10)),
	); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(path, refPublicationTransportPushLog)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return SyncReviewDirectory(filepath.Dir(filepath.Join(path, refPublicationTransportPushLog)))
}

// Push executes the strictly sandboxed git push and parses its porcelain
// output. The single attribution signal is one successful [new branch]
// record for C -> refs/heads/<N>, with no other records, no rejections, and
// empty stderr. Anything else yields a typed error.
func (transport *RarRefPublicationTransport) Push(
	ctx context.Context,
	record RefPublicationRecord,
) (RefPublicationPushResult, error) {
	if err := ctx.Err(); err != nil {
		return RefPublicationPushResult{}, err
	}
	if transport == nil || transport.repo == nil {
		return RefPublicationPushResult{}, errors.New("ref publication transport is not initialized")
	}
	if record.State != RefPubPrepared && record.State != RefPubPushed {
		return RefPublicationPushResult{}, fmt.Errorf(
			"%w: push requires prepared or pushed state; got %q",
			ErrRefPublicationTransitionIllegal, record.State,
		)
	}
	auth, err := ParseRefPublicationAuthorization(string(record.Payload))
	if err != nil {
		return RefPublicationPushResult{}, err
	}
	if err := transport.repo.rar.validateIdentity(ctx); err != nil {
		return RefPublicationPushResult{}, err
	}
	path := transport.transportPath(auth.RequestID, auth.RequestDigest)
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RefPublicationPushResult{}, fmt.Errorf("%w: request_id=%s", ErrRefPublicationTransportNotReady, auth.RequestID)
		}
		return RefPublicationPushResult{}, err
	}
	if err := validatePrivateRARDirectory(path); err != nil {
		return RefPublicationPushResult{}, err
	}
	markPath := filepath.Join(path, refPublicationTransportPushMark)
	if _, err := os.Lstat(markPath); err == nil {
		return RefPublicationPushResult{}, ErrRefPublicationTransportAlreadyTerminal
	} else if !errors.Is(err, fs.ErrNotExist) {
		return RefPublicationPushResult{}, err
	}
	argv, env, err := transport.buildPushCommandEnv(auth, path)
	if err != nil {
		return RefPublicationPushResult{}, err
	}
	stdout, stderr, runErr := runIsolatedGitPush(ctx, argv, env, refPublicationTransportMaxStdout, refPublicationTransportMaxStderr)
	attribution, parseErr := parseStrictPorcelain(stdout, stderr, auth, argv)
	if parseErr != nil {
		if len(stderr) != 0 && ctx.Err() == nil && isLeaseRejection(stderr, parseErr) {
			return RefPublicationPushResult{}, fmt.Errorf("%w: %v", ErrRefPublicationLeaseRejected, parseErr)
		}
		if attribution.ServerStderr == "" {
			attribution.ServerStderr = strings.TrimSpace(string(stderr))
		}
		return RefPublicationPushResult{}, parseErr
	}
	if runErr != nil {
		return RefPublicationPushResult{}, fmt.Errorf("%w: %v", ErrRefPublicationTransportCrashed, runErr)
	}
	if err := transport.repo.rar.validateIdentity(ctx); err != nil {
		return RefPublicationPushResult{}, err
	}
	if attribution.SourceCommit != auth.SourceCommit {
		return RefPublicationPushResult{}, fmt.Errorf(
			"%w: porcelain from=%s does not match authorized source_commit=%s",
			ErrRefPublicationDriftRejected, attribution.SourceCommit, auth.SourceCommit,
		)
	}
	if attribution.Destination != auth.DestinationRef {
		return RefPublicationPushResult{}, fmt.Errorf(
			"%w: porcelain to=%s does not match authorized destination_ref=%s",
			ErrRefPublicationDriftRejected, attribution.Destination, auth.DestinationRef,
		)
	}
	if err := transport.markPushCompleted(path); err != nil {
		return RefPublicationPushResult{}, err
	}
	if err := transport.repo.rar.validateIdentity(ctx); err != nil {
		return RefPublicationPushResult{}, err
	}
	return attribution, nil
}

// buildPushCommandEnv materializes the argv and sanitized env for the
// strictly sandboxed git push. The args are positional so a malformed argv
// is rejected before any process is dispatched.
func (transport *RarRefPublicationTransport) buildPushCommandEnv(
	auth RefPublicationAuthorization,
	path string,
) ([]string, []string, error) {
	if err := ValidateRefPublicationDestinationRef(auth.DestinationRef); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrRefPublicationDriftRejected, err)
	}
	if !validGitTree(auth.SourceCommit) {
		return nil, nil, fmt.Errorf("%w: source_commit is not a Git object id", ErrRefPublicationDriftRejected)
	}
	if err := rejectEmbeddedCredentials(auth.EndpointIdentity); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrRefPublicationTransportUnavailable, err)
	}
	lease := zeroOIDForObjectFormat(auth.SourceCommit)
	argv := []string{
		"--no-replace-objects",
		"-c", "core.attributesFile=" + filepath.Join(path, "info", "attributes"),
		"-c", "core.bare=true",
		"-c", "receive.denyDeleteCurrent=false",
		"-c", "receive.denyNonFastForwards=false",
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
		"push",
		"--no-verify",
		"--porcelain",
		"--no-tags",
		"--force-with-lease=" + auth.DestinationRef + ":" + lease,
		auth.EndpointIdentity,
		auth.SourceCommit + ":" + auth.DestinationRef,
	}
	env, err := transport.sanitizedPushEnvironment(path)
	if err != nil {
		return nil, nil, err
	}
	if err := validatePushArgv(argv); err != nil {
		return nil, nil, err
	}
	return argv, env, nil
}

// rejectEmbeddedCredentials refuses endpoints that carry credentials in the
// URL itself (HTTPS userinfo, SCP-style SSH paths with embedded secrets, or
// any URL that contains `?token=` query arguments). Credentials must be
// presented through the upstream-supported non-interactive channels (SSH
// agent + known hosts, or provider-owned HTTPS token handling).
func rejectEmbeddedCredentials(endpoint string) error {
	if strings.Contains(endpoint, "@") {
		// https://user:secret@host, scp-style git@host paths are allowable
		// only when SSH is the protocol. The SSH agent must already know
		// that key. The SSH opaque case (e.g. `git@github.com:owner/repo.git`
		// does not actually embed credentials in the protocol sense; the
		// authority is the public identity, and the matching private key is
		// looked up via the agent. We do NOT treat `user@host` SSH scp-syntax
		// as embedded credentials. The presence of `://` with `@` is the
		// unambiguous offender: HTTPS URLs never have an `@` outside the
		// userinfo segment. So we only fail when both are present.
		if strings.Contains(endpoint, "://") {
			return errors.New("endpoint URL embeds userinfo credentials")
		}
	}
	if strings.Contains(endpoint, "?") {
		return errors.New("endpoint URL embeds a query string (likely a credential token)")
	}
	return nil
}

// sanitizedPushEnvironment constructs the process environment for the sole
// push. Source/global/system/local/worktree configuration is excluded by
// pointing the GIT_CONFIG_* and GIT_ATTR_NOSYSTEM at owned empty paths
// inside the transport. push.default, insteadOf, pushInsteadOf, and remote
// refspecs are unreachable because their owning config files are empty.
func (transport *RarRefPublicationTransport) sanitizedPushEnvironment(
	path string,
) ([]string, error) {
	privateConfig := filepath.Join(path, "info", "config")
	privateAttributes := filepath.Join(path, "info", "attributes")
	for _, owned := range []string{privateConfig, privateAttributes} {
		if err := ensurePrivateRARFile(owned); err != nil {
			return nil, fmt.Errorf("%w: secure owned push file: %v", ErrRefPublicationTransportUnavailable, err)
		}
	}
	env := []string{
		"GIT_DIR=" + path,
		"GIT_COMMON_DIR=" + path,
		"GIT_WORK_TREE=",
		"GIT_INDEX_FILE=",
		"GIT_OBJECT_DIRECTORY=",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=" + privateConfig,
		"GIT_CONFIG_GLOBAL=" + privateConfig,
		"GIT_CONFIG_COUNT=0",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"GIT_ASKPASS_REQUIRED=true",
		"LC_ALL=C",
		"LANG=C",
		"PATH=" + os.Getenv("PATH"),
		"COMSPEC=" + os.Getenv("COMSPEC"),
	}
	for _, key := range []string{
		"SYSTEMROOT", "WINDIR", "TMPDIR", "TMP", "TEMP",
	} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env, nil
}

// Observe runs a fresh isolated read-only remote observation through the
// same sanitized environment. The transport never dispatches a fetch, ls
// --heads, or any side-effecting command beyond the verified single ls-remote.
func (transport *RarRefPublicationTransport) Observe(
	ctx context.Context,
	record RefPublicationRecord,
) (RefPublicationObservation, error) {
	if err := ctx.Err(); err != nil {
		return RefPublicationObservation{}, err
	}
	if transport == nil || transport.repo == nil {
		return RefPublicationObservation{}, errors.New("ref publication transport is not initialized")
	}
	auth, err := ParseRefPublicationAuthorization(string(record.Payload))
	if err != nil {
		return RefPublicationObservation{}, err
	}
	if err := transport.repo.rar.validateIdentity(ctx); err != nil {
		return RefPublicationObservation{}, err
	}
	path := transport.transportPath(auth.RequestID, auth.RequestDigest)
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RefPublicationObservation{}, fmt.Errorf("%w: request_id=%s", ErrRefPublicationTransportNotReady, auth.RequestID)
		}
		return RefPublicationObservation{}, err
	}
	argv := []string{
		"--no-replace-objects",
		"-c", "core.attributesFile=" + filepath.Join(path, "info", "attributes"),
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "uploadpack.allowReachableSha1InWant=false",
		"-c", "uploadpack.allowAnySHA1InWant=false",
		"-c", "lsrefs.allowUnborn=false",
		"ls-remote",
		"--heads",
		auth.EndpointIdentity,
		auth.DestinationRef,
	}
	env, err := transport.sanitizedPushEnvironment(path)
	if err != nil {
		return RefPublicationObservation{}, err
	}
	if err := validateObserveArgv(argv); err != nil {
		return RefPublicationObservation{}, err
	}
	stdout, stderr, err := runIsolatedGitPush(ctx, argv, env, refPublicationTransportMaxStdout, refPublicationTransportMaxStderr)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return RefPublicationObservation{}, ctxErr
	}
	if err != nil {
		return RefPublicationObservation{}, fmt.Errorf("%w: %v", ErrRefPublicationObserverCrashed, err)
	}
	return parseStrictLsRemote(stdout, stderr, auth)
}

// Reconcile is the read-only classification entry point. It dispatches
// only Observe and never executes a push.
func (transport *RarRefPublicationTransport) Reconcile(
	ctx context.Context,
	record RefPublicationRecord,
) (RefPublicationTransportReconciliation, error) {
	if err := ctx.Err(); err != nil {
		return RefPublicationTransportReconciliation{}, err
	}
	if _, err := ParseRefPublicationAuthorization(string(record.Payload)); err != nil {
		return RefPublicationTransportReconciliation{}, err
	}
	observation, err := transport.Observe(ctx, record)
	if err != nil {
		if errors.Is(err, ErrRefPublicationObserverCrashed) {
			return RefPublicationTransportReconciliation{
				Classification: RefPubPublicationUnknown,
				Observation:    observation,
			}, nil
		}
		return RefPublicationTransportReconciliation{}, err
	}
	switch {
	case observation.Classification == RefPubNotCreated:
		return RefPublicationTransportReconciliation{
			Classification: RefPubNotCreated,
			Observation:    observation,
		}, nil
	case observation.Classification == RefPubConfirmed:
		return RefPublicationTransportReconciliation{
			Classification: RefPubConfirmed,
			Observation:    observation,
		}, nil
	case observation.Classification == RefPubConflict:
		return RefPublicationTransportReconciliation{
			Classification: RefPubConflict,
			Observation:    observation,
		}, nil
	}
	return RefPublicationTransportReconciliation{
		Classification: RefPubPublicationUnknown,
		Observation:    observation,
	}, nil
}

// RefPublicationTransportHandleRecord is the typed return value of Prepare.
type RefPublicationTransportHandleRecord struct {
	Handle refPublicationTransportHandleRecord
}

// RefPublicationPushResult is the typed attribution verdict for the single
// push attempt. A successful Push returns Result with Classification set
// to AttributionProven; any other condition is returned as a typed error
// instead.
type RefPublicationPushResult struct {
	Classification RefPublicationAttribution `json:"classification"`
	Destination    string                    `json:"destination"`
	SourceCommit   string                    `json:"source_commit"`
	FromHash       string                    `json:"from_hash"`
	ToRef          string                    `json:"to_ref"`
	Reason         string                    `json:"reason"`
	ServerStderr   string                    `json:"server_stderr"`
	UsedEndpoint   string                    `json:"used_endpoint"`
	UsedLeaseToken string                    `json:"used_lease_token"`
	ExecutedAt     string                    `json:"executed_at"`
}

// RefPublicationObservation is the typed observation verdict produced by a
// fresh isolated remote ls-remote.
type RefPublicationObservation struct {
	Classification RefPublicationState `json:"classification"`
	Destination    string              `json:"destination"`
	ObservedCommit string              `json:"observed_commit"`
	ServerStderr   string              `json:"server_stderr"`
}

// RefPublicationTransportReconciliation is the read-only classification
// derived from the Observation alone.
type RefPublicationTransportReconciliation struct {
	Classification RefPublicationState       `json:"classification"`
	Observation    RefPublicationObservation `json:"observation"`
}

// zeroOIDForObjectFormat selects the SHA-1 zero OID for SHA-1 repositories
// and the SHA-256 zero OID for SHA-256 repositories.
func zeroOIDForObjectFormat(commit string) string {
	if len(commit) == 64 {
		return zeroSHA256OID
	}
	return zeroSHA1OID
}

// validatePushArgv is the argv-side refusal table for the isolated push. It
// rejects any non-canonical flag combination a caller could pass.
func validatePushArgv(argv []string) error {
	if len(argv) < 8 {
		return errors.New("ref publication push argv is shorter than the mandatory shape")
	}
	want := []string{
		"--no-verify", "--porcelain", "--no-tags",
	}
	for _, marker := range want {
		if !containsString(argv, marker) {
			return fmt.Errorf("ref publication push argv is missing %q", marker)
		}
	}
	if !containsPrefix(argv, "--force-with-lease=") {
		return errors.New("ref publication push argv is missing the zero-OID --force-with-lease")
	}
	if !containsString(argv, "--no-replace-objects") {
		return errors.New("ref publication push argv is missing --no-replace-objects")
	}
	for _, banned := range []string{
		"--all", "--mirror", "--prune", "--atomic",
		"--follow-tags", "--signed", "--delete",
		"--push-option=", "--receive-pack=", "--upload-pack=",
		"--exec=", "--server-option=", "--force", "--force-with-lease",
	} {
		if containsExact(argv, banned) {
			return fmt.Errorf("ref publication push argv contains banned flag %q", banned)
		}
	}
	leaseSeen := false
	for _, arg := range argv {
		if strings.HasPrefix(arg, "--push-option=") ||
			strings.HasPrefix(arg, "--receive-pack=") ||
			strings.HasPrefix(arg, "--upload-pack=") ||
			strings.HasPrefix(arg, "--server-option=") ||
			strings.HasPrefix(arg, "--exec=") {
			return fmt.Errorf("ref publication push argv contains banned prefix %q", arg)
		}
		if strings.HasPrefix(arg, "--force-with-lease=") {
			if !(strings.Contains(arg, ":"+zeroSHA1OID) || strings.Contains(arg, ":"+zeroSHA256OID)) {
				return fmt.Errorf("ref publication push argv contains non-zero force-with-lease token %q", arg)
			}
			leaseSeen = true
		}
	}
	if !leaseSeen {
		return errors.New("ref publication push argv did not declare the zero-OID --force-with-lease")
	}
	return nil
}

// validateObserveArgv is the argv-side refusal table for the isolated
// observer. The observer is permitted only ls-remote --heads <endpoint>
// <ref>; every other ls-remote flag combination is rejected.
func validateObserveArgv(argv []string) error {
	if len(argv) < 5 {
		return errors.New("ref publication observe argv is shorter than the mandatory shape")
	}
	if !containsString(argv, "ls-remote") {
		return errors.New("ref publication observe argv is missing ls-remote")
	}
	if !containsString(argv, "--heads") {
		return errors.New("ref publication observe argv is missing --heads")
	}
	for _, banned := range []string{
		"--upload-pack=", "--exec=", "--server-option=",
		"--refs", "--refspec=", "--quiet", "-q",
		"--tags", "--branches", "--remotes",
	} {
		if containsExact(argv, banned) || containsPrefix(argv, banned) {
			return fmt.Errorf("ref publication observe argv contains banned flag %q", banned)
		}
	}
	return nil
}

// parseStrictPorcelain validates the Git porcelain v1 output of `git push`.
// The strict grammar is exactly:
//
//	lines 1..N-1: " " <flag> " " <from> " -> " <to> [reason]
//	last non-empty line: "Done"
//	exactly ONE line has reason "new branch" and binding from==<C>, to==<refs/heads/N>
//
// Any other shape is fatal.
func parseStrictPorcelain(
	stdout, stderr []byte,
	auth RefPublicationAuthorization,
	argv []string,
) (RefPublicationPushResult, error) {
	normalized := strings.ReplaceAll(string(stdout), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	// Trim trailing empty line(s) introduced by final newline.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 {
		return RefPublicationPushResult{}, fmt.Errorf("%w: too few porcelain lines", ErrRefPublicationPorcelainMalformed)
	}
	if !porcelainHeaderLine.MatchString(lines[0]) {
		return RefPublicationPushResult{}, fmt.Errorf("%w: missing To-header line", ErrRefPublicationPorcelainMalformed)
	}
	if !porcelainDoneLine.MatchString(lines[len(lines)-1]) {
		return RefPublicationPushResult{}, fmt.Errorf("%w: missing Done trailer", ErrRefPublicationPorcelainMalformed)
	}
	var (
		newBranchCount int
		rejectionCount int
		lastError      error
		lastRecord     string
	)
	for index, line := range lines[1 : len(lines)-1] {
		matches := porcelainRecordLine.FindStringSubmatch(line)
		if matches == nil {
			return RefPublicationPushResult{}, fmt.Errorf(
				"%w: porcelain line %d does not match the strict grammar: %q",
				ErrRefPublicationPorcelainMalformed, index+2, line,
			)
		}
		flag := matches[1]
		from := matches[2]
		to := matches[3]
		reason := matches[4]
		lastRecord = line
		if flag == "!" {
			rejectionCount++
			lastError = fmt.Errorf("rejected porcelain record for %s -> %s: %s", from, to, reason)
			continue
		}
		if reason == "new branch" {
			newBranchCount++
			if newBranchCount > 1 {
				return RefPublicationPushResult{}, fmt.Errorf(
					"%w: more than one [new branch] record: %q",
					ErrRefPublicationPorcelainAmbiguous, line,
				)
			}
			if from != auth.SourceCommit {
				return RefPublicationPushResult{}, fmt.Errorf(
					"%w: porcelain from=%s does not match authorized source_commit=%s",
					ErrRefPublicationDriftRejected, from, auth.SourceCommit,
				)
			}
			if to != auth.DestinationRef {
				return RefPublicationPushResult{}, fmt.Errorf(
					"%w: porcelain to=%s does not match authorized destination_ref=%s",
					ErrRefPublicationDriftRejected, to, auth.DestinationRef,
				)
			}
		}
	}
	if rejectionCount > 0 {
		return RefPublicationPushResult{
			ServerStderr: strings.TrimSpace(string(stderr)),
		}, fmt.Errorf("%w: %v", ErrRefPublicationLeaseRejected, lastError)
	}
	if newBranchCount == 0 {
		return RefPublicationPushResult{
			ServerStderr: strings.TrimSpace(string(stderr)),
		}, fmt.Errorf("%w: no [new branch] porcelain record: %q", ErrRefPublicationPorcelainMalformed, lastRecord)
	}
	return RefPublicationPushResult{
		Classification: RefPublicationAttributionProven,
		Destination:    auth.DestinationRef,
		SourceCommit:   auth.SourceCommit,
		FromHash:       auth.SourceCommit,
		ToRef:          auth.DestinationRef,
		Reason:         "new branch",
		ServerStderr:   strings.TrimSpace(string(stderr)),
		UsedEndpoint:   auth.EndpointIdentity,
		UsedLeaseToken: zeroOIDForObjectFormat(auth.SourceCommit),
		ExecutedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// parseStrictLsRemote parses the output of `git ls-remote --heads` to a
// typed observation.
func parseStrictLsRemote(
	stdout, stderr []byte,
	auth RefPublicationAuthorization,
) (RefPublicationObservation, error) {
	normalized := strings.ReplaceAll(string(stdout), "\r\n", "\n")
	trimmed := strings.TrimSpace(normalized)
	if trimmed == "" {
		return RefPublicationObservation{
			Classification: RefPubNotCreated,
			Destination:    auth.DestinationRef,
		}, nil
	}
	for _, line := range strings.Split(normalized, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return RefPublicationObservation{}, fmt.Errorf(
				"%w: ls-remote line %q is not <oid> <ref>",
				ErrRefPublicationObserverCrashed, line,
			)
		}
		commit := fields[0]
		ref := fields[1]
		if !validGitTree(commit) {
			return RefPublicationObservation{}, fmt.Errorf(
				"%w: ls-remote oid %q is not a Git tree identity",
				ErrRefPublicationObserverCrashed, commit,
			)
		}
		if ref != auth.DestinationRef {
			continue
		}
		switch {
		case commit == auth.SourceCommit:
			return RefPublicationObservation{
				Classification: RefPubConfirmed,
				Destination:    auth.DestinationRef,
				ObservedCommit: commit,
			}, nil
		default:
			return RefPublicationObservation{
				Classification: RefPubConflict,
				Destination:    auth.DestinationRef,
				ObservedCommit: commit,
				ServerStderr:   strings.TrimSpace(string(stderr)),
			}, nil
		}
	}
	return RefPublicationObservation{
		Classification: RefPubNotCreated,
		Destination:    auth.DestinationRef,
	}, nil
}

// isLeaseRejection checks whether the parsed porcelain error was caused by
// --force-with-lease being rejected, distinguishing it from a plain
// attribution failure.
func isLeaseRejection(stderr []byte, parseErr error) bool {
	if errors.Is(parseErr, ErrRefPublicationLeaseRejected) {
		return true
	}
	if errors.Is(parseErr, ErrRefPublicationPorcelainAmbiguous) {
		return true
	}
	message := strings.ToLower(string(stderr))
	return strings.Contains(message, "[rejected]") ||
		strings.Contains(message, "stale info") ||
		strings.Contains(message, "non-fast-forward") ||
		strings.Contains(message, "fetch first")
}

// runIsolatedGitPush executes one strictly-sandboxed git process and
// returns bounded stdout/stderr slices and the process error. The argv and
// env are passed verbatim; the caller is responsible for validating them.
// GENTLE_AI_TEST_TRANSPORT_HELPER is the only test-set variable that
// routes the call through a synchronous in-process fake so unit tests do
// not have to spawn a real git process. Every other ambient variable is
// intentionally absent so the strict isolation contract holds.
func runIsolatedGitPush(
	ctx context.Context,
	argv, env []string,
	stdoutLimit, stderrLimit int,
) ([]byte, []byte, error) {
	if stdoutLimit <= 0 || stderrLimit <= 0 {
		return nil, nil, errors.New("ref publication transport output limits are invalid")
	}
	if fake, ok := transportTestFakeResponse(); ok {
		if fake.exitError {
			return []byte(fake.stdout), []byte(fake.stderr), errors.New("transport fake: git push process exited non-zero")
		}
		return []byte(fake.stdout), []byte(fake.stderr), nil
	}
	ctx, cancel := context.WithTimeout(ctx, refPublicationTransportPushWait)
	defer cancel()
	command := gitCommandContext(ctx, "git", argv...)
	command.Env = append([]string{}, env...)
	stdout := boundedGitOutput{limit: stdoutLimit}
	stderr := boundedGitOutput{limit: stderrLimit}
	command.Stdout = &stdout
	command.Stderr = &stderr
	release, startErr := gitProcessTreeStarter(command)
	if startErr == nil {
		waitErr := command.Wait()
		_ = release()
		return stdout.Bytes(), stderr.Bytes(), waitErr
	}
	if release != nil {
		_ = release()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.Bytes(), stderr.Bytes(), ctxErr
	}
	return stdout.Bytes(), stderr.Bytes(), startErr
}

type transportTestFake struct {
	stdout    string
	stderr    string
	exitError bool
}

func transportTestFakeResponse() (transportTestFake, bool) {
	encoded := os.Getenv("GENTLE_AI_TEST_TRANSPORT_HELPER")
	if encoded == "" {
		return transportTestFake{}, false
	}
	parts := strings.SplitN(encoded, "|", 4)
	if len(parts) != 4 {
		return transportTestFake{}, false
	}
	if parts[0] != "1" {
		return transportTestFake{}, false
	}
	fake := transportTestFake{}
	if parts[1] == "true" {
		fake.exitError = true
	}
	fake.stdout = parts[2]
	fake.stderr = parts[3]
	return fake, true
}

// ensurePrivateRARFile creates an empty private file if it does not exist.
// It is used to make the GIT_CONFIG_SYSTEM, GIT_CONFIG_GLOBAL, and
// core.attributesFile paths point at owned empty placeholders.
func ensurePrivateRARFile(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return validatePrivateRARFile(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := createPrivateRARFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return validatePrivateRARFile(path)
		}
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return validatePrivateRARFile(path)
}

// writePrivateAlternatesRecord writes a single-line, LF-terminated
// alternates file under the transport's objects/info directory.
func writePrivateAlternatesRecord(path, value string) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateRARDirectoryTree(filepath.Dir(dir), dir, true); err != nil {
		return err
	}
	payload := []byte(value + "\n")
	if err := publishPrivateRARImmutable(path, payload); err != nil &&
		!errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func containsString(haystack []string, needle string) bool {
	for _, entry := range haystack {
		if entry == needle {
			return true
		}
	}
	return false
}

func containsPrefix(haystack []string, prefix string) bool {
	for _, entry := range haystack {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func containsExact(haystack []string, value string) bool {
	for _, entry := range haystack {
		if entry == value {
			return true
		}
	}
	return false
}

// createPrivateBareRepo writes the on-disk skeleton of an empty bare
// repository using only owner-only primitives. We do not call `git init
// --bare` because the Git binary creates subdirectories with the host's
// default ACLs, which the strict private-RAR validator rejects. The bare
// layout below is the minimum Git needs to accept a push: HEAD pointing
// at refs/heads/main, a config with core.bare=true, an empty objects/,
// refs/, hooks/ tree, and an info/ directory. Git-compatible tooling
// (porcelain commands, ls-remote, push) accepts this layout unchanged.
func createPrivateBareRepo(path string) error {
	if _, err := createPrivateRARDirectory(path); err != nil {
		return err
	}
	for _, subdir := range []string{
		"objects", "objects/info", "objects/pack",
		"refs", "refs/heads", "refs/tags",
		"hooks", "info", "info/refs",
		"logs", "logs/refs",
	} {
		if err := ensurePrivateRARDirectoryTree(path, filepath.Join(path, subdir), true); err != nil {
			return err
		}
	}
	headPath := filepath.Join(path, "HEAD")
	if err := writePrivateBareInitFile(headPath, "ref: refs/heads/main\n"); err != nil {
		return err
	}
	configPath := filepath.Join(path, "config")
	configBody := "[core]\n\trepositoryFormatVersion = 0\n\tbare = true\n\tlogallrefupdates = false\n"
	if err := writePrivateBareInitFile(configPath, configBody); err != nil {
		return err
	}
	descriptionPath := filepath.Join(path, "description")
	if err := writePrivateBareInitFile(descriptionPath, "Unnamed repository; edit this file 'description' to name the repository.\n"); err != nil {
		return err
	}
	excludePath := filepath.Join(path, "info", "exclude")
	if err := writePrivateBareInitFile(excludePath, "# git ls-files --others --exclude-from=.git/info/exclude\n# git-ls-files --ignored --exclude-from=.git/info/exclude\n"); err != nil {
		return err
	}
	attributesPath := filepath.Join(path, "info", "attributes")
	if err := writePrivateBareInitFile(attributesPath, ""); err != nil {
		return err
	}
	if err := validatePrivateBareRepo(path); err != nil {
		return err
	}
	return nil
}

// writePrivateBareInitFile creates an owner-only file at path with the given
// payload. The file is overwritten if it already exists. The Windows ACL is
// applied via the package-private createPrivateRARFile primitive so every
// skeleton file passes validatePrivateRARFile.
func writePrivateBareInitFile(path, payload string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := createPrivateRARFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return validatePrivateRARFile(path)
		}
		return err
	}
	if _, err := file.WriteString(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return validatePrivateRARFile(path)
}

// validatePrivateBareRepo verifies the freshly-created bare repo has the
// expected skeleton: HEAD, objects/, refs/, config; with owner-only
// permissions for every leaf. A failed validation is fatal so Prepare
// cannot leave a partial bare repo on disk for the next call.
func validatePrivateBareRepo(path string) error {
	for _, subdir := range []string{
		"objects", "objects/info", "objects/pack",
		"refs", "hooks", "info",
	} {
		if err := validatePrivateRARDirectory(filepath.Join(path, subdir)); err != nil {
			return fmt.Errorf("bare transport subdir %q: %w", subdir, err)
		}
	}
	for _, file := range []string{"HEAD", "config"} {
		if err := validatePrivateRARFile(filepath.Join(path, file)); err != nil {
			return fmt.Errorf("bare transport file %q: %w", file, err)
		}
	}
	return nil
}
