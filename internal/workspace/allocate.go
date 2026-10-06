package workspace

import (
	"fmt"
	"os"
)

func (m *Manager) Allocate(spec Spec) (Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	workspace, err := m.Validate(spec)
	if err != nil {
		return Workspace{}, err
	}
	baseCommit, err := resolveCommit(workspace.Repository, spec.BaseRef)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve base ref %q: %w", spec.BaseRef, err)
	}

	rootCreated, err := ensureDirectory(m.root)
	if err != nil {
		return Workspace{}, err
	}
	if err := os.Mkdir(workspace.Path, 0o755); err != nil {
		if rootCreated {
			_ = os.Remove(m.root)
		}
		return Workspace{}, fmt.Errorf("reserve workspace path %q: %w", workspace.Path, err)
	}
	if _, err := gitOutput(workspace.Repository, "branch", workspace.Branch, baseCommit); err != nil {
		_ = os.Remove(workspace.Path)
		if rootCreated {
			_ = os.Remove(m.root)
		}
		return Workspace{}, fmt.Errorf("reserve workspace branch %q: %w", workspace.Branch, err)
	}
	if _, err := gitOutput(
		workspace.Repository,
		"worktree", "add", "--", workspace.Path, workspace.Branch,
	); err != nil {
		m.rollbackAllocation(workspace, rootCreated)
		return Workspace{}, fmt.Errorf("allocate workspace %q: %w", workspace.ID, err)
	}

	listed, err := m.List(workspace.Repository)
	if err != nil || !containsWorkspace(listed, workspace) {
		m.rollbackAllocation(workspace, rootCreated)
		if err != nil {
			return Workspace{}, fmt.Errorf("verify workspace %q: %w", workspace.ID, err)
		}
		return Workspace{}, fmt.Errorf("verify workspace %q: Git did not report the created worktree", workspace.ID)
	}
	return workspace, nil
}

func ensureDirectory(path string) (bool, error) {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("workspace root %q is not a directory", path)
		}
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect workspace root %q: %w", path, err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return false, fmt.Errorf("create workspace root %q: %w", path, err)
	}
	return true, nil
}

func (m *Manager) rollbackAllocation(workspace Workspace, rootCreated bool) {
	_, _ = gitOutput(workspace.Repository, "worktree", "remove", "--force", "--", workspace.Path)
	_, _ = gitOutput(workspace.Repository, "worktree", "prune")
	_, _ = gitOutput(workspace.Repository, "branch", "-D", "--", workspace.Branch)
	_ = os.RemoveAll(workspace.Path)
	if rootCreated {
		_ = os.Remove(m.root)
	}
}

func containsWorkspace(workspaces []Workspace, want Workspace) bool {
	for _, workspace := range workspaces {
		if workspace.Managed && workspace.ID == want.ID &&
			samePath(workspace.Path, want.Path) && workspace.Branch == want.Branch {
			return true
		}
	}
	return false
}
