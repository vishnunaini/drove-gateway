package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNotifyProxyLifecycleOverUnixSocket(t *testing.T) {
	tempDir, err := os.MkdirTemp("/tmp", "drove-gateway-test-")
	if err != nil {
		t.Fatalf("create temp directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	socketPath := filepath.Join(tempDir, "lifecycle.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer func() { _ = connection.Close() }()

		command, readErr := bufio.NewReader(connection).ReadString('\n')
		if readErr != nil {
			serverDone <- readErr
			return
		}
		if command != "begin\n" {
			serverDone <- &unexpectedLifecycleCommandError{command: command}
			return
		}
		_, writeErr := connection.Write([]byte("ok\n"))
		serverDone <- writeErr
	}()

	if err := notifyProxyLifecycle("begin", socketPath); err != nil {
		t.Fatalf("notifyProxyLifecycle() error = %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server error = %v", err)
	}
}

func TestValidateProxyLifecycleEvent(t *testing.T) {
	for _, event := range []string{"begin", "complete"} {
		if err := validateProxyLifecycleEvent(event); err != nil {
			t.Fatalf("validateProxyLifecycleEvent(%q) error = %v", event, err)
		}
	}
	if err := validateProxyLifecycleEvent("restart"); err == nil {
		t.Fatal("validateProxyLifecycleEvent(restart) expected error")
	}
}

func TestProxyLifecycleServerHandlesBegin(t *testing.T) {
	proxyRestartState.Lock()
	proxyRestartState.inProgress = false
	proxyRestartState.startedAt = time.Time{}
	proxyRestartState.fullReloadRequired = false
	proxyRestartState.Unlock()
	t.Cleanup(func() {
		proxyRestartState.Lock()
		proxyRestartState.inProgress = false
		proxyRestartState.startedAt = time.Time{}
		proxyRestartState.fullReloadRequired = false
		proxyRestartState.Unlock()
	})

	tempDir, err := os.MkdirTemp("/tmp", "drove-gateway-test-")
	if err != nil {
		t.Fatalf("create temp directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })

	listener, err := startProxyLifecycleServer(filepath.Join(tempDir, "lifecycle.sock"))
	if err != nil {
		t.Fatalf("startProxyLifecycleServer() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	if err := notifyProxyLifecycle("begin", listener.Addr().String()); err != nil {
		t.Fatalf("notifyProxyLifecycle() error = %v", err)
	}
	inProgress, _ := getProxyRestartState()
	if !inProgress {
		t.Fatal("proxy restart state was not marked in progress")
	}
}

type unexpectedLifecycleCommandError struct {
	command string
}

func (err *unexpectedLifecycleCommandError) Error() string {
	return "unexpected lifecycle command: " + err.command
}
