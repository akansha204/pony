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
	workspace.BaseCommit = baseCommit

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

// RollbackUnusedAllocation removes a freshly allocated, unchanged workspace
// after its agent failed to start. User-facing Release intentionally keeps
// the task branch, so startup rollback has its own compare-and-swap cleanup.
func (m *Manager) RollbackUnusedAllocation(workspace Workspace) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !workspace.Managed || workspace.ID == "" || workspace.BaseCommit == "" {
		return fmt.Errorf("rollback unused allocation: workspace identity is incomplete")
	}
	listed, err := m.List(workspace.Repository)
	if err != nil {
		return fmt.Errorf("inspect allocation before rollback: %w", err)
	}
	registered := false
	for _, current := range listed {
		if current.ID == workspace.ID && current.Managed && current.Branch == workspace.Branch && samePath(current.Path, workspace.Path) {
			registered = true
			break
		}
	}
	if !registered {
		return fmt.Errorf("rollback unused allocation: workspace %q is no longer registered at its allocated path", workspace.ID)
	}
	if err := requireCleanWorktree(workspace.Path); err != nil {
		return fmt.Errorf("preserving failed-start workspace %q: %w", workspace.ID, err)
	}
	head, err := resolveCommit(workspace.Path, "HEAD")
	if err != nil {
		return fmt.Errorf("inspect workspace %q before rollback: %w", workspace.ID, err)
	}
	if head != workspace.BaseCommit {
		return fmt.Errorf("preserving failed-start workspace %q: branch advanced from %s to %s", workspace.ID, workspace.BaseCommit, head)
	}
	if _, err := gitOutput(workspace.Repository, "worktree", "remove", "--", workspace.Path); err != nil {
		return fmt.Errorf("remove unused workspace %q: %w", workspace.ID, err)
	}

	listed, err = m.List(workspace.Repository)
	if err != nil {
		return fmt.Errorf("inspect allocation branch after removing workspace %q: %w", workspace.ID, err)
	}
	for _, current := range listed {
		if current.Branch == workspace.Branch {
			return fmt.Errorf("preserving allocation branch %q: it is checked out at %q", workspace.Branch, current.Path)
		}
	}
	branchCommit, err := resolveCommit(workspace.Repository, "refs/heads/"+workspace.Branch)
	if err != nil {
		return fmt.Errorf("inspect allocation branch %q: %w", workspace.Branch, err)
	}
	if branchCommit != workspace.BaseCommit {
		return fmt.Errorf("preserving allocation branch %q: it advanced from %s to %s", workspace.Branch, workspace.BaseCommit, branchCommit)
	}
	if _, err := gitOutput(workspace.Repository, "update-ref", "-d", "refs/heads/"+workspace.Branch, workspace.BaseCommit); err != nil {
		return fmt.Errorf("remove unused allocation branch %q: %w", workspace.Branch, err)
	}
	return nil
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
