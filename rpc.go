package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// JSON-RPC 2.0 types

type RPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type RPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func successResponse(id interface{}, result interface{}) RPCResponse {
	return RPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id interface{}, code int, msg string) RPCResponse {
	return RPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
}

// Method handlers

type HelloParams struct {
	ClientVersion string `json:"client_version"`
}

type HelloResult struct {
	AgentVersion    string       `json:"agent_version"`
	ProtocolVersion int          `json:"protocol_version"`
	Capabilities    Capabilities `json:"capabilities"`
	UpdateAvailable *UpdateInfo  `json:"update_available,omitempty"`
}

type UpdateInfo struct {
	AgentVersion string `json:"agent_version"`
	Message      string `json:"message"`
}

type Capabilities struct {
	WatchMode     string `json:"watch_mode"`
	SearchBackend string `json:"search_backend"`
	WriteFile     bool   `json:"write_file"`
	Reconnect     bool   `json:"reconnect"`
	GitSnapshot   bool   `json:"git_snapshot"`
	GitReadBlob   bool   `json:"git_read_blob"`
	// Whether this agent knows the git_worktree_* / fs_symlink verbs.
	// The Mac disables worktree tasks on a host that answers false,
	// with a message, instead of failing halfway through creating one.
	GitWorktree bool `json:"git_worktree"`
}

func handleHello(id interface{}, params json.RawMessage) RPCResponse {
	var p HelloParams
	if len(params) > 0 {
		json.Unmarshal(params, &p)
	}

	// Detect capabilities
	watchMode := "polling" // default, upgrade to "native" later
	searchBackend := "grep"
	if _, err := lookPath("rg"); err == nil {
		searchBackend = "rg"
	}

	result := HelloResult{
		AgentVersion:    AgentVersion,
		ProtocolVersion: ProtocolVersion,
		Capabilities: Capabilities{
			WatchMode:     watchMode,
			SearchBackend: searchBackend,
			WriteFile:     true,
			Reconnect:     true,
			GitSnapshot:   true,
			GitReadBlob:   true,
			GitWorktree:   true,
		},
	}

	// If client is older than this agent, suggest updating.
	if p.ClientVersion != "" && isVersionOlder(p.ClientVersion, AgentVersion) {
		result.UpdateAvailable = &UpdateInfo{
			AgentVersion: AgentVersion,
			Message:      fmt.Sprintf("Nexion %s is available. Update the app to get the latest features.", AgentVersion),
		}
	}

	return successResponse(id, result)
}

// isVersionOlder returns true if a is strictly older than b (semver comparison).
func isVersionOlder(a, b string) bool {
	pa := strings.SplitN(a, ".", 3)
	pb := strings.SplitN(b, ".", 3)
	for i := 0; i < 3; i++ {
		va, vb := 0, 0
		if i < len(pa) {
			va, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			vb, _ = strconv.Atoi(pb[i])
		}
		if va < vb {
			return true
		}
		if va > vb {
			return false
		}
	}
	return false
}

func handlePing(id interface{}, _ json.RawMessage) RPCResponse {
	return successResponse(id, map[string]interface{}{
		"time": time.Now().Unix(),
	})
}

// Router

// dispatchWithConn handles methods that need access to the connection (for subscriptions).
func dispatchWithConn(req RPCRequest, conn net.Conn) RPCResponse {
	var id interface{}
	if req.ID != nil {
		json.Unmarshal(*req.ID, &id)
	}
	switch req.Method {
	case "subscribe_snapshot":
		return handleSubscribeSnapshot(id, req.Params, conn)
	default:
		return dispatch(req)
	}
}

func dispatch(req RPCRequest) RPCResponse {
	var id interface{}
	if req.ID != nil {
		json.Unmarshal(*req.ID, &id)
	}

	switch req.Method {
	case "hello":
		return handleHello(id, req.Params)
	case "ping":
		return handlePing(id, req.Params)
	case "open_workspace":
		return handleOpenWorkspace(id, req.Params)
	case "build_snapshot":
		return handleBuildSnapshot(id, req.Params)
	case "get_snapshot":
		return handleGetSnapshot(id, req.Params)
	case "list_dir":
		return handleListDir(id, req.Params)
	case "read_file":
		return handleReadFile(id, req.Params)
	case "write_file":
		return handleWriteFile(id, req.Params)
	case "stat":
		return handleStat(id, req.Params)
	case "git_snapshot":
		return handleGitSnapshot(id, req.Params)
	case "git_ai_context":
		return handleGitAIContext(id, req.Params)
	case "git_branch_list":
		return handleGitBranchList(id, req.Params)
	case "git_branch_create":
		return handleGitBranchCreate(id, req.Params)
	case "git_branch_switch":
		return handleGitBranchSwitch(id, req.Params)
	case "git_branch_checkout_remote":
		return handleGitBranchCheckoutRemote(id, req.Params)
	case "git_commit":
		return handleGitCommit(id, req.Params)
	case "git_push":
		return handleGitPush(id, req.Params)
	case "git_worktree_list":
		return handleGitWorktreeList(req.ID, req.Params)
	case "git_worktree_add":
		return handleGitWorktreeAdd(req.ID, req.Params)
	case "git_worktree_remove":
		return handleGitWorktreeRemove(req.ID, req.Params)
	case "git_worktree_prune":
		return handleGitWorktreePrune(req.ID, req.Params)
	case "git_check_ignore":
		return handleGitCheckIgnore(req.ID, req.Params)
	case "fs_symlink":
		return handleFsSymlink(req.ID, req.Params)
	case "git_read_blob":
		return handleGitReadBlob(id, req.Params)
	case "delete_file":
		return handleDeleteFile(id, req.Params)
	case "search":
		return handleSearch(id, req.Params)
	case "cancel_search":
		return handleCancelSearch(id, req.Params)
	// tmux control plane
	case "tmux.list_sessions":
		return handleTmuxListSessions(id, req.Params)
	case "tmux.create_session":
		return handleTmuxCreateSession(id, req.Params)
	case "tmux.kill_session":
		return handleTmuxKillSession(id, req.Params)
	case "tmux.session_info":
		return handleTmuxSessionInfo(id, req.Params)
	case "tmux.configure":
		return handleTmuxConfigure(id, req.Params)
	case "shutdown":
		return successResponse(id, map[string]string{"status": "shutting_down"})
	default:
		return errorResponse(id, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

// lookPath is a minimal check for an executable in PATH
func lookPath(name string) (string, error) {
	// Use os/exec but avoid importing it just for this
	// Simple PATH search
	for _, dir := range filepath.SplitList(envPATH()) {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in PATH", name)
}

func envPATH() string {
	if p := os.Getenv("PATH"); p != "" {
		return p
	}
	return "/usr/local/bin:/usr/bin:/bin"
}
