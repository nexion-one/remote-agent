package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func runProxy(projectPath string) {
	if err := ensureRunDir(); err != nil {
		fmt.Fprintf(os.Stderr, "proxy: error creating run dir: %v\n", err)
		os.Exit(1)
	}

	sock := socketPath(projectPath)

	// Check if daemon is already running
	if !isDaemonAlive(sock, projectPath) {
		if err := startDaemon(projectPath); err != nil {
			fmt.Fprintf(os.Stderr, "proxy: failed to start daemon: %v\n", err)
			os.Exit(1)
		}

		// Wait for socket to appear
		if err := waitForSocket(sock, 10*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "proxy: daemon socket not ready: %v\n", err)
			os.Exit(1)
		}
	}

	// Connect to daemon socket
	conn, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy: failed to connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Bridge stdio <-> socket
	done := make(chan struct{}, 2)

	// stdin -> socket
	go func() {
		io.Copy(conn, os.Stdin)
		done <- struct{}{}
	}()

	// socket -> stdout
	go func() {
		io.Copy(os.Stdout, conn)
		done <- struct{}{}
	}()

	// Wait for either direction to close
	<-done
}

func isDaemonAlive(sock, projectPath string) bool {
	// Check PID file
	pid := pidPath(projectPath)
	data, err := os.ReadFile(pid)
	if err != nil {
		return false
	}

	pidNum, err := strconv.Atoi(string(data))
	if err != nil {
		return false
	}

	// Check if process is alive
	proc, err := os.FindProcess(pidNum)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}

	// Try to connect
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func startDaemon(projectPath string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find own executable: %w", err)
	}

	logFile := logPath(projectPath)
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("cannot open log file: %w", err)
	}

	cmd := exec.Command(self, "daemon", "--project", projectPath)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // detach from controlling terminal
	}

	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("failed to start daemon process: %w", err)
	}

	// Detach without waiting.
	cmd.Process.Release()
	lf.Close()

	fmt.Fprintf(os.Stderr, "proxy: started daemon pid=%d\n", cmd.Process.Pid)
	return nil
}

func waitForSocket(sock string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for socket %s", sock)
}
