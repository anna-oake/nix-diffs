package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
func gitCommit(t *testing.T, dir, message, date string) string {
	t.Helper()
	command := exec.Command("git", "-c", "core.hooksPath=/dev/null", "-C", dir, "-c", "user.name=Test Author", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", message)
	command.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, output)
	}
	return gitRun(t, dir, "rev-parse", "HEAD")
}
func addSnapshot(t *testing.T, a *app, k requestKey, sha string) {
	t.Helper()
	name := k.snapshot(sha)
	if err := a.root.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := a.root.WriteFile(name, []byte(`{"snapshot":true}`), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestGitMetadataFetchAndCommitOrder(t *testing.T) {
	a, k, _ := fixture(t)
	a.root.RemoveAll("repos")
	a.root.MkdirAll("repos", 0755)
	a.root.RemoveAll("snapshots")
	source := t.TempDir()
	gitRun(t, source, "init", "-b", "main")
	old := gitCommit(t, source, "First configuration", "2026-01-01T12:00:00Z")
	next := gitCommit(t, source, "Update packages", "2026-01-02T12:00:00Z")
	gitRun(t, source, "branch", "testing")
	a.repoSource = func(owner, repo string) string { return source }
	k.Old = old
	k.New = next
	addSnapshot(t, a, k, old)
	addSnapshot(t, a, k, next)
	repos, err := a.catalog(context.Background())
	if err != nil || len(repos) != 1 || repos[0].MetadataError != "" {
		t.Fatalf("catalog: %+v %v", repos, err)
	}
	commits := repos[0].Commits
	if len(commits) != 2 || commits[0].Message != "Update packages" || commits[0].Author != "Test Author" || commits[0].Date.Format(time.RFC3339) != "2026-01-02T12:00:00Z" {
		t.Fatalf("bad metadata: %+v", commits)
	}
	if strings.Join(commits[0].Branches, ",") != "main,testing" {
		t.Fatal(commits[0].Branches)
	}
	if _, _, err := a.orderedCommits(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	reversed := k
	reversed.Old, reversed.New = k.New, k.Old
	if _, _, err := a.orderedCommits(context.Background(), reversed); err != errBackwards {
		t.Fatalf("backwards accepted: %v", err)
	}
	path := "/api/comparisons/" + k.Owner + "/" + k.Repo + "/" + reversed.Old + "/" + reversed.New + "?attr=" + k.Attr
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	future := gitCommit(t, source, "Backdated descendant", "2025-12-01T12:00:00Z")
	clone := filepath.Join(a.root.Name(), "repos", k.Owner, k.Repo)
	repos, err = a.catalog(context.Background())
	if err != nil || repos[0].MetadataError != "" {
		t.Fatal(err, repos)
	}
	if got := gitRun(t, clone, "rev-parse", "refs/heads/main"); got != next {
		t.Fatal("fetched without a new snapshot")
	}
	addSnapshot(t, a, k, future)
	repos, err = a.catalog(context.Background())
	if err != nil || repos[0].MetadataError != "" {
		t.Fatal(err, repos)
	}
	if repos[0].Commits[0].SHA != future {
		t.Fatal("timestamp sorted descendant backwards")
	}
	if got := gitRun(t, clone, "rev-parse", "refs/heads/main"); got != future {
		t.Fatal("did not fetch new snapshot")
	}
	k.Old = next
	k.New = future
	if _, _, err = a.orderedCommits(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join("repos", k.Owner, k.Repo, "nix-diffs-state.json")
	raw, err := a.root.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state gitState
	if json.Unmarshal(raw, &state) != nil || len(state.Seen) != 3 {
		t.Fatal("snapshot observation state not persisted")
	}
	restarted, err := newApp(a.root.Name(), a.dix)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.root.Close()
	restarted.repoSource = a.repoSource
	originalGit := restarted.git
	restarted.git = "/no/git/available"
	repos, err = restarted.catalog(context.Background())
	if err != nil || repos[0].MetadataError != "" || repos[0].Commits[0].SHA != future {
		t.Fatal("restart did not reuse metadata", repos, err)
	}
	restarted.git = originalGit
}
func TestOnlyConfigurations(t *testing.T) {
	a, k, _ := fixture(t)
	other := k
	other.Attr = "x86_64-linux.disko-eule"
	addSnapshot(t, a, other, k.New)
	attrs, err := a.attributes(k.Owner, k.Repo, k.New)
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs) != 1 {
		t.Fatal("included non-configuration", attrs)
	}
}
