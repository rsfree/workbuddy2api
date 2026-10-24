package reqlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadAllArchiveBypassesPanelLimit(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "requests-2026-10-24.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	for i := 0; i < 1005; i++ {
		raw, _ := json.Marshal(Event{Time: time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second), Status: 200, OK: true})
		_, _ = w.Write(append(raw, '\n'))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Enabled: true, Dir: dir, RetentionDays: 7, MaxBytes: 1 << 20})
	defer r.Close()
	rows, err := r.ReadAllArchive(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1005 {
		t.Fatalf("rows=%d want 1005", len(rows))
	}
	page, err := r.ReadArchive(5000, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != defaultReadMax {
		t.Fatalf("panel page rows=%d want %d", len(page), defaultReadMax)
	}
}
