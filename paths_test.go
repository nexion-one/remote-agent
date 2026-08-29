package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// One daemon per project is enforced by naming its socket after the project,
// so two projects must never land on the same name and one project must never
// change name between runs.

func TestProjectHashIsStable(t *testing.T) {
	first := projectHash("/Users/someone/code/thing")
	second := projectHash("/Users/someone/code/thing")
	if first != second {
		t.Fatalf("same path hashed two ways: %q then %q", first, second)
	}
}

func TestProjectHashSeparatesNeighbours(t *testing.T) {
	paths := []string{
		"/Users/someone/code/thing",
		"/Users/someone/code/thing2",
		"/Users/someone/code/thing/",
		"/Users/someone/code/Thing",
		"/users/someone/code/thing",
	}
	seen := map[string]string{}
	for _, path := range paths {
		hash := projectHash(path)
		if other, clash := seen[hash]; clash {
			t.Fatalf("%q and %q share the hash %q", other, path, hash)
		}
		seen[hash] = path
	}
}

func TestProjectHashIsSixteenHexCharacters(t *testing.T) {
	hash := projectHash("/tmp/whatever")
	if len(hash) != 16 {
		t.Fatalf("hash is %d characters, want 16: %q", len(hash), hash)
	}
	for _, c := range hash {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("hash has a non-hex character %q: %q", c, hash)
		}
	}
}

func TestRuntimeFilesShareTheProjectAndDifferBySuffix(t *testing.T) {
	const project = "/Users/someone/code/thing"
	sock, pid, log := socketPath(project), pidPath(project), logPath(project)

	for _, path := range []string{sock, pid, log} {
		if filepath.Dir(path) != runDir() {
			t.Fatalf("%q is not in the run directory %q", path, runDir())
		}
		if !strings.Contains(filepath.Base(path), projectHash(project)) {
			t.Fatalf("%q does not carry the project hash", path)
		}
	}
	if sock == pid || sock == log || pid == log {
		t.Fatalf("two runtime files collide: %q %q %q", sock, pid, log)
	}
	if filepath.Ext(sock) != ".sock" || filepath.Ext(pid) != ".pid" || filepath.Ext(log) != ".log" {
		t.Fatalf("unexpected extensions: %q %q %q", sock, pid, log)
	}
}
