package crdt

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDbgReencode(t *testing.T) {
	dir := fixtureDir(t)
	d := NewDoc()
	ApplyUpdate(d, readBin(t, dir, "edits/base.snapshot.bin"))
	entries, _ := os.ReadDir(filepath.Join(dir, "edits"))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".update" {
			ApplyUpdate(d, readBin(t, dir, "edits/"+e.Name()))
		}
	}
	snap, _ := Snapshot(d)
	fmt.Printf("snapshot len=%d\n", len(snap))
	d2 := NewDoc()
	if err := ApplyUpdate(d2, readBin(t, dir, "edits/merged_apply.bin")); err != nil {
		t.Fatalf("reapply: %v", err)
	}
	sv, _ := StateVector(d2)
	fmt.Printf("sv=% x\n", sv)
	meta := d2.GetMap("meta")
	for _, k := range meta.Keys() {
		v, _ := meta.Get(k)
		fmt.Printf("meta[%q]=%.40v\n", k, v)
	}
}
