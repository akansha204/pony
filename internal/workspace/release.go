package workspace

import (
	"fmt"
	"os"
	"strings"
)

func (m *Manager) Release(repository string, id WorkspaceID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	taskID, err := taskIDFromWorkspaceID(id)
	if err != nil {
		return err
	}
	repo, err := repositoryRoot(repository)
	if err != nil {
		return err
	}
	if _, err := gitOutput(repo, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune worktrees before release: %w", err)
	}

	path, err := m.pathFor(taskID)
	if err != nil {
		return err
	}
	branch := "pony/" + taskID
	workspaces, err := m.List(repo)
	if err != nil {
		return err
	}

	for _, workspace := range workspaces {
		if workspace.ID == id && workspace.Managed {
			if _, err := gitOutput(repo, "worktree", "remove", "--", workspace.Path); err != nil {
				return fmt.Errorf("release workspace %q: %w", id, err)
			}
			if _, err := gitOutput(repo, "worktree", "prune"); err != nil {
				return fmt.Errorf("prune released workspace %q: %w", id, err)
			}
			_ = os.Remove(m.root)
			return nil
		}
		if samePath(workspace.Path, path) || workspace.Branch == branch {
			return fmt.Errorf("workspace %q is not owned by Pony", id)
		}
	}

	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("workspace path %q exists without Pony ownership", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect workspace path %q: %w", path, err)
	}
	exists, err := branchExists(repo, branch)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return fmt.Errorf("no workspace %q", id)
}

func taskIDFromWorkspaceID(id WorkspaceID) (string, error) {
	taskID, ok := strings.CutPrefix(string(id), "ws-")
	if !ok || !validTaskID.MatchString(taskID) || taskID == "." || taskID == ".." {
		return "", fmt.Errorf("invalid workspace id %q", id)
	}
	return taskID, nil
}
