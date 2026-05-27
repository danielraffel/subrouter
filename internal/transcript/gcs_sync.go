package transcript

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultGCSSyncCommand = "gsutil"
	defaultGCSSyncTimeout = 2 * time.Minute
)

type GCSSyncer struct {
	sourceDir   string
	destination string
	interval    time.Duration
	command     string
	timeout     time.Duration
	retention   time.Duration
	maxBytes    int64
	logger      *slog.Logger
}

type GCSSyncerConfig struct {
	SourceDir      string
	Destination    string
	Interval       time.Duration
	Command        string
	Timeout        time.Duration
	LocalRetention time.Duration
	MaxLocalBytes  int64
	Logger         *slog.Logger
}

func NewGCSSyncer(config GCSSyncerConfig) *GCSSyncer {
	destination := normalizeGCSDestination(config.Destination)
	sourceDir := strings.TrimSpace(config.SourceDir)
	if sourceDir == "" || destination == "" {
		return nil
	}
	command := strings.TrimSpace(config.Command)
	if command == "" {
		command = defaultGCSSyncCommand
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultGCSSyncTimeout
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &GCSSyncer{
		sourceDir:   sourceDir,
		destination: destination,
		interval:    config.Interval,
		command:     command,
		timeout:     timeout,
		retention:   config.LocalRetention,
		maxBytes:    config.MaxLocalBytes,
		logger:      logger,
	}
}

func (s *GCSSyncer) Enabled() bool {
	return s != nil && s.sourceDir != "" && s.destination != ""
}

func (s *GCSSyncer) Run(ctx context.Context) {
	if !s.Enabled() || s.interval <= 0 {
		return
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := s.SyncOnce(ctx); err != nil {
				s.logger.Warn("transcript gcs sync failed", "destination", s.destination, "error", err)
			}
			timer.Reset(s.interval)
		}
	}
}

func (s *GCSSyncer) SyncOnce(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	if _, err := os.Stat(s.sourceDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	syncCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if err := s.runCommand(syncCtx, "-m", "rsync", "-r", s.sourceDir, s.destination); err != nil {
		return err
	}
	if err := s.pruneLocal(ctx, time.Now()); err != nil {
		return err
	}
	return nil
}

func (s *GCSSyncer) runCommand(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, s.command, args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

type localFile struct {
	path    string
	relPath string
	size    int64
	modTime time.Time
}

func (s *GCSSyncer) pruneLocal(ctx context.Context, now time.Time) error {
	if s.retention <= 0 && s.maxBytes <= 0 {
		return nil
	}
	files, totalBytes, err := s.localTranscriptFiles()
	if err != nil {
		return err
	}
	selected := map[string]localFile{}
	for _, file := range files {
		if s.retention > 0 && !file.modTime.After(now.Add(-s.retention)) {
			selected[file.path] = file
		}
	}
	if s.maxBytes > 0 && totalBytes > s.maxBytes {
		sort.Slice(files, func(i, j int) bool {
			if files[i].modTime.Equal(files[j].modTime) {
				return files[i].path < files[j].path
			}
			return files[i].modTime.Before(files[j].modTime)
		})
		remaining := totalBytes
		for _, file := range files {
			if remaining <= s.maxBytes {
				break
			}
			selected[file.path] = file
			remaining -= file.size
		}
	}
	if len(selected) == 0 {
		return nil
	}

	files = files[:0]
	for _, file := range selected {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path < files[j].path
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	for _, file := range files {
		if err := s.archiveAndRemove(ctx, file); err != nil {
			return err
		}
	}
	_ = pruneEmptyDirs(s.sourceDir)
	return nil
}

func (s *GCSSyncer) localTranscriptFiles() ([]localFile, int64, error) {
	var files []localFile
	var totalBytes int64
	err := filepath.WalkDir(s.sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(s.sourceDir, path)
		if err != nil {
			return err
		}
		files = append(files, localFile{
			path:    path,
			relPath: filepath.ToSlash(relPath),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		totalBytes += info.Size()
		return nil
	})
	return files, totalBytes, err
}

func (s *GCSSyncer) archiveAndRemove(ctx context.Context, file localFile) error {
	info, err := os.Stat(file.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() != file.size || !info.ModTime().Equal(file.modTime) {
		return nil
	}
	archiveURI, err := s.archiveURI(file)
	if err != nil {
		return err
	}

	copyCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.runCommand(copyCtx, "cp", "-n", file.path, archiveURI); err != nil {
		return err
	}

	after, err := os.Stat(file.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if after.Size() != file.size || !after.ModTime().Equal(file.modTime) {
		return nil
	}
	if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *GCSSyncer) archiveURI(file localFile) (string, error) {
	sum, err := fileSHA256(file.path)
	if err != nil {
		return "", err
	}
	archiveName := fmt.Sprintf("%s-%d-%s.jsonl", file.modTime.UTC().Format("20060102T150405.000000000Z"), file.size, sum[:16])
	return s.destination + "_archive/" + file.relPath + "/" + archiveName, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func pruneEmptyDirs(root string) error {
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})
	for _, dir := range dirs {
		_ = os.Remove(dir)
	}
	return nil
}

func normalizeGCSDestination(destination string) string {
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return ""
	}
	if !strings.HasPrefix(destination, "gs://") {
		return ""
	}
	return strings.TrimRight(destination, "/") + "/"
}
