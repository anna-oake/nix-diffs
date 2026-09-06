package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const cacheSchema = 1
const maxSnapshot = 64 << 20

var componentPattern = regexp.MustCompile(`^[A-Za-z0-9_.%!-]+$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
var errNotReady = errors.New("snapshot not ready")

func component(s string) bool {
	return s != "" && s != "." && s != ".." && componentPattern.MatchString(s)
}
func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

type app struct {
	root       *os.Root
	dix        string
	engine     string
	slots      chan struct{}
	mu         sync.Mutex
	jobs       map[string]*flight
	git        string
	repoLocks  map[string]*sync.Mutex
	repoSource func(string, string) string
}
type flight struct {
	done  chan struct{}
	entry cacheEntry
	err   error
}
type cacheEntry struct {
	Schema    int             `json:"schema_version"`
	Engine    string          `json:"engine"`
	OldHash   string          `json:"old_snapshot_sha256"`
	NewHash   string          `json:"new_snapshot_sha256"`
	Generated time.Time       `json:"generated_at"`
	Report    json.RawMessage `json:"report"`
}
type requestKey struct{ Owner, Repo, Old, New, Attr string }

func (k requestKey) valid() bool {
	return component(k.Owner) && component(k.Repo) && commitPattern.MatchString(k.Old) && commitPattern.MatchString(k.New) && component(k.Attr)
}
func (k requestKey) snapshot(commit string) string {
	return filepath.Join("snapshots", k.Owner, k.Repo, commit, k.Attr+".json")
}
func (k requestKey) cache() string {
	return filepath.Join("diffs", k.Owner, k.Repo, k.Old, k.New, k.Attr+".json")
}

func newApp(data, dix string) (*app, error) {
	executable, err := exec.LookPath(dix)
	if err != nil {
		return nil, fmt.Errorf("find dix in PATH: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(executable)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	file.Close()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(data, 0755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(data)
	if err != nil {
		return nil, err
	}
	if err = root.MkdirAll("diffs", 0755); err != nil {
		root.Close()
		return nil, err
	}
	git, err := exec.LookPath("git")
	if err != nil {
		root.Close()
		return nil, err
	}
	if err = root.MkdirAll("repos", 0755); err != nil {
		root.Close()
		return nil, err
	}
	return &app{git: git, repoLocks: make(map[string]*sync.Mutex), root: root, dix: executable, engine: hex.EncodeToString(hash.Sum(nil)), slots: make(chan struct{}, 2), jobs: make(map[string]*flight)}, nil
}

func (a *app) read(name string, limit int64) ([]byte, error) {
	file, err := a.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("invalid or oversized file: %s", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}

func (a *app) compare(ctx context.Context, k requestKey) (cacheEntry, bool, error) {
	if !k.valid() {
		return cacheEntry{}, false, errors.New("invalid comparison path")
	}
	if k.Attr == "all" {
		old, next, _, err := a.combinedSnapshots(k)
		if err != nil {
			return cacheEntry{}, false, err
		}
		return a.compareInputs(ctx, k, old, next)
	}
	old, err := a.read(k.snapshot(k.Old), maxSnapshot)
	if errors.Is(err, fs.ErrNotExist) {
		return cacheEntry{}, false, errNotReady
	}
	if err != nil {
		return cacheEntry{}, false, err
	}
	next, err := a.read(k.snapshot(k.New), maxSnapshot)
	if errors.Is(err, fs.ErrNotExist) {
		return cacheEntry{}, false, errNotReady
	}
	if err != nil {
		return cacheEntry{}, false, err
	}
	return a.compareInputs(ctx, k, old, next)
}

func (a *app) compareInputs(ctx context.Context, k requestKey, old, next []byte) (cacheEntry, bool, error) {
	oldHash, newHash := digest(old), digest(next)
	var cached cacheEntry
	if data, err := a.read(k.cache(), maxSnapshot); err == nil && json.Unmarshal(data, &cached) == nil && cached.Schema == cacheSchema && cached.Engine == a.engine && cached.OldHash == oldHash && cached.NewHash == newHash && validReport(cached.Report) {
		return cached, true, nil
	}
	jobKey := k.cache() + ":" + oldHash + ":" + newHash
	a.mu.Lock()
	if existing := a.jobs[jobKey]; existing != nil {
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return cacheEntry{}, false, ctx.Err()
		case <-existing.done:
			return existing.entry, true, existing.err
		}
	}
	job := &flight{done: make(chan struct{})}
	a.jobs[jobKey] = job
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.jobs, jobKey); close(job.done); a.mu.Unlock() }()
	job.entry, job.err = a.generate(ctx, k, old, next, oldHash, newHash)
	return job.entry, false, job.err
}

func validReport(data []byte) bool {
	var r struct {
		Diffs *[]json.RawMessage `json:"diffs"`
		Paths *json.RawMessage   `json:"paths"`
		Old   *int64             `json:"size_old"`
		New   *int64             `json:"size_new"`
	}
	return json.Unmarshal(data, &r) == nil && r.Diffs != nil && r.Paths != nil && r.Old != nil && r.New != nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("dix output exceeds limit")
	}
	return b.Buffer.Write(p)
}

func (a *app) generate(ctx context.Context, k requestKey, old, next []byte, oldHash, newHash string) (cacheEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	case <-ctx.Done():
		return cacheEntry{}, ctx.Err()
	}
	dir, err := os.MkdirTemp("", "nix-diffs-")
	if err != nil {
		return cacheEntry{}, err
	}
	defer os.RemoveAll(dir)
	oldPath, newPath := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")
	if err = os.WriteFile(oldPath, old, 0600); err != nil {
		return cacheEntry{}, err
	}
	if err = os.WriteFile(newPath, next, 0600); err != nil {
		return cacheEntry{}, err
	}
	command := exec.CommandContext(ctx, a.dix, "diff-snapshots", oldPath, newPath, "--output", "json")
	command.WaitDelay = 2 * time.Second
	stdout, stderr := &limitedBuffer{limit: maxSnapshot}, &limitedBuffer{limit: 64 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	if err = command.Run(); err != nil {
		return cacheEntry{}, fmt.Errorf("dix failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if !validReport(stdout.Bytes()) {
		return cacheEntry{}, errors.New("dix returned an invalid JSON report")
	}
	entry := cacheEntry{cacheSchema, a.engine, oldHash, newHash, time.Now().UTC(), json.RawMessage(append([]byte(nil), stdout.Bytes()...))}
	data, err := json.Marshal(entry)
	if err != nil {
		return cacheEntry{}, err
	}
	directory := filepath.Dir(k.cache())
	if err = a.root.MkdirAll(directory, 0755); err != nil {
		return cacheEntry{}, err
	}
	temp := filepath.Join(directory, "."+digest([]byte(fmt.Sprintf("%s-%d", k.Attr, time.Now().UnixNano())))+".tmp")
	f, err := a.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return cacheEntry{}, err
	}
	defer a.root.Remove(temp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return cacheEntry{}, err
	}
	if err = a.root.Rename(temp, k.cache()); err != nil {
		return cacheEntry{}, err
	}
	return entry, nil
}

type attribute struct {
	Pending     int    `json:"pending,omitempty"`
	Changes     *int   `json:"changes"`
	CountFailed bool   `json:"countFailed,omitempty"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Kind        string `json:"kind"`
	Old         bool   `json:"old"`
	New         bool   `json:"new"`
}

func describe(id string) attribute {
	name, err := url.PathUnescape(id)
	if err != nil {
		name = id
	}
	platform, rest, found := strings.Cut(name, ".")
	if !found {
		rest = platform
		platform = ""
	}
	kind := "Other"
	for _, prefix := range []struct{ p, k string }{{"nixos-", "NixOS"}, {"darwin-", "Darwin"}, {"home-manager-", "Home Manager"}, {"home-", "Home Manager"}, {"hm-", "Home Manager"}} {
		if strings.HasPrefix(rest, prefix.p) {
			rest = strings.TrimPrefix(rest, prefix.p)
			kind = prefix.k
			break
		}
	}
	return attribute{ID: id, Name: rest, Platform: platform, Kind: kind}
}
func (a *app) attributes(owner, repo, commit string) (map[string]attribute, error) {
	dir := filepath.Join("snapshots", owner, repo, commit)
	entries, err := fs.ReadDir(a.root.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]attribute{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := map[string]attribute{}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			id := strings.TrimSuffix(entry.Name(), ".json")
			if component(id) && describe(id).Kind != "Other" {
				result[id] = describe(id)
			}
		}
	}
	return result, nil
}
func (a *app) comparisonAttributes(owner, repo, old, next string) ([]attribute, error) {
	before, err := a.attributes(owner, repo, old)
	if err != nil {
		return nil, err
	}
	after, err := a.attributes(owner, repo, next)
	if err != nil {
		return nil, err
	}
	for id, item := range before {
		item.Old = true
		_, item.New = after[id]
		before[id] = item
	}
	for id, item := range after {
		if _, ok := before[id]; !ok {
			item.New = true
			before[id] = item
		}
	}
	result := make([]attribute, 0, len(before))
	for _, item := range before {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		if (result[i].Kind == "Other") != (result[j].Kind == "Other") {
			return result[i].Kind != "Other"
		}
		if result[i].Name == result[j].Name {
			return result[i].ID < result[j].ID
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func (a *app) comparisonHosts(ctx context.Context, k requestKey) ([]attribute, error) {
	attrs, err := a.comparisonAttributes(k.Owner, k.Repo, k.Old, k.New)
	if err != nil {
		return nil, err
	}
	hidden := make([]bool, len(attrs))
	jobs := make(chan int, len(attrs))
	for i := range attrs {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for i := range jobs {
				attr := &attrs[i]
				if !attr.Old || !attr.New || ctx.Err() != nil {
					continue
				}
				key := k
				key.Attr = attr.ID
				entry, _, err := a.compare(ctx, key)
				if err == nil {
					var report struct {
						Diffs []struct {
							Name string `json:"name"`
						} `json:"diffs"`
					}
					err = json.Unmarshal(entry.Report, &report)
					if err == nil {
						count := len(report.Diffs)
						attr.Changes = &count
						hidden[i] = count == 1 && strings.HasPrefix(report.Diffs[0].Name, "nixos-system-")
					}
				}
				if err != nil {
					attr.CountFailed = true
					slog.Warn("Could not count package changes", "attribute", attr.ID, "error", err)
				}
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]attribute, 0, len(attrs))
	for i, attr := range attrs {
		if !hidden[i] {
			result = append(result, attr)
		}
	}
	count := func(a attribute) int {
		if a.Changes == nil {
			return -1
		}
		return *a.Changes
	}
	sort.SliceStable(result, func(i, j int) bool { return count(result[i]) > count(result[j]) })
	return result, nil
}

type commitInfo struct {
	SHA      string    `json:"sha"`
	Updated  time.Time `json:"-"`
	Date     time.Time `json:"date"`
	Message  string    `json:"message"`
	Body     string    `json:"body,omitempty"`
	Author   string    `json:"author"`
	Branches []string  `json:"branches"`
	Order    int       `json:"order"`
	Count    int       `json:"count"`
}
type repository struct {
	RecentCommits []commitInfo `json:"recent_commits,omitempty"`
	Owner         string       `json:"owner"`
	Name          string       `json:"name"`
	Commits       []commitInfo `json:"commits"`
	SnapshotIDs   []string     `json:"-"`
	MetadataError string       `json:"metadata_error,omitempty"`
}

func (a *app) scanCatalog() ([]repository, error) {
	result := []repository{}
	owners, err := fs.ReadDir(a.root.FS(), "snapshots")
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	for _, owner := range owners {
		if !owner.IsDir() || !component(owner.Name()) {
			continue
		}
		repos, err := fs.ReadDir(a.root.FS(), filepath.Join("snapshots", owner.Name()))
		if err != nil {
			return nil, err
		}
		for _, repo := range repos {
			if !repo.IsDir() || !component(repo.Name()) {
				continue
			}
			commits, err := fs.ReadDir(a.root.FS(), filepath.Join("snapshots", owner.Name(), repo.Name()))
			if err != nil {
				return nil, err
			}
			r := repository{Owner: owner.Name(), Name: repo.Name(), Commits: []commitInfo{}}
			for _, commit := range commits {
				if !commit.IsDir() || !commitPattern.MatchString(commit.Name()) {
					continue
				}
				dir := filepath.Join("snapshots", owner.Name(), repo.Name(), commit.Name())
				files, err := fs.ReadDir(a.root.FS(), dir)
				if err != nil {
					return nil, err
				}
				c := commitInfo{SHA: commit.Name()}
				for _, f := range files {
					if !f.Type().IsRegular() || !strings.HasSuffix(f.Name(), ".json") {
						continue
					}
					info, err := f.Info()
					if err != nil {
						return nil, err
					}
					r.SnapshotIDs = append(r.SnapshotIDs, commit.Name()+"/"+f.Name())
					if describe(strings.TrimSuffix(f.Name(), ".json")).Kind == "Other" {
						continue
					}
					c.Count++
					if info.ModTime().After(c.Updated) {
						c.Updated = info.ModTime()
					}
				}
				if c.Count > 0 {
					r.Commits = append(r.Commits, c)
				}
			}
			sort.Slice(r.Commits, func(i, j int) bool {
				if r.Commits[i].Updated.Equal(r.Commits[j].Updated) {
					return r.Commits[i].SHA < r.Commits[j].SHA
				}
				return r.Commits[i].Updated.After(r.Commits[j].Updated)
			})
			result = append(result, r)
		}
	}
	return result, nil
}
