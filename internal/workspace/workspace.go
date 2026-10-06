package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type WorkspaceID string

type Spec struct {
	TaskID     string
	Repository string
	BaseRef    string
}

type Workspace struct {
	ID         WorkspaceID
	TaskID     string
	Path       string
	Branch     string
	Repository string
	Managed    bool
}

type Manager struct {
	root string
	mu   sync.Mutex
}

var validTaskID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func NewManager(root string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("workspace root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	canonical, err := canonicalPath(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	return &Manager{root: canonical}, nil
}

func (m *Manager) Validate(spec Spec) (Workspace, error) {
	if !validTaskID.MatchString(spec.TaskID) || spec.TaskID == "." || spec.TaskID == ".." {
		return Workspace{}, fmt.Errorf("invalid task id %q", spec.TaskID)
	}
	repo, err := ResolveRepository(spec.Repository, spec.BaseRef)
	if err != nil {
		return Workspace{}, err
	}

	branch := "pony/" + spec.TaskID
	if err := checkBranchName(branch); err != nil {
		return Workspace{}, err
	}
	path, err := m.pathFor(spec.TaskID)
	if err != nil {
		return Workspace{}, err
	}
	if pathsOverlap(repo, path) {
		return Workspace{}, fmt.Errorf("workspace path %q overlaps repository %q", path, repo)
	}
	if _, err := os.Lstat(path); err == nil {
		return Workspace{}, fmt.Errorf("workspace path %q already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, fmt.Errorf("inspect workspace path %q: %w", path, err)
	}
	if err := verifyBranchAvailable(repo, branch); err != nil {
		return Workspace{}, err
	}

	return Workspace{
		ID:         WorkspaceID("ws-" + spec.TaskID),
		TaskID:     spec.TaskID,
		Path:       path,
		Branch:     branch,
		Repository: repo,
		Managed:    true,
	}, nil
}

func (m *Manager) pathFor(taskID string) (string, error) {
	path := filepath.Join(m.root, taskID)
	rel, err := filepath.Rel(m.root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace path escapes root")
	}
	return path, nil
}

func pathsOverlap(a, b string) bool {
	return pathContains(a, b) || pathContains(b, a)
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func canonicalPath(path string) (string, error) {
	path = filepath.Clean(path)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
}
