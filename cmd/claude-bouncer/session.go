package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
)

// hookInput holds the fields of the PreToolUse JSON that identify the session.
type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// registryEntry is one file of Claude Code's live-session registry,
// <config dir>/sessions/<pid>.json. It maps a session ID to its name: the
// name given with --name or /rename, or one Claude Code derived itself.
type registryEntry struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	UpdatedAt int64  `json:"updatedAt"`
}

// sessionName finds the name of the session that invoked the hook. The PreToolUse
// input carries no name, so it is looked up, read-only, in:
//
//  1. the registry file of the Claude Code process ($CLAUDE_PID),
//  2. any registry file with the same session ID,
//  3. the last custom title recorded in the session's transcript.
//
// It returns "" when none of them knows the session.
func sessionName(in hookInput, sys *system) string {
	if in.SessionID == "" {
		return ""
	}
	dir := claudeConfigDir(in.TranscriptPath, sys)
	if dir != "" {
		sessions := filepath.Join(dir, "sessions")
		if pid, err := strconv.Atoi(strings.TrimSpace(sys.getenv("CLAUDE_PID"))); err == nil && pid > 0 {
			if e, ok := readRegistryEntry(filepath.Join(sessions, strconv.Itoa(pid)+".json"), sys); ok && e.SessionID == in.SessionID && e.Name != "" {
				return e.Name
			}
		}
		if paths, err := sys.glob(filepath.Join(sessions, "*.json")); err == nil {
			var best registryEntry
			for _, p := range paths {
				e, ok := readRegistryEntry(p, sys)
				if ok && e.SessionID == in.SessionID && e.Name != "" && (best.Name == "" || e.UpdatedAt > best.UpdatedAt) {
					best = e
				}
			}
			if best.Name != "" {
				return best.Name
			}
		}
	}
	if in.TranscriptPath != "" {
		if data, err := sys.readFile(in.TranscriptPath); err == nil {
			return lastCustomTitle(data)
		}
	}
	return ""
}

func readRegistryEntry(path string, sys *system) (registryEntry, bool) {
	data, err := sys.readFile(path)
	if err != nil {
		return registryEntry{}, false
	}
	var e registryEntry
	if json.Unmarshal(data, &e) != nil {
		return registryEntry{}, false
	}
	e.Name = strings.TrimSpace(e.Name)
	return e, true
}

// claudeConfigDir locates Claude Code's config directory. The transcript lives
// at <config dir>/projects/<project>/<session>.jsonl, which also covers a
// custom CLAUDE_CONFIG_DIR; otherwise the usual locations are used.
func claudeConfigDir(transcript string, sys *system) string {
	if transcript != "" {
		projects := filepath.Dir(filepath.Dir(transcript))
		if strings.EqualFold(filepath.Base(projects), "projects") {
			return filepath.Dir(projects)
		}
	}
	if dir := strings.TrimSpace(sys.getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir
	}
	home, err := sys.userHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// lastCustomTitle returns the latest session title recorded in a transcript.
func lastCustomTitle(data []byte) string {
	title := ""
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.Contains(line, []byte(`"custom-title"`)) {
			continue
		}
		var rec struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Type == "custom-title" && strings.TrimSpace(rec.CustomTitle) != "" {
			title = strings.TrimSpace(rec.CustomTitle)
		}
	}
	return title
}

// patronMatcher decides, at most one lookup per run, whether the calling
// session is the patron. The patron field may hold a session name (compared
// case-insensitively) or a session ID.
type patronMatcher struct {
	in       hookInput
	name     string
	resolved bool
	resolve  func() string
}

func newPatronMatcher(in hookInput, sys *system) *patronMatcher {
	return &patronMatcher{in: in, resolve: func() string { return sessionName(in, sys) }}
}

// fixedPatronMatcher is used when the session name is given on the command line.
func fixedPatronMatcher(name string) *patronMatcher {
	return &patronMatcher{name: strings.TrimSpace(name), resolved: true}
}

func (m *patronMatcher) Name() string {
	if !m.resolved {
		m.name = m.resolve()
		m.resolved = true
	}
	return m.name
}

func (m *patronMatcher) Matches(patron string) bool {
	if m.in.SessionID != "" && m.in.SessionID == patron {
		return true
	}
	name := m.Name()
	return name != "" && strings.EqualFold(name, patron)
}
