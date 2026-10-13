package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseRemovesOnlyRequestedWorktree(t *testing.T) {
	repo := testRepository(t)
	m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	first, err := m.Allocate(Spec{TaskID: "first", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate first: %v", err)
	}
	second, err := m.Allocate(Spec{TaskID: "second", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate second: %v", err)
	}

	if err := m.Release(repo, first.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("released path remains: %v", err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Fatalf("second workspace was affected: %v", err)
	}
	listed, err := m.List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !containsWorkspace(listed, second) {
		t.Errorf("second workspace missing after release: %+v", listed)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	repo := testRepository(t)
	m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	workspace, err := m.Allocate(Spec{TaskID: "repeat", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	for i := range 2 {
		if err := m.Release(repo, workspace.ID); err != nil {
			t.Fatalf("Release call %d: %v", i+1, err)
		}
	}
}

func TestReleaseRefusesDirtyWorktree(t *testing.T) {
	repo := testRepository(t)
	m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	workspace, err := m.Allocate(Spec{TaskID: "dirty", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	dirtyFile := filepath.Join(workspace.Path, "uncommitted.txt")
	if err := os.WriteFile(dirtyFile, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := m.Release(repo, workspace.ID); err == nil {
		t.Fatal("Release removed a dirty worktree")
	}
	if _, err := os.Stat(dirtyFile); err != nil {
		t.Fatalf("dirty content was removed: %v", err)
	}
}

func TestReleasePreservesIgnoredFile(t *testing.T) {
	repo := testRepository(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("secret.env\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .gitignore: %v", err)
	}
	run(t, repo, "git", "add", ".gitignore")
	run(t, repo, "git", "commit", "-m", "ignore secret files")
	m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	workspace, err := m.Allocate(Spec{TaskID: "ignored", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	secret := filepath.Join(workspace.Path, "secret.env")
	if err := os.WriteFile(secret, []byte("keep me\n"), 0o600); err != nil {
		t.Fatalf("WriteFile ignored file: %v", err)
	}

	if err := m.Release(repo, workspace.ID); err == nil {
		t.Fatal("Release removed a worktree containing an ignored file")
	}
	contents, err := os.ReadFile(secret)
	if err != nil {
		t.Fatalf("ignored file was removed: %v", err)
	}
	if string(contents) != "keep me\n" {
		t.Fatalf("ignored file contents = %q, want preserved contents", contents)
	}
}

func TestReleasePreservesIgnoredDirectory(t *testing.T) {
	repo := testRepository(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("build-cache/\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .gitignore: %v", err)
	}
	run(t, repo, "git", "add", ".gitignore")
	run(t, repo, "git", "commit", "-m", "ignore build cache")
	m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	workspace, err := m.Allocate(Spec{TaskID: "ignored-dir", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	cacheFile := filepath.Join(workspace.Path, "build-cache", "nested", "artifact.bin")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		t.Fatalf("MkdirAll ignored directory: %v", err)
	}
	if err := os.WriteFile(cacheFile, []byte("keep cache\n"), 0o600); err != nil {
		t.Fatalf("WriteFile ignored file: %v", err)
	}

	if err := m.Release(repo, workspace.ID); err == nil {
		t.Fatal("Release removed a worktree containing an ignored directory")
	}
	contents, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatalf("ignored directory contents were removed: %v", err)
	}
	if string(contents) != "keep cache\n" {
		t.Fatalf("ignored file contents = %q, want preserved contents", contents)
	}
}

func TestReleaseRemovesCompletelyCleanWorktree(t *testing.T) {
	repo := testRepository(t)
	m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	workspace, err := m.Allocate(Spec{TaskID: "clean", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := m.Release(repo, workspace.ID); err != nil {
		t.Fatalf("Release clean worktree: %v", err)
	}
	if _, err := os.Stat(workspace.Path); !os.IsNotExist(err) {
		t.Fatalf("clean worktree path remains or stat failed unexpectedly: %v", err)
	}
}

func TestReleaseRefusesUnmanagedWorktree(t *testing.T) {
	repo := testRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	path := filepath.Join(root, "foreign")
	run(t, repo, "git", "worktree", "add", "-b", "someone-else", path, "main")
	m, _ := NewManager(root)

	if err := m.Release(repo, "ws-foreign"); err == nil {
		t.Fatal("Release accepted an unmanaged worktree")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unmanaged worktree was removed: %v", err)
	}
}

func TestReleaseCleansStaleWorktreeMetadata(t *testing.T) {
	repo := testRepository(t)
	m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	workspace, err := m.Allocate(Spec{TaskID: "stale", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if err := os.RemoveAll(workspace.Path); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	if err := m.Release(repo, workspace.ID); err != nil {
		t.Fatalf("Release stale workspace: %v", err)
	}
	if listed := strings.TrimSpace(run(t, repo, "git", "worktree", "list", "--porcelain")); strings.Contains(listed, workspace.Path) {
		t.Fatalf("stale worktree metadata remains:\n%s", listed)
	}
}

func TestReleaseRejectsUnknownAndInvalidIDs(t *testing.T) {
	repo := testRepository(t)
	m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	for _, id := range []WorkspaceID{"bad", "ws-../escape", "ws-unknown"} {
		if err := m.Release(repo, id); err == nil {
			t.Errorf("Release accepted %q", id)
		}
	}
}
