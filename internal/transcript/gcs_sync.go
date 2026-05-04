package transcript

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
	logger      *slog.Logger
}

type GCSSyncerConfig struct {
	SourceDir   string
	Destination string
	Interval    time.Duration
	Command     string
	Timeout     time.Duration
	Logger      *slog.Logger
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

	cmd := exec.CommandContext(syncCtx, s.command, "-m", "rsync", "-r", s.sourceDir, s.destination)
	output, err := cmd.CombinedOutput()
	if syncCtx.Err() != nil {
		return syncCtx.Err()
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
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
