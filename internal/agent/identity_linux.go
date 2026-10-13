//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type foregroundIdentity struct {
	PID     int
	Process string
	Kind    AgentKind
}

// lookupForegroundIdentity resolves a PTY foreground process group through
// /proc. A process that exited or changed groups during inspection is unknown.
func lookupForegroundIdentity(procRoot string, sessionID, pgid int) (foregroundIdentity, error) {
	if sessionID <= 0 || pgid <= 0 {
		return foregroundIdentity{}, fmt.Errorf("invalid process session or group")
	}
	statPath := filepath.Join(procRoot, strconv.Itoa(pgid), "stat")
	raw, err := os.ReadFile(statPath)
	if err != nil {
		return foregroundIdentity{}, err
	}
	name, state, processGroup, sid, err := parseProcStat(raw)
	if err != nil {
		return foregroundIdentity{}, err
	}
	if state == "Z" || state == "X" || processGroup != pgid || sid != sessionID {
		return foregroundIdentity{}, fmt.Errorf("foreground process changed during inspection")
	}

	argv := readProcArgs(filepath.Join(procRoot, strconv.Itoa(pgid), "cmdline"))
	exe := ""
	if link, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pgid), "exe")); err == nil {
		exe = strings.TrimSuffix(link, " (deleted)")
	}
	process := filepath.Base(exe)
	if process == "." || process == string(filepath.Separator) || process == "" {
		process = name
	}
	kind := classifyAgentExecutable(process)
	if kind == AgentKindUnknown {
		kind = classifyAgentExecutable(name)
	}
	if kind == AgentKindUnknown && len(argv) > 0 {
		kind = classifyAgentExecutable(filepath.Base(argv[0]))
	}
	if kind == AgentKindUnknown {
		for _, arg := range argv[1:] {
			if candidate := classifyAgentExecutable(filepath.Base(arg)); candidate != AgentKindUnknown {
				kind = candidate
				break
			}
		}
	}
	return foregroundIdentity{PID: pgid, Process: process, Kind: kind}, nil
}

func parseProcStat(raw []byte) (name, state string, processGroup, sessionID int, err error) {
	open := strings.IndexByte(string(raw), '(')
	close := strings.LastIndexByte(string(raw), ')')
	if open < 0 || close <= open {
		return "", "", 0, 0, fmt.Errorf("malformed /proc stat")
	}
	name = string(raw[open+1 : close])
	fields := strings.Fields(string(raw[close+1:]))
	if len(fields) < 4 {
		return "", "", 0, 0, fmt.Errorf("incomplete /proc stat")
	}
	processGroup, err = strconv.Atoi(fields[2])
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("parse process group in /proc stat: %w", err)
	}
	sessionID, err = strconv.Atoi(fields[3])
	if err != nil {
		return "", "", 0, 0, fmt.Errorf("parse session in /proc stat: %w", err)
	}
	return name, fields[0], processGroup, sessionID, nil
}

func readProcArgs(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil
	}
	parts := strings.Split(string(raw), "\x00")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func classifyAgentExecutable(name string) AgentKind {
	base := strings.ToLower(filepath.Base(strings.TrimSuffix(name, " (deleted)")))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "codex":
		return AgentKindCodex
	case "opencode":
		return AgentKindOpenCode
	case "claude":
		return AgentKindClaude
	case "aider":
		return AgentKindAider
	default:
		return AgentKindUnknown
	}
}
