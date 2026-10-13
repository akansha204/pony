// Package detect classifies a known agent from a bounded terminal screen
// snapshot using reloadable, data-only profiles.
package detect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/akansha204/pony/internal/screen"
)

type State string

const (
	Unknown State = "unknown"
	Idle    State = "idle"
	Working State = "working"
	Blocked State = "blocked"
)

type profileFile struct {
	Kind  string       `json:"kind"`
	Rules []ruleConfig `json:"rules"`
}

type ruleConfig struct {
	State    State    `json:"state"`
	Contains []string `json:"contains,omitempty"`
	Regex    []string `json:"regex,omitempty"`
}

type rule struct {
	state    State
	contains []string
	regex    []*regexp.Regexp
}

type loadedProfile struct {
	modified time.Time
	size     int64
	rules    []rule
}

// ProfileStore reloads a profile when its file changes. Files use JSON so
// users can update detection rules without rebuilding Pony.
type ProfileStore struct {
	dir    string
	mu     sync.Mutex
	byKind map[string]loadedProfile
}

func NewProfileStore(dir string) *ProfileStore {
	return &ProfileStore{dir: dir, byKind: make(map[string]loadedProfile)}
}

// Classify evaluates rules in file order against only the visible bottom of
// the screen. Unreliable snapshots and absent or invalid profiles stay unknown.
func (s *ProfileStore) Classify(kind string, snapshot screen.Snapshot) State {
	if kind == "" || kind == "unknown" || !snapshot.Reliable {
		return Unknown
	}
	profile, ok := s.profile(kind)
	if !ok {
		return Unknown
	}
	lines := bottomLines(snapshot.Visible, 8)
	for _, rule := range profile.rules {
		for _, line := range lines {
			lower := strings.ToLower(line)
			for _, phrase := range rule.contains {
				if strings.Contains(lower, phrase) {
					return rule.state
				}
			}
			for _, pattern := range rule.regex {
				if pattern.MatchString(line) {
					return rule.state
				}
			}
		}
	}
	return Unknown
}

func bottomLines(visible []string, limit int) []string {
	end := len(visible)
	for end > 0 && strings.TrimSpace(visible[end-1]) == "" {
		end--
	}
	start := max(0, end-limit)
	return visible[start:end]
}

func (s *ProfileStore) profile(kind string) (loadedProfile, bool) {
	path := filepath.Join(s.dir, kind+".json")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return loadedProfile{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cached, ok := s.byKind[kind]; ok && cached.size == info.Size() && cached.modified.Equal(info.ModTime()) {
		return cached, true
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		delete(s.byKind, kind)
		return loadedProfile{}, false
	}
	var config profileFile
	if err := json.Unmarshal(raw, &config); err != nil || config.Kind != kind {
		delete(s.byKind, kind)
		return loadedProfile{}, false
	}
	compiled := loadedProfile{modified: info.ModTime(), size: info.Size()}
	for _, item := range config.Rules {
		if item.State != Idle && item.State != Working && item.State != Blocked {
			delete(s.byKind, kind)
			return loadedProfile{}, false
		}
		if len(item.Contains)+len(item.Regex) == 0 {
			delete(s.byKind, kind)
			return loadedProfile{}, false
		}
		r := rule{state: item.State}
		for _, phrase := range item.Contains {
			phrase = strings.ToLower(strings.TrimSpace(phrase))
			if phrase == "" {
				delete(s.byKind, kind)
				return loadedProfile{}, false
			}
			r.contains = append(r.contains, phrase)
		}
		for _, expression := range item.Regex {
			pattern, err := regexp.Compile("(?i)" + expression)
			if err != nil {
				delete(s.byKind, kind)
				return loadedProfile{}, false
			}
			r.regex = append(r.regex, pattern)
		}
		compiled.rules = append(compiled.rules, r)
	}
	if len(compiled.rules) == 0 {
		return loadedProfile{}, false
	}
	s.byKind[kind] = compiled
	return compiled, true
}
