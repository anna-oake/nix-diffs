package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

type snapshotPath struct {
	Path string `json:"path"`
	Size int64  `json:"nar_size"`
}
type snapshotFile struct {
	Schema   int            `json:"schema_version"`
	Root     string         `json:"root"`
	Closure  []snapshotPath `json:"closure"`
	Selected []string       `json:"selected"`
}
type snapshotUnion struct {
	root     string
	paths    map[string]int64
	selected map[string]bool
}

func (u *snapshotUnion) add(data []byte) error {
	var snapshot snapshotFile
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	if snapshot.Schema != 1 || snapshot.Root == "" {
		return fmt.Errorf("invalid snapshot schema or root")
	}
	if u.paths == nil {
		u.paths = map[string]int64{}
		u.selected = map[string]bool{}
		u.root = snapshot.Root
	}
	foundRoot := false
	present := map[string]bool{}
	for _, p := range snapshot.Closure {
		if p.Path == snapshot.Root {
			foundRoot = true
		}
		if p.Size < 0 || present[p.Path] {
			return fmt.Errorf("invalid snapshot path %s", p.Path)
		}
		present[p.Path] = true
		if size, exists := u.paths[p.Path]; exists && size != p.Size {
			return fmt.Errorf("conflicting sizes for %s", p.Path)
		}
		u.paths[p.Path] = p.Size
	}
	if !foundRoot {
		return fmt.Errorf("snapshot root missing from closure")
	}
	for _, path := range snapshot.Selected {
		if !present[path] {
			return fmt.Errorf("selected path missing from closure")
		}
		u.selected[path] = true
	}
	return nil
}
func (u *snapshotUnion) encode() ([]byte, error) {
	if u.root == "" {
		return nil, errNotReady
	}
	snapshot := snapshotFile{Schema: 1, Root: u.root, Closure: []snapshotPath{}, Selected: []string{}}
	for path, size := range u.paths {
		snapshot.Closure = append(snapshot.Closure, snapshotPath{path, size})
	}
	for path := range u.selected {
		snapshot.Selected = append(snapshot.Selected, path)
	}
	sort.Slice(snapshot.Closure, func(i, j int) bool { return snapshot.Closure[i].Path < snapshot.Closure[j].Path })
	sort.Strings(snapshot.Selected)
	data, err := json.Marshal(snapshot)
	if len(data) > maxSnapshot {
		return nil, fmt.Errorf("combined snapshot exceeds size limit")
	}
	return data, err
}
func (a *app) combinedSnapshots(k requestKey) ([]byte, []byte, int, error) {
	attrs, err := a.comparisonAttributes(k.Owner, k.Repo, k.Old, k.New)
	if err != nil {
		return nil, nil, 0, err
	}
	var old, next snapshotUnion
	pending := 0
	for _, attr := range attrs {
		if !attr.Old || !attr.New {
			pending++
			continue
		}
		key := k
		key.Attr = attr.ID
		for _, item := range []struct {
			sha   string
			union *snapshotUnion
		}{{k.Old, &old}, {k.New, &next}} {
			data, err := a.read(key.snapshot(item.sha), maxSnapshot)
			if err != nil {
				return nil, nil, pending, err
			}
			if err := item.union.add(data); err != nil {
				return nil, nil, pending, err
			}
		}
	}
	before, err := old.encode()
	if err != nil {
		return nil, nil, pending, err
	}
	after, err := next.encode()
	return before, after, pending, err
}
