package task

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func testRepository(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	runGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Pony Test",
		"GIT_AUTHOR_EMAIL=pony@example.test",
		"GIT_COMMITTER_NAME=Pony Test",
		"GIT_COMMITTER_EMAIL=pony@example.test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCreateStoresPendingTask(t *testing.T) {
	repo := testRepository(t)
	m := NewManager()

	got, err := m.Create(Spec{
		ID:         "fix-login",
		Goal:       "Fix the login flow",
		Repository: filepath.Join(repo, "."),
		BaseRef:    "main",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID != "fix-login" || got.Goal != "Fix the login flow" || got.State != StatePending {
		t.Fatalf("task = %+v", got)
	}
	if got.Repository != repo || got.BaseRef != "main" {
		t.Errorf("repository metadata = %+v", got)
	}
	if got.WorkspaceID != "" || got.SessionID != "" {
		t.Errorf("new task has runtime resources: %+v", got)
	}
}

func TestCreateRejectsInvalidSpec(t *testing.T) {
	repo := testRepository(t)
	tests := []struct {
		name string
		spec Spec
	}{
		{"empty id", Spec{Goal: "goal", Repository: repo, BaseRef: "main"}},
		{"unsafe id", Spec{ID: "../escape", Goal: "goal", Repository: repo, BaseRef: "main"}},
		{"empty goal", Spec{ID: "task", Repository: repo, BaseRef: "main"}},
		{"empty repository", Spec{ID: "task", Goal: "goal", BaseRef: "main"}},
		{"non repository", Spec{ID: "task", Goal: "goal", Repository: t.TempDir(), BaseRef: "main"}},
		{"empty base ref", Spec{ID: "task", Goal: "goal", Repository: repo}},
		{"missing base ref", Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "missing"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewManager().Create(tt.spec); err == nil {
				t.Fatalf("Create accepted %+v", tt.spec)
			}
		})
	}
}

func TestCreateRejectsDuplicateID(t *testing.T) {
	repo := testRepository(t)
	m := NewManager()
	spec := Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}
	if _, err := m.Create(spec); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := m.Create(spec); err == nil {
		t.Fatal("second Create accepted duplicate ID")
	}
}

func TestGetReturnsSnapshot(t *testing.T) {
	repo := testRepository(t)
	m := NewManager()
	created, err := m.Create(Spec{ID: "task", Goal: "original", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	created.Goal = "changed"
	got, ok := m.Get("task")
	if !ok {
		t.Fatal("Get did not find task")
	}
	if got.Goal != "original" {
		t.Fatalf("stored goal = %q", got.Goal)
	}
	if _, ok := m.Get("missing"); ok {
		t.Fatal("Get found a missing task")
	}
}

func TestListIsSortedAndIndependent(t *testing.T) {
	repo := testRepository(t)
	m := NewManager()
	for _, id := range []TaskID{"charlie", "alpha", "bravo"} {
		if _, err := m.Create(Spec{ID: id, Goal: string(id), Repository: repo, BaseRef: "main"}); err != nil {
			t.Fatalf("Create(%q): %v", id, err)
		}
	}

	got := m.List()
	if len(got) != 3 || got[0].ID != "alpha" || got[1].ID != "bravo" || got[2].ID != "charlie" {
		t.Fatalf("List = %+v", got)
	}
	got[0].Goal = "changed"
	stored, _ := m.Get("alpha")
	if stored.Goal != "alpha" {
		t.Fatalf("stored goal = %q", stored.Goal)
	}
}

func TestConcurrentCreateKeepsOneTaskPerID(t *testing.T) {
	repo := testRepository(t)
	m := NewManager()
	spec := Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}

	const attempts = 8
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Create(spec)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var successes int
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 || len(m.List()) != 1 {
		t.Fatalf("successes = %d, tasks = %d", successes, len(m.List()))
	}
}
