package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// search

type SearchParams struct {
	WorkspaceID string `json:"workspace_id"`
	Query       string `json:"query"`
	Glob        string `json:"glob,omitempty"` // e.g. "*.go", "*.swift"
	MaxResults  int    `json:"max_results"`    // 0 = default 500
	Regex       bool   `json:"regex"`          // treat query as regex
}

type SearchMatch struct {
	Path       string `json:"path"` // relative to workspace root
	LineNumber int    `json:"line_number"`
	LineText   string `json:"line_text"`
}

type SearchResult struct {
	WorkspaceID string        `json:"workspace_id"`
	Query       string        `json:"query"`
	Matches     []SearchMatch `json:"matches"`
	Truncated   bool          `json:"truncated"` // true if max_results hit
	Backend     string        `json:"backend"`   // "rg" or "grep"
}

// Active searches for cancellation
var (
	activeSearches   = map[string]*searchContext{}
	activeSearchesMu sync.Mutex
)

type searchContext struct {
	cancel func()
}

func handleSearch(id interface{}, params json.RawMessage) RPCResponse {
	var p SearchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	if p.WorkspaceID == "" {
		return errorResponse(id, -32602, "workspace_id is required")
	}
	if p.Query == "" {
		return errorResponse(id, -32602, "query is required")
	}
	// Input size limits to prevent DoS
	if len(p.Query) > 10000 {
		return errorResponse(id, -32602, "query too long")
	}
	if len(p.Glob) > 1000 {
		return errorResponse(id, -32602, "glob pattern too long")
	}

	ws := getWorkspace(p.WorkspaceID)
	if ws == nil {
		return errorResponse(id, -32000, "workspace not found")
	}

	maxResults := p.MaxResults
	if maxResults <= 0 {
		maxResults = 500
	}
	if maxResults > 5000 {
		maxResults = 5000
	}

	// Detect backend
	backend := "grep"
	rgPath, rgErr := lookPath("rg")
	if rgErr == nil {
		backend = "rg"
	}

	var matches []SearchMatch
	var truncated bool

	if backend == "rg" {
		matches, truncated = searchWithRg(rgPath, ws.RootPath, p.Query, p.Glob, p.Regex, maxResults)
	} else {
		matches, truncated = searchWithGrep(ws.RootPath, p.Query, p.Glob, maxResults)
	}

	return successResponse(id, SearchResult{
		WorkspaceID: ws.ID,
		Query:       p.Query,
		Matches:     matches,
		Truncated:   truncated,
		Backend:     backend,
	})
}

func searchWithRg(rgPath, root, query, glob string, regex bool, maxResults int) ([]SearchMatch, bool) {
	args := []string{
		"--line-number",
		"--no-heading",
		"--color", "never",
		"--max-count", "5", // max matches per file
	}

	if !regex {
		args = append(args, "--fixed-strings")
	}

	if glob != "" {
		args = append(args, "--glob", glob)
	}

	args = append(args, "--", query, root)

	return runSearchCommand(rgPath, args, root, maxResults)
}

func searchWithGrep(root, query, glob string, maxResults int) ([]SearchMatch, bool) {
	args := []string{
		"-r", "-n",
		"--color=never",
		"-m", "5", // max matches per file
	}

	if glob != "" {
		args = append(args, "--include="+glob)
	}

	args = append(args, "--", query, root)

	grepPath := "/usr/bin/grep"
	if p, err := lookPath("grep"); err == nil {
		grepPath = p
	}

	return runSearchCommand(grepPath, args, root, maxResults)
}

func runSearchCommand(executable string, args []string, root string, maxResults int) ([]SearchMatch, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = root

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false
	}

	if err := cmd.Start(); err != nil {
		return nil, false
	}

	var matches []SearchMatch
	var truncated bool
	var count int32

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		if int(atomic.LoadInt32(&count)) >= maxResults {
			truncated = true
			break
		}

		line := scanner.Text()
		match := parseSearchLine(line, root)
		if match != nil {
			matches = append(matches, *match)
			atomic.AddInt32(&count, 1)
		}
	}

	// Kill process if we stopped early
	if truncated {
		cmd.Process.Kill()
	}
	cmd.Wait()

	return matches, truncated
}

// Parse a line like "/path/to/file:42:matched text" into a SearchMatch
func parseSearchLine(line, root string) *SearchMatch {
	// Format: path:linenum:text
	firstColon := strings.Index(line, ":")
	if firstColon < 0 {
		return nil
	}

	rest := line[firstColon+1:]
	secondColon := strings.Index(rest, ":")
	if secondColon < 0 {
		return nil
	}

	filePath := line[:firstColon]
	lineNumStr := rest[:secondColon]
	lineText := rest[secondColon+1:]

	lineNum := 0
	fmt.Sscanf(lineNumStr, "%d", &lineNum)

	// Make path relative to root
	relPath := filePath
	if strings.HasPrefix(filePath, root) {
		relPath = filePath[len(root):]
		if strings.HasPrefix(relPath, "/") {
			relPath = relPath[1:]
		}
	}

	// Trim long lines
	if len(lineText) > 500 {
		lineText = lineText[:500] + "..."
	}

	return &SearchMatch{
		Path:       relPath,
		LineNumber: lineNum,
		LineText:   strings.TrimSpace(lineText),
	}
}

// cancel_search

type CancelSearchParams struct {
	SearchID string `json:"search_id"`
}

func handleCancelSearch(id interface{}, params json.RawMessage) RPCResponse {
	var p CancelSearchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return errorResponse(id, -32602, "invalid params: "+err.Error())
	}

	activeSearchesMu.Lock()
	if ctx, ok := activeSearches[p.SearchID]; ok {
		ctx.cancel()
		delete(activeSearches, p.SearchID)
	}
	activeSearchesMu.Unlock()

	return successResponse(id, map[string]string{"status": "cancelled"})
}
