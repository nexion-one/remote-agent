package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

func nexionDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".nexion")
}

func runDir() string {
	return filepath.Join(nexionDir(), "run")
}

func projectHash(projectPath string) string {
	h := sha256.Sum256([]byte(projectPath))
	return fmt.Sprintf("%x", h[:8]) // 16 hex chars
}

func socketPath(projectPath string) string {
	return filepath.Join(runDir(), fmt.Sprintf("agent-%s.sock", projectHash(projectPath)))
}

func pidPath(projectPath string) string {
	return filepath.Join(runDir(), fmt.Sprintf("agent-%s.pid", projectHash(projectPath)))
}

func logPath(projectPath string) string {
	return filepath.Join(runDir(), fmt.Sprintf("agent-%s.log", projectHash(projectPath)))
}

func ensureRunDir() error {
	return os.MkdirAll(runDir(), 0700)
}
