package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Pony Test",
		"GIT_AUTHOR_EMAIL=pony@example.test",
		"GIT_COMMITTER_NAME=Pony Test",
		"GIT_COMMITTER_EMAIL=pony@example.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

func testRepository(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	run(t, repo, "git", "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	run(t, repo, "git", "add", "README.md")
	run(t, repo, "git", "commit", "-m", "initial")
	return repo
}

func TestValidateBuildsSafeWorkspaceIdentity(t *testing.T) {
	repo := testRepository(t)
	root := filepath.Join(t.TempDir(), "workspaces")
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	got, err := m.Validate(Spec{TaskID: "fix-login", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.ID != "ws-fix-login" || got.TaskID != "fix-login" || got.Branch != "pony/fix-login" {
		t.Fatalf("workspace identity = %+v", got)
	}
	if got.Path != filepath.Join(root, "fix-login") {
		t.Errorf("path = %q, want %q", got.Path, filepath.Join(root, "fix-login"))
	}
	if got.Repository != repo || !got.Managed {
		t.Errorf("workspace metadata = %+v", got)
	}
}

func TestValidateRejectsUnsafeTaskIDs(t *testing.T) {
	repo := testRepository(t)
	m, err := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	for _, id := range []string{"", ".", "..", "../escape", "two words", "UPPER", "slash/name"} {
		t.Run(id, func(t *testing.T) {
			if _, err := m.Validate(Spec{TaskID: id, Repository: repo, BaseRef: "main"}); err == nil {
				t.Fatalf("Validate accepted task id %q", id)
			}
		})
	}
}

func TestValidateRejectsBadRepositoryAndBaseRef(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := m.Validate(Spec{TaskID: "task", Repository: t.TempDir(), BaseRef: "main"}); err == nil {
		t.Fatal("Validate accepted a non-repository")
	}

	repo := testRepository(t)
	if _, err := m.Validate(Spec{TaskID: "task", Repository: repo, BaseRef: "missing"}); err == nil {
		t.Fatal("Validate accepted a missing base ref")
	}
	if _, err := m.Validate(Spec{TaskID: "task", Repository: repo}); err == nil {
		t.Fatal("Validate accepted an empty base ref")
	}
}

func TestValidateRejectsWorkspaceConflicts(t *testing.T) {
	repo := testRepository(t)

	t.Run("path exists", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "workspaces")
		if err := os.MkdirAll(filepath.Join(root, "task"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		m, _ := NewManager(root)
		if _, err := m.Validate(Spec{TaskID: "task", Repository: repo, BaseRef: "main"}); err == nil {
			t.Fatal("Validate accepted an existing workspace path")
		}
	})

	t.Run("branch exists", func(t *testing.T) {
		run(t, repo, "git", "branch", "pony/taken", "main")
		m, _ := NewManager(filepath.Join(t.TempDir(), "workspaces"))
		if _, err := m.Validate(Spec{TaskID: "taken", Repository: repo, BaseRef: "main"}); err == nil {
			t.Fatal("Validate accepted an existing workspace branch")
		}
	})

	t.Run("overlaps repository", func(t *testing.T) {
		m, _ := NewManager(filepath.Join(repo, "workspaces"))
		if _, err := m.Validate(Spec{TaskID: "task", Repository: repo, BaseRef: "main"}); err == nil {
			t.Fatal("Validate accepted a workspace inside the repository")
		}
	})
}

func TestNewManagerRejectsEmptyRoot(t *testing.T) {
	if _, err := NewManager(" "); err == nil {
		t.Fatal("NewManager accepted an empty root")
	}
}
