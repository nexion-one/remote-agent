package main

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
)

// All tmux operations execute locally on the remote host via the agent.
// No SSH overhead: direct exec.Command("tmux", ...).

// Nexion tmux session configuration (matches TmuxControlClient.sessionCommands)
var nexionTmuxConfig = []string{
	"set -g status off",
	"set -g mouse on",
	"set -g set-clipboard on",
	"set -g allow-passthrough on",
	"set -g default-terminal tmux-256color",
	"set -s terminal-overrides[999] ,xterm-256color:RGB",
	"set -g extended-keys on",
	"set -g extended-keys-format csi-u",
	"setenv -g COLORTERM truecolor",
	"set -g update-environment[900] NEXION",
	"set -g update-environment[901] NEXION_VERSION",
	"set -g update-environment[902] NEXION_PANE_ID",
	"set -g update-environment[903] NEXION_PROJECT_ID",
	"set -g update-environment[904] NEXION_PROJECT_NAME",
	"set -g update-environment[905] NEXION_PROJECT_PATH",
	"set -g update-environment[906] NEXION_SOCKET",
	"set -g update-environment[907] NEXION_TOKEN_FILE",
	"set -g update-environment[908] NEXION_WORKSPACE_ID",
	"set -g update-environment[909] NEXION_WORKSPACE_PATH",
	"set -g update-environment[910] NEXION_CONNECTION_KEY",
	"unbind -T root MouseDown3Pane",
	"unbind -T prefix <",
	"unbind -T prefix >",
}

// tmux.list_sessions

type TmuxListSessionsParams struct {
	// No params: lists every session on this host.
}

type TmuxSessionResult struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Windows  int    `json:"windows"`
	Attached bool   `json:"attached"`
	Created  int64  `json:"created"`
	Path     string `json:"path"`
}

func handleTmuxListSessions(id interface{}, params json.RawMessage) RPCResponse {
	format := "#{session_id}|#{session_name}|#{session_windows}|#{session_attached}|#{session_created}|#{session_path}"
	out, err := exec.Command("tmux", "list-sessions", "-F", format).Output()
	if err != nil {
		// tmux not running or not installed: empty list, not an error.
		return successResponse(id, map[string]interface{}{
			"sessions": []TmuxSessionResult{},
		})
	}

	var sessions []TmuxSessionResult
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 6)
		if len(parts) < 6 {
			continue
		}
		windows, _ := strconv.Atoi(parts[2])
		created, _ := strconv.ParseInt(parts[4], 10, 64)
		sessions = append(sessions, TmuxSessionResult{
			ID:       parts[0],
			Name:     parts[1],
			Windows:  windows,
			Attached: parts[3] == "1",
			Created:  created,
			Path:     parts[5],
		})
	}

	if sessions == nil {
		sessions = []TmuxSessionResult{}
	}
	return successResponse(id, map[string]interface{}{
		"sessions": sessions,
	})
}

// tmux.create_session

type TmuxCreateSessionParams struct {
	Name      string `json:"name"`
	StartDir  string `json:"start_dir,omitempty"`
	Configure bool   `json:"configure"` // apply Nexion options atomically
}

func handleTmuxCreateSession(id interface{}, params json.RawMessage) RPCResponse {
	var p TmuxCreateSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params")
	}
	if p.Name == "" {
		return errorResponse(id, -32602, "name is required")
	}
	// Validate name: no shell metacharacters.
	for _, c := range p.Name {
		if c == ';' || c == '|' || c == '&' || c == '$' || c == '`' || c == '\'' || c == '"' || c == 0 {
			return errorResponse(id, -32602, "invalid session name")
		}
	}

	// Step 1: Create session (must succeed)
	args := []string{"new-session", "-d", "-s", p.Name}
	if p.StartDir != "" {
		args = append(args, "-c", p.StartDir)
	}

	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errorResponse(id, -32000, "tmux create failed: "+msg)
	}

	// Step 2: Apply config options individually (best-effort, ignore errors
	// from options not supported by older tmux versions)
	if p.Configure {
		applyTmuxConfig()
	}

	return successResponse(id, map[string]interface{}{
		"name": p.Name,
		"ok":   true,
	})
}

// tmux.kill_session

type TmuxKillSessionParams struct {
	Name string `json:"name"`
}

func handleTmuxKillSession(id interface{}, params json.RawMessage) RPCResponse {
	var p TmuxKillSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params")
	}
	if p.Name == "" {
		return errorResponse(id, -32602, "name is required")
	}

	out, err := exec.Command("tmux", "kill-session", "-t", p.Name).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errorResponse(id, -32000, "tmux kill failed: "+msg)
	}

	return successResponse(id, map[string]interface{}{
		"ok": true,
	})
}

// tmux.session_info

type TmuxSessionInfoParams struct {
	Name string `json:"name"`
}

func handleTmuxSessionInfo(id interface{}, params json.RawMessage) RPCResponse {
	var p TmuxSessionInfoParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params")
	}
	if p.Name == "" {
		return errorResponse(id, -32602, "name is required")
	}

	format := "#{session_id}|#{session_name}|#{session_windows}|#{session_attached}|#{session_created}|#{session_path}|#{pane_current_path}"
	out, err := exec.Command("tmux", "display-message", "-t", p.Name, "-p", format).Output()
	if err != nil {
		return errorResponse(id, -32000, "session not found")
	}

	line := strings.TrimSpace(string(out))
	parts := strings.SplitN(line, "|", 7)
	if len(parts) < 7 {
		return errorResponse(id, -32000, "unexpected tmux output")
	}

	windows, _ := strconv.Atoi(parts[2])
	created, _ := strconv.ParseInt(parts[4], 10, 64)

	return successResponse(id, map[string]interface{}{
		"id":                parts[0],
		"name":              parts[1],
		"windows":           windows,
		"attached":          parts[3] == "1",
		"created":           created,
		"path":              parts[5],
		"pane_current_path": parts[6],
	})
}

// tmux.configure

type TmuxConfigureParams struct {
	SessionName string `json:"session_name,omitempty"` // if empty, apply globally
}

func handleTmuxConfigure(id interface{}, params json.RawMessage) RPCResponse {
	var p TmuxConfigureParams
	json.Unmarshal(params, &p)

	applyTmuxConfig()

	return successResponse(id, map[string]interface{}{
		"ok": true,
	})
}

// applyTmuxConfig applies each Nexion option individually.
// Options unsupported by older tmux versions are silently ignored.
func applyTmuxConfig() {
	for _, cmd := range nexionTmuxConfig {
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			continue
		}
		// Each option is a separate tmux command. Errors are ignored.
		exec.Command("tmux", fields...).Run()
	}
}
