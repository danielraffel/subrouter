package main

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

func TestParseSupervisorConfigSeparatesWorkerArguments(t *testing.T) {
	config, err := parseSupervisorConfig([]string{
		"--addr", "0.0.0.0:31415",
		"--control-addr", "127.0.0.1:31414",
		"--worker-bin", "/usr/local/bin/subrouter",
		"--",
		"serve", "--sr-switch-interval", "10m", "--bedrock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.Addr != "0.0.0.0:31415" || config.WorkerBin != "/usr/local/bin/subrouter" {
		t.Fatalf("unexpected config: %+v", config)
	}
	want := []string{"--sr-switch-interval", "10m", "--bedrock"}
	if fmt.Sprint(config.WorkerArgs) != fmt.Sprint(want) {
		t.Fatalf("worker args = %v, want %v", config.WorkerArgs, want)
	}
}

func TestValidateSupervisorConfigRejectsPublicControlAddress(t *testing.T) {
	config := supervisorConfig{
		Addr:         "0.0.0.0:31415",
		ControlAddr:  "0.0.0.0:31414",
		WorkerBin:    "/usr/local/bin/subrouter",
		ReadyTimeout: time.Second,
	}
	if err := validateSupervisorConfig(config); err == nil {
		t.Fatal("expected public control address to be rejected")
	}
}

func TestValidateSupervisorConfigRejectsWorkerAddress(t *testing.T) {
	config := supervisorConfig{
		Addr:         "0.0.0.0:31415",
		ControlAddr:  "127.0.0.1:31414",
		WorkerBin:    "/usr/local/bin/subrouter",
		ReadyTimeout: time.Second,
		WorkerArgs:   []string{"--addr", "127.0.0.1:1"},
	}
	if err := validateSupervisorConfig(config); err == nil {
		t.Fatal("expected worker --addr to be rejected")
	}
}

func TestInheritedListenerFromEnv(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpListener := listener.(*net.TCPListener)
	file, err := tcpListener.File()
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	t.Setenv(inheritedListenerFDEnv, fmt.Sprint(file.Fd()))

	inherited, err := inheritedListenerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	if inherited == nil {
		t.Fatal("expected inherited listener")
	}
	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := inherited.Accept()
		if acceptErr == nil {
			_ = connection.Close()
			close(accepted)
		}
	}()
	connection, err := net.Dial("tcp", inherited.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(connection, "PROXY TCP4 192.0.2.10 198.51.100.20 43210 31415\r\n")
	_ = connection.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("inherited listener did not accept a connection")
	}
	_ = file.Close()
	if value := os.Getenv(inheritedListenerFDEnv); value != "" {
		t.Fatalf("%s was not cleared", inheritedListenerFDEnv)
	}
}
