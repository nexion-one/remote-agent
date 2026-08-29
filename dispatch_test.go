package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// The verbs, through the router the connection actually uses.
//
// Arguments that reach `git` and `tmux` are validated against a narrow
// character set rather than escaped, which is the stronger choice and the one
// worth proving: these check that the refusals really happen, and that a
// legitimate argument is not caught with them.

func call(t *testing.T, method string, params map[string]interface{}) RPCResponse {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	id := json.RawMessage(`1`)
	return dispatch(RPCRequest{Method: method, Params: raw, ID: &id})
}

func expectInvalidParams(t *testing.T, what string, response RPCResponse) {
	t.Helper()
	if response.Error == nil {
		t.Errorf("%s was accepted", what)
		return
	}
	if response.Error.Code != -32602 {
		t.Errorf("%s produced code %d, want -32602", what, response.Error.Code)
	}
}

func TestAnUnknownMethodIsMethodNotFound(t *testing.T) {
	response := call(t, "no.such.verb", nil)
	if response.Error == nil {
		t.Fatal("an unknown method was accepted")
	}
	if response.Error.Code != -32601 {
		t.Fatalf("code %d, want -32601", response.Error.Code)
	}
}

func TestHelloAnswersWithAVersionAndCapabilities(t *testing.T) {
	response := call(t, "hello", map[string]interface{}{"client_version": "0.1.0"})
	if response.Error != nil {
		t.Fatalf("hello failed: %v", response.Error)
	}
	result, ok := response.Result.(HelloResult)
	if !ok {
		t.Fatalf("hello returned %T, want HelloResult", response.Result)
	}
	if result.AgentVersion != AgentVersion {
		t.Errorf("agent version %q, want %q", result.AgentVersion, AgentVersion)
	}
	if result.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocol version %d, want %d", result.ProtocolVersion, ProtocolVersion)
	}
	if result.Capabilities.SearchBackend != "rg" && result.Capabilities.SearchBackend != "grep" {
		t.Errorf("search backend %q, want rg or grep", result.Capabilities.SearchBackend)
	}
}

func TestAnOlderClientIsToldToUpdate(t *testing.T) {
	response := call(t, "hello", map[string]interface{}{"client_version": "0.0.1"})
	result := response.Result.(HelloResult)
	if result.UpdateAvailable == nil {
		t.Fatalf("a client on 0.0.1 was not told that %s exists", AgentVersion)
	}
}

func TestACurrentClientIsLeftAlone(t *testing.T) {
	response := call(t, "hello", map[string]interface{}{"client_version": AgentVersion})
	if result := response.Result.(HelloResult); result.UpdateAvailable != nil {
		t.Fatal("a client on the same version was told to update")
	}
}

// MARK: arguments that reach git

func TestGitRevRejectsShellMetacharacters(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	for _, rev := range []string{
		"HEAD; rm -rf /",
		"HEAD && whoami",
		"HEAD | tee /tmp/x",
		"$(whoami)",
		"`whoami`",
		"HEAD\x00",
		"--upload-pack=evil",
	} {
		expectInvalidParams(t, fmt.Sprintf("rev %q", rev), call(t, "git_read_blob", map[string]interface{}{
			"workspace_id": ws.ID,
			"rev":          rev,
			"path":         "src/main.go",
		}))
	}
}

func TestGitRevAcceptsTheSpellingsPeopleUse(t *testing.T) {
	// A refusal that catches "HEAD~2" or "origin/main" would be a bug of its
	// own, so the accepted set is pinned too. These reach git and fail there
	// for other reasons; what matters is that they are not refused as
	// malformed.
	ws, _ := newTestWorkspace(t)
	for _, rev := range []string{"HEAD", "HEAD~2", "HEAD^", "main", "origin/main", "v1.2.3", "a1b2c3d"} {
		response := call(t, "git_read_blob", map[string]interface{}{
			"workspace_id": ws.ID,
			"rev":          rev,
			"path":         "src/main.go",
		})
		if response.Error != nil && response.Error.Code == -32602 {
			t.Errorf("rev %q was refused as malformed", rev)
		}
	}
}

func TestGitPathRejectsTraversalAndAbsolutePaths(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	for _, path := range []string{"../outside", "/etc/passwd", "src/../../etc/passwd", "src\x00"} {
		expectInvalidParams(t, fmt.Sprintf("path %q", path), call(t, "git_read_blob", map[string]interface{}{
			"workspace_id": ws.ID,
			"rev":          "HEAD",
			"path":         path,
		}))
	}
}

// MARK: arguments that reach tmux

func TestTmuxSessionNameRejectsShellMetacharacters(t *testing.T) {
	for _, name := range []string{
		"work; rm -rf /",
		"work && id",
		"work | cat",
		"$(id)",
		"`id`",
		`work"quote`,
		"work'quote",
	} {
		expectInvalidParams(t, fmt.Sprintf("session name %q", name), call(t, "tmux.create_session", map[string]interface{}{
			"name": name,
			"path": "/tmp",
		}))
	}
}

func TestTmuxSessionNameAcceptsOrdinaryNames(t *testing.T) {
	for _, name := range []string{"work", "my-project", "project_2", "nexion.main"} {
		response := call(t, "tmux.create_session", map[string]interface{}{
			"name": name,
			"path": "/tmp",
		})
		if response.Error != nil && response.Error.Code == -32602 {
			t.Errorf("session name %q was refused as malformed", name)
		}
	}
}
