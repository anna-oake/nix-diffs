package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSnapshotUnionDeduplicates(t *testing.T) {
	var union snapshotUnion
	for _, root := range []string{"/nix/store/host-a", "/nix/store/host-b"} {
		snapshot := snapshotFile{Schema: 1, Root: root, Closure: []snapshotPath{{root, 10}, {"/nix/store/shared", 20}}, Selected: []string{"/nix/store/shared"}}
		data, _ := json.Marshal(snapshot)
		if err := union.add(data); err != nil {
			t.Fatal(err)
		}
	}
	data, err := union.encode()
	if err != nil {
		t.Fatal(err)
	}
	again, _ := union.encode()
	if !bytes.Equal(data, again) {
		t.Fatal("unstable aggregate encoding")
	}
	var snapshot snapshotFile
	json.Unmarshal(data, &snapshot)
	if len(snapshot.Closure) != 3 || len(snapshot.Selected) != 1 {
		t.Fatalf("not deduplicated: %+v", snapshot)
	}
	var size int64
	for _, p := range snapshot.Closure {
		size += p.Size
	}
	if size != 40 {
		t.Fatalf("shared size counted twice: %d", size)
	}
	bad := snapshotFile{Schema: 1, Root: "/nix/store/shared", Closure: []snapshotPath{{"/nix/store/shared", 21}}}
	encoded, _ := json.Marshal(bad)
	if union.add(encoded) == nil {
		t.Fatal("accepted conflicting sizes")
	}
}
