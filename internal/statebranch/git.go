package statebranch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var fullSHA1 = regexp.MustCompile(`^[0-9a-f]{40}$`)

type commandResult struct {
	stdout []byte
	stderr []byte
	code   int
}

func runGit(ctx context.Context, repoDir string, stdin []byte, extraEnv []string, args ...string) (commandResult, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoDir}, args...)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Env = append(safeGitEnvironment(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_REPLACE_OBJECTS=1",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	// Bound Wait even if a spawned transport/helper process inherits Git's
	// output pipes and outlives Git after the caller cancels the context.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.code = exitErr.ExitCode()
		return result, err
	}
	return result, err
}

func safeGitEnvironment() []string {
	environment := os.Environ()
	safe := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if unsafeGitEnvironmentVariable(name) {
			continue
		}
		safe = append(safe, entry)
	}
	return safe
}

func unsafeGitEnvironmentVariable(name string) bool {
	switch name {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_INDEX_FILE", "GIT_NAMESPACE",
		"GIT_SHALLOW_FILE", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM",
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_TERMINAL_PROMPT",
		"GIT_NO_REPLACE_OBJECTS", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE":
		return true
	}
	return strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

func validateRemote(ctx context.Context, repoDir, remote string) error {
	if remote == "" {
		return fmt.Errorf("remote must be a configured Git remote name")
	}
	result, err := runGit(ctx, repoDir, nil, nil, "remote")
	if err != nil {
		return gitError("list configured Git remotes", result, err)
	}
	for _, configured := range strings.Split(strings.TrimSpace(string(result.stdout)), "\n") {
		if configured == remote {
			return nil
		}
	}
	return fmt.Errorf("remote %q is not a configured Git remote name", remote)
}

func validateCleanCheckout(ctx context.Context, repoDir string, allowedArtifacts ...string) error {
	root, err := sourceCheckoutRoot(ctx, repoDir)
	if err != nil {
		return err
	}
	result, err := runGit(ctx, root, nil, nil,
		"status", "--porcelain=v1", "--untracked-files=no", "--ignore-submodules=none")
	if err != nil {
		return gitError("inspect source checkout", result, err)
	}
	if detail := strings.TrimSpace(string(result.stdout)); detail != "" {
		return fmt.Errorf("source checkout has tracked changes; refusing an inaccurate source SHA attribution: %s", detail)
	}

	allowed, err := allowedArtifactPaths(root, allowedArtifacts)
	if err != nil {
		return err
	}
	result, err = runGit(ctx, root, nil, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return gitError("inspect untracked source files", result, err)
	}
	for _, rawPath := range bytes.Split(bytes.TrimSuffix(result.stdout, []byte{0}), []byte{0}) {
		if len(rawPath) == 0 {
			continue
		}
		path := string(rawPath)
		if !allowed[path] {
			return fmt.Errorf("source checkout has unignored file %q; refusing an inaccurate source SHA attribution", path)
		}
	}
	return nil
}

func sourceCheckoutRoot(ctx context.Context, repoDir string) (string, error) {
	result, err := runGit(ctx, repoDir, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", gitError("resolve source checkout root", result, err)
	}
	root, err := filepath.Abs(strings.TrimSpace(string(result.stdout)))
	if err != nil {
		return "", fmt.Errorf("resolve source checkout root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("canonicalize source checkout root: %w", err)
	}
	return root, nil
}

func allowedArtifactPaths(root string, paths []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve allowed artifact %q: %w", path, err)
		}
		if canonical, evalErr := filepath.EvalSymlinks(absolute); evalErr == nil {
			absolute = canonical
		} else if !os.IsNotExist(evalErr) {
			return nil, fmt.Errorf("canonicalize allowed artifact %q: %w", path, evalErr)
		}
		relative, err := filepath.Rel(root, absolute)
		if err != nil {
			return nil, fmt.Errorf("locate allowed artifact %q: %w", path, err)
		}
		if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		allowed[filepath.ToSlash(relative)] = true
	}
	return allowed, nil
}

func gitError(action string, result commandResult, err error) error {
	detail := strings.TrimSpace(string(result.stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(result.stdout))
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %s: %w", action, detail, err)
}

func branchRef(ctx context.Context, repoDir, branch string) (string, error) {
	if branch == "" || strings.HasPrefix(branch, "refs/") {
		return "", fmt.Errorf("branch must be a non-empty short branch name")
	}
	result, err := runGit(ctx, repoDir, nil, nil, "check-ref-format", "--branch", branch)
	if err != nil {
		return "", gitError(fmt.Sprintf("invalid branch %q", branch), result, err)
	}
	return "refs/heads/" + branch, nil
}

// advertisedSHA returns exists=false only for git ls-remote's documented
// --exit-code result 2 (no matching ref). Authentication, transport, and all
// other failures remain fatal and visible.
func advertisedSHA(ctx context.Context, repoDir, remote, ref string) (sha string, exists bool, err error) {
	result, runErr := runGit(ctx, repoDir, nil, nil, "ls-remote", "--exit-code", "--refs", "--", remote, ref)
	if runErr != nil {
		if result.code == 2 {
			return "", false, nil
		}
		return "", false, gitError("query state branch", result, runErr)
	}

	lines := strings.Split(strings.TrimSpace(string(result.stdout)), "\n")
	if len(lines) != 1 {
		return "", false, fmt.Errorf("query state branch: expected one exact ref, got %d", len(lines))
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[1] != ref || !fullSHA1.MatchString(fields[0]) {
		return "", false, fmt.Errorf("query state branch: malformed ls-remote response %q", lines[0])
	}
	return fields[0], true, nil
}

func resolveCommit(ctx context.Context, repoDir, sha string) (string, error) {
	result, err := runGit(ctx, repoDir, nil, nil, "rev-parse", "--verify", sha+"^{commit}")
	if err != nil {
		return "", gitError("resolve commit "+sha, result, err)
	}
	resolved := strings.TrimSpace(string(result.stdout))
	if !fullSHA1.MatchString(resolved) {
		return "", fmt.Errorf("resolve commit %s: Git returned malformed SHA %q", sha, resolved)
	}
	return resolved, nil
}

func readStateBlob(ctx context.Context, repoDir, commit string) ([]byte, string, error) {
	result, err := runGit(ctx, repoDir, nil, nil, "ls-tree", "-z", "--full-tree", commit)
	if err != nil {
		return nil, "", gitError("inspect state commit tree", result, err)
	}
	entries := bytes.Split(bytes.TrimSuffix(result.stdout, []byte{0}), []byte{0})
	if len(entries) != 1 || len(entries[0]) == 0 {
		return nil, "", fmt.Errorf("state commit tree must contain exactly one state.json file")
	}
	metadata, name, ok := bytes.Cut(entries[0], []byte{'\t'})
	if !ok || string(name) != "state.json" {
		return nil, "", fmt.Errorf("state commit tree must contain only state.json")
	}
	fields := strings.Fields(string(metadata))
	if len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || !fullSHA1.MatchString(fields[2]) {
		return nil, "", fmt.Errorf("state.json must be a regular Git blob")
	}

	blobResult, err := runGit(ctx, repoDir, nil, nil, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, "", gitError("read state.json blob", blobResult, err)
	}
	return blobResult.stdout, fields[2], nil
}

func hashBlob(ctx context.Context, repoDir string, data []byte) (string, error) {
	result, err := runGit(ctx, repoDir, data, nil, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", gitError("write state blob", result, err)
	}
	sha := strings.TrimSpace(string(result.stdout))
	if !fullSHA1.MatchString(sha) {
		return "", fmt.Errorf("write state blob: Git returned malformed SHA %q", sha)
	}
	return sha, nil
}

func buildCommit(ctx context.Context, repoDir, blobSHA, baseSHA, sourceSHA string) (commitSHA, treeSHA string, err error) {
	treeInput := []byte("100644 blob " + blobSHA + "\tstate.json\n")
	treeResult, err := runGit(ctx, repoDir, treeInput, nil, "mktree")
	if err != nil {
		return "", "", gitError("build state tree", treeResult, err)
	}
	treeSHA = strings.TrimSpace(string(treeResult.stdout))
	if !fullSHA1.MatchString(treeSHA) {
		return "", "", fmt.Errorf("build state tree: Git returned malformed SHA %q", treeSHA)
	}

	args := []string{"commit-tree", treeSHA}
	if baseSHA != "" {
		args = append(args, "-p", baseSHA)
	}
	message := []byte("state: update from " + sourceSHA + "\n")
	identity := []string{
		"GIT_AUTHOR_NAME=jobwatch state publisher",
		"GIT_AUTHOR_EMAIL=41898282+github-actions[bot]@users.noreply.github.com",
		"GIT_COMMITTER_NAME=jobwatch state publisher",
		"GIT_COMMITTER_EMAIL=41898282+github-actions[bot]@users.noreply.github.com",
	}
	commitResult, err := runGit(ctx, repoDir, message, identity, args...)
	if err != nil {
		return "", "", gitError("create state commit", commitResult, err)
	}
	commitSHA = strings.TrimSpace(string(commitResult.stdout))
	if !fullSHA1.MatchString(commitSHA) {
		return "", "", fmt.Errorf("create state commit: Git returned malformed SHA %q", commitSHA)
	}
	return commitSHA, treeSHA, nil
}

func verifyCommit(ctx context.Context, repoDir, commitSHA, treeSHA, baseSHA string) error {
	result, err := runGit(ctx, repoDir, nil, nil, "cat-file", "-p", commitSHA)
	if err != nil {
		return gitError("verify state commit", result, err)
	}
	lines := strings.Split(string(result.stdout), "\n")
	var gotTree string
	var parents []string
	for _, line := range lines {
		if strings.HasPrefix(line, "tree ") {
			gotTree = strings.TrimPrefix(line, "tree ")
		}
		if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimPrefix(line, "parent "))
		}
	}
	if gotTree != treeSHA {
		return fmt.Errorf("verify state commit: tree is %s, want %s", gotTree, treeSHA)
	}
	if baseSHA == "" {
		if len(parents) != 0 {
			return fmt.Errorf("verify bootstrap state commit: got %d parents, want none", len(parents))
		}
		return nil
	}
	if len(parents) != 1 || parents[0] != baseSHA {
		return fmt.Errorf("verify state commit: parents are %v, want exactly [%s]", parents, baseSHA)
	}
	return nil
}
