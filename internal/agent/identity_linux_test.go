//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookupForegroundIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		comm string
		exe  string
		argv string
		want AgentKind
	}{
		{name: "codex executable", comm: "node", exe: "/opt/codex", argv: "codex\x00", want: AgentKindCodex},
		{name: "opencode executable", comm: "opencode", exe: "/opt/opencode", argv: "opencode\x00", want: AgentKindOpenCode},
		{name: "claude executable", comm: "claude", exe: "/opt/claude", argv: "claude\x00", want: AgentKindClaude},
		{name: "aider executable", comm: "aider", exe: "/opt/aider", argv: "aider\x00", want: AgentKindAider},
		{name: "unknown executable", comm: "sh", exe: "/bin/sh", argv: "sh\x00", want: AgentKindUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			proc := filepath.Join(root, "123")
			if err := os.Mkdir(proc, 0o755); err != nil {
				t.Fatal(err)
			}
			// The comm field may contain spaces and parentheses; parsing must
			// locate its final closing parenthesis before reading numeric fields.
			stat := "123 (agent (helper) name) S 1 123 99 0 0 0\n"
			if err := os.WriteFile(filepath.Join(proc, "stat"), []byte(stat), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(proc, "cmdline"), []byte(tc.argv), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.exe, filepath.Join(proc, "exe")); err != nil {
				t.Fatal(err)
			}

			got, err := lookupForegroundIdentity(root, 99, 123)
			if err != nil {
				t.Fatalf("lookupForegroundIdentity: %v", err)
			}
			if got.PID != 123 || got.Kind != tc.want {
				t.Fatalf("identity = %+v, want pid=123 kind=%s", got, tc.want)
			}
		})
	}
}

func TestLookupForegroundIdentityRejectsRacedOrUnsafeProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		stat string
		pgid int
		sid  int
	}{
		{name: "different group", stat: "123 (codex) S 1 124 99 0 0 0", pgid: 123, sid: 99},
		{name: "different session", stat: "123 (codex) S 1 123 98 0 0 0", pgid: 123, sid: 99},
		{name: "zombie", stat: "123 (codex) Z 1 123 99 0 0 0", pgid: 123, sid: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			proc := filepath.Join(root, "123")
			if err := os.Mkdir(proc, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(proc, "stat"), []byte(tc.stat), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := lookupForegroundIdentity(root, tc.sid, tc.pgid); err == nil {
				t.Fatal("lookupForegroundIdentity succeeded for stale or mismatched process")
			}
		})
	}
}

func TestLookupForegroundIdentityMissingProcIsError(t *testing.T) {
	if _, err := lookupForegroundIdentity(t.TempDir(), 99, 123); err == nil {
		t.Fatal("missing /proc entry should be reported to caller")
	}
}
