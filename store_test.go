package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const sampleReport = `{"diffs":[],"paths":{"old":1,"new":1,"added":0,"removed":0},"size_old":10,"size_new":20}`

func fixture(t *testing.T) (*app, requestKey, string) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	dix := filepath.Join(dir, "dix")
	script := fmt.Sprintf("#!/bin/sh\nset -eu\n[ \"$1\" = diff-snapshots ]\n[ \"$4\" = --output ]\n[ \"$5\" = json ]\ncat \"$2\" \"$3\" >/dev/null\nprintf x >> '%s'\nsleep 0.05\nprintf '%%s' '%s'\n", counter, sampleReport)
	if err := os.WriteFile(dix, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(filepath.Join(dir, "data"), dix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.root.Close() })
	k := requestKey{"anna-oake", "nixos-config", strings.Repeat("a", 40), strings.Repeat("b", 40), "aarch64-darwin.darwin-eule"}
	for _, commit := range []string{k.Old, k.New} {
		name := k.snapshot(commit)
		if err := a.root.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := a.root.WriteFile(name, []byte(`{"snapshot":"`+commit+`"}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	state := gitState{Version: 1, Seen: map[string]bool{}, Commits: map[string]commitInfo{}}
	for i, sha := range []string{k.New, k.Old} {
		state.Seen[sha+"/"+k.Attr+".json"] = true
		state.Commits[sha] = commitInfo{SHA: sha, Message: "Test commit", Order: i}
	}
	rel := filepath.Join("repos", k.Owner, k.Repo)
	a.root.MkdirAll(rel, 0755)
	stateData, _ := json.Marshal(state)
	a.root.WriteFile(filepath.Join(rel, "nix-diffs-state.json"), stateData, 0644)
	return a, k, counter
}
func calls(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(data)
}
func TestCacheReuseAndInvalidation(t *testing.T) {
	a, k, counter := fixture(t)
	for i := 0; i < 2; i++ {
		entry, hit, err := a.compare(context.Background(), k)
		if err != nil {
			t.Fatal(err)
		}
		if hit != (i == 1) || !validReport(entry.Report) {
			t.Fatalf("unexpected cache result %v", hit)
		}
	}
	if calls(t, counter) != 1 {
		t.Fatal("cache did not prevent execution")
	}
	if err := a.root.WriteFile(k.snapshot(k.New), []byte(`{"updated":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := a.compare(context.Background(), k); err != nil || hit {
		t.Fatalf("changed snapshot reused: %v", err)
	}
	a.engine = "different-dix"
	if _, hit, err := a.compare(context.Background(), k); err != nil || hit {
		t.Fatalf("changed engine reused: %v", err)
	}
	if err := a.root.WriteFile(k.cache(), []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := a.compare(context.Background(), k); err != nil || hit {
		t.Fatalf("corrupt cache reused: %v", err)
	}
	if calls(t, counter) != 4 {
		t.Fatal("wrong invocation count")
	}
	var cached cacheEntry
	data, _ := a.root.ReadFile(k.cache())
	if json.Unmarshal(data, &cached) != nil || !validReport(cached.Report) {
		t.Fatal("invalid persistent cache")
	}
}
func TestConcurrentRequestsShareExecution(t *testing.T) {
	a, k, counter := fixture(t)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, _, err := a.compare(context.Background(), k); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls(t, counter) != 1 {
		t.Fatal("duplicate dix invocations")
	}
}
func TestMissingSnapshotDoesNotCacheAndRecovers(t *testing.T) {
	a, k, counter := fixture(t)
	data, _ := a.root.ReadFile(k.snapshot(k.New))
	a.root.Remove(k.snapshot(k.New))
	path := "/api/comparisons/" + k.Owner + "/" + k.Repo + "/" + k.Old + "/" + k.New + "?attr=" + k.Attr
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	if calls(t, counter) != 0 {
		t.Fatal("executed without snapshot")
	}
	if _, err := a.root.Stat(k.cache()); !os.IsNotExist(err) {
		t.Fatal("cached missing snapshot")
	}
	a.root.WriteFile(k.snapshot(k.New), data, 0644)
	w = httptest.NewRecorder()
	a.handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 || w.Header().Get("X-Diff-Cache") != "MISS" {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestDixFailureNotCached(t *testing.T) {
	a, k, _ := fixture(t)
	os.WriteFile(a.dix, []byte("#!/bin/sh\necho broken >&2\nexit 1\n"), 0755)
	if _, _, err := a.compare(context.Background(), k); err == nil {
		t.Fatal("failure ignored")
	}
	if _, err := a.root.Stat(k.cache()); !os.IsNotExist(err) {
		t.Fatal("cached a failure")
	}
}
func TestContainment(t *testing.T) {
	a, k, _ := fixture(t)
	bad := k
	bad.Attr = "../../outside"
	if _, _, err := a.compare(context.Background(), bad); err == nil {
		t.Fatal("accepted traversal")
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	os.WriteFile(outside, []byte(`{}`), 0644)
	a.root.Remove(k.snapshot(k.New))
	if err := a.root.Symlink(outside, k.snapshot(k.New)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.compare(context.Background(), k); err == nil {
		t.Fatal("followed external symlink")
	}
}
func TestCatalogAndAttributeUnion(t *testing.T) {
	a, k, _ := fixture(t)
	other := k
	other.Attr = "x86_64-linux.nixos-builder"
	a.root.WriteFile(other.snapshot(k.New), []byte(`{}`), 0644)
	repos, err := a.scanCatalog()
	if err != nil || len(repos) != 1 || len(repos[0].Commits) != 2 {
		t.Fatalf("catalog: %+v %v", repos, err)
	}
	attrs, err := a.comparisonAttributes(k.Owner, k.Repo, k.Old, k.New)
	if err != nil || len(attrs) != 2 {
		t.Fatal(attrs, err)
	}
	for _, attr := range attrs {
		if attr.Name == "builder" && (attr.Old || !attr.New) {
			t.Fatal("wrong readiness")
		}
		if attr.Name == "eule" && (!attr.Old || !attr.New || attr.Kind != "Darwin") {
			t.Fatal("wrong configuration")
		}
	}
}
func TestCancelledRequestDoesNotCache(t *testing.T) {
	a, k, counter := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := a.compare(ctx, k); err == nil {
		t.Fatal("cancel ignored")
	}
	if calls(t, counter) != 0 {
		t.Fatal("started cancelled command")
	}
	if _, err := a.root.Stat(k.cache()); !os.IsNotExist(err) {
		t.Fatal("cached cancellation")
	}
}

func TestCacheSurvivesRestart(t *testing.T) {
	a, k, counter := fixture(t)
	if _, _, err := a.compare(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	restarted, err := newApp(a.root.Name(), a.dix)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.root.Close()
	if _, hit, err := restarted.compare(context.Background(), k); err != nil || !hit {
		t.Fatalf("persistent cache missed: %v", err)
	}
	if calls(t, counter) != 1 {
		t.Fatal("re-executed after restart")
	}
}
func TestRootRouting(t *testing.T) {
	a, k, _ := fixture(t)
	handler := a.handler()
	for path, want := range map[string]int{"/": 200, "/healthz": 404, "/diff": 404, "/diff/": 404, "/static/style.css": 200, "/static/app.js": 200, "/static/fonts/light-94a07e06a1-v2.woff2": 200, "/api/repos": 200, "/" + k.Owner + "/" + k.Repo + "/" + k.Old + "/" + k.New: 200} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("%s: got %d, want %d", path, w.Code, want)
		}
	}
}

func TestComparisonHostsCountsSortAndFilter(t *testing.T) {
	a, k, _ := fixture(t)
	if err := os.WriteFile(a.dix, []byte("#!/bin/sh\ncat \"$2\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id, report string
		missing    bool
	}{
		{k.Attr, `{"diffs":[{"name":"nixos-system-test"}]}`, false},
		{"x86_64-linux.nixos-small", `{"diffs":[{"name":"curl"}]}`, false},
		{"x86_64-linux.nixos-large", `{"diffs":[{"name":"nixos-system-large"},{"name":"curl"},{"name":"git"}]}`, false},
		{"x86_64-linux.nixos-pending", `{"diffs":[]}`, true},
	}
	for _, c := range cases {
		key := k
		key.Attr = c.id
		for _, sha := range []string{k.Old, k.New} {
			if c.missing && sha == k.New {
				continue
			}
			if err := a.root.WriteFile(key.snapshot(sha), []byte(strings.TrimSuffix(c.report, "}")+`,"paths":{},"size_old":0,"size_new":0}`), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	hosts, err := a.comparisonHosts(context.Background(), k)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 3 {
		t.Fatalf("got %d hosts, want 3", len(hosts))
	}
	if hosts[0].Name != "large" || hosts[1].Name != "small" || hosts[2].Name != "pending" {
		t.Fatalf("wrong order: %+v", hosts)
	}
	if *hosts[0].Changes != 3 || *hosts[1].Changes != 1 || hosts[2].Changes != nil {
		t.Fatalf("wrong counts: %+v", hosts)
	}
}

func TestCatalogResponseLimits(t *testing.T) {
	for _, total := range []int{0, 3, 5, 6, 50, 75} {
		commits := make([]commitInfo, total)
		for i := range commits {
			commits[i].SHA = fmt.Sprint(i)
		}
		repos := limitCatalog([]repository{{Commits: commits}})
		encoded, err := json.Marshal(repos)
		if err != nil {
			t.Fatal(err)
		}
		var decoded []repository
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		r := decoded[0]
		if len(r.Commits) != min(total, 50) || len(r.RecentCommits) != min(total, 5) {
			t.Fatalf("total %d: got %d selector commits and %d homepage commits", total, len(r.Commits), len(r.RecentCommits))
		}
		for i, c := range r.Commits {
			if c.SHA != fmt.Sprint(i) {
				t.Fatal("commit ordering changed")
			}
		}
	}
}
