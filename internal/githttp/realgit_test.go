package githttp

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/httpd"
)

// TestRealGit runs the git command line client against the surface over a real socket: clone of an
// empty repository, pushes (git sends thin packs with deltas), clone, shallow clone, incremental
// fetch (a negotiation with haves), tags, branch deletion, and the same again over protocol v0.
// It is skipped where there is no git binary, and with -short.
func TestRealGit(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git binary on this machine")
	}
	f := newForge(t)
	r := f.create("real")
	ln, err := httpd.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go (&httpd.Server{Token: token, Handler: f.dispatch}).Serve(ln)
	url := fmt.Sprintf("http://127.0.0.1:%d/adaiversen/real.git", ln.Addr().(*net.TCPAddr).Port)

	home := t.TempDir()
	run := func(dir string, version int, args ...string) string {
		t.Helper()
		full := append([]string{
			"-c", "http.extraHeader=Authorization: Bearer " + token,
			"-c", "user.name=tester", "-c", "user.email=tester@example.com",
			"-c", "core.autocrlf=false", "-c", "init.defaultBranch=main", "-c", "gc.auto=0",
			"-c", fmt.Sprintf("protocol.version=%d", version),
		}, args...)
		cmd := exec.Command(gitPath, full...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(home, "none"),
			"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=echo")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(dir, name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// Clone the empty repository, commit, push.
	run(home, 2, "clone", url, "work")
	work := filepath.Join(home, "work")
	run(work, 2, "checkout", "-B", "main")
	big := strings.Repeat("a fairly long line of text that repeats and compresses well\n", 400)
	write(work, "big.txt", big)
	write(work, "small.txt", "one\n")
	run(work, 2, "add", ".")
	run(work, 2, "commit", "-m", "first")
	run(work, 2, "push", "origin", "main")
	if r.Head() == nil {
		t.Fatal("the first push did not create main")
	}
	// A small change to the big file: git sends a delta against the blob the server has.
	big2 := big + "and one more line at the end\n"
	write(work, "big.txt", big2)
	run(work, 2, "commit", "-am", "second")
	run(work, 2, "push", "origin", "main")
	run(work, 2, "tag", "-a", "v1", "-m", "release one")
	run(work, 2, "push", "origin", "v1", "main:refs/heads/topic")

	// A fresh clone has everything, byte for byte.
	run(home, 2, "clone", url, "copy")
	copyDir := filepath.Join(home, "copy")
	if read(copyDir, "big.txt") != big2 || read(copyDir, "small.txt") != "one\n" {
		t.Fatal("cloned files differ from what was pushed")
	}
	if out := run(copyDir, 2, "fsck", "--strict"); strings.Contains(out, "error") {
		t.Fatalf("fsck of the clone: %s", out)
	}
	if out := run(copyDir, 2, "tag", "-l"); strings.TrimSpace(out) != "v1" {
		t.Fatalf("tags in the clone: %q", out)
	}
	if out := run(copyDir, 2, "branch", "-r"); !strings.Contains(out, "origin/topic") {
		t.Fatalf("remote branches in the clone: %q", out)
	}
	// Shallow clone, then a third commit, then an incremental fetch into both clones.
	run(home, 2, "clone", "--depth", "1", url, "shallow")
	shallow := filepath.Join(home, "shallow")
	if out := run(shallow, 2, "rev-list", "--count", "HEAD"); strings.TrimSpace(out) != "1" {
		t.Fatalf("shallow clone has %s commits", strings.TrimSpace(out))
	}
	write(work, "small.txt", "two\n")
	run(work, 2, "commit", "-am", "third")
	run(work, 2, "push", "origin", "main")
	run(copyDir, 2, "pull", "--ff-only")
	if read(copyDir, "small.txt") != "two\n" {
		t.Fatal("pull did not bring the third commit")
	}
	run(shallow, 2, "pull", "--ff-only")
	run(shallow, 2, "fetch", "--deepen", "1")
	if out := run(shallow, 2, "rev-list", "--count", "HEAD"); strings.TrimSpace(out) == "1" {
		t.Fatal("deepening the shallow clone brought nothing")
	}
	run(shallow, 2, "fetch", "--unshallow")
	if out := run(shallow, 2, "rev-list", "--count", "HEAD"); strings.TrimSpace(out) != "3" {
		t.Fatalf("unshallowed clone has %s commits", strings.TrimSpace(out))
	}
	run(shallow, 2, "fsck", "--strict")

	// A non-fast-forward push is refused by the server; a branch can be deleted.
	run(copyDir, 2, "reset", "--hard", "HEAD~1")
	write(copyDir, "small.txt", "diverged\n")
	run(copyDir, 2, "commit", "-am", "diverged")
	cmd := exec.Command(gitPath, "-c", "http.extraHeader=Authorization: Bearer "+token, "push", "--force", "origin", "main")
	cmd.Dir = copyDir
	if out, err := cmd.CombinedOutput(); err == nil || !bytes.Contains(out, []byte("non-fast-forward")) {
		t.Fatalf("forced non-fast-forward push: %v\n%s", err, out)
	}
	run(work, 2, "push", "origin", "--delete", "topic")
	if _, ok := r.Branch("topic"); ok {
		t.Fatal("the deleted branch is still there")
	}
	if out := run(home, 2, "ls-remote", url); !strings.Contains(out, "refs/tags/v1^{}") || !strings.Contains(out, "HEAD") {
		t.Fatalf("ls-remote: %q", out)
	}

	// The same reads over protocol v0.
	run(home, 0, "clone", url, "copy-v0")
	if read(filepath.Join(home, "copy-v0"), "small.txt") != "two\n" {
		t.Fatal("v0 clone differs")
	}
	run(filepath.Join(home, "copy-v0"), 0, "fsck", "--strict")
	run(home, 0, "clone", "--depth", "1", url, "shallow-v0")
	write(work, "small.txt", "four\n")
	run(work, 0, "commit", "-am", "fourth")
	run(work, 0, "push", "origin", "main")
	run(filepath.Join(home, "copy-v0"), 0, "pull", "--ff-only")
	if read(filepath.Join(home, "copy-v0"), "small.txt") != "four\n" {
		t.Fatal("v0 pull did not bring the fourth commit")
	}
}
