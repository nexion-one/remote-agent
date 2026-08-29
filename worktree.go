package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Git worktree support.
//
// The Mac side needs the same operations it runs locally, but a remote
// project's checkout lives here. The agent exposes a closed set of verbs
// rather than an "run this git command" escape hatch, in keeping with
// every other git verb in workspace.go.
//
// One rule shapes all of this: a worktree lives OUTSIDE the workspace it
// belongs to, so resolveWorkspacePath (which rejects anything outside a
// workspace root) cannot be used to validate the destination. The
// destination is checked separately, against a root the client passes in,
// and once the worktree exists the client opens it as a workspace of its
// own so everything afterwards is sandboxed normally.

type GitWorktreeListParams struct {
	WorkspaceID string `json:"workspace_id"`
}

type WorktreeEntry struct {
	Path       string `json:"path"`
	Branch     string `json:"branch,omitempty"`
	Head       string `json:"head,omitempty"`
	Bare       bool   `json:"bare,omitempty"`
	Detached   bool   `json:"detached,omitempty"`
	Prunable   bool   `json:"prunable,omitempty"`
	LockReason string `json:"lock_reason,omitempty"`
	Locked     bool   `json:"locked,omitempty"`
}

type GitWorktreeAddParams struct {
	WorkspaceID string `json:"workspace_id"`
	// Absolute (or ~-prefixed) destination. Must sit under AllowedRoot.
	Path string `json:"path"`
	// Root the destination has to live under, so a caller cannot write
	// a checkout anywhere on the filesystem.
	AllowedRoot       string `json:"allowed_root"`
	Branch            string `json:"branch"`
	BaseRef           string `json:"base_ref,omitempty"`
	UseExistingBranch bool   `json:"use_existing_branch,omitempty"`
}

type GitWorktreeRemoveParams struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"`
	AllowedRoot string `json:"allowed_root"`
	Force       bool   `json:"force,omitempty"`
}

type GitWorktreePruneParams struct {
	WorkspaceID string `json:"workspace_id"`
}

type GitCheckIgnoreParams struct {
	WorkspaceID string   `json:"workspace_id"`
	Paths       []string `json:"paths"`
}

type FsSymlinkParams struct {
	WorkspaceID string `json:"workspace_id"`
	// Repo-root-relative source, resolved inside the workspace.
	Source string `json:"source"`
	// Absolute destination, validated against AllowedRoot.
	Destination string `json:"destination"`
	AllowedRoot string `json:"allowed_root"`
}

// withinRoot reports whether path sits inside root after both have been
// cleaned and ~-expanded. Symlinks are resolved when the target exists so
// a link cannot be used to escape.
func withinRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	r := filepath.Clean(expandHome(root))
	p := filepath.Clean(expandHome(path))
	if resolved, err := filepath.EvalSymlinks(r); err == nil {
		r = resolved
	}
	// The destination usually does not exist yet, so resolve its parent.
	parent := filepath.Dir(p)
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		p = filepath.Join(resolved, filepath.Base(p))
	}
	if p == r {
		return false
	}
	return strings.HasPrefix(p, r+string(filepath.Separator))
}

func parseWorktreeList(out string) []WorktreeEntry {
	var entries []WorktreeEntry
	var current *WorktreeEntry
	flush := func() {
		if current != nil && current.Path != "" {
			entries = append(entries, *current)
		}
		current = nil
	}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current = &WorktreeEntry{Path: strings.TrimPrefix(line, "worktree ")}
		case current == nil:
			continue
		case strings.HasPrefix(line, "HEAD "):
			current.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			current.Branch = strings.TrimPrefix(ref, "refs/heads/")
		case line == "bare":
			current.Bare = true
		case line == "detached":
			current.Detached = true
		case line == "locked":
			current.Locked = true
		case strings.HasPrefix(line, "locked "):
			current.Locked = true
			current.LockReason = strings.TrimPrefix(line, "locked ")
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			current.Prunable = true
		}
	}
	flush()
	return entries
}

func handleGitWorktreeList(id interface{}, params json.RawMessage) RPCResponse {
	var p GitWorktreeListParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	root, _, errResp, ok := gitWorkspaceRoot(p.WorkspaceID)
	if !ok {
		errResp.ID = id
		return errResp
	}
	out, err := runGitInWorkspace(root, "worktree", "list", "--porcelain")
	if err != nil {
		return errorResponse(id, -32000, "git worktree list failed: "+trimGitError(out, err))
	}
	return successResponse(id, map[string]interface{}{
		"worktrees": parseWorktreeList(string(out)),
	})
}

func handleGitWorktreeAdd(id interface{}, params json.RawMessage) RPCResponse {
	var p GitWorktreeAddParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	root, _, errResp, ok := gitWorkspaceRoot(p.WorkspaceID)
	if !ok {
		errResp.ID = id
		return errResp
	}
	if p.Branch == "" {
		return errorResponse(id, -32602, "branch is required")
	}
	if strings.ContainsAny(p.Branch, " \t\n~^:?*[\\") || strings.HasPrefix(p.Branch, "-") {
		return errorResponse(id, -32602, "invalid branch name")
	}
	dest := filepath.Clean(expandHome(p.Path))
	if !withinRoot(p.AllowedRoot, p.Path) {
		return errorResponse(id, -32602, "path must live under "+p.AllowedRoot)
	}
	if _, err := os.Stat(dest); err == nil {
		return errorResponse(id, -32000, dest+" already exists")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return errorResponse(id, -32000, "cannot create "+filepath.Dir(dest)+": "+err.Error())
	}

	args := []string{"worktree", "add"}
	if _, err := os.Stat(filepath.Join(root, ".gitmodules")); err == nil {
		args = append(args, "--recurse-submodules")
	}
	if p.UseExistingBranch {
		args = append(args, dest, p.Branch)
	} else {
		base := p.BaseRef
		if base == "" {
			base = "HEAD"
		}
		// --no-track on purpose: a branch tracking origin/main would make
		// a plain `git pull` inside the task merge main into it.
		args = append(args, "--no-track", "-b", p.Branch, dest, base)
	}
	out, err := runGitInWorkspace(root, args...)
	if err != nil {
		return errorResponse(id, -32000, "git worktree add failed: "+trimGitError(out, err))
	}
	listOut, _ := runGitInWorkspace(root, "worktree", "list", "--porcelain")
	return successResponse(id, map[string]interface{}{
		"path":      dest,
		"branch":    p.Branch,
		"worktrees": parseWorktreeList(string(listOut)),
	})
}

func handleGitWorktreeRemove(id interface{}, params json.RawMessage) RPCResponse {
	var p GitWorktreeRemoveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	root, _, errResp, ok := gitWorkspaceRoot(p.WorkspaceID)
	if !ok {
		errResp.ID = id
		return errResp
	}
	if !withinRoot(p.AllowedRoot, p.Path) {
		return errorResponse(id, -32602, "path must live under "+p.AllowedRoot)
	}
	dest := filepath.Clean(expandHome(p.Path))

	if !p.Force {
		statusOut, err := runGitInWorkspace(dest, "status", "--porcelain", "-uall")
		if err == nil {
			changes := 0
			for _, line := range strings.Split(strings.TrimSpace(string(statusOut)), "\n") {
				if strings.TrimSpace(line) != "" {
					changes++
				}
			}
			if changes > 0 {
				return errorResponse(id, -32000, "worktree has uncommitted changes")
			}
		}
	}

	args := []string{"worktree", "remove"}
	if p.Force {
		args = append(args, "--force")
	}
	args = append(args, dest)
	out, err := runGitInWorkspace(root, args...)
	if err != nil {
		lowered := strings.ToLower(string(out))
		// git genuinely wants the doubled flag when a submodule holds
		// modified tracked files.
		if p.Force && strings.Contains(lowered, "submodule") && strings.Contains(lowered, "modified") {
			out, err = runGitInWorkspace(root, "worktree", "remove", "--force", "--force", dest)
		}
		if err != nil {
			return errorResponse(id, -32000, "git worktree remove failed: "+trimGitError(out, err))
		}
	}
	_, _ = runGitInWorkspace(root, "worktree", "prune")
	return successResponse(id, map[string]interface{}{"removed": dest})
}

func handleGitWorktreePrune(id interface{}, params json.RawMessage) RPCResponse {
	var p GitWorktreePruneParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	root, _, errResp, ok := gitWorkspaceRoot(p.WorkspaceID)
	if !ok {
		errResp.ID = id
		return errResp
	}
	out, err := runGitInWorkspace(root, "worktree", "prune")
	if err != nil {
		return errorResponse(id, -32000, "git worktree prune failed: "+trimGitError(out, err))
	}
	return successResponse(id, map[string]interface{}{"ok": true})
}

// handleGitCheckIgnore answers, for each path, whether git ignores it and
// whether git tracks it. The Mac needs both to decide if a path may be
// symlinked into a worktree: a symlink over a TRACKED path registers as a
// type change and dirties the checkout immediately.
func handleGitCheckIgnore(id interface{}, params json.RawMessage) RPCResponse {
	var p GitCheckIgnoreParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	root, _, errResp, ok := gitWorkspaceRoot(p.WorkspaceID)
	if !ok {
		errResp.ID = id
		return errResp
	}
	results := make([]map[string]interface{}, 0, len(p.Paths))
	for _, path := range p.Paths {
		if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
			results = append(results, map[string]interface{}{
				"path": path, "ignored": false, "tracked": false, "invalid": true,
			})
			continue
		}
		_, trackedErr := runGitInWorkspace(root, "ls-files", "--error-unmatch", "--", path)
		_, ignoreErr := runGitInWorkspace(root, "check-ignore", "-q", "--", path)
		_, statErr := os.Lstat(filepath.Join(root, path))
		results = append(results, map[string]interface{}{
			"path":    path,
			"tracked": trackedErr == nil,
			"ignored": ignoreErr == nil,
			"exists":  statErr == nil,
		})
	}
	return successResponse(id, map[string]interface{}{"paths": results})
}

// handleFsSymlink links a workspace-relative source to an absolute
// destination inside AllowedRoot. This is what puts .env and node_modules
// into a fresh remote worktree: git only materializes tracked files, so
// everything ignored is missing until it is linked.
func handleFsSymlink(id interface{}, params json.RawMessage) RPCResponse {
	var p FsSymlinkParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	source, _, err := resolveWorkspacePath(p.WorkspaceID, p.Source)
	if err != nil {
		return errorResponse(id, -32602, err.Error())
	}
	if _, statErr := os.Lstat(source); statErr != nil {
		return errorResponse(id, -32000, "source does not exist: "+p.Source)
	}
	if !withinRoot(p.AllowedRoot, p.Destination) {
		return errorResponse(id, -32602, "destination must live under "+p.AllowedRoot)
	}
	dest := filepath.Clean(expandHome(p.Destination))
	if _, statErr := os.Lstat(dest); statErr == nil {
		return successResponse(id, map[string]interface{}{"skipped": "destination exists"})
	}
	if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
		return errorResponse(id, -32000, mkErr.Error())
	}
	if linkErr := os.Symlink(source, dest); linkErr != nil {
		return errorResponse(id, -32000, linkErr.Error())
	}
	return successResponse(id, map[string]interface{}{"linked": dest, "target": source})
}

func trimGitError(out []byte, err error) string {
	msg := strings.TrimSpace(string(out))
	if msg == "" && err != nil {
		msg = err.Error()
	}
	return msg
}
