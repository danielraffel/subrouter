package claude

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ResumeSessionID extracts the session UUID from Claude CLI args when they
// request a resume (`--resume <id>`, `-r <id>`, or `--resume=<id>`). Returns
// "" when the args do not name a concrete session.
func ResumeSessionID(args []string) string {
	for i, arg := range args {
		if arg == "--resume" || arg == "-r" {
			if i+1 < len(args) && sessionIDPattern.MatchString(args[i+1]) {
				return args[i+1]
			}
			continue
		}
		for _, prefix := range []string{"--resume=", "-r="} {
			if strings.HasPrefix(arg, prefix) {
				candidate := strings.TrimPrefix(arg, prefix)
				if sessionIDPattern.MatchString(candidate) {
					return candidate
				}
			}
		}
	}
	return ""
}

// MigrateSession copies a conversation (transcript, topic dir, file history,
// session env) into the target profile's config dir when another profile owns
// it. Claude Code stores conversations per CLAUDE_CONFIG_DIR, so resuming a
// session started under a different profile fails with "No conversation found"
// unless the files move with it. Returns the source profile name when a copy
// happened, "" when the target already had the session or nobody has it.
func (s Store) MigrateSession(targetProfile, sessionID string) (string, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return "", fmt.Errorf("invalid session id %q", sessionID)
	}
	targetDir := s.ClaudeConfigDir(targetProfile)
	if len(sessionTranscripts(targetDir, sessionID)) > 0 {
		return "", nil
	}
	for _, profile := range s.ListProfiles() {
		if profile.Name == targetProfile {
			continue
		}
		sourceDir := s.ClaudeConfigDir(profile.Name)
		transcripts := sessionTranscripts(sourceDir, sessionID)
		if len(transcripts) == 0 {
			continue
		}
		for _, transcript := range transcripts {
			projectDir := filepath.Base(filepath.Dir(transcript))
			destProject := filepath.Join(targetDir, "projects", projectDir)
			if err := os.MkdirAll(destProject, 0o700); err != nil {
				return "", err
			}
			if err := copyPath(transcript, filepath.Join(destProject, filepath.Base(transcript))); err != nil {
				return "", err
			}
			// Topic/metadata dir sharing the session id, when present.
			topicDir := strings.TrimSuffix(transcript, ".jsonl")
			if info, err := os.Stat(topicDir); err == nil && info.IsDir() {
				if err := copyPath(topicDir, filepath.Join(destProject, sessionID)); err != nil {
					return "", err
				}
			}
		}
		for _, sub := range []string{"file-history", "session-env"} {
			source := filepath.Join(sourceDir, sub, sessionID)
			if _, err := os.Stat(source); err != nil {
				continue
			}
			if err := os.MkdirAll(filepath.Join(targetDir, sub), 0o700); err != nil {
				return "", err
			}
			if err := copyPath(source, filepath.Join(targetDir, sub, sessionID)); err != nil {
				return "", err
			}
		}
		return profile.Name, nil
	}
	return "", nil
}

func sessionTranscripts(configDir, sessionID string) []string {
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return nil
	}
	return matches
}

func copyPath(source, dest string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(dest, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
