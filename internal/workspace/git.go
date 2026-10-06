package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

type worktreeRecord struct {
	path   string
	branch string
}

func (m *Manager) List(repository string) ([]Workspace, error) {
	repo, err := repositoryRoot(repository)
	if err != nil {
		return nil, err
	}
	out, err := gitOutput(repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	records, err := parseWorktrees(out)
	if err != nil {
		return nil, err
	}

	workspaces := make([]Workspace, 0, len(records))
	for _, record := range records {
		branch := strings.TrimPrefix(record.branch, "refs/heads/")
		workspace := Workspace{
			Path:       filepath.Clean(record.path),
			Branch:     branch,
			Repository: repo,
		}

		if taskID, ok := strings.CutPrefix(branch, "pony/"); ok && validTaskID.MatchString(taskID) {
			expected, pathErr := m.pathFor(taskID)
			if pathErr == nil && samePath(expected, workspace.Path) {
				workspace.ID = WorkspaceID("ws-" + taskID)
				workspace.TaskID = taskID
				workspace.Managed = true
			}
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, nil
}

func parseWorktrees(data []byte) ([]worktreeRecord, error) {
	fields := bytes.Split(data, []byte{0})
	var records []worktreeRecord
	var current worktreeRecord
	flush := func() error {
		if current.path == "" {
			return nil
		}
		records = append(records, current)
		current = worktreeRecord{}
		return nil
	}

	for _, raw := range fields {
		line := string(raw)
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			if current.path != "" {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			if value == "" {
				return nil, fmt.Errorf("git worktree record has no path")
			}
			current.path = value
		case "branch":
			current.branch = value
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return records, nil
}

func repositoryRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("repository path must not be empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	out, err := gitOutput(abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("repository %q is not a Git worktree: %w", path, err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("repository %q has no worktree root", path)
	}
	return filepath.Clean(root), nil
}

func verifyCommit(repo, ref string) error {
	if _, err := gitOutput(repo, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}"); err != nil {
		return fmt.Errorf("base ref %q does not resolve to a commit: %w", ref, err)
	}
	return nil
}

func checkBranchName(branch string) error {
	cmd := exec.Command("git", "check-ref-format", "--branch", branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("invalid workspace branch %q: %s", branch, strings.TrimSpace(string(out)))
	}
	return nil
}

func verifyBranchAvailable(repo, branch string) error {
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	err := cmd.Run()
	if err == nil {
		return fmt.Errorf("workspace branch %q already exists", branch)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil
	}
	return fmt.Errorf("check workspace branch %q: %w", branch, err)
}

func gitOutput(repo string, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", commandArgs...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			message := strings.TrimSpace(string(exitErr.Stderr))
			if message != "" {
				return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
			}
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func samePath(a, b string) bool {
	aEval, aErr := filepath.EvalSymlinks(a)
	bEval, bErr := filepath.EvalSymlinks(b)
	if aErr == nil && bErr == nil {
		return filepath.Clean(aEval) == filepath.Clean(bEval)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
