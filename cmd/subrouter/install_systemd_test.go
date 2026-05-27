package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdUnitUsesServerDefaults(t *testing.T) {
	config := systemdConfig{
		ServiceName:      defaultSystemdServiceName,
		User:             "subrouter",
		Group:            "subrouter",
		Home:             "/var/lib/subrouter",
		Addr:             "0.0.0.0:31415",
		InstallPath:      "/usr/local/bin/subrouter",
		SessionsPath:     "/var/lib/subrouter/sessions.json",
		SRSwitchInterval: "10m",
	}
	unit, err := systemdUnit(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Requires=subrouter.socket",
		"After=network-online.target subrouter.socket",
		"Sockets=subrouter.socket",
		"User=subrouter",
		"Group=subrouter",
		"WorkingDirectory=/var/lib/subrouter",
		"Environment=HOME=/var/lib/subrouter",
		"EnvironmentFile=-/etc/default/subrouter",
		"ExecStart=/usr/local/bin/subrouter serve --addr ${SUBROUTER_ADDR}",
		"$SUBROUTER_TRANSCRIPT_ARGS",
		"--sr-switch-interval ${SUBROUTER_SR_SWITCH_INTERVAL}",
		"TimeoutStopSec=10min",
		"ReadWritePaths=/var/lib/subrouter /var/log/subrouter",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestSystemdSocketUsesConfiguredAddress(t *testing.T) {
	config := systemdConfig{
		ServiceName: defaultSystemdServiceName,
		Addr:        "0.0.0.0:31415",
	}
	socketUnit, err := systemdSocket(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ListenStream=0.0.0.0:31415",
		"NoDelay=true",
		"Service=subrouter.service",
		"WantedBy=sockets.target",
	} {
		if !strings.Contains(socketUnit, want) {
			t.Fatalf("socket unit missing %q:\n%s", want, socketUnit)
		}
	}
}

func TestSystemdDefaultsEscapesExtraArgs(t *testing.T) {
	config := systemdConfig{
		Addr:             "0.0.0.0:31415",
		Home:             "/var/lib/subrouter",
		SessionsPath:     "/var/lib/subrouter/sessions.json",
		TranscriptsDir:   "/var/lib/subrouter/transcripts",
		SRSwitchInterval: "10m",
		AdminToken:       "secret-token",
		ExtraArgs:        "--transcript-gcs-uri=gs://bucket/prefix --fetch-usage=false",
	}
	defaults := systemdDefaults(config)
	if !strings.Contains(defaults, "SUBROUTER_STATE_DIR=/var/lib/subrouter") {
		t.Fatalf("defaults missing state dir:\n%s", defaults)
	}
	if !strings.Contains(defaults, "SUBROUTER_SR_SWITCH_INTERVAL=10m") {
		t.Fatalf("defaults missing sr switch interval:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_TRANSCRIPT_ARGS="--transcripts=/var/lib/subrouter/transcripts"`) {
		t.Fatalf("defaults missing transcript args:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_EXTRA_ARGS="--transcript-gcs-uri=gs://bucket/prefix --fetch-usage=false"`) {
		t.Fatalf("defaults did not quote extra args:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_ADMIN_TOKEN="secret-token"`) {
		t.Fatalf("defaults did not quote admin token:\n%s", defaults)
	}
}

func TestSystemdDefaultsDisableTranscriptsByDefault(t *testing.T) {
	config := systemdConfig{
		Addr:             "0.0.0.0:31415",
		Home:             "/var/lib/subrouter",
		SessionsPath:     "/var/lib/subrouter/sessions.json",
		SRSwitchInterval: "10m",
	}
	defaults := systemdDefaults(config)
	if !strings.Contains(defaults, "SUBROUTER_TRANSCRIPTS=\n") {
		t.Fatalf("defaults should leave transcript dir empty:\n%s", defaults)
	}
	if !strings.Contains(defaults, `SUBROUTER_TRANSCRIPT_ARGS=""`) {
		t.Fatalf("defaults should leave transcript args empty:\n%s", defaults)
	}
}

func TestReadDefaultValueUnquotesEnvFileValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subrouter")
	if err := os.WriteFile(path, []byte("SUBROUTER_EXTRA_ARGS=\"--transcript-gcs-uri=gs://bucket/prefix --fetch-usage=false\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readDefaultValue(path, "SUBROUTER_EXTRA_ARGS")
	want := "--transcript-gcs-uri=gs://bucket/prefix --fetch-usage=false"
	if got != want {
		t.Fatalf("extra args = %q, want %q", got, want)
	}
}
