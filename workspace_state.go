package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Workspace state.

// Subscriber represents a connection subscribed to snapshot updates.
type Subscriber struct {
	conn       net.Conn
	showHidden bool
}

type Workspace struct {
	ID       string `json:"workspace_id"`
	RootPath string `json:"root_path"`
	// Snapshot: full recursive tree keyed by relative path
	mu       sync.RWMutex
	snapshot map[string]SnapshotEntry // relative path -> entry
	version  int64
	// Subscription
	subMu       sync.Mutex
	subscribers []Subscriber
	watcherStop chan struct{}
	watcherOnce sync.Once
}

type SnapshotEntry struct {
	Path    string `json:"path"` // relative to workspace root
	Name    string `json:"name"`
	Kind    string `json:"kind"` // "file" or "folder"
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Hidden  bool   `json:"hidden"`
	Symlink bool   `json:"symlink"`
}

var (
	workspaces   = map[string]*Workspace{}
	workspacesMu sync.RWMutex
)

func workspaceID(rootPath string) string {
	h := sha256.Sum256([]byte(rootPath))
	return fmt.Sprintf("w-%x", h[:8])
}

func getOrCreateWorkspace(rootPath string) *Workspace {
	id := workspaceID(rootPath)
	workspacesMu.Lock()
	defer workspacesMu.Unlock()
	if ws, ok := workspaces[id]; ok {
		return ws
	}
	ws := &Workspace{
		ID:       id,
		RootPath: rootPath,
		snapshot: make(map[string]SnapshotEntry),
	}
	workspaces[id] = ws
	return ws
}

func getWorkspace(id string) *Workspace {
	workspacesMu.RLock()
	defer workspacesMu.RUnlock()
	return workspaces[id]
}

// open_workspace

type OpenWorkspaceParams struct {
	Path string `json:"path"`
}

func handleOpenWorkspace(id interface{}, params json.RawMessage) RPCResponse {
	var p OpenWorkspaceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	root := expandHome(p.Path)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return errorResponse(id, -32000, "path is not a valid directory")
	}
	// Canonicalize root path (resolve symlinks) for consistent containment checks
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return errorResponse(id, -32000, "failed to resolve workspace path")
	}

	ws := getOrCreateWorkspace(canonicalRoot)
	return successResponse(id, map[string]interface{}{
		"workspace_id": ws.ID,
		"root_path":    ws.RootPath,
	})
}

// build_snapshot

type BuildSnapshotParams struct {
	WorkspaceID string `json:"workspace_id"`
	ShowHidden  bool   `json:"show_hidden"`
	MaxDepth    int    `json:"max_depth"` // 0 = unlimited
}

func handleBuildSnapshot(id interface{}, params json.RawMessage) RPCResponse {
	var p BuildSnapshotParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	ws := getWorkspace(p.WorkspaceID)
	if ws == nil {
		return errorResponse(id, -32000, "workspace not found: "+p.WorkspaceID)
	}

	maxDepth := p.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 50 // safety cap
	}

	entries := scanWorktree(ws.RootPath, p.ShowHidden, maxDepth)

	// Store in workspace snapshot
	ws.mu.Lock()
	ws.snapshot = make(map[string]SnapshotEntry, len(entries))
	for _, e := range entries {
		ws.snapshot[e.Path] = e
	}
	ws.version++
	version := ws.version
	ws.mu.Unlock()

	return successResponse(id, map[string]interface{}{
		"workspace_id": ws.ID,
		"version":      version,
		"entry_count":  len(entries),
		"entries":      entries,
	})
}

// get_snapshot
// Returns the cached snapshot without re-scanning.

type GetSnapshotParams struct {
	WorkspaceID string `json:"workspace_id"`
}

func handleGetSnapshot(id interface{}, params json.RawMessage) RPCResponse {
	var p GetSnapshotParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	ws := getWorkspace(p.WorkspaceID)
	if ws == nil {
		return errorResponse(id, -32000, "workspace not found: "+p.WorkspaceID)
	}

	ws.mu.RLock()
	entries := make([]SnapshotEntry, 0, len(ws.snapshot))
	for _, e := range ws.snapshot {
		entries = append(entries, e)
	}
	version := ws.version
	ws.mu.RUnlock()

	return successResponse(id, map[string]interface{}{
		"workspace_id": ws.ID,
		"version":      version,
		"entry_count":  len(entries),
		"entries":      entries,
	})
}

// Recursive scanner.

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".DS_Store": true,
	"__pycache__": true, ".Trashes": true, ".Spotlight-V100": true,
	".fseventsd": true, "DerivedData": true, ".build": true,
	"vendor": false, // don't skip vendor by default
}

func scanWorktree(root string, showHidden bool, maxDepth int) []SnapshotEntry {
	var entries []SnapshotEntry

	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors silently
		}

		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}

		name := d.Name()

		// Skip common heavy/irrelevant directories
		if d.IsDir() {
			if skipDirs[name] {
				return filepath.SkipDir
			}
			// Depth check
			depth := strings.Count(rel, string(filepath.Separator)) + 1
			if depth > maxDepth {
				return filepath.SkipDir
			}
		}

		// Hidden files
		if !showHidden && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// macOS metadata
		if name == ".DS_Store" || name == "Icon\r" || strings.HasPrefix(name, "._") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		kind := "file"
		if d.IsDir() {
			kind = "folder"
		}

		entries = append(entries, SnapshotEntry{
			Path:    rel,
			Name:    name,
			Kind:    kind,
			Size:    info.Size(),
			MtimeNs: info.ModTime().UnixNano(),
			Hidden:  strings.HasPrefix(name, "."),
			Symlink: d.Type()&fs.ModeSymlink != 0,
		})

		return nil
	})

	return entries
}

// Workspace-aware wrappers over the path-based methods.

type WorkspacePathParams struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"` // relative to workspace root
	ShowHidden  bool   `json:"show_hidden,omitempty"`
	Offset      int64  `json:"offset,omitempty"`
	Length      int    `json:"length,omitempty"`
}

// resolveWorkspacePath resolves a client-supplied path to a safe absolute path
// within the workspace root. Prevents path traversal, absolute path escape,
// and symlink attacks.
func resolveWorkspacePath(wsID, relPath string) (string, *Workspace, error) {
	ws := getWorkspace(wsID)
	if ws == nil {
		return "", nil, fmt.Errorf("workspace not found")
	}

	// For absolute paths: verify they are within the workspace root
	if filepath.IsAbs(relPath) {
		cleanPath := filepath.Clean(relPath)
		if !isWithinRoot(cleanPath, ws.RootPath) {
			return "", nil, fmt.Errorf("path outside workspace")
		}
		// Resolve symlinks for existing targets
		if realPath, err := filepath.EvalSymlinks(cleanPath); err == nil {
			if !isWithinRoot(realPath, ws.RootPath) {
				return "", nil, fmt.Errorf("path escapes workspace")
			}
			return realPath, ws, nil
		}
		return cleanPath, ws, nil
	}

	// Reject NUL bytes
	if strings.ContainsRune(relPath, 0) {
		return "", nil, fmt.Errorf("invalid path")
	}

	// Clean the relative path and reject any remaining ".." components
	cleaned := filepath.Clean(relPath)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return "", nil, fmt.Errorf("path traversal not allowed")
	}

	fullPath := filepath.Join(ws.RootPath, cleaned)

	// For existing targets: resolve symlinks and verify containment
	if realPath, err := filepath.EvalSymlinks(fullPath); err == nil {
		// Target exists: check the resolved path is still inside the workspace root.
		if !isWithinRoot(realPath, ws.RootPath) {
			return "", nil, fmt.Errorf("path escapes workspace")
		}
		return realPath, ws, nil
	}

	// For non-existing targets (e.g., write_file creating a new file):
	// Resolve the parent directory and verify containment
	parentDir := filepath.Dir(fullPath)
	if realParent, err := filepath.EvalSymlinks(parentDir); err == nil {
		if !isWithinRoot(realParent, ws.RootPath) {
			return "", nil, fmt.Errorf("path escapes workspace")
		}
	}
	// Return the joined path (parent may not exist yet for nested creates)
	return fullPath, ws, nil
}

// isWithinRoot checks that path is equal to or under root.
// Both paths must be absolute and already cleaned/resolved.
func isWithinRoot(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

func updateWorkspaceSnapshotForPath(ws *Workspace, fullPath string, info fs.FileInfo) {
	rel, err := filepath.Rel(ws.RootPath, fullPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}

	name := filepath.Base(fullPath)
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.snapshot[rel] = SnapshotEntry{
		Path:    rel,
		Name:    name,
		Kind:    map[bool]string{true: "folder", false: "file"}[info.IsDir()],
		Size:    info.Size(),
		MtimeNs: info.ModTime().UnixNano(),
		Hidden:  strings.HasPrefix(name, "."),
		Symlink: info.Mode()&fs.ModeSymlink != 0,
	}
	ws.version++
}

// subscribe_snapshot

type SubscribeSnapshotParams struct {
	WorkspaceID string `json:"workspace_id"`
	ShowHidden  bool   `json:"show_hidden"`
}

// handleSubscribeSnapshot registers the connection for push notifications.
// The response is immediate; events arrive as JSON-RPC notifications later.
func handleSubscribeSnapshot(id interface{}, params json.RawMessage, conn net.Conn) RPCResponse {
	var p SubscribeSnapshotParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	ws := getWorkspace(p.WorkspaceID)
	if ws == nil {
		return errorResponse(id, -32000, "workspace not found: "+p.WorkspaceID)
	}

	ws.addSubscriber(conn, p.ShowHidden)
	ws.startWatcher()

	return successResponse(id, map[string]interface{}{
		"workspace_id": ws.ID,
		"subscribed":   true,
	})
}

func (ws *Workspace) addSubscriber(conn net.Conn, showHidden bool) {
	ws.subMu.Lock()
	defer ws.subMu.Unlock()
	ws.subscribers = append(ws.subscribers, Subscriber{conn: conn, showHidden: showHidden})
}

func (ws *Workspace) removeSubscriber(conn net.Conn) {
	ws.subMu.Lock()
	defer ws.subMu.Unlock()
	filtered := ws.subscribers[:0]
	for _, s := range ws.subscribers {
		if s.conn != conn {
			filtered = append(filtered, s)
		}
	}
	ws.subscribers = filtered
}

// startWatcher starts the background goroutine that polls for changes
// and pushes snapshot_updated events to subscribers. Runs once per workspace.
func (ws *Workspace) startWatcher() {
	ws.watcherOnce.Do(func() {
		ws.watcherStop = make(chan struct{})
		go ws.watchLoop()
	})
}

func (ws *Workspace) watchLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ws.watcherStop:
			return
		case <-ticker.C:
			ws.subMu.Lock()
			subs := make([]Subscriber, len(ws.subscribers))
			copy(subs, ws.subscribers)
			ws.subMu.Unlock()

			if len(subs) == 0 {
				continue
			}

			// Determine showHidden from first subscriber (all should be the same in practice)
			showHidden := false
			if len(subs) > 0 {
				showHidden = subs[0].showHidden
			}

			// Rescan worktree
			entries := scanWorktree(ws.RootPath, showHidden, 50)

			// Check if anything changed by comparing entry count and version
			ws.mu.Lock()
			changed := len(entries) != len(ws.snapshot)
			if !changed {
				// Quick check: compare a sample of mtimes
				for _, e := range entries {
					if old, ok := ws.snapshot[e.Path]; ok {
						if old.MtimeNs != e.MtimeNs || old.Size != e.Size {
							changed = true
							break
						}
					} else {
						changed = true
						break
					}
				}
			}

			if !changed {
				ws.mu.Unlock()
				continue
			}

			// Update snapshot
			ws.snapshot = make(map[string]SnapshotEntry, len(entries))
			for _, e := range entries {
				ws.snapshot[e.Path] = e
			}
			ws.version++
			version := ws.version
			ws.mu.Unlock()

			// Push event to all subscribers
			event := RPCResponse{
				JSONRPC: "2.0",
				Result: map[string]interface{}{
					"event":        "snapshot_updated",
					"workspace_id": ws.ID,
					"version":      version,
					"entry_count":  len(entries),
					"entries":      entries,
				},
			}

			ws.subMu.Lock()
			alive := ws.subscribers[:0]
			for _, sub := range ws.subscribers {
				if err := writeJSON(sub.conn, event); err != nil {
					// Subscriber disconnected, remove
					continue
				}
				alive = append(alive, sub)
			}
			ws.subscribers = alive
			ws.subMu.Unlock()
		}
	}
}
