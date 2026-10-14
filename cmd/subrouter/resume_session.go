package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// sessionKind names the agent that owns a session id found on disk.
type sessionKind string

const (
	sessionKindNone   sessionKind = ""
	sessionKindCodex  sessionKind = "codex"
	sessionKindClaude sessionKind = "claude"
)

// Session ids are UUIDs for both agents; anything else never reaches a path.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const (
	maxRolloutMetadataBytes = 8 << 20
	maxSessionScanEntries   = 50_000
)

// sessionLocation is where a session id was found and, for Codex, the model
// provider its rollout recorded when the session was created.
type sessionLocation struct {
	kind          sessionKind
	path          string
	modelProvider string
}

// sessionSearchRoots are the directories searched for a session id.
type sessionSearchRoots struct {
	codexHome        string
	claudeConfigDirs []string
}

func defaultSessionSearchRoots() sessionSearchRoots {
	home, _ := os.UserHomeDir()
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" && home != "" {
		codexHome = filepath.Join(home, ".codex")
	}
	var claudeDirs []string
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		claudeDirs = append(claudeDirs, dir)
	}
	if home != "" {
		claudeDirs = append(claudeDirs, filepath.Join(home, ".claude"))
	}
	if proxyDirs, err := filepath.Glob(filepath.Join(storepath.CodexDirForReadOnlyInspection(), "claude-proxy", "*")); err == nil {
		claudeDirs = append(claudeDirs, proxyDirs...)
	}
	return sessionSearchRoots{codexHome: codexHome, claudeConfigDirs: claudeDirs}
}

// resumeSession resumes an agent session by id through Subrouter, choosing
// the agent from the session's own files: a Codex rollout under CODEX_HOME or
// a Claude transcript under a Claude config directory. It exists so a session
// whose terminal host recorded a plain `codex resume` (which cannot load a
// session created with the `subrouter` provider) can be recovered with one
// command that does not need the user to know which agent created it.
func resumeSession(program string, args []string, roots sessionSearchRoots, errOut io.Writer,
	runCodex func([]string) error, runSR func(string, []string) error) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: %s resume <session-id> [agent args...]", program)
	}
	sessionID := args[0]
	if !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("resume: %q is not a session id (expected a UUID)", sessionID)
	}
	location, err := locateSession(sessionID, roots)
	if err != nil {
		return err
	}
	rest := args[1:]
	switch location.kind {
	case sessionKindCodex:
		if location.modelProvider != "" && location.modelProvider != "subrouter" {
			fmt.Fprintf(errOut, "%s resume: Codex session %s was created with model provider %q; resuming it through Subrouter.\n",
				program, sessionID, location.modelProvider)
		}
		return runCodex(append([]string{"resume", sessionID}, rest...))
	case sessionKindClaude:
		return runSR(program, append([]string{"claude", "proxy", "--resume", sessionID}, rest...))
	default:
		return fmt.Errorf("resume: no Codex rollout under %s and no Claude transcript found for session %s",
			roots.codexHome, sessionID)
	}
}

// locateSession finds the Codex rollout or Claude transcript for sessionID.
// Codex is checked first because its rollout records the provider; a Claude
// transcript only proves the session exists.
func locateSession(sessionID string, roots sessionSearchRoots) (sessionLocation, error) {
	if roots.codexHome != "" {
		if path := findCodexRollout(roots.codexHome, sessionID); path != "" {
			return sessionLocation{
				kind:          sessionKindCodex,
				path:          path,
				modelProvider: codexRolloutModelProvider(path, sessionID),
			}, nil
		}
	}
	for _, dir := range roots.claudeConfigDirs {
		matches, err := filepath.Glob(filepath.Join(dir, "projects", "*", sessionID+".jsonl"))
		if err != nil {
			continue
		}
		for _, match := range matches {
			if info, statErr := os.Stat(match); statErr == nil && info.Mode().IsRegular() {
				return sessionLocation{kind: sessionKindClaude, path: match}, nil
			}
		}
	}
	return sessionLocation{}, nil
}

var errStopSessionScan = errors.New("stop session scan")

func findCodexRollout(codexHome, sessionID string) string {
	suffix := "-" + strings.ToLower(sessionID) + ".jsonl"
	scanned := 0
	found := ""
	for _, dir := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(codexHome, dir)
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if path == root {
					return fs.SkipDir
				}
				return nil
			}
			scanned++
			if scanned > maxSessionScanEntries {
				return errStopSessionScan
			}
			name := strings.ToLower(entry.Name())
			if entry.Type().IsRegular() && strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, suffix) {
				found = path
				return errStopSessionScan
			}
			return nil
		})
		if found != "" || scanned > maxSessionScanEntries {
			break
		}
	}
	return found
}

// codexRolloutModelProvider returns the model_provider from the rollout's
// leading session_meta line, or "" when that line is not this session's.
func codexRolloutModelProvider(path, sessionID string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	line, err := bufio.NewReaderSize(io.LimitReader(file, maxRolloutMetadataBytes), 64<<10).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID            string `json:"id"`
			ModelProvider string `json:"model_provider"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" ||
		!strings.EqualFold(meta.Payload.ID, sessionID) {
		return ""
	}
	return strings.TrimSpace(meta.Payload.ModelProvider)
}
