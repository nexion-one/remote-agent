package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workspace root is the security boundary: everything the file verbs touch
// has to resolve inside it. These are the ways out that matter.

func newTestWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	// The tests compare resolved paths, and on macOS /var is a symlink to
	// /private/var, so resolve the root once here or every comparison is
	// against a path the code has already rewritten.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return getOrCreateWorkspace(root), root
}

func TestRelativePathResolvesInsideTheRoot(t *testing.T) {
	ws, root := newTestWorkspace(t)
	got, _, err := resolveWorkspacePath(ws.ID, "src/main.go")
	if err != nil {
		t.Fatalf("a file in the workspace was refused: %v", err)
	}
	if want := filepath.Join(root, "src", "main.go"); got != want {
		t.Fatalf("resolved to %q, want %q", got, want)
	}
}

func TestAPathThatDoesNotExistYetStillResolves(t *testing.T) {
	// write_file creates new files, so a missing target is not an error.
	ws, root := newTestWorkspace(t)
	got, _, err := resolveWorkspacePath(ws.ID, "src/brand-new.go")
	if err != nil {
		t.Fatalf("a new file was refused: %v", err)
	}
	if !strings.HasPrefix(got, root) {
		t.Fatalf("resolved outside the root: %q", got)
	}
}

func TestTraversalIsRefused(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	for _, path := range []string{
		"..",
		"../outside",
		"../../etc/passwd",
		"src/../../outside",
		"src/../../../etc/passwd",
		"./../../outside",
	} {
		if _, _, err := resolveWorkspacePath(ws.ID, path); err == nil {
			t.Errorf("%q was allowed out of the workspace", path)
		}
	}
}

func TestTraversalThatCancelsOutIsAllowed(t *testing.T) {
	// "src/../src/main.go" never leaves, and refusing it would be wrong.
	ws, root := newTestWorkspace(t)
	got, _, err := resolveWorkspacePath(ws.ID, "src/../src/main.go")
	if err != nil {
		t.Fatalf("a path that stays inside was refused: %v", err)
	}
	if want := filepath.Join(root, "src", "main.go"); got != want {
		t.Fatalf("resolved to %q, want %q", got, want)
	}
}

func TestNulByteIsRefused(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	if _, _, err := resolveWorkspacePath(ws.ID, "src/main.go\x00.txt"); err == nil {
		t.Fatal("a path with a NUL was allowed")
	}
}

func TestAnAbsolutePathOutsideTheRootIsRefused(t *testing.T) {
	ws, _ := newTestWorkspace(t)
	for _, path := range []string{"/etc/passwd", "/", os.TempDir()} {
		if _, _, err := resolveWorkspacePath(ws.ID, path); err == nil {
			t.Errorf("%q was allowed", path)
		}
	}
}

func TestAnAbsolutePathInsideTheRootIsAllowed(t *testing.T) {
	ws, root := newTestWorkspace(t)
	inside := filepath.Join(root, "src", "main.go")
	got, _, err := resolveWorkspacePath(ws.ID, inside)
	if err != nil {
		t.Fatalf("an absolute path inside the root was refused: %v", err)
	}
	if got != inside {
		t.Fatalf("resolved to %q, want %q", got, inside)
	}
}

func TestASymlinkPointingOutIsRefused(t *testing.T) {
	// The obvious escape once traversal is closed: a link inside the
	// workspace whose target is not.
	ws, root := newTestWorkspace(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("not yours\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveWorkspacePath(ws.ID, "escape.txt"); err == nil {
		t.Fatal("a symlink out of the workspace was followed")
	}
}

func TestASymlinkInsideTheWorkspaceIsFollowed(t *testing.T) {
	ws, root := newTestWorkspace(t)
	link := filepath.Join(root, "alias.go")
	if err := os.Symlink(filepath.Join(root, "src", "main.go"), link); err != nil {
		t.Fatal(err)
	}
	got, _, err := resolveWorkspacePath(ws.ID, "alias.go")
	if err != nil {
		t.Fatalf("a link inside the workspace was refused: %v", err)
	}
	if want := filepath.Join(root, "src", "main.go"); got != want {
		t.Fatalf("resolved to %q, want %q", got, want)
	}
}

func TestAnUnknownWorkspaceIsRefused(t *testing.T) {
	if _, _, err := resolveWorkspacePath("not-a-workspace", "src/main.go"); err == nil {
		t.Fatal("a path was resolved against a workspace that does not exist")
	}
}

func TestIsWithinRootDoesNotMatchOnAPrefix(t *testing.T) {
	// "/home/user/project-evil" starts with "/home/user/project" as a
	// string, and is a different directory.
	if isWithinRoot("/home/user/project-evil", "/home/user/project") {
		t.Fatal("a sibling sharing a name prefix counted as inside")
	}
	if !isWithinRoot("/home/user/project/src", "/home/user/project") {
		t.Fatal("a real child counted as outside")
	}
	if !isWithinRoot("/home/user/project", "/home/user/project") {
		t.Fatal("the root itself counted as outside")
	}
}
