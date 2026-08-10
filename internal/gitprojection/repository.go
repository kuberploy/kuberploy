package gitprojection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/variables"
)

const (
	DefaultGitAskPass  = "/usr/local/bin/kuberploy-git-askpass"
	defaultGitTimeout  = 30 * time.Second
	defaultMaxGitBytes = int64(512 << 20)
	defaultMaxGitFiles = 100_000
)

const trustedBareConfig = `[core]
	repositoryformatversion = 0
	filemode = true
	bare = true
	symlinks = false
	ignorecase = false
[http]
	followRedirects = false
`

// MirrorManager owns bounded, disposable bare mirrors and detached worktrees.
// Every filesystem name is derived from immutable UUIDs. LocalRemote exists
// exclusively for hermetic tests; production remote URLs are reconstructed
// from provider-verified RepositoryIdentity.
type MirrorManager struct {
	Root               string
	Timeout            time.Duration
	MaxBytes           int64
	MaxFiles           int
	UseCredentials     bool
	CredentialProvider GitCredentialProvider
	AllowLocalTests    bool
	LocalRemote        string
	credential         *GitCredential
}

type PreparedRepository struct {
	Binding      Binding
	Head         VerifiedHead
	MirrorPath   string
	WorktreePath string
	remote       string
	manager      *MirrorManager
}

// CleanupOperation removes only the deterministic disposable worktree owned by
// one UUID operation. It is the startup/retry repair for a process that died
// before PreparedRepository.Close; mirrors and unrelated worktrees are never
// selected by this method.
func (m *MirrorManager) CleanupOperation(ctx context.Context, bindingID, operationID string) error {
	if m == nil || !uuidRE.MatchString(bindingID) || !uuidRE.MatchString(operationID) || m.validate() != nil {
		return ErrInvalid
	}
	if err := ensureRealRoot(m.Root); err != nil {
		return err
	}
	if err := ensureRealSubdirectories(m.Root, "mirrors", "worktrees"); err != nil {
		return err
	}
	mirror := filepath.Join(m.Root, "mirrors", bindingID+".git")
	worktree := filepath.Join(m.Root, "worktrees", operationID)
	info, err := os.Lstat(worktree)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Git operation worktree is not a real directory")
	}
	if mirrorInfo, mirrorErr := os.Lstat(mirror); mirrorErr != nil || mirrorInfo.Mode()&os.ModeSymlink != 0 || !mirrorInfo.IsDir() {
		return errors.New("Git operation mirror is not a real directory")
	}
	if _, err = m.git(ctx, mirror, "worktree", "remove", "--force", worktree); err != nil {
		return fmt.Errorf("repair Git operation worktree: %w", err)
	}
	return nil
}

func (m *MirrorManager) Prepare(ctx context.Context, binding Binding, head VerifiedHead, operationID string) (*PreparedRepository, error) {
	if err := binding.Validate(); err != nil || head.ValidateFor(binding) != nil || !uuidRE.MatchString(operationID) {
		return nil, ErrInvalid
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	managerCopy := *m
	manager := &managerCopy
	remote, err := binding.Repository.CanonicalRemote()
	if err != nil {
		return nil, err
	}
	if m.AllowLocalTests {
		if m.LocalRemote == "" || !filepath.IsAbs(m.LocalRemote) || filepath.Clean(m.LocalRemote) != m.LocalRemote || m.LocalRemote == string(os.PathSeparator) {
			return nil, ErrInvalid
		}
		remote = m.LocalRemote
	}
	if err = ensureRealRoot(manager.Root); err != nil {
		return nil, err
	}
	mirrorsRoot := filepath.Join(manager.Root, "mirrors")
	worktreesRoot := filepath.Join(manager.Root, "worktrees")
	if err = ensureRealSubdirectories(manager.Root, "mirrors", "worktrees"); err != nil {
		return nil, err
	}
	mirror := filepath.Join(mirrorsRoot, binding.ID+".git")
	if err = manager.ensureMirror(ctx, mirror); err != nil {
		return nil, err
	}
	if err = manager.withCredential(ctx, binding, func() error {
		actual, remoteErr := manager.remoteHead(ctx, mirror, remote, binding.TargetRef)
		if remoteErr != nil {
			return remoteErr
		}
		if remoteErr = ValidateCandidateHead(binding, head.Commit, actual); remoteErr != nil {
			return remoteErr
		}
		// The disposable local tracking ref may move backwards so a force-pushed
		// authoritative ref can be detected and repaired. This plus refspec never
		// writes remotely; outbound pushes remain normal fast-forwards only.
		_, remoteErr = manager.gitNetwork(ctx, mirror, "fetch", "--no-tags", "--no-write-fetch-head", remote, "+"+binding.TargetRef+":refs/kuberploy/target")
		return remoteErr
	}); err != nil {
		return nil, err
	}
	fetched, err := manager.git(ctx, mirror, "rev-parse", "refs/kuberploy/target^{commit}")
	if err != nil || strings.TrimSpace(fetched) != head.Commit {
		return nil, fmt.Errorf("%w: fetched object is not the provider-verified target head", ErrConflict)
	}
	if err = manager.checkBudget(mirror); err != nil {
		return nil, err
	}
	if err = manager.checkTreeBudget(ctx, mirror, head.Commit); err != nil {
		return nil, err
	}
	worktree := filepath.Join(worktreesRoot, operationID)
	if _, err = os.Lstat(worktree); err == nil {
		return nil, errors.New("Git worktree already exists and requires repair")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err = manager.git(ctx, mirror, "worktree", "add", "--detach", worktree, head.Commit); err != nil {
		return nil, err
	}
	prepared := &PreparedRepository{Binding: binding, Head: head, MirrorPath: mirror, WorktreePath: worktree, remote: remote, manager: manager}
	if err = prepared.manager.checkWorktree(worktree); err != nil {
		_ = prepared.Close(context.Background())
		return nil, err
	}
	return prepared, nil
}

func (m *MirrorManager) checkTreeBudget(ctx context.Context, mirror, revision string) error {
	output, err := m.git(ctx, mirror, "ls-tree", "-l", "-r", "-z", "--full-tree", revision)
	if err != nil {
		return err
	}
	maxBytes := m.MaxBytes
	if maxBytes == 0 {
		maxBytes = defaultMaxGitBytes
	}
	maxFiles := m.MaxFiles
	if maxFiles == 0 {
		maxFiles = defaultMaxGitFiles
	}
	var expandedBytes int64
	files := 0
	for _, entry := range strings.Split(output, "\x00") {
		if entry == "" {
			continue
		}
		metadata, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 4 || fields[1] != "blob" || !blobRE.MatchString(fields[2]) || !validRelativePath(filepath.ToSlash(name)) {
			return errors.New("Git tree contains a submodule, special object, or unsafe path")
		}
		size, parseErr := strconv.ParseInt(fields[3], 10, 64)
		if parseErr != nil || size < 0 {
			return errors.New("Git tree contains an invalid blob size")
		}
		files++
		expandedBytes += size
		if files > maxFiles || size > maxBytes || expandedBytes > maxBytes {
			return errors.New("expanded Git worktree exceeds configured disk budget")
		}
	}
	return nil
}

func (m *MirrorManager) validate() error {
	if m.Root == "" || !filepath.IsAbs(m.Root) || filepath.Clean(m.Root) != m.Root || m.Root == string(os.PathSeparator) {
		return errors.New("Git cache root must be a clean non-root absolute path")
	}
	if m.Timeout < 0 || m.MaxBytes < 0 || m.MaxFiles < 0 || m.UseCredentials && m.AllowLocalTests ||
		m.CredentialProvider != nil && (m.UseCredentials || m.AllowLocalTests || m.credential != nil) || m.credential != nil && m.AllowLocalTests {
		return ErrInvalid
	}
	if m.UseCredentials || m.CredentialProvider != nil {
		info, err := os.Stat(DefaultGitAskPass)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return errors.New("Git askpass executable is unavailable")
		}
	}
	return nil
}

// Validate reports whether the hardened Git transport is completely and
// safely configured without performing provider, filesystem, or Git I/O.
func (m *MirrorManager) Validate() error { return m.validate() }

func (m *MirrorManager) ensureMirror(ctx context.Context, mirror string) error {
	info, err := os.Lstat(mirror)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(mirror, 0o750); err != nil {
			return err
		}
		if _, err = m.git(ctx, mirror, "init", "--bare"); err != nil {
			return err
		}
	} else if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Git mirror path must be a real directory")
	}
	return replaceTrustedConfig(mirror)
}

func replaceTrustedConfig(mirror string) error {
	info, err := os.Lstat(mirror)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Git mirror path must be a real directory")
	}
	configPath := filepath.Join(mirror, "config")
	if info, inspectErr := os.Lstat(configPath); inspectErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return errors.New("Git mirror config must be a regular file")
	} else if inspectErr != nil && !errors.Is(inspectErr, os.ErrNotExist) {
		return inspectErr
	}
	temporary, err := os.CreateTemp(mirror, ".config-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name) //nolint:errcheck
	if err = temporary.Chmod(0o600); err == nil {
		_, err = io.WriteString(temporary, trustedBareConfig)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, configPath)
}

func (m *MirrorManager) remoteHead(ctx context.Context, mirror, remote, targetRef string) (string, error) {
	output, err := m.gitNetwork(ctx, mirror, "ls-remote", "--refs", remote, targetRef)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return "", ErrMissingRef
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) != 1 {
		return "", ErrMissingRef
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[1] != targetRef || !commitRE.MatchString(fields[0]) {
		return "", errors.New("Git provider returned an ambiguous target ref")
	}
	return fields[0], nil
}

// Commit builds a commit directly from an isolated temporary index, verifies
// the exact path blob, and performs a normal fast-forward push. It never uses a
// force refspec. The caller must hold either a durable PathReservation or the
// equivalent fenced protected-intent lane, and finalize it only after this
// method returns the exact remotely observed commit.
func (p *PreparedRepository) Commit(ctx context.Context, mutation Mutation) (string, error) {
	if p == nil {
		return "", ErrInvalid
	}
	return p.commitToRef(ctx, mutation, p.Binding.TargetRef, true)
}

// CommitCandidate creates the same exact operation commit as Commit but
// publishes it only to the deterministic protected-operation branch. It never
// advances Binding.TargetRef and never force-pushes. A retry first inspects an
// existing candidate ref so an ambiguous successful push is recovered without
// creating a second commit or branch.
func (p *PreparedRepository) CommitCandidate(ctx context.Context, mutation Mutation, candidateRef string) (string, error) {
	if p == nil || candidateRef != "refs/heads/kuberploy/operations/"+mutation.OperationID {
		return "", ErrInvalid
	}
	if revision, present, err := p.FindCandidateOperationCommit(ctx, mutation, candidateRef); err != nil || present {
		return revision, err
	}
	return p.commitToRef(ctx, mutation, candidateRef, false)
}

func (p *PreparedRepository) commitToRef(ctx context.Context, mutation Mutation, destinationRef string, advanceTarget bool) (string, error) {
	if p.manager == nil || mutation.Validate(p.Binding) != nil || (advanceTarget && mutation.BaseRevision != p.Head.Commit) ||
		(!advanceTarget && destinationRef != "refs/heads/kuberploy/operations/"+mutation.OperationID) ||
		(advanceTarget && destinationRef != p.Binding.TargetRef) {
		return "", ErrInvalid
	}
	pathExists, err := p.pathExists(ctx, mutation.BaseRevision, mutation.Path)
	if err != nil {
		return "", err
	}
	switch mutation.EffectivePrecondition() {
	case MutationCreateIfAbsent:
		if pathExists {
			return "", fmt.Errorf("%w: create-if-absent path already exists", ErrConflict)
		}
	case MutationMatchETag:
		if !pathExists {
			return "", fmt.Errorf("%w: update path is absent", ErrConflict)
		}
	default:
		return "", ErrInvalid
	}
	if advanceTarget {
		if err := p.manager.withCredential(ctx, p.Binding, func() error {
			actual, remoteErr := p.manager.remoteHead(ctx, p.MirrorPath, p.remote, p.Binding.TargetRef)
			if remoteErr != nil {
				return remoteErr
			}
			return ValidateCandidateHead(p.Binding, mutation.BaseRevision, actual)
		}); err != nil {
			return "", err
		}
	}
	index, err := os.CreateTemp(p.manager.Root, ".index-*")
	if err != nil {
		return "", err
	}
	indexFile := index.Name()
	if closeErr := index.Close(); closeErr != nil {
		return "", closeErr
	}
	// read-tree expects either a valid index or no file.
	if err = os.Remove(indexFile); err != nil {
		return "", err
	}
	defer os.Remove(indexFile) //nolint:errcheck
	env := []string{"GIT_INDEX_FILE=" + indexFile}
	if _, err = p.manager.gitEnv(ctx, p.MirrorPath, env, nil, "read-tree", mutation.BaseRevision); err != nil {
		return "", err
	}
	if mutation.EffectiveAction() == MutationDelete {
		deletion := "0 " + strings.Repeat("0", len(mutation.BaseRevision)) + "\t" + mutation.Path + "\n"
		if _, err = p.manager.gitEnv(ctx, p.MirrorPath, env, strings.NewReader(deletion), "update-index", "--index-info"); err != nil {
			return "", err
		}
	} else {
		blob, blobErr := p.manager.gitEnv(ctx, p.MirrorPath, env, bytes.NewReader(mutation.Content), "hash-object", "-w", "--stdin")
		if blobErr != nil {
			return "", blobErr
		}
		blob = strings.TrimSpace(blob)
		if !blobRE.MatchString(blob) {
			return "", errors.New("Git returned an invalid blob object id")
		}
		if _, err = p.manager.gitEnv(ctx, p.MirrorPath, env, nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+mutation.Path); err != nil {
			return "", err
		}
	}
	tree, err := p.manager.gitEnv(ctx, p.MirrorPath, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	message := mutation.Message + "\n\nKuberploy-Operation: " + mutation.OperationID
	if mutation.CommitTrailer != "" {
		message += "\n" + mutation.CommitTrailer
	}
	commit, err := p.manager.gitEnv(ctx, p.MirrorPath, nil, nil, "commit-tree", strings.TrimSpace(tree), "-p", mutation.BaseRevision, "-m", message)
	if err != nil {
		return "", err
	}
	commit = strings.TrimSpace(commit)
	if !commitRE.MatchString(commit) {
		return "", errors.New("Git returned an invalid commit object id")
	}
	if mutation.EffectiveAction() == MutationDelete {
		if present, inspectErr := p.pathExists(ctx, commit, mutation.Path); inspectErr != nil || present {
			return "", errors.New("committed Git deletion differs from accepted mutation")
		}
	} else {
		committed, inspectErr := p.manager.git(ctx, p.MirrorPath, "cat-file", "blob", commit+":"+mutation.Path)
		if inspectErr != nil || !bytes.Equal([]byte(committed), mutation.Content) {
			return "", errors.New("committed Git blob differs from accepted mutation")
		}
	}
	if _, err = p.manager.git(ctx, p.MirrorPath, "merge-base", "--is-ancestor", mutation.BaseRevision, commit); err != nil {
		return "", errors.New("candidate Git commit is not a fast-forward")
	}
	if err = p.manager.withCredential(ctx, p.Binding, func() error {
		var remoteErr error
		if advanceTarget {
			var current string
			current, remoteErr = p.manager.remoteHead(ctx, p.MirrorPath, p.remote, p.Binding.TargetRef)
			if remoteErr != nil {
				return remoteErr
			}
			if remoteErr = ValidateCandidateHead(p.Binding, mutation.BaseRevision, current); remoteErr != nil {
				return remoteErr
			}
		}
		if !advanceTarget {
			if existing, candidateErr := p.manager.remoteHead(ctx, p.MirrorPath, p.remote, destinationRef); candidateErr == nil {
				return fmt.Errorf("%w: candidate ref already exists at %s", ErrConflict, existing)
			} else if !errors.Is(candidateErr, ErrMissingRef) {
				return candidateErr
			}
		}
		if _, remoteErr = p.manager.gitNetwork(ctx, p.MirrorPath, "push", p.remote, commit+":"+destinationRef); remoteErr != nil {
			return fmt.Errorf("normal Git fast-forward push failed: %w", remoteErr)
		}
		remoteCommit, remoteErr := p.manager.remoteHead(ctx, p.MirrorPath, p.remote, destinationRef)
		if remoteErr != nil || remoteCommit != commit {
			return errors.New("pushed Git commit is not the exact provider ref head")
		}
		return nil
	}); err != nil {
		return "", err
	}
	return commit, nil
}

// FindCandidateOperationCommit inspects only the deterministic operation ref.
// It validates the exact direct parent, operation trailer, and accepted path
// postimage; a substituted or reused branch is a conflict, never a replay.
func (p *PreparedRepository) FindCandidateOperationCommit(ctx context.Context, mutation Mutation, candidateRef string) (string, bool, error) {
	if p == nil || p.manager == nil || mutation.Validate(p.Binding) != nil ||
		candidateRef != "refs/heads/kuberploy/operations/"+mutation.OperationID {
		return "", false, ErrInvalid
	}
	var revision string
	err := p.manager.withCredential(ctx, p.Binding, func() error {
		remote, remoteErr := p.manager.remoteHead(ctx, p.MirrorPath, p.remote, candidateRef)
		if errors.Is(remoteErr, ErrMissingRef) {
			return nil
		}
		if remoteErr != nil {
			return remoteErr
		}
		localRef := "refs/kuberploy/candidates/" + mutation.OperationID
		if _, remoteErr = p.manager.gitNetwork(ctx, p.MirrorPath, "fetch", "--no-tags", "--no-write-fetch-head", p.remote, "+"+candidateRef+":"+localRef); remoteErr != nil {
			return remoteErr
		}
		resolved, remoteErr := p.manager.git(ctx, p.MirrorPath, "rev-parse", localRef)
		resolved = strings.TrimSpace(resolved)
		if remoteErr != nil || resolved != remote {
			return ErrProviderMismatch
		}
		revision = resolved
		return nil
	})
	if err != nil || revision == "" {
		return "", false, err
	}
	if err = p.verifyOperationCommit(ctx, mutation, revision); err != nil {
		return "", false, fmt.Errorf("%w: deterministic candidate ref does not contain the accepted operation: %v", ErrConflict, err)
	}
	return revision, true, nil
}

func (p *PreparedRepository) verifyOperationCommit(ctx context.Context, mutation Mutation, revision string) error {
	if !commitRE.MatchString(revision) {
		return ErrInvalid
	}
	commitObject, err := p.manager.git(ctx, p.MirrorPath, "cat-file", "commit", revision)
	if err != nil {
		return err
	}
	_, message, ok := strings.Cut(commitObject, "\n\n")
	if !ok || !hasExactTrailer(message, "Kuberploy-Operation: "+mutation.OperationID) ||
		(mutation.CommitTrailer != "" && !hasExactTrailer(message, mutation.CommitTrailer)) {
		return ErrProviderMismatch
	}
	parents, err := p.manager.git(ctx, p.MirrorPath, "rev-list", "--parents", "-n", "1", revision)
	parentFields := strings.Fields(parents)
	if err != nil || len(parentFields) != 2 || parentFields[0] != revision || parentFields[1] != mutation.BaseRevision {
		return ErrProviderMismatch
	}
	if mutation.EffectiveAction() == MutationDelete {
		present, inspectErr := p.pathExists(ctx, revision, mutation.Path)
		if inspectErr != nil || present {
			return ErrProviderMismatch
		}
		return nil
	}
	committed, err := p.manager.git(ctx, p.MirrorPath, "cat-file", "blob", revision+":"+mutation.Path)
	if err != nil || !bytes.Equal([]byte(committed), mutation.Content) {
		return ErrProviderMismatch
	}
	return nil
}

// pathExists distinguishes an absent object from every other Git failure. It
// runs only against the already provider-verified base commit in the isolated
// bare mirror and never consults the mutable worktree filesystem.
func (p *PreparedRepository) pathExists(ctx context.Context, revision, documentPath string) (bool, error) {
	if p == nil || p.manager == nil || !commitRE.MatchString(revision) || !validRelativePath(documentPath) {
		return false, ErrInvalid
	}
	output, err := p.manager.git(ctx, p.MirrorPath, "ls-tree", "-z", "--full-tree", revision, "--", documentPath)
	if err != nil {
		return false, err
	}
	if output == "" {
		return false, nil
	}
	entries := strings.Split(strings.TrimSuffix(output, "\x00"), "\x00")
	if len(entries) != 1 {
		return false, errors.New("Git returned an ambiguous path entry")
	}
	metadata, foundPath, ok := strings.Cut(entries[0], "\t")
	fields := strings.Fields(metadata)
	if !ok || foundPath != documentPath || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" || !blobRE.MatchString(fields[2]) {
		return false, errors.New("Git returned an unsafe path entry")
	}
	return true, nil
}

// VerifyPathContentETag compares a quoted SHA-256 over the exact blob bytes at
// the provider-pinned prepared head. It is intentionally separate from
// Mutation: deployment AppConfig uses a bundle ETag that also covers parser,
// chart, policy, and dependency blob identities and is proven transactionally
// by the control-plane store.
func (p *PreparedRepository) VerifyPathContentETag(ctx context.Context, documentPath, expectedETag string) error {
	if ctx == nil || p == nil || p.manager == nil || p.Binding.Validate() != nil || p.Head.ValidateFor(p.Binding) != nil ||
		!validProtectedDocumentPath(p.Binding, documentPath) || !validStrongETag(expectedETag) {
		return ErrInvalid
	}
	present, err := p.pathExists(ctx, p.Head.Commit, documentPath)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: protected Git path is absent", ErrConflict)
	}
	content, err := p.manager.git(ctx, p.MirrorPath, "cat-file", "blob", p.Head.Commit+":"+documentPath)
	if err != nil {
		return err
	}
	if len(content) == 0 || len(content) > MaxDocumentBytes {
		return ErrInvalid
	}
	digest := sha256.Sum256([]byte(content))
	actual := `"sha256:` + hex.EncodeToString(digest[:]) + `"`
	if actual != expectedETag {
		return fmt.Errorf("%w: protected Git path ETag changed", ErrConflict)
	}
	return nil
}

// VerifyAncestor proves that an accepted base revision is an ancestor of the
// exact provider-pinned prepared head. It performs no fetch, checkout, or
// mutation; callers use it before persisting a claim-time write-base receipt.
func (p *PreparedRepository) VerifyAncestor(ctx context.Context, ancestor string) error {
	if ctx == nil || p == nil || p.manager == nil || p.Binding.Validate() != nil || p.Head.ValidateFor(p.Binding) != nil || !commitRE.MatchString(ancestor) {
		return ErrInvalid
	}
	if _, err := p.manager.git(ctx, p.MirrorPath, "merge-base", "--is-ancestor", ancestor, p.Head.Commit); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: accepted Git base is not an ancestor of the provider head", ErrConflict)
	}
	return nil
}

// VerifyMutationUnchangedSince proves that a protected deployment command
// accepted at Mutation.BaseRevision can be safely rebased onto this exact
// provider-pinned head. The planned base must be an ancestor and the single
// protected path must have the same before-image (or remain absent).
func (p *PreparedRepository) VerifyMutationUnchangedSince(ctx context.Context, mutation Mutation) error {
	if ctx == nil || p == nil || p.manager == nil || mutation.Validate(p.Binding) != nil || mutation.Authority != "" && mutation.Authority != MutationAuthorityVariables ||
		p.Head.ValidateFor(p.Binding) != nil {
		return ErrInvalid
	}
	if err := p.VerifyAncestor(ctx, mutation.BaseRevision); err != nil {
		return err
	}
	basePresent, err := p.pathExists(ctx, mutation.BaseRevision, mutation.Path)
	if err != nil {
		return err
	}
	headPresent, err := p.pathExists(ctx, p.Head.Commit, mutation.Path)
	if err != nil {
		return err
	}
	switch mutation.EffectivePrecondition() {
	case MutationCreateIfAbsent:
		if basePresent || headPresent {
			return fmt.Errorf("%w: create-if-absent path changed before protected publication", ErrConflict)
		}
		return nil
	case MutationMatchETag:
		if !basePresent || !headPresent {
			return fmt.Errorf("%w: protected path presence changed before publication", ErrConflict)
		}
		baseBlob, baseErr := p.manager.git(ctx, p.MirrorPath, "rev-parse", mutation.BaseRevision+":"+mutation.Path)
		headBlob, headErr := p.manager.git(ctx, p.MirrorPath, "rev-parse", p.Head.Commit+":"+mutation.Path)
		baseBlob, headBlob = strings.TrimSpace(baseBlob), strings.TrimSpace(headBlob)
		if baseErr != nil || headErr != nil || baseBlob != headBlob || !blobRE.MatchString(baseBlob) {
			return fmt.Errorf("%w: protected path changed before publication", ErrConflict)
		}
		return nil
	default:
		return ErrInvalid
	}
}

// VerifyPathAbsent proves absence only at the exact provider-pinned prepared
// head and only within the binding's protected prefix. Git/read failures are
// never interpreted as absence.
func (p *PreparedRepository) VerifyPathAbsent(ctx context.Context, documentPath string) error {
	if ctx == nil || p == nil || p.manager == nil || p.Binding.Validate() != nil || p.Head.ValidateFor(p.Binding) != nil ||
		!validProtectedDocumentPath(p.Binding, documentPath) {
		return ErrInvalid
	}
	present, err := p.pathExists(ctx, p.Head.Commit, documentPath)
	if err != nil {
		return err
	}
	if present {
		return fmt.Errorf("%w: protected Git path is already present", ErrConflict)
	}
	return nil
}

// VerifyProtectedMutationPrecondition proves the exact before-image used by a
// protected Helm CAS operation at this provider-pinned head. It is not a
// general path API: Mutation.Validate closes it to the two Helm path families.
func (p *PreparedRepository) VerifyProtectedMutationPrecondition(ctx context.Context, mutation Mutation) error {
	if ctx == nil || p == nil || p.manager == nil || mutation.Validate(p.Binding) != nil || mutation.Authority == "" {
		return ErrInvalid
	}
	switch mutation.EffectivePrecondition() {
	case MutationCreateIfAbsent:
		return p.verifyMutationPathAbsent(ctx, mutation)
	case MutationMatchETag:
		content, err := p.protectedMutationPathContent(ctx, mutation)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		if `"sha256:`+hex.EncodeToString(digest[:])+`"` != mutation.ExpectedETag {
			return fmt.Errorf("%w: protected Git path ETag changed", ErrConflict)
		}
		return nil
	default:
		return ErrInvalid
	}
}

// VerifyProtectedMutationPostimage proves exact accepted bytes, or exact
// absence for a match-delete receipt, at this provider-pinned head.
func (p *PreparedRepository) VerifyProtectedMutationPostimage(ctx context.Context, mutation Mutation) error {
	if ctx == nil || p == nil || p.manager == nil || mutation.Validate(p.Binding) != nil || mutation.Authority == "" {
		return ErrInvalid
	}
	if mutation.EffectiveAction() == MutationDelete {
		return p.verifyMutationPathAbsent(ctx, mutation)
	}
	content, err := p.protectedMutationPathContent(ctx, mutation)
	if err != nil {
		return err
	}
	if !bytes.Equal(content, mutation.Content) {
		return fmt.Errorf("%w: protected Git postimage bytes changed", ErrConflict)
	}
	return nil
}

func (p *PreparedRepository) verifyMutationPathAbsent(ctx context.Context, mutation Mutation) error {
	present, err := p.pathExists(ctx, p.Head.Commit, mutation.Path)
	if err != nil {
		return err
	}
	if present {
		return fmt.Errorf("%w: protected Git path is present", ErrConflict)
	}
	return nil
}

func (p *PreparedRepository) protectedMutationPathContent(ctx context.Context, mutation Mutation) ([]byte, error) {
	present, err := p.pathExists(ctx, p.Head.Commit, mutation.Path)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fmt.Errorf("%w: protected Git path is absent", ErrConflict)
	}
	content, err := p.manager.git(ctx, p.MirrorPath, "cat-file", "blob", p.Head.Commit+":"+mutation.Path)
	if err != nil {
		return nil, err
	}
	maximum := MaxProtectedHelmApplicationBytes
	if mutation.Authority == MutationAuthorityHelmPayload {
		maximum = MaxProtectedHelmPayloadBytes
	} else if mutation.Authority == MutationAuthorityFoundation {
		maximum = MaxProtectedFoundationBytes
	} else if mutation.Authority == MutationAuthorityCertificateIssuer {
		maximum = MaxProtectedCertificateIssuerBytes
	} else if mutation.Authority == MutationAuthorityExternalDNS {
		maximum = MaxProtectedExternalDNSBytes
	} else if mutation.Authority == MutationAuthorityVariables {
		maximum = variables.MaxDocumentBytes
	}
	if len(content) == 0 || len(content) > maximum {
		return nil, ErrInvalid
	}
	return []byte(content), nil
}

// ProtectedExternalDNSPreimage is the closed CAS read for one exact dynamic
// ExternalDNS integration bundle. It cannot read sibling platform paths.
func (p *PreparedRepository) ProtectedExternalDNSPreimage(ctx context.Context, documentPath, revision string) (bool, string, string, error) {
	if ctx == nil || p == nil || p.manager == nil || p.Binding.Validate() != nil || p.Head.ValidateFor(p.Binding) != nil || !commitRE.MatchString(revision) || !validExternalDNSPath(p.Binding, documentPath) {
		return false, "", "", ErrInvalid
	}
	if _, err := p.manager.git(ctx, p.MirrorPath, "merge-base", "--is-ancestor", revision, p.Head.Commit); err != nil {
		return false, "", "", fmt.Errorf("%w: external-dns preimage revision is not in verified ancestry", ErrConflict)
	}
	present, err := p.pathExists(ctx, revision, documentPath)
	if err != nil || !present {
		return present, "", "", err
	}
	content, err := p.manager.git(ctx, p.MirrorPath, "cat-file", "blob", revision+":"+documentPath)
	if err != nil {
		return false, "", "", err
	}
	if len(content) == 0 || len(content) > MaxProtectedExternalDNSBytes {
		return false, "", "", ErrInvalid
	}
	digest := sha256.Sum256([]byte(content))
	value := "sha256:" + hex.EncodeToString(digest[:])
	return true, `"` + value + `"`, value, nil
}

// ProtectedCertificateIssuerPreimage returns only the bounded digest receipt
// needed to construct a certificate-issuer CAS mutation. It is deliberately
// closed to the platform certificate-issuer path family and an already
// provider-pinned head (or one of its verified ancestors); it is not a generic
// Git blob reader.
func (p *PreparedRepository) ProtectedCertificateIssuerPreimage(ctx context.Context, documentPath, revision string) (bool, string, string, error) {
	if ctx == nil || p == nil || p.manager == nil || p.Binding.Validate() != nil || p.Head.ValidateFor(p.Binding) != nil ||
		!commitRE.MatchString(revision) || !validCertificateIssuerPath(p.Binding, documentPath) {
		return false, "", "", ErrInvalid
	}
	if _, err := p.manager.git(ctx, p.MirrorPath, "merge-base", "--is-ancestor", revision, p.Head.Commit); err != nil {
		return false, "", "", fmt.Errorf("%w: certificate-issuer preimage revision is not in verified ancestry", ErrConflict)
	}
	present, err := p.pathExists(ctx, revision, documentPath)
	if err != nil || !present {
		return present, "", "", err
	}
	content, err := p.manager.git(ctx, p.MirrorPath, "cat-file", "blob", revision+":"+documentPath)
	if err != nil {
		return false, "", "", err
	}
	if len(content) == 0 || len(content) > MaxProtectedCertificateIssuerBytes {
		return false, "", "", ErrInvalid
	}
	digest := sha256.Sum256([]byte(content))
	value := "sha256:" + hex.EncodeToString(digest[:])
	return true, `"` + value + `"`, value, nil
}

// validProtectedDocumentPath closes read-side receipt proofs to the same
// server-owned document families used by the environment AppConfig writer and
// protected Argo desired-state writer. A merely prefix-contained sibling is
// not sufficient authority.
func validProtectedDocumentPath(binding Binding, documentPath string) bool {
	if binding.Validate() != nil || !validRelativePath(documentPath) {
		return false
	}
	if dependencies, err := DependencyPaths(binding); err == nil && slices.Contains(dependencies, documentPath) {
		return true
	}
	if !strings.HasPrefix(documentPath+"/", binding.Prefix+"/") {
		return false
	}
	relative := strings.TrimPrefix(documentPath, binding.Prefix+"/")
	parts := strings.Split(relative, "/")
	switch binding.Kind {
	case BindingEnvironment:
		return len(parts) == 3 && parts[0] == "apps" && uuidRE.MatchString(parts[1]) && parts[2] == "app.yaml"
	case BindingPlatform:
		if len(parts) != 3 || parts[0] != "argocd" || parts[1] != "environments" || !strings.HasSuffix(parts[2], ".yaml") {
			return false
		}
		return uuidRE.MatchString(strings.TrimSuffix(parts[2], ".yaml"))
	default:
		return false
	}
}

// FindOperationCommit recovers the exact result of a push whose database
// acknowledgement was lost. It searches a bounded ancestry path, accepts one
// exact operation trailer only, requires our normal writer's direct base
// parent, and verifies the committed path bytes before returning an OID.
func (p *PreparedRepository) FindOperationCommit(ctx context.Context, mutation Mutation) (string, bool, error) {
	if p == nil || p.manager == nil || mutation.Validate(p.Binding) != nil {
		return "", false, ErrInvalid
	}
	if _, err := p.manager.git(ctx, p.MirrorPath, "merge-base", "--is-ancestor", mutation.BaseRevision, p.Head.Commit); err != nil {
		return "", false, nil
	}
	output, err := p.manager.git(ctx, p.MirrorPath, "rev-list", "--ancestry-path", "--max-count=257", p.Head.Commit, "^"+mutation.BaseRevision, "--", mutation.Path)
	if err != nil {
		return "", false, err
	}
	revisions := strings.Fields(output)
	if len(revisions) > 256 {
		return "", false, errors.New("Git operation recovery history exceeded its bound")
	}
	wantedTrailer := "Kuberploy-Operation: " + mutation.OperationID
	found := ""
	for _, revision := range revisions {
		if !commitRE.MatchString(revision) {
			return "", false, errors.New("Git returned an invalid recovery revision")
		}
		commitObject, readErr := p.manager.git(ctx, p.MirrorPath, "cat-file", "commit", revision)
		if readErr != nil {
			return "", false, readErr
		}
		_, message, ok := strings.Cut(commitObject, "\n\n")
		if !ok || !hasExactTrailer(message, wantedTrailer) ||
			(mutation.CommitTrailer != "" && !hasExactTrailer(message, mutation.CommitTrailer)) {
			continue
		}
		parents, parentsErr := p.manager.git(ctx, p.MirrorPath, "rev-list", "--parents", "-n", "1", revision)
		parentFields := strings.Fields(parents)
		if parentsErr != nil || len(parentFields) != 2 || parentFields[0] != revision || parentFields[1] != mutation.BaseRevision {
			return "", false, errors.New("Git operation trailer appeared on an unexpected commit")
		}
		if mutation.EffectiveAction() == MutationDelete {
			if present, inspectErr := p.pathExists(ctx, revision, mutation.Path); inspectErr != nil || present {
				return "", false, errors.New("recovered Git operation deletion differs from its durable command")
			}
		} else {
			committed, blobErr := p.manager.git(ctx, p.MirrorPath, "cat-file", "blob", revision+":"+mutation.Path)
			if blobErr != nil || !bytes.Equal([]byte(committed), mutation.Content) {
				return "", false, errors.New("recovered Git operation content differs from its durable command")
			}
		}
		if found != "" && found != revision {
			return "", false, errors.New("Git operation trailer is ambiguous")
		}
		found = revision
	}
	return found, found != "", nil
}

func hasExactTrailer(message, wanted string) bool {
	count := 0
	for _, line := range strings.Split(strings.TrimSuffix(message, "\n"), "\n") {
		if line == wanted {
			count++
		}
	}
	return count == 1
}

func (p *PreparedRepository) Close(ctx context.Context) error {
	if p.manager == nil {
		return nil
	}
	defer p.manager.clearCredential()
	if p.MirrorPath == "" || p.WorktreePath == "" {
		return nil
	}
	_, err := p.manager.git(ctx, p.MirrorPath, "worktree", "remove", "--force", p.WorktreePath)
	if err != nil {
		return err
	}
	return nil
}

func (m *MirrorManager) git(ctx context.Context, mirror string, args ...string) (string, error) {
	return m.runGit(ctx, mirror, nil, nil, false, args...)
}

func (m *MirrorManager) gitEnv(ctx context.Context, mirror string, extraEnv []string, stdin io.Reader, args ...string) (string, error) {
	return m.runGit(ctx, mirror, extraEnv, stdin, false, args...)
}

func (m *MirrorManager) gitNetwork(ctx context.Context, mirror string, args ...string) (string, error) {
	return m.runGit(ctx, mirror, nil, nil, true, args...)
}

func (m *MirrorManager) runGit(ctx context.Context, mirror string, extraEnv []string, stdin io.Reader, network bool, args ...string) (string, error) {
	if len(args) == 0 {
		return "", ErrInvalid
	}
	if info, err := os.Lstat(mirror); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("Git mirror path must be a real directory")
	}
	if err := replaceTrustedConfig(mirror); err != nil {
		return "", err
	}
	protocol := "never"
	if m.AllowLocalTests {
		protocol = "always"
	}
	secure := []string{"--git-dir=" + mirror,
		"-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null", "-c", "credential.helper=", "-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false", "-c", "protocol.file.allow=" + protocol, "-c", "submodule.recurse=false", "-c", "core.symlinks=false",
		"-c", "http.followRedirects=false"}
	secure = append(secure, args...)
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = defaultGitTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, "git", secure...)
	command.Dir = m.Root
	command.Stdin = stdin
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + m.Root, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_DISCOVERY_ACROSS_FILESYSTEM=0", "GIT_CEILING_DIRECTORIES=" + filepath.Dir(m.Root),
		"GIT_AUTHOR_NAME=Kuberploy", "GIT_AUTHOR_EMAIL=gitops@kuberploy.local", "GIT_COMMITTER_NAME=Kuberploy", "GIT_COMMITTER_EMAIL=gitops@kuberploy.local"}
	command.Env = append(command.Env, extraEnv...)
	var broker *askPassBroker
	if network && m.credential != nil {
		if err := m.credential.validate(time.Now().UTC()); err != nil {
			return "", err
		}
		var err error
		broker, err = startAskPassBroker(m.Root, m.credential)
		if err != nil {
			return "", err
		}
		defer broker.close()
		command.Env = append(command.Env, "GIT_ASKPASS="+DefaultGitAskPass, "GIT_ASKPASS_REQUIRE=force", GitAskPassSocketEnv+"="+broker.path)
	} else if network && m.UseCredentials {
		command.Env = append(command.Env, "GIT_ASKPASS="+DefaultGitAskPass, "GIT_ASKPASS_REQUIRE=force")
	}
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = MaxProtectedHelmPayloadBytes+1, 64<<10
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s failed: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	if stdout.overflow || stderr.overflow {
		return "", errors.New("Git command output exceeded its limit")
	}
	return stdout.String(), nil
}

func (m *MirrorManager) withCredential(ctx context.Context, binding Binding, operation func() error) error {
	if operation == nil || m == nil || m.credential != nil {
		return ErrInvalid
	}
	if m.CredentialProvider == nil {
		return operation()
	}
	credential, err := m.CredentialProvider.AcquireGitCredential(ctx, binding)
	if err != nil {
		return err
	}
	minimumLife := 3*m.gitTimeout() + minimumGitCredentialLife
	if err = credential.validateFor(time.Now().UTC(), minimumLife); err != nil {
		credential.clear()
		return err
	}
	m.credential = &credential
	defer m.clearCredential()
	return operation()
}

func (m *MirrorManager) gitTimeout() time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return defaultGitTimeout
}

func (m *MirrorManager) clearCredential() {
	if m != nil && m.credential != nil {
		m.credential.clear()
		m.credential = nil
	}
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return original, nil
	}
	if len(value) > remaining {
		b.overflow = true
		value = value[:remaining]
	}
	_, _ = b.Buffer.Write(value)
	return original, nil
}

func (m *MirrorManager) checkBudget(root string) error {
	maxBytes := m.MaxBytes
	if maxBytes == 0 {
		maxBytes = defaultMaxGitBytes
	}
	maxFiles := m.MaxFiles
	if maxFiles == 0 {
		maxFiles = defaultMaxGitFiles
	}
	var bytesSeen int64
	filesSeen := 0
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
			return errors.New("Git cache contains a symlink or special file")
		}
		filesSeen++
		if info.Mode().IsRegular() {
			bytesSeen += info.Size()
		}
		if filesSeen > maxFiles || bytesSeen > maxBytes {
			return errors.New("Git cache exceeds configured disk budget")
		}
		return nil
	})
	return err
}

func (m *MirrorManager) checkWorktree(root string) error {
	if err := m.checkBudget(root); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, name)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return errors.New("Git worktree path escaped its root")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("Git worktree contains a symlink")
		}
		return nil
	})
}

func ensureRealRoot(root string) error {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Git cache root must be a real directory")
	}
	return nil
}

func ensureRealSubdirectories(root string, names ...string) error {
	for _, name := range names {
		if name == "" || filepath.Base(name) != name {
			return ErrInvalid
		}
		full := filepath.Join(root, name)
		if err := os.Mkdir(full, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(full)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("Git cache subdirectory must be a real directory")
		}
	}
	return nil
}
