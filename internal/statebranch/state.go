// Package statebranch safely restores and publishes jobwatch's state branch.
package statebranch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	ModeRestored  = "restored"
	ModeBootstrap = "bootstrap"
)

var restoreRefSequence atomic.Uint64

type RestoreOptions struct {
	RepoDir        string
	Remote         string
	Branch         string
	StatePath      string
	AllowBootstrap bool
	GitHubOutput   string
}

type RestoreResult struct {
	Mode    string
	BaseSHA string
	Count   int
}

type PublishOptions struct {
	RepoDir      string
	Remote       string
	Branch       string
	StatePath    string
	Mode         string
	BaseSHA      string
	SourceSHA    string
	GitHubOutput string
}

type PublishResult struct {
	PublishSHA string
	Changed    bool
}

func Restore(ctx context.Context, opts RestoreOptions) (RestoreResult, error) {
	opts = restoreDefaults(opts)
	if err := validateRemote(ctx, opts.RepoDir, opts.Remote); err != nil {
		return RestoreResult{}, err
	}
	ref, err := branchRef(ctx, opts.RepoDir, opts.Branch)
	if err != nil {
		return RestoreResult{}, err
	}
	advertised, exists, err := advertisedSHA(ctx, opts.RepoDir, opts.Remote, ref)
	if err != nil {
		return RestoreResult{}, err
	}
	if !exists {
		if !opts.AllowBootstrap {
			return RestoreResult{}, fmt.Errorf("state branch %s does not exist; explicit --allow-bootstrap is required", ref)
		}
		result := RestoreResult{Mode: ModeBootstrap, Count: 0}
		if err := writeAtomic(opts.StatePath, []byte("{}")); err != nil {
			return RestoreResult{}, fmt.Errorf("write bootstrap state: %w", err)
		}
		if err := writeRestoreOutput(opts.GitHubOutput, result); err != nil {
			return RestoreResult{}, err
		}
		return result, nil
	}
	if opts.AllowBootstrap {
		return RestoreResult{}, fmt.Errorf("refusing bootstrap: state branch %s already exists at %s", ref, advertised)
	}

	privateRef := fmt.Sprintf("refs/jobwatch-state/restore-%d-%d-%d",
		os.Getpid(), time.Now().UnixNano(), restoreRefSequence.Add(1))
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = runGit(cleanupCtx, opts.RepoDir, nil, nil, "update-ref", "-d", privateRef)
	}()
	fetchResult, fetchErr := runGit(ctx, opts.RepoDir, nil, nil,
		"fetch", "--depth=1", "--no-tags", "--no-write-fetch-head", "--", opts.Remote, ref+":"+privateRef)
	if fetchErr != nil {
		return RestoreResult{}, gitError("fetch state branch", fetchResult, fetchErr)
	}
	fetched, err := resolveCommit(ctx, opts.RepoDir, privateRef)
	if err != nil {
		return RestoreResult{}, err
	}
	if fetched != advertised {
		return RestoreResult{}, fmt.Errorf("state branch changed during restore: advertised %s, fetched %s", advertised, fetched)
	}

	data, _, err := readStateBlob(ctx, opts.RepoDir, fetched)
	if err != nil {
		return RestoreResult{}, err
	}
	records, err := validateState(data)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("validate restored state: %w", err)
	}
	if err := writeAtomic(opts.StatePath, data); err != nil {
		return RestoreResult{}, fmt.Errorf("write restored state: %w", err)
	}
	result := RestoreResult{Mode: ModeRestored, BaseSHA: fetched, Count: len(records)}
	if err := writeRestoreOutput(opts.GitHubOutput, result); err != nil {
		return RestoreResult{}, err
	}
	return result, nil
}

func Publish(ctx context.Context, opts PublishOptions) (PublishResult, error) {
	return publish(ctx, opts, nil)
}

// publish's hook is a deterministic test seam at the compare-and-swap
// boundary. Production callers always enter through Publish with a nil hook.
func publish(ctx context.Context, opts PublishOptions, beforePush func()) (PublishResult, error) {
	opts = publishDefaults(opts)
	if err := validateRemote(ctx, opts.RepoDir, opts.Remote); err != nil {
		return PublishResult{}, err
	}
	ref, err := branchRef(ctx, opts.RepoDir, opts.Branch)
	if err != nil {
		return PublishResult{}, err
	}
	if err := validatePublishMode(opts.Mode, opts.BaseSHA); err != nil {
		return PublishResult{}, err
	}
	if !fullSHA1.MatchString(opts.SourceSHA) {
		return PublishResult{}, fmt.Errorf("source SHA must be a full lowercase 40-hex commit ID")
	}
	resolvedSource, err := resolveCommit(ctx, opts.RepoDir, opts.SourceSHA)
	if err != nil {
		return PublishResult{}, fmt.Errorf("validate source SHA: %w", err)
	}
	if resolvedSource != opts.SourceSHA {
		return PublishResult{}, fmt.Errorf("source SHA resolved to %s, want exact %s", resolvedSource, opts.SourceSHA)
	}
	checkedOutSource, err := resolveCommit(ctx, opts.RepoDir, "HEAD")
	if err != nil {
		return PublishResult{}, fmt.Errorf("resolve checked-out source commit: %w", err)
	}
	if checkedOutSource != opts.SourceSHA {
		return PublishResult{}, fmt.Errorf("source SHA %s does not match checked-out commit %s", opts.SourceSHA, checkedOutSource)
	}
	checkpointMatches, err := filepath.Glob(filepath.Join(filepath.Dir(opts.StatePath), ".state-*.tmp"))
	if err != nil {
		return PublishResult{}, fmt.Errorf("find state checkpoint artifacts: %w", err)
	}
	checkpointTemps := make([]string, 0, len(checkpointMatches))
	for _, path := range checkpointMatches {
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return PublishResult{}, fmt.Errorf("inspect state checkpoint artifact %q: %w", path, statErr)
		}
		if info.Mode().IsRegular() {
			checkpointTemps = append(checkpointTemps, path)
		}
	}
	allowedArtifacts := []string{opts.StatePath, opts.StatePath + ".lock", opts.GitHubOutput}
	allowedArtifacts = append(allowedArtifacts, checkpointTemps...)
	if err := validateCleanCheckout(ctx, opts.RepoDir, allowedArtifacts...); err != nil {
		return PublishResult{}, err
	}

	candidate, err := os.ReadFile(opts.StatePath)
	if err != nil {
		return PublishResult{}, fmt.Errorf("read candidate state: %w", err)
	}
	candidateRecords, err := validateState(candidate)
	if err != nil {
		return PublishResult{}, fmt.Errorf("validate candidate state: %w", err)
	}

	var baseBlobSHA string
	if opts.Mode == ModeRestored {
		resolvedBase, err := resolveCommit(ctx, opts.RepoDir, opts.BaseSHA)
		if err != nil {
			return PublishResult{}, fmt.Errorf("validate base SHA: %w", err)
		}
		if resolvedBase != opts.BaseSHA {
			return PublishResult{}, fmt.Errorf("base SHA resolved to %s, want exact %s", resolvedBase, opts.BaseSHA)
		}
		base, blobSHA, err := readStateBlob(ctx, opts.RepoDir, opts.BaseSHA)
		if err != nil {
			return PublishResult{}, fmt.Errorf("validate base state: %w", err)
		}
		baseRecords, err := validateState(base)
		if err != nil {
			return PublishResult{}, fmt.Errorf("validate base state: %w", err)
		}
		// Removals are refused unless the candidate itself explains them; see
		// shrink.go for the four accepted explanations and why each is
		// checkable without config.
		if err := checkRemovals(baseRecords, candidateRecords); err != nil {
			return PublishResult{}, err
		}
		for id, baseRecord := range baseRecords {
			candidateRecord, exists := candidateRecords[id]
			if !exists {
				continue
			}
			if baseRecord.notified && !candidateRecord.notified {
				return PublishResult{}, fmt.Errorf("candidate state rolled notified job ID %q back to pending", id)
			}
		}
		baseBlobSHA = blobSHA
	}

	advertised, exists, err := advertisedSHA(ctx, opts.RepoDir, opts.Remote, ref)
	if err != nil {
		return PublishResult{}, err
	}
	if opts.Mode == ModeRestored {
		if !exists {
			return PublishResult{}, fmt.Errorf("state branch %s was deleted after restore", ref)
		}
		if advertised != opts.BaseSHA {
			return PublishResult{}, fmt.Errorf("state branch %s advanced after restore: now %s, base %s", ref, advertised, opts.BaseSHA)
		}
	} else if exists {
		return PublishResult{}, fmt.Errorf("state branch %s was created after bootstrap restore at %s", ref, advertised)
	}

	candidateBlobSHA, err := hashBlob(ctx, opts.RepoDir, candidate)
	if err != nil {
		return PublishResult{}, err
	}
	changed := opts.Mode == ModeBootstrap || candidateBlobSHA != baseBlobSHA
	publishSHA := opts.BaseSHA
	if changed {
		commitSHA, treeSHA, err := buildCommit(ctx, opts.RepoDir, candidateBlobSHA, opts.BaseSHA, opts.SourceSHA)
		if err != nil {
			return PublishResult{}, err
		}
		if err := verifyCommit(ctx, opts.RepoDir, commitSHA, treeSHA, opts.BaseSHA); err != nil {
			return PublishResult{}, err
		}
		publishSHA = commitSHA
	}

	if beforePush != nil {
		beforePush()
	}
	lease := "--force-with-lease=" + ref + ":" + opts.BaseSHA
	pushResult, pushErr := runGit(ctx, opts.RepoDir, nil, nil,
		"push", lease, "--", opts.Remote, publishSHA+":"+ref)
	if pushErr != nil {
		return PublishResult{}, gitError("publish state branch without overwriting concurrent changes", pushResult, pushErr)
	}

	published, publishedExists, err := advertisedSHA(ctx, opts.RepoDir, opts.Remote, ref)
	if err != nil {
		return PublishResult{}, fmt.Errorf("verify published state: %w", err)
	}
	if !publishedExists || published != publishSHA {
		return PublishResult{}, fmt.Errorf("verify published state: remote is %q, want %s", published, publishSHA)
	}
	result := PublishResult{PublishSHA: publishSHA, Changed: changed}
	if err := writePublishOutput(opts.GitHubOutput, result); err != nil {
		return PublishResult{}, err
	}
	return result, nil
}

func restoreDefaults(opts RestoreOptions) RestoreOptions {
	if opts.RepoDir == "" {
		opts.RepoDir = "."
	}
	if opts.Remote == "" {
		opts.Remote = "origin"
	}
	if opts.Branch == "" {
		opts.Branch = "state"
	}
	if opts.StatePath == "" {
		opts.StatePath = "state.json"
	}
	return opts
}

func publishDefaults(opts PublishOptions) PublishOptions {
	if opts.RepoDir == "" {
		opts.RepoDir = "."
	}
	if opts.Remote == "" {
		opts.Remote = "origin"
	}
	if opts.Branch == "" {
		opts.Branch = "state"
	}
	if opts.StatePath == "" {
		opts.StatePath = "state.json"
	}
	return opts
}

func validatePublishMode(mode, baseSHA string) error {
	switch mode {
	case ModeRestored:
		if !fullSHA1.MatchString(baseSHA) {
			return fmt.Errorf("restored mode requires a full lowercase 40-hex base SHA")
		}
	case ModeBootstrap:
		if baseSHA != "" {
			return fmt.Errorf("bootstrap mode requires an empty base SHA")
		}
	default:
		return fmt.Errorf("mode must be %q or %q", ModeRestored, ModeBootstrap)
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-restore-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if handle, err := os.Open(dir); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}

func writeRestoreOutput(path string, result RestoreResult) error {
	return appendOutput(path,
		"mode="+result.Mode+"\n"+
			"base_sha="+result.BaseSHA+"\n"+
			"count="+strconv.Itoa(result.Count)+"\n")
}

func writePublishOutput(path string, result PublishResult) error {
	return appendOutput(path,
		"publish_sha="+result.PublishSHA+"\n"+
			"changed="+strconv.FormatBool(result.Changed)+"\n")
}

func appendOutput(path, output string) error {
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open GitHub output: %w", err)
	}
	if _, err := file.WriteString(output); err != nil {
		file.Close()
		return fmt.Errorf("write GitHub output: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close GitHub output: %w", err)
	}
	return nil
}
