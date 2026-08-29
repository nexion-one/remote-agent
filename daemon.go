package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const idleTimeout = 10 * time.Minute

func runDaemon(projectPath string) {
	if err := ensureRunDir(); err != nil {
		fmt.Fprintf(os.Stderr, "error creating run dir: %v\n", err)
		os.Exit(1)
	}

	sock := socketPath(projectPath)
	pid := pidPath(projectPath)

	// Exclusive lock to prevent multiple daemons for the same project.
	// flock is released automatically when the process exits.
	lockPath := pid + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating lock file: %v\n", err)
		os.Exit(1)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintf(os.Stderr, "another daemon is already running for this project\n")
		os.Exit(0)
	}
	// Keep lockFile open: the flock is released when the process exits.

	// Clean up stale socket
	os.Remove(sock)

	listener, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error listening on %s: %v\n", sock, err)
		os.Exit(1)
	}
	os.Chmod(sock, 0600)

	// Write PID file
	os.WriteFile(pid, []byte(strconv.Itoa(os.Getpid())), 0600)

	fmt.Fprintf(os.Stderr, "daemon started: pid=%d socket=%s project=%s\n", os.Getpid(), sock, projectPath)

	// Cleanup on exit
	cleanup := func() {
		listener.Close()
		os.Remove(sock)
		os.Remove(pid)
		os.Remove(lockPath)
	}

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Idle timer
	var idleMu sync.Mutex
	activeConnections := 0
	idleSince := time.Now()
	resetIdle := func() {
		idleMu.Lock()
		idleSince = time.Now()
		idleMu.Unlock()
	}
	connectionOpened := func() {
		idleMu.Lock()
		activeConnections++
		idleMu.Unlock()
	}
	connectionClosed := func() {
		idleMu.Lock()
		if activeConnections > 0 {
			activeConnections--
		}
		if activeConnections == 0 {
			idleSince = time.Now()
		}
		idleMu.Unlock()
	}
	isIdleExpired := func() bool {
		idleMu.Lock()
		defer idleMu.Unlock()
		return activeConnections == 0 && time.Since(idleSince) >= idleTimeout
	}

	// Shutdown channel
	shutdownCh := make(chan struct{})
	var shutdownOnce sync.Once
	doShutdown := func() {
		shutdownOnce.Do(func() {
			close(shutdownCh)
		})
	}

	// Handle signals and idle timeout
	go func() {
		select {
		case <-sigCh:
			fmt.Fprintf(os.Stderr, "daemon received signal, shutting down\n")
		case <-shutdownCh:
			doShutdown()
			cleanup()
			os.Exit(0)
		}
		doShutdown()
		cleanup()
		os.Exit(0)
	}()

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-shutdownCh:
				return
			case <-ticker.C:
				if isIdleExpired() {
					fmt.Fprintf(os.Stderr, "daemon idle timeout, shutting down\n")
					doShutdown()
					cleanup()
					os.Exit(0)
				}
			}
		}
	}()

	// Accept connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-shutdownCh:
				return
			default:
				fmt.Fprintf(os.Stderr, "accept error: %v\n", err)
				continue
			}
		}
		resetIdle()
		connectionOpened()
		go handleConnection(conn, resetIdle, doShutdown, connectionClosed)
	}
}

func handleConnection(conn net.Conn, resetIdle func(), shutdown func(), onClose func()) {
	defer conn.Close()
	defer onClose()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max line

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		resetIdle()

		var req RPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			resp := errorResponse(nil, -32700, "parse error")
			if writeJSON(conn, resp) != nil {
				return
			}
			continue
		}

		// Handle shutdown specially
		if req.Method == "shutdown" {
			resp := dispatch(req)
			writeJSON(conn, resp)
			shutdown()
			return
		}

		resp := dispatchWithConn(req, conn)
		if writeJSON(conn, resp) != nil {
			fmt.Fprintf(os.Stderr, "write error, closing connection\n")
			return
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "connection read error: %v\n", err)
	}
}

func writeJSON(conn net.Conn, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}
