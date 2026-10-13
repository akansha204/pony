package detect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akansha204/pony/internal/screen"
)

func TestClassifyVisibleScreen(t *testing.T) {
	store := NewProfileStore(filepath.Join("..", "..", "profiles", "agents"))
	for _, tc := range []struct {
		name       string
		visible    []string
		scrollback []string
		reliable   bool
		kind       string
		want       State
	}{
		{name: "working", visible: []string{"", "Thinking…"}, reliable: true, kind: "codex", want: Working},
		{name: "approval wins over working", visible: []string{"Thinking…", "Approval required: allow this action?"}, reliable: true, kind: "codex", want: Blocked},
		{name: "idle prompt", visible: []string{"answer complete", "❯"}, reliable: true, kind: "codex", want: Idle},
		{name: "unknown tail", visible: []string{"connected", "ready"}, reliable: true, kind: "codex", want: Unknown},
		{name: "scrollback is ignored", visible: []string{"", "ready"}, scrollback: []string{"Thinking…"}, reliable: true, kind: "codex", want: Unknown},
		{name: "unreliable screen", visible: []string{"Thinking…"}, reliable: false, kind: "codex", want: Unknown},
		{name: "unknown kind", visible: []string{"Thinking…"}, reliable: true, kind: "unknown", want: Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := store.Classify(tc.kind, screen.Snapshot{Visible: tc.visible, Scrollback: tc.scrollback, Reliable: tc.reliable})
			if got != tc.want {
				t.Fatalf("Classify() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProfileReloadsAfterFileChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"kind":"codex","rules":[{"state":"working","contains":["profile-token"]}]}`)
	store := NewProfileStore(dir)
	snapshot := screen.Snapshot{Visible: []string{"profile-token"}, Reliable: true}
	if got := store.Classify("codex", snapshot); got != Working {
		t.Fatalf("initial classification = %q, want working", got)
	}
	write(`{"kind":"codex","rules":[{"state":"blocked","contains":["profile-token-after-update"]}]}`)
	snapshot.Visible = []string{"profile-token-after-update"}
	if got := store.Classify("codex", snapshot); got != Blocked {
		t.Fatalf("classification after profile update = %q, want blocked", got)
	}
}

func TestInvalidOrMissingProfileStaysUnknown(t *testing.T) {
	dir := t.TempDir()
	store := NewProfileStore(dir)
	snapshot := screen.Snapshot{Visible: []string{"Thinking"}, Reliable: true}
	if got := store.Classify("codex", snapshot); got != Unknown {
		t.Fatalf("missing profile classification = %q, want unknown", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex.json"), []byte(`{"kind":"codex","rules":[{"state":"made-up","contains":["Thinking"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := store.Classify("codex", snapshot); got != Unknown {
		t.Fatalf("invalid profile classification = %q, want unknown", got)
	}
}
