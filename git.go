package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errBackwards = errors.New("Choose an older From commit and a newer To commit.")

type gitState struct {
	Version int                   `json:"version"`
	Seen    map[string]bool       `json:"seen_snapshots"`
	Commits map[string]commitInfo `json:"commits"`
}

func (a *app) gitCommand(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, a.git, append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	command.WaitDelay = 2 * time.Second
	out, stderr := &limitedBuffer{limit: 64 << 20}, &limitedBuffer{limit: 64 << 10}
	command.Stdout = out
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

func (a *app) syncRepository(ctx context.Context, r *repository) error {
	key := r.Owner + "/" + r.Name
	a.mu.Lock()
	lock := a.repoLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		a.repoLocks[key] = lock
	}
	a.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	rel := filepath.Join("repos", r.Owner, r.Name)
	absolute, err := filepath.Abs(filepath.Join(a.root.Name(), rel))
	if err != nil {
		return err
	}
	stateFile := filepath.Join(rel, "nix-diffs-state.json")
	state := gitState{}
	if data, err := a.read(stateFile, maxSnapshot); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if state.Version != 1 || state.Seen == nil || state.Commits == nil {
		state = gitState{Version: 1, Seen: map[string]bool{}, Commits: map[string]commitInfo{}}
	}
	fresh := false
	for _, id := range r.SnapshotIDs {
		if !state.Seen[id] {
			fresh = true
			break
		}
	}
	repoRoot, err := a.root.OpenRoot(rel)
	if errors.Is(err, fs.ErrNotExist) {
		if err = a.root.MkdirAll(filepath.Dir(rel), 0755); err != nil {
			return err
		}
		parent, err := a.root.OpenRoot(filepath.Dir(rel))
		if err != nil {
			return err
		}
		parent.Close()
		temp, err := os.MkdirTemp(filepath.Dir(absolute), ".clone-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(temp)
		source := "https://github.com/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name) + ".git"
		if a.repoSource != nil {
			source = a.repoSource(r.Owner, r.Name)
		}
		if _, err = a.gitCommand(ctx, "clone", "--bare", "--filter=tree:0", "--no-tags", "--", source, temp); err != nil {
			return err
		}
		if err = os.Rename(temp, absolute); err != nil {
			return err
		}
		fresh = true
	} else if err != nil {
		return err
	} else {
		repoRoot.Close()
	}
	git := func(args ...string) ([]byte, error) {
		return a.gitCommand(ctx, append([]string{"--git-dir", absolute}, args...)...)
	}
	if fresh {
		if _, err = git("fetch", "--filter=tree:0", "--no-tags", "--prune", "origin", "+refs/heads/*:refs/heads/*"); err != nil {
			return err
		}
		for _, c := range r.Commits {
			raw, err := git("show", "-s", "--format=%ct%x00%an%x00%B", c.SHA, "--")
			if err != nil {
				if _, fetchErr := git("fetch", "--filter=tree:0", "--no-tags", "origin", c.SHA); fetchErr != nil {
					return fetchErr
				}
				raw, err = git("show", "-s", "--format=%ct%x00%an%x00%B", c.SHA, "--")
				if err != nil {
					return err
				}
			}
			fields := strings.SplitN(strings.TrimSpace(string(raw)), "\x00", 3)
			if len(fields) != 3 {
				return errors.New("invalid Git commit metadata")
			}
			seconds, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				return err
			}
			c.Date = time.Unix(seconds, 0).UTC()
			c.Author = fields[1]
			c.Body = strings.TrimSpace(fields[2])
			c.Message = strings.SplitN(c.Body, "\n", 2)[0]
			branches, err := git("for-each-ref", "--contains="+c.SHA, "--format=%(refname:short)", "refs/heads")
			if err != nil {
				return err
			}
			c.Branches = []string{}
			for _, branch := range strings.Split(strings.TrimSpace(string(branches)), "\n") {
				if branch != "" {
					c.Branches = append(c.Branches, branch)
				}
			}
			state.Commits[c.SHA] = c
		}
		args := []string{"rev-list", "--date-order"}
		for _, c := range r.Commits {
			args = append(args, c.SHA)
		}
		if len(r.Commits) > 0 {
			history, err := git(args...)
			if err != nil {
				return err
			}
			for rank, sha := range strings.Fields(string(history)) {
				if c, ok := state.Commits[sha]; ok {
					c.Order = rank
					state.Commits[sha] = c
				}
			}
		}
		for _, id := range r.SnapshotIDs {
			state.Seen[id] = true
		}
		data, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if err = a.root.WriteFile(stateFile+".tmp", data, 0644); err != nil {
			return err
		}
		if err = a.root.Rename(stateFile+".tmp", stateFile); err != nil {
			return err
		}
	}
	for i, c := range r.Commits {
		metadata, ok := state.Commits[c.SHA]
		if !ok {
			return fmt.Errorf("commit metadata missing for %s", c.SHA)
		}
		metadata.Count = c.Count
		r.Commits[i] = metadata
	}
	sort.Slice(r.Commits, func(i, j int) bool { return r.Commits[i].Order < r.Commits[j].Order })
	return nil
}

func (a *app) catalog(ctx context.Context) ([]repository, error) {
	repos, err := a.scanCatalog()
	if err != nil {
		return nil, err
	}
	visible := []repository{}
	for i := range repos {
		if err := a.syncRepository(ctx, &repos[i]); err != nil {
			slog.Warn("Git metadata unavailable", "repository", repos[i].Owner+"/"+repos[i].Name, "error", err)
			repos[i].MetadataError = "Git metadata unavailable. Try refreshing."
		}
		if len(repos[i].Commits) > 0 {
			visible = append(visible, repos[i])
		}
	}
	return visible, nil
}

func (a *app) orderedCommits(ctx context.Context, k requestKey) (commitInfo, commitInfo, error) {
	if k.Old == k.New {
		return commitInfo{}, commitInfo{}, errBackwards
	}
	repos, err := a.scanCatalog()
	if err != nil {
		return commitInfo{}, commitInfo{}, err
	}
	for _, repo := range repos {
		if repo.Owner != k.Owner || repo.Name != k.Repo {
			continue
		}
		var hasOld, hasNew bool
		for _, c := range repo.Commits {
			hasOld = hasOld || c.SHA == k.Old
			hasNew = hasNew || c.SHA == k.New
		}
		if !hasOld || !hasNew {
			return commitInfo{}, commitInfo{}, errNotReady
		}
		if err = a.syncRepository(ctx, &repo); err != nil {
			return commitInfo{}, commitInfo{}, err
		}
		var old, next commitInfo
		for _, c := range repo.Commits {
			if c.SHA == k.Old {
				old = c
			}
			if c.SHA == k.New {
				next = c
			}
		}
		if old.Order <= next.Order {
			return old, next, errBackwards
		}
		return old, next, nil
	}
	return commitInfo{}, commitInfo{}, errNotReady
}
