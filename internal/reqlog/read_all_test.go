package reqlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

	for _, limit := range []int{-1, 0} {
		legacy, err := r.ReadArchive(limit, Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(legacy) != 200 {
			t.Fatalf("ReadArchive(%d) rows=%d want legacy default 200", limit, len(legacy))
		}
	}
}

func TestReadAllArchiveFlushesLiveQueue(t *testing.T) {
	r := New(Config{Enabled: true, Dir: t.TempDir(), RetentionDays: 7, MaxBytes: 1 << 20})
	defer r.Close()
	r.Record(Event{Time: time.Now(), Path: "/v1/test", Status: 200, OK: true})
	rows, err := r.ReadAllArchive(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "/v1/test" {
		t.Fatalf("live queue was not flushed before full read: %+v", rows)
	}
}

func TestReadAllArchiveContextCancelAndPrunedFile(t *testing.T) {
	r := New(Config{Enabled: true, Dir: t.TempDir(), RetentionDays: 7, MaxBytes: 1 << 20})
	defer r.Close()
	r.Record(Event{Time: time.Now(), Status: 200, OK: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ReadAllArchiveContext(ctx, Filter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled full read error=%v", err)
	}
	rows, err := readFile(context.Background(), filepath.Join(r.archive.cfg.Dir, "requests-pruned.jsonl"), Filter{})
	if err != nil || rows != nil {
		t.Fatalf("file pruned during scan must be skipped: rows=%v err=%v", rows, err)
	}
}
