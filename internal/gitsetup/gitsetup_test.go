package gitsetup

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeRemote creates a bare repository with one commit on main so a
// file:// clone url is usable as the "remote" in Configure tests.
func makeRemote(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "remote.git")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", work},
		{"-C", work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"},
		{"clone", "-q", "--bare", work, bare},
	} {
		cmd := exec.Command("git", args...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, stderr.String())
		}
	}
	return bare
}

func TestConfigure_ClonesAndPlacesOnBranch(t *testing.T) {
	remote := makeRemote(t)
	work := t.TempDir()
	opts := Options{
		AgentID: "be-1",
		WorkDir: work,
		RepoURL: remote,
		Token:   "ignored-by-file-url",
	}
	ctx := context.Background()
	clone, err := Configure(ctx, opts)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if want := filepath.Join(work, "repo"); clone != want {
		t.Errorf("clone path = %q want %q", clone, want)
	}
	branch := mustRunOutput(t, "git", "-C", clone, "rev-parse", "--abbrev-ref", "HEAD")
	if branch != "teem/be-1" {
		t.Errorf("branch = %q want teem/be-1", branch)
	}
}

func TestConfigure_Idempotent(t *testing.T) {
	remote := makeRemote(t)
	work := t.TempDir()
	opts := Options{AgentID: "rv-1", WorkDir: work, RepoURL: remote}
	ctx := context.Background()
	if _, err := Configure(ctx, opts); err != nil {
		t.Fatal(err)
	}
	// Second call should not re-clone and should not error.
	if _, err := Configure(ctx, opts); err != nil {
		t.Fatalf("second Configure: %v", err)
	}
}

func TestConfigure_ResumesExistingRemoteBranch(t *testing.T) {
	// Set up a remote that already has teem/x; the worker should pick it up.
	remote := makeRemote(t)
	scratch := t.TempDir()
	if _, err := Configure(context.Background(), Options{AgentID: "x", WorkDir: scratch, RepoURL: remote}); err != nil {
		t.Fatal(err)
	}
	// Make a commit on teem/x and push it to the remote.
	clone := filepath.Join(scratch, "repo")
	mustRun(t, "git", "-C", clone, "commit", "--allow-empty", "-m", "side")
	mustRun(t, "git", "-C", clone, "push", "origin", "teem/x")

	// Fresh worker workdir; Configure should resume the existing branch.
	work := t.TempDir()
	clone2, err := Configure(context.Background(), Options{AgentID: "x", WorkDir: work, RepoURL: remote})
	if err != nil {
		t.Fatal(err)
	}
	commits := mustRunOutput(t, "git", "-C", clone2, "rev-list", "--count", "teem/x")
	if commits != "2" {
		t.Errorf("teem/x commits = %q want 2 (init + side)", commits)
	}
}

func TestConfigure_WritesCredentialHelperScript(t *testing.T) {
	remote := makeRemote(t)
	work := t.TempDir()
	opts := Options{AgentID: "be-1", WorkDir: work, RepoURL: remote, Username: "x-access-token"}
	if _, err := Configure(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	body := mustReadFile(t, filepath.Join(work, ".teem-git-credentials.sh"))
	if !strings.Contains(body, "username=x-access-token") {
		t.Errorf("helper missing username line: %q", body)
	}
	if !strings.Contains(body, "$TEEM_GIT_TOKEN") {
		t.Errorf("helper should reference $TEEM_GIT_TOKEN at runtime, got: %q", body)
	}
	// The configured value of credential.helper should be the absolute
	// path to the script.
	val := mustRunOutput(t, "git", "-C", filepath.Join(work, "repo"), "config", "credential.helper")
	if val != filepath.Join(work, ".teem-git-credentials.sh") {
		t.Errorf("credential.helper = %q, want script path", val)
	}
}

func TestPush_PushesNewBranch(t *testing.T) {
	remote := makeRemote(t)
	work := t.TempDir()
	if _, err := Configure(context.Background(), Options{AgentID: "be-1", WorkDir: work, RepoURL: remote}); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(work, "repo")
	mustRun(t, "git", "-C", clone, "commit", "--allow-empty", "-m", "from worker")
	if _, err := Push(context.Background(), Options{AgentID: "be-1", WorkDir: work, RepoURL: remote}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// Verify the branch landed on the remote.
	cmd := exec.Command("git", "-C", remote, "show-ref", "--verify", "refs/heads/teem/be-1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("push did not land on remote: %v", err)
	}
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append([]string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}, "PATH="+pathenv(), "HOME="+t.TempDir())
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, stderr.String())
	}
}

func mustRunOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append([]string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}, "PATH="+pathenv(), "HOME="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimSpace(string(out))
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	cmd := exec.Command("cat", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(out)
}

func pathenv() string {
	return "/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"
}
