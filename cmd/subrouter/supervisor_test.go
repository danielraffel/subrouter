package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestParseSupervisorConfigSeparatesWorkerArguments(t *testing.T) {
	config, err := parseSupervisorConfig([]string{
		"--addr", "0.0.0.0:31415",
		"--control-socket", "/var/run/subrouter-test.sock",
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
	if config.ControlSocket != "/var/run/subrouter-test.sock" {
		t.Fatalf("control socket = %q", config.ControlSocket)
	}
	want := []string{"--sr-switch-interval", "10m", "--bedrock"}
	if fmt.Sprint(config.WorkerArgs) != fmt.Sprint(want) {
		t.Fatalf("worker args = %v, want %v", config.WorkerArgs, want)
	}
}

func TestPrepareControlSocketRefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareControlSocket(path); err == nil {
		t.Fatal("expected regular control-socket path to be rejected")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "keep" {
		t.Fatalf("regular file was modified: %q", body)
	}
}

func TestValidateSupervisorConfigRejectsRelativeControlSocket(t *testing.T) {
	config := supervisorConfig{
		Addr:          "0.0.0.0:31415",
		ControlSocket: "subrouter.sock",
		WorkerBin:     "/usr/local/bin/subrouter",
		ReadyTimeout:  time.Second,
	}
	if err := validateSupervisorConfig(config); err == nil {
		t.Fatal("expected relative control socket to be rejected")
	}
}

func TestValidateSupervisorConfigRejectsWorkerAddress(t *testing.T) {
	config := supervisorConfig{
		Addr:          "0.0.0.0:31415",
		ControlSocket: "/var/run/subrouter-test.sock",
		WorkerBin:     "/usr/local/bin/subrouter",
		ReadyTimeout:  time.Second,
		WorkerArgs:    []string{"--addr", "127.0.0.1:1"},
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

func TestTerminateWorkerKillsAfterGracePeriod(t *testing.T) {
	if os.Getenv("SUBROUTER_TEST_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		for {
			time.Sleep(time.Hour)
		}
	}

	command := exec.Command(os.Args[0], "-test.run=TestTerminateWorkerKillsAfterGracePeriod")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_IGNORE_TERM=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
		}
	})
	worker := &workerGeneration{command: command, done: make(chan struct{})}
	go func() { worker.setWaitError(command.Wait()) }()
	if !bufio.NewScanner(stdout).Scan() {
		t.Fatal("worker helper did not become ready")
	}

	terminateWorker(worker, 10*time.Millisecond)
	if command.ProcessState == nil || command.ProcessState.Success() {
		t.Fatalf("worker was not killed after grace period: %v", command.ProcessState)
	}
}
