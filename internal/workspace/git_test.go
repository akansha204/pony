package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseWorktrees(t *testing.T) {
	input := []byte("worktree /repo\x00HEAD abc123\x00branch refs/heads/main\x00\x00" +
		"worktree /tmp/work trees/task-one\x00HEAD def456\x00branch refs/heads/pony/task-one\x00\x00" +
		"worktree /tmp/detached\x00HEAD fedcba\x00detached\x00\x00")

	got, err := parseWorktrees(input)
	if err != nil {
		t.Fatalf("parseWorktrees: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3: %+v", len(got), got)
	}
	if got[1].path != "/tmp/work trees/task-one" || got[1].branch != "refs/heads/pony/task-one" {
		t.Errorf("second record = %+v", got[1])
	}
	if got[2].branch != "" {
		t.Errorf("detached branch = %q, want empty", got[2].branch)
	}
}

func TestListFindsManagedAndUnmanagedWorktrees(t *testing.T) {
	repo := testRepository(t)
	base := t.TempDir()
	root := filepath.Join(base, "workspaces")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	managedPath := filepath.Join(root, "task-one")
	unmanagedPath := filepath.Join(base, "elsewhere")
	run(t, repo, "git", "worktree", "add", "-b", "pony/task-one", managedPath, "main")
	run(t, repo, "git", "worktree", "add", "-b", "other-branch", unmanagedPath, "main")

	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	got, err := m.List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d worktrees, want 3: %+v", len(got), got)
	}

	byPath := make(map[string]Workspace, len(got))
	for _, workspace := range got {
		byPath[workspace.Path] = workspace
	}
	managed := byPath[managedPath]
	if !managed.Managed || managed.ID != "ws-task-one" || managed.TaskID != "task-one" || managed.Branch != "pony/task-one" {
		t.Errorf("managed workspace = %+v", managed)
	}
	if byPath[repo].Managed {
		t.Errorf("main checkout was classified as managed: %+v", byPath[repo])
	}
	if byPath[unmanagedPath].Managed {
		t.Errorf("unmanaged worktree was classified as managed: %+v", byPath[unmanagedPath])
	}
}

func TestListRequiresRepository(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := m.List(t.TempDir()); err == nil {
		t.Fatal("List accepted a non-repository")
	}
}
