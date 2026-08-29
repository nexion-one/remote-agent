package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// list_dir
// Accepts either workspace_id + relative path, or absolute path (legacy compat).

type ListDirParams struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"` // relative to workspace root
	ShowHidden  bool   `json:"show_hidden"`
}

type DirEntry struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"` // "file" or "folder"
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime_ns"`
	Hidden  bool   `json:"hidden"`
	Symlink bool   `json:"symlink"`
}

func handleListDir(id interface{}, params json.RawMessage) RPCResponse {
	var p ListDirParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}
	path, _, err := resolveWorkspacePath(p.WorkspaceID, p.Path)
	if err != nil {
		return errorResponse(id, -32000, err.Error())
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return errorResponse(id, -32000, "failed to read directory: "+err.Error())
	}

	var result []DirEntry
	for _, e := range entries {
		name := e.Name()
		if !p.ShowHidden && strings.HasPrefix(name, ".") {
			continue
		}
		// Skip macOS metadata
		if name == ".DS_Store" || name == ".Trashes" || name == "Icon\r" || strings.HasPrefix(name, "._") {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		kind := "file"
		if e.IsDir() {
			kind = "folder"
		}

		result = append(result, DirEntry{
			Name:    name,
			Kind:    kind,
			Size:    info.Size(),
			MtimeNs: info.ModTime().UnixNano(),
			Hidden:  strings.HasPrefix(name, "."),
			Symlink: e.Type()&fs.ModeSymlink != 0,
		})
	}

	return successResponse(id, map[string]interface{}{
		"path":    p.Path,
		"entries": result,
	})
}

// read_file

type ReadFileParams struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"` // relative to workspace root
	Offset      int64  `json:"offset"`
	Length      int    `json:"length"` // 0 = entire file
}

func handleReadFile(id interface{}, params json.RawMessage) RPCResponse {
	var p ReadFileParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}
	path, _, err := resolveWorkspacePath(p.WorkspaceID, p.Path)
	if err != nil {
		return errorResponse(id, -32000, err.Error())
	}

	info, err := os.Stat(path)
	if err != nil {
		return errorResponse(id, -32000, "stat failed: "+err.Error())
	}

	maxBytes := p.Length
	if maxBytes <= 0 {
		maxBytes = 2 * 1024 * 1024 // 2MB default
	}
	if maxBytes > 10*1024*1024 {
		maxBytes = 10 * 1024 * 1024 // 10MB cap
	}

	f, err := os.Open(path)
	if err != nil {
		return errorResponse(id, -32000, "open failed: "+err.Error())
	}
	defer f.Close()

	if p.Offset > 0 {
		f.Seek(p.Offset, 0)
	}

	buf := make([]byte, maxBytes)
	n, _ := f.Read(buf)
	buf = buf[:n]

	// Detect binary (null bytes in first 4KB)
	checkLen := 4096
	if len(buf) < checkLen {
		checkLen = len(buf)
	}
	isBinary := false
	for _, b := range buf[:checkLen] {
		if b == 0 {
			isBinary = true
			break
		}
	}

	encoding := "utf8"
	var content string
	if isBinary {
		encoding = "base64"
		content = base64.StdEncoding.EncodeToString(buf)
	} else if utf8.Valid(buf) {
		content = string(buf)
	} else {
		// Not valid UTF-8, use base64
		encoding = "base64"
		content = base64.StdEncoding.EncodeToString(buf)
	}

	return successResponse(id, map[string]interface{}{
		"path":     p.Path,
		"encoding": encoding,
		"content":  content,
		"size":     info.Size(),
		"mtime_ns": info.ModTime().UnixNano(),
	})
}

// write_file

type WriteFileParams struct {
	WorkspaceID     string `json:"workspace_id"`
	Path            string `json:"path"` // relative to workspace root
	ContentBase64   string `json:"content_base64"`
	ExpectedMtimeNs int64  `json:"expected_mtime_ns,omitempty"`
}

func handleWriteFile(id interface{}, params json.RawMessage) RPCResponse {
	var p WriteFileParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}
	if p.Path == "" {
		return errorResponse(id, -32602, "path is required")
	}

	path, ws, err := resolveWorkspacePath(p.WorkspaceID, p.Path)
	if err != nil {
		return errorResponse(id, -32000, err.Error())
	}

	content, err := base64.StdEncoding.DecodeString(p.ContentBase64)
	if err != nil {
		return errorResponse(id, -32602, "content_base64 is invalid: "+err.Error())
	}

	var existingMode fs.FileMode = 0644
	if info, statErr := os.Stat(path); statErr == nil {
		existingMode = info.Mode().Perm()
		if p.ExpectedMtimeNs > 0 && info.ModTime().UnixNano() != p.ExpectedMtimeNs {
			return errorResponse(id, -32001, "file changed on remote")
		}
	} else if !os.IsNotExist(statErr) {
		return errorResponse(id, -32000, "stat failed: "+statErr.Error())
	}

	dir := filepath.Dir(path)
	// Ensure parent directory exists (e.g., .nexion/attachments/)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return errorResponse(id, -32000, "mkdir failed: "+err.Error())
	}
	// Use os.CreateTemp for unpredictable temp file name (prevents TOCTOU symlink attacks)
	tmpFile, err := os.CreateTemp(dir, ".nexion-write-*.tmp")
	if err != nil {
		return errorResponse(id, -32000, "temp file creation failed")
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(content); err != nil {
		tmpFile.Close()
		return errorResponse(id, -32000, "write failed")
	}
	tmpFile.Close()

	if err := os.Chmod(tmpPath, existingMode); err != nil {
		return errorResponse(id, -32000, "chmod failed")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return errorResponse(id, -32000, "rename failed")
	}

	info, err := os.Stat(path)
	if err != nil {
		return errorResponse(id, -32000, "post-write stat failed: "+err.Error())
	}

	updateWorkspaceSnapshotForPath(ws, path, info)

	return successResponse(id, map[string]interface{}{
		"path":     p.Path,
		"size":     info.Size(),
		"mtime_ns": info.ModTime().UnixNano(),
	})
}

// delete_file

type DeleteFileParams struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"` // relative to workspace root
	Recursive   bool   `json:"recursive,omitempty"`
}

func handleDeleteFile(id interface{}, params json.RawMessage) RPCResponse {
	var p DeleteFileParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}
	if p.Path == "" {
		return errorResponse(id, -32602, "path is required")
	}

	fullPath, ws, err := resolveWorkspacePath(p.WorkspaceID, p.Path)
	if err != nil {
		return errorResponse(id, -32000, err.Error())
	}

	// Safety: prevent deleting the workspace root itself
	if fullPath == ws.RootPath {
		return errorResponse(id, -32000, "cannot delete workspace root")
	}

	info, statErr := os.Stat(fullPath)
	if statErr != nil {
		return errorResponse(id, -32000, "file not found: "+statErr.Error())
	}

	if info.IsDir() {
		if !p.Recursive {
			return errorResponse(id, -32000, "path is a directory; set recursive=true to delete")
		}
		if err := os.RemoveAll(fullPath); err != nil {
			return errorResponse(id, -32000, "delete failed: "+err.Error())
		}
	} else {
		if err := os.Remove(fullPath); err != nil {
			return errorResponse(id, -32000, "delete failed: "+err.Error())
		}
	}

	// Remove from snapshot
	ws.mu.Lock()
	if info.IsDir() {
		// Remove all entries under this directory
		prefix := p.Path + "/"
		for key := range ws.snapshot {
			if key == p.Path || strings.HasPrefix(key, prefix) {
				delete(ws.snapshot, key)
			}
		}
	} else {
		delete(ws.snapshot, p.Path)
	}
	ws.version++
	ws.mu.Unlock()

	return successResponse(id, map[string]interface{}{
		"path":    p.Path,
		"deleted": true,
	})
}

// stat

type StatParams struct {
	WorkspaceID string `json:"workspace_id"`
	Path        string `json:"path"`
}

func handleStat(id interface{}, params json.RawMessage) RPCResponse {
	var p StatParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	var path string
	if p.WorkspaceID != "" {
		resolved, _, err := resolveWorkspacePath(p.WorkspaceID, p.Path)
		if err != nil {
			return errorResponse(id, -32000, "path resolution failed")
		}
		path = resolved
	} else {
		// Legacy compat: stat without workspace_id (e.g., absolute paths from older clients)
		path = expandHome(p.Path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return errorResponse(id, -32000, "stat failed")
	}

	kind := "file"
	if info.IsDir() {
		kind = "folder"
	}

	return successResponse(id, map[string]interface{}{
		"path":     p.Path,
		"name":     info.Name(),
		"kind":     kind,
		"size":     info.Size(),
		"mtime_ns": info.ModTime().UnixNano(),
		"symlink":  info.Mode()&fs.ModeSymlink != 0,
	})
}

// git_snapshot

type GitSnapshotParams struct {
	WorkspaceID string `json:"workspace_id"`
}

type GitReadBlobParams struct {
	WorkspaceID string `json:"workspace_id"`
	Rev         string `json:"rev"`
	Path        string `json:"path"`
}

type GitAIContextParams struct {
	WorkspaceID   string `json:"workspace_id"`
	BaseRef       string `json:"base_ref,omitempty"`
	IncludeStatus bool   `json:"include_status"`
	IncludeLog    bool   `json:"include_log"`
}

type GitBranchListParams struct {
	WorkspaceID string `json:"workspace_id"`
}

type GitBranchCreateParams struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
}

type GitBranchSwitchParams struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
}

type GitBranchCheckoutRemoteParams struct {
	WorkspaceID  string `json:"workspace_id"`
	RemoteBranch string `json:"remote_branch"`
	LocalBranch  string `json:"local_branch,omitempty"`
}

type GitCommitParams struct {
	WorkspaceID string `json:"workspace_id"`
	Message     string `json:"message"`
	AddAll      bool   `json:"add_all,omitempty"`
}

type GitPushParams struct {
	WorkspaceID string `json:"workspace_id"`
	Remote      string `json:"remote,omitempty"`
	Refspec     string `json:"refspec,omitempty"`
	SetUpstream bool   `json:"set_upstream,omitempty"`
}

type GitSnapshotResult struct {
	IsRepo            bool             `json:"is_repo"`
	Branch            *string          `json:"branch"`
	DefaultBranch     string           `json:"default_branch"`
	StagedCount       int              `json:"staged_count"`
	ModifiedCount     int              `json:"modified_count"`
	UntrackedCount    int              `json:"untracked_count"`
	AheadCount        int              `json:"ahead_count"`
	BehindCount       int              `json:"behind_count"`
	TotalLinesAdded   int              `json:"total_lines_added"`
	TotalLinesRemoved int              `json:"total_lines_removed"`
	Entries           []GitEntryResult `json:"entries"`
	IgnoredPaths      []string         `json:"ignored_paths"`
}

type GitEntryResult struct {
	Path           string `json:"path"`
	Status         string `json:"status"`
	IndexStatus    string `json:"index_status"`
	WorktreeStatus string `json:"worktree_status"`
	LinesAdded     int    `json:"lines_added"`
	LinesRemoved   int    `json:"lines_removed"`
}

type GitBranchListResult struct {
	Local  []string `json:"local"`
	Remote []string `json:"remote"`
}

func handleGitSnapshot(id interface{}, params json.RawMessage) RPCResponse {
	var p GitSnapshotParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}
	ws := getWorkspace(p.WorkspaceID)
	if ws == nil {
		return errorResponse(id, -32000, "workspace not found: "+p.WorkspaceID)
	}
	root := ws.RootPath

	// Use the same batch script as the Swift local path for consistency
	sep := "---NEXION_SEP---"
	script := fmt.Sprintf(`cd '%s' 2>/dev/null || exit 1
git -c safe.directory='*' rev-parse --is-inside-work-tree 2>/dev/null || echo "false"
echo '%s'
git -c safe.directory='*' symbolic-ref --quiet --short HEAD 2>/dev/null || git -c safe.directory='*' rev-parse --short HEAD 2>/dev/null
echo '%s'
git -c safe.directory='*' symbolic-ref refs/remotes/origin/HEAD 2>/dev/null
echo '%s'
git -c safe.directory='*' rev-list --left-right --count @{upstream}...HEAD 2>/dev/null
echo '%s'
git -c safe.directory='*' status --porcelain 2>/dev/null
echo '%s'
git -c safe.directory='*' diff --numstat 2>/dev/null
echo '%s'
git -c safe.directory='*' diff --cached --numstat 2>/dev/null
echo '%s'
git -c safe.directory='*' ls-files --others --ignored --exclude-standard 2>/dev/null
echo '%s'
git -c safe.directory='*' ls-files --others --ignored --exclude-standard --directory 2>/dev/null`,
		shellEscape(root), sep, sep, sep, sep, sep, sep, sep, sep)

	cmd := exec.Command("sh", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		// Not a git repo or git not installed
		return successResponse(id, GitSnapshotResult{
			DefaultBranch: "main",
			Entries:       []GitEntryResult{},
			IgnoredPaths:  []string{},
		})
	}

	sections := strings.SplitN(string(out), sep+"\n", 9)
	if len(sections) < 9 {
		return successResponse(id, GitSnapshotResult{
			DefaultBranch: "main",
			Entries:       []GitEntryResult{},
			IgnoredPaths:  []string{},
		})
	}

	repoCheck := strings.TrimSpace(sections[0])
	if repoCheck != "true" {
		return successResponse(id, GitSnapshotResult{
			DefaultBranch: "main",
			Entries:       []GitEntryResult{},
			IgnoredPaths:  []string{},
		})
	}

	result := GitSnapshotResult{
		IsRepo:        true,
		DefaultBranch: "main",
		Entries:       []GitEntryResult{},
		IgnoredPaths:  []string{},
	}

	// [1] Branch
	branch := strings.TrimSpace(sections[1])
	if branch != "" {
		result.Branch = &branch
	}

	// [2] Default branch
	defaultBranch := strings.TrimSpace(sections[2])
	if parts := strings.Split(defaultBranch, "/"); len(parts) > 0 {
		last := parts[len(parts)-1]
		if last != "" {
			result.DefaultBranch = last
		}
	}

	// [3] Ahead/behind
	ab := strings.TrimSpace(sections[3])
	if parts := strings.Split(ab, "\t"); len(parts) == 2 {
		result.BehindCount, _ = strconv.Atoi(parts[0])
		result.AheadCount, _ = strconv.Atoi(parts[1])
	}

	// [4] Status
	statusLines := sections[4]
	var staged, modified, untracked int
	for _, line := range strings.Split(statusLines, "\n") {
		if len(line) < 2 {
			continue
		}
		if strings.HasPrefix(line, "??") {
			untracked++
			continue
		}
		if line[0] != ' ' {
			staged++
		}
		if line[1] != ' ' {
			modified++
		}
	}
	result.StagedCount = staged
	result.ModifiedCount = modified
	result.UntrackedCount = untracked

	// [5] & [6] Line stats
	lineStats := map[string][2]int{} // path -> [added, removed]
	for _, section := range []string{sections[5], sections[6]} {
		for _, line := range strings.Split(section, "\n") {
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) < 3 {
				continue
			}
			added, _ := strconv.Atoi(parts[0])
			removed, _ := strconv.Atoi(parts[1])
			path := parts[2]
			existing := lineStats[path]
			lineStats[path] = [2]int{existing[0] + added, existing[1] + removed}
		}
	}

	// Parse entries from status
	for _, line := range strings.Split(statusLines, "\n") {
		if len(line) < 4 {
			continue
		}
		status := line[:2]
		path := strings.TrimSpace(line[3:])
		// Handle renamed files: "R  old -> new"
		if idx := strings.Index(path, " -> "); idx != -1 {
			path = path[idx+4:]
		}
		if path == "" {
			continue
		}

		entry := GitEntryResult{
			Path:           path,
			Status:         status,
			IndexStatus:    string(status[0]),
			WorktreeStatus: string(status[1]),
		}
		if stats, ok := lineStats[path]; ok {
			entry.LinesAdded = stats[0]
			entry.LinesRemoved = stats[1]
		}
		result.Entries = append(result.Entries, entry)
	}

	// Totals
	for _, e := range result.Entries {
		result.TotalLinesAdded += e.LinesAdded
		result.TotalLinesRemoved += e.LinesRemoved
	}

	// [7] & [8] Ignored paths
	ignoredSet := map[string]bool{}
	for _, line := range strings.Split(sections[7], "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ignoredSet[line] = true
		}
	}
	for _, line := range strings.Split(sections[8], "\n") {
		line = strings.TrimRight(line, "/")
		line = strings.TrimSpace(line)
		if line != "" {
			ignoredSet[line] = true
		}
	}
	for p := range ignoredSet {
		result.IgnoredPaths = append(result.IgnoredPaths, p)
	}

	return successResponse(id, result)
}

func handleGitReadBlob(id interface{}, params json.RawMessage) RPCResponse {
	var p GitReadBlobParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}
	if p.Path == "" {
		return errorResponse(id, -32602, "path is required")
	}

	rev := p.Rev
	if rev == "" {
		rev = "HEAD"
	}
	// Validate rev: alphanumerics and a short safe set only.
	for _, c := range rev {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '/' || c == '_' || c == '.' || c == '-' || c == '^' || c == '~') {
			return errorResponse(id, -32602, "invalid rev format")
		}
	}
	// Validate path: no absolute paths, no traversal, no NUL.
	if filepath.IsAbs(p.Path) || strings.Contains(p.Path, "..") || strings.ContainsRune(p.Path, 0) {
		return errorResponse(id, -32602, "invalid path format")
	}

	ws := getWorkspace(p.WorkspaceID)
	if ws == nil {
		return errorResponse(id, -32000, "workspace not found")
	}

	cmd := exec.Command(
		"git",
		"-c", "safe.directory=*",
		"-C", ws.RootPath,
		"show", fmt.Sprintf("%s:%s", rev, p.Path),
	)
	output, err := cmd.Output()
	if err != nil {
		return errorResponse(id, -32000, "git show failed: "+err.Error())
	}

	encoding := "utf8"
	content := ""
	if utf8.Valid(output) {
		content = string(output)
	} else {
		encoding = "base64"
		content = base64.StdEncoding.EncodeToString(output)
	}

	return successResponse(id, map[string]interface{}{
		"rev":      rev,
		"path":     p.Path,
		"encoding": encoding,
		"content":  content,
		"size":     len(output),
	})
}

func handleGitAIContext(id interface{}, params json.RawMessage) RPCResponse {
	var p GitAIContextParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}

	ws := getWorkspace(p.WorkspaceID)
	if ws == nil {
		return errorResponse(id, -32000, "workspace not found")
	}

	root := ws.RootPath
	baseRef := strings.TrimSpace(p.BaseRef)

	// Validate baseRef: alphanumerics and a short safe set, no shell metacharacters.
	if baseRef != "" {
		for _, c := range baseRef {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
				c == '/' || c == '_' || c == '.' || c == '-' || c == '^' || c == '~') {
				return errorResponse(id, -32602, "invalid base_ref format")
			}
		}
	}

	emptyResult := map[string]interface{}{
		"is_repo": false,
		"status":  "",
		"diff":    "",
		"stat":    "",
		"log":     "",
	}

	// Check if we're inside a git repo (no shell, direct exec)
	checkOut, err := exec.Command("git", "-c", "safe.directory=*", "-C", root,
		"rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(checkOut)) != "true" {
		return successResponse(id, emptyResult)
	}

	// git status --short
	statusStr := ""
	if p.IncludeStatus {
		if out, err := exec.Command("git", "-c", "safe.directory=*", "-C", root,
			"status", "--short").Output(); err == nil {
			statusStr = string(out)
		}
	}

	// git diff
	diffRef := "HEAD"
	if baseRef != "" {
		diffRef = baseRef + "...HEAD"
	}
	diffStr := ""
	if out, err := exec.Command("git", "-c", "safe.directory=*", "-C", root,
		"diff", diffRef).Output(); err == nil {
		diffStr = string(out)
	}

	// git diff --stat
	statStr := ""
	if out, err := exec.Command("git", "-c", "safe.directory=*", "-C", root,
		"diff", "--stat", diffRef).Output(); err == nil {
		statStr = string(out)
	}

	// git log
	logStr := ""
	if p.IncludeLog && baseRef != "" {
		logRef := baseRef + "..HEAD"
		if out, err := exec.Command("git", "-c", "safe.directory=*", "-C", root,
			"log", logRef, "--oneline").Output(); err == nil {
			logStr = string(out)
		}
	}

	return successResponse(id, map[string]interface{}{
		"is_repo": true,
		"status":  statusStr,
		"diff":    diffStr,
		"stat":    statStr,
		"log":     logStr,
	})
}

func gitWorkspaceRoot(workspaceID string) (string, *Workspace, RPCResponse, bool) {
	if workspaceID == "" {
		return "", nil, errorResponse(nil, -32602, "workspace_id is required"), false
	}
	ws := getWorkspace(workspaceID)
	if ws == nil {
		return "", nil, errorResponse(nil, -32000, "workspace not found: "+workspaceID), false
	}
	return ws.RootPath, ws, RPCResponse{}, true
}

func runGitInWorkspace(root string, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"-c", "safe.directory=*", "-C", root}, args...)
	return exec.Command("git", cmdArgs...).CombinedOutput()
}

func handleGitBranchList(id interface{}, params json.RawMessage) RPCResponse {
	var p GitBranchListParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	root, _, errResp, ok := gitWorkspaceRoot(p.WorkspaceID)
	if !ok {
		errResp.ID = id
		return errResp
	}

	localOut, err := runGitInWorkspace(root, "branch", "--format=%(refname:short)")
	if err != nil {
		msg := strings.TrimSpace(string(localOut))
		if msg == "" {
			msg = err.Error()
		}
		return errorResponse(id, -32000, "git branch failed: "+msg)
	}

	remoteOut, err := runGitInWorkspace(root, "branch", "-r", "--format=%(refname:short)")
	if err != nil {
		msg := strings.TrimSpace(string(remoteOut))
		if msg == "" {
			msg = err.Error()
		}
		return errorResponse(id, -32000, "git branch -r failed: "+msg)
	}

	parseLines := func(raw string) []string {
		lines := strings.Split(raw, "\n")
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			out = append(out, line)
		}
		return out
	}

	localBranches := parseLines(string(localOut))
	remoteBranches := make([]string, 0)
	for _, line := range parseLines(string(remoteOut)) {
		if strings.Contains(line, "HEAD") || !strings.Contains(line, "/") {
			continue
		}
		remoteBranches = append(remoteBranches, line)
	}

	return successResponse(id, GitBranchListResult{
		Local:  localBranches,
		Remote: remoteBranches,
	})
}

func handleGitBranchCreate(id interface{}, params json.RawMessage) RPCResponse {
	var p GitBranchCreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	if strings.TrimSpace(p.Name) == "" {
		return errorResponse(id, -32602, "name is required")
	}

	root, _, errResp, ok := gitWorkspaceRoot(p.WorkspaceID)
	if !ok {
		errResp.ID = id
		return errResp
	}

	out, err := runGitInWorkspace(root, "checkout", "-b", p.Name)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errorResponse(id, -32000, msg)
	}

	return successResponse(id, map[string]interface{}{
		"ok":     true,
		"output": string(out),
	})
}

func handleGitBranchSwitch(id interface{}, params json.RawMessage) RPCResponse {
	var p GitBranchSwitchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	if strings.TrimSpace(p.Name) == "" {
		return errorResponse(id, -32602, "name is required")
	}

	root, _, errResp, ok := gitWorkspaceRoot(p.WorkspaceID)
	if !ok {
		errResp.ID = id
		return errResp
	}

	out, err := runGitInWorkspace(root, "checkout", p.Name)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errorResponse(id, -32000, msg)
	}

	return successResponse(id, map[string]interface{}{
		"ok":     true,
		"output": string(out),
	})
}

func handleGitBranchCheckoutRemote(id interface{}, params json.RawMessage) RPCResponse {
	var p GitBranchCheckoutRemoteParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	if strings.TrimSpace(p.RemoteBranch) == "" {
		return errorResponse(id, -32602, "remote_branch is required")
	}

	root, _, errResp, ok := gitWorkspaceRoot(p.WorkspaceID)
	if !ok {
		errResp.ID = id
		return errResp
	}

	local := strings.TrimSpace(p.LocalBranch)
	if local == "" {
		parts := strings.Split(p.RemoteBranch, "/")
		if len(parts) > 1 {
			local = strings.Join(parts[1:], "/")
		} else {
			local = p.RemoteBranch
		}
	}

	out, err := runGitInWorkspace(root, "checkout", "-b", local, "--track", p.RemoteBranch)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "already exists") {
			switchOut, switchErr := runGitInWorkspace(root, "checkout", local)
			if switchErr != nil {
				switchMsg := strings.TrimSpace(string(switchOut))
				if switchMsg == "" {
					switchMsg = switchErr.Error()
				}
				return errorResponse(id, -32000, switchMsg)
			}
			return successResponse(id, map[string]interface{}{
				"ok":     true,
				"output": string(switchOut),
			})
		}
		return errorResponse(id, -32000, msg)
	}

	return successResponse(id, map[string]interface{}{
		"ok":     true,
		"output": string(out),
	})
}

func handleGitCommit(id interface{}, params json.RawMessage) RPCResponse {
	var p GitCommitParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}
	if strings.TrimSpace(p.Message) == "" {
		return errorResponse(id, -32602, "message is required")
	}

	ws := getWorkspace(p.WorkspaceID)
	if ws == nil {
		return errorResponse(id, -32000, "workspace not found: "+p.WorkspaceID)
	}

	if p.AddAll {
		addCmd := exec.Command("git", "-c", "safe.directory=*", "-C", ws.RootPath, "add", "-A")
		if out, err := addCmd.CombinedOutput(); err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			return errorResponse(id, -32000, "git add failed: "+msg)
		}
	}

	commitCmd := exec.Command("git", "-c", "safe.directory=*", "-C", ws.RootPath, "commit", "-m", p.Message)
	out, err := commitCmd.CombinedOutput()
	output := string(out)
	if err != nil {
		msg := strings.TrimSpace(output)
		if msg == "" {
			msg = err.Error()
		}
		return errorResponse(id, -32000, "git commit failed: "+msg)
	}

	return successResponse(id, map[string]interface{}{
		"ok":     true,
		"output": output,
	})
}

func handleGitPush(id interface{}, params json.RawMessage) RPCResponse {
	var p GitPushParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}
	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}

	ws := getWorkspace(p.WorkspaceID)
	if ws == nil {
		return errorResponse(id, -32000, "workspace not found: "+p.WorkspaceID)
	}

	remote := strings.TrimSpace(p.Remote)
	if remote == "" {
		remote = "origin"
	}
	// Validate remote: simple names only, not URLs or flags.
	for _, c := range remote {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.') {
			return errorResponse(id, -32602, "invalid remote name")
		}
	}
	refspec := strings.TrimSpace(p.Refspec)
	// Validate refspec: no flags, no shell characters.
	if strings.HasPrefix(refspec, "-") || strings.ContainsAny(refspec, ";|&$`") {
		return errorResponse(id, -32602, "invalid refspec")
	}

	args := []string{"-c", "safe.directory=*", "-C", ws.RootPath, "push"}
	if p.SetUpstream {
		args = append(args, "-u")
	}
	args = append(args, remote)
	if refspec != "" {
		args = append(args, refspec)
	}

	pushCmd := exec.Command("git", args...)
	out, err := pushCmd.CombinedOutput()
	output := string(out)
	if err != nil {
		msg := strings.TrimSpace(output)
		if msg == "" {
			msg = err.Error()
		}
		return errorResponse(id, -32000, "git push failed: "+msg)
	}

	return successResponse(id, map[string]interface{}{
		"ok":     true,
		"output": output,
	})
}

// Helpers.

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	return path
}

func shellEscape(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}
