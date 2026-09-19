package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultProxyLifecycleSocket = "/run/drove-gateway/proxy-lifecycle.sock"
	proxyLifecycleIOTimeout     = 5 * time.Second
	proxyLifecycleMaxCommand    = 64
)

func notifyProxyLifecycle(event, socketPath string) error {
	if err := validateProxyLifecycleEvent(event); err != nil {
		return err
	}

	connection, err := net.DialTimeout("unix", socketPath, proxyLifecycleIOTimeout)
	if err != nil {
		return fmt.Errorf("connect to proxy lifecycle socket %q: %w", socketPath, err)
	}
	defer func() {
		_ = connection.Close()
	}()

	if err := connection.SetDeadline(time.Now().Add(proxyLifecycleIOTimeout)); err != nil {
		return fmt.Errorf("set proxy lifecycle socket deadline: %w", err)
	}
	if _, err := io.WriteString(connection, event+"\n"); err != nil {
		return fmt.Errorf("send proxy lifecycle %q: %w", event, err)
	}

	response, err := bufio.NewReader(io.LimitReader(connection, proxyLifecycleMaxCommand)).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read proxy lifecycle %q response: %w", event, err)
	}
	if strings.TrimSpace(response) != "ok" {
		return fmt.Errorf("proxy lifecycle %q failed: %s", event, strings.TrimSpace(response))
	}
	return nil
}

func validateProxyLifecycleEvent(event string) error {
	switch event {
	case "begin", "complete":
		return nil
	default:
		return fmt.Errorf("invalid proxy lifecycle event %q: expected begin or complete", event)
	}
}

func startProxyLifecycleServer(socketPath string) (net.Listener, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("proxy lifecycle socket path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, fmt.Errorf("create proxy lifecycle socket directory: %w", err)
	}

	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("proxy lifecycle socket path %q exists and is not a socket", socketPath)
		}
		connection, dialErr := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, fmt.Errorf("proxy lifecycle socket %q is already in use", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, fmt.Errorf("remove stale proxy lifecycle socket %q: %w", socketPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect proxy lifecycle socket %q: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on proxy lifecycle socket %q: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o660); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("set proxy lifecycle socket permissions: %w", err)
	}

	go serveProxyLifecycle(listener)
	logger.WithField("socket", socketPath).Info("Proxy lifecycle socket listening")
	return listener, nil
}

func serveProxyLifecycle(listener net.Listener) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			logger.WithError(err).Error("Proxy lifecycle socket stopped accepting connections")
			return
		}
		go handleProxyLifecycleConnection(connection)
	}
}

func handleProxyLifecycleConnection(connection net.Conn) {
	defer func() {
		_ = connection.Close()
	}()
	if err := connection.SetDeadline(time.Now().Add(proxyLifecycleIOTimeout)); err != nil {
		logger.WithError(err).Warn("Unable to set proxy lifecycle connection deadline")
		return
	}

	reader := bufio.NewReader(io.LimitReader(connection, proxyLifecycleMaxCommand))
	event, err := reader.ReadString('\n')
	if err != nil {
		writeProxyLifecycleResponse(connection, "error: unable to read command")
		return
	}
	event = strings.TrimSpace(event)
	if err := validateProxyLifecycleEvent(event); err != nil {
		writeProxyLifecycleResponse(connection, "error: "+err.Error())
		return
	}

	switch event {
	case "begin":
		proxyRestartStarted("unix_socket")
	case "complete":
		proxyRestartCompleted("unix_socket")
	}
	writeProxyLifecycleResponse(connection, "ok")
}

func writeProxyLifecycleResponse(connection net.Conn, response string) {
	if _, err := io.WriteString(connection, response+"\n"); err != nil {
		logger.WithError(err).Warn("Unable to write proxy lifecycle socket response")
	}
}
