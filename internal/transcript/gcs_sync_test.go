package transcript

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGCSSyncOnceUsesGsutilRsync(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "transcripts")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "gsutil.log")
	fakeGsutil := filepath.Join(dir, "gsutil")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"" + logPath + "\"\n"
	if err := os.WriteFile(fakeGsutil, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	syncer := NewGCSSyncer(GCSSyncerConfig{
		SourceDir:   source,
		Destination: "gs://example-bucket/subrouter",
		Command:     fakeGsutil,
		Timeout:     time.Second,
	})
	if syncer == nil {
		t.Fatal("syncer was nil")
	}
	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "-m rsync -r " + source + " gs://example-bucket/subrouter/\n"
	if string(got) != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestGCSSyncerRejectsNonGCSURI(t *testing.T) {
	syncer := NewGCSSyncer(GCSSyncerConfig{
		SourceDir:   t.TempDir(),
		Destination: "https://example.com/not-gcs",
	})
	if syncer != nil {
		t.Fatal("syncer accepted non-GCS destination")
	}
}
