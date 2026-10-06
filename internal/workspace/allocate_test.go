package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAllocateCreatesManagedWorktree(t *testing.T) {
	repo := testRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	workspace, err := m.Allocate(Spec{TaskID: "feature-a", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if workspace.Path != filepath.Join(root, "feature-a") || workspace.Branch != "pony/feature-a" {
		t.Fatalf("workspace = %+v", workspace)
	}
	if _, err := os.Stat(filepath.Join(workspace.Path, "README.md")); err != nil {
		t.Fatalf("allocated checkout: %v", err)
	}
	branch := strings.TrimSpace(run(t, workspace.Path, "git", "branch", "--show-current"))
	if branch != workspace.Branch {
		t.Errorf("checked out branch = %q, want %q", branch, workspace.Branch)
	}

	listed, err := m.List(repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !containsWorkspace(listed, workspace) {
		t.Errorf("allocated workspace missing from List: %+v", listed)
	}
}

func TestAllocateCreatesDistinctWorktreesConcurrently(t *testing.T) {
	repo := testRepository(t)
	m, err := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ids := []string{"task-a", "task-b"}
	results := make(chan Workspace, len(ids))
	errs := make(chan error, len(ids))
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workspace, err := m.Allocate(Spec{TaskID: id, Repository: repo, BaseRef: "main"})
			results <- workspace
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
	}

	var got []Workspace
	for workspace := range results {
		got = append(got, workspace)
	}
	if len(got) != 2 || got[0].Path == got[1].Path || got[0].Branch == got[1].Branch {
		t.Fatalf("workspaces are not distinct: %+v", got)
	}
}

func TestAllocateRejectsDuplicateTask(t *testing.T) {
	repo := testRepository(t)
	m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	spec := Spec{TaskID: "duplicate", Repository: repo, BaseRef: "main"}
	if _, err := m.Allocate(spec); err != nil {
		t.Fatalf("first Allocate: %v", err)
	}
	if _, err := m.Allocate(spec); err == nil {
		t.Fatal("second Allocate succeeded")
	}
}

func TestAllocateRollsBackFailedCheckout(t *testing.T) {
	repo := testRepository(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	root := filepath.Join(t.TempDir(), "workspaces")
	m, _ := NewManager(root)

	workspace, err := m.Allocate(Spec{TaskID: "broken", Repository: repo, BaseRef: "main"})
	if err == nil {
		t.Fatalf("Allocate succeeded: %+v", workspace)
	}
	if _, err := os.Stat(filepath.Join(root, "broken")); !os.IsNotExist(err) {
		t.Fatalf("failed workspace path remains: %v", err)
	}
	if out := run(t, repo, "git", "branch", "--list", "pony/broken"); strings.TrimSpace(out) != "" {
		t.Fatalf("failed workspace branch remains: %q", out)
	}
	listed, listErr := m.List(repo)
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(listed) != 1 || listed[0].Path != repo {
		t.Fatalf("failed worktree metadata remains: %+v", listed)
	}
}
