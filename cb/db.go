package cb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// DB is an LSM-tree key-value store.
//
// Writes: WAL append -> memtable (skiplist). Memtable full -> flush to an L0
// SSTable -> reset WAL. L0 files piling up -> compact (k-way merge) into L1,
// dropping shadowed keys and tombstones. Reads: memtable -> L0 (newest first)
// -> L1, with a bloom filter per table so most misses cost ~1 microsecond.

var ErrClosed = errors.New("cipherbase: db closed")

const (
	flushThresholdBytes = 1 << 20 // 1 MiB memtable -> flush
	l0CompactionTrigger = 4       // this many L0 files -> compact into L1
)

type DB struct {
	dir      string
	mu       sync.RWMutex
	mem      *skiplist
	wal      *wal
	l0       []*sstable // newest last
	l1       []*sstable
	nextFile int
	closed   bool

	// stats, surfaced on the dashboard
	Stats struct {
		Puts, Gets, Dels     int64
		Flushes, Compactions int64
		BloomSkips           int64
	}
}

type manifest struct {
	L0       []string `json:"l0"`
	L1       []string `json:"l1"`
	NextFile int      `json:"next_file"`
}

func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db := &DB{dir: dir, mem: newSkiplist(42), nextFile: 1}
	// restore manifest
	if data, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
		var m manifest
		if err := json.Unmarshal(data, &m); err == nil {
			db.nextFile = m.NextFile
			for _, p := range m.L0 {
				if t, err := openSSTable(filepath.Join(dir, p)); err == nil {
					db.l0 = append(db.l0, t)
				}
			}
			for _, p := range m.L1 {
				if t, err := openSSTable(filepath.Join(dir, p)); err == nil {
					db.l1 = append(db.l1, t)
				}
			}
		}
	}
	// replay WAL (writes after the last flush), then drop any corrupt or
	// torn tail so post-recovery writes stay visible to the next replay
	walPath := filepath.Join(dir, "wal.log")
	validLen, err := replayWAL(walPath, func(op byte, key string, val []byte) {
		if op == opDel {
			db.mem.set(key, nil, true)
		} else {
			db.mem.set(key, val, false)
		}
	})
	if err != nil && !errors.Is(err, errCorruptTail) {
		return nil, err
	}
	if fi, statErr := os.Stat(walPath); statErr == nil && fi.Size() > validLen {
		if err := os.Truncate(walPath, validLen); err != nil {
			return nil, err
		}
	}
	w, err := openWAL(walPath)
	if err != nil {
		return nil, err
	}
	db.wal = w
	return db, nil
}

func (db *DB) Put(key string, val []byte) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if err := db.wal.append(opPut, key, val); err != nil {
		return err
	}
	db.mem.set(key, val, false)
	db.Stats.Puts++
	if db.mem.size >= flushThresholdBytes {
		return db.flushLocked()
	}
	return nil
}

func (db *DB) Delete(key string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if err := db.wal.append(opDel, key, nil); err != nil {
		return err
	}
	db.mem.set(key, nil, true)
	db.Stats.Dels++
	return nil
}

func (db *DB) Get(key string) ([]byte, bool, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return nil, false, ErrClosed
	}
	db.Stats.Gets++
	if val, tomb, found := db.mem.get(key); found {
		return val, !tomb, nil
	}
	for i := len(db.l0) - 1; i >= 0; i-- { // newest L0 first
		val, tomb, found, err := db.l0[i].get(key)
		if err != nil {
			return nil, false, err
		}
		if !found {
			db.Stats.BloomSkips++
			continue
		}
		return val, !tomb, nil
	}
	for _, t := range db.l1 {
		val, tomb, found, err := t.get(key)
		if err != nil {
			return nil, false, err
		}
		if !found {
			db.Stats.BloomSkips++
			continue
		}
		return val, !tomb, nil
	}
	return nil, false, nil
}

// Scan returns up to limit live entries from key `start` onward, in key
// order. Merge is oldest-to-newest so newer data simply overwrites older:
// L1, then L0 oldest-first, then the memtable.
// Scan returns up to limit live entries with key >= start, in key order.
// K-way merge across the memtable, L0 (newest first) and L1: each source is
// positioned with its sparse index, so a bounded scan costs O(limit), not
// O(dataset). Newer sources win on duplicate keys; tombstones suppress.
func (db *DB) Scan(start string, limit int) ([]Entry, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	type src struct {
		cur  Entry
		rank int
		ok   bool
		it   *sstIter // nil for the memtable source
		mem  []Entry
		mi   int
	}
	advance := func(s *src) {
		if s.it != nil {
			if s.it.next() {
				s.cur = s.it.cur
			} else {
				s.ok = false
			}
			return
		}
		s.mi++
		if s.mi < len(s.mem) {
			s.cur = s.mem[s.mi]
		} else {
			s.ok = false
		}
	}

	var srcs []*src
	closeAll := func() {
		for _, s := range srcs {
			if s.it != nil {
				s.it.close()
			}
		}
	}

	m := &src{rank: 0}
	db.mem.iter(func(k string, v []byte, tomb bool) {
		if k >= start {
			m.mem = append(m.mem, Entry{Key: k, Val: v, Tomb: tomb})
		}
	})
	if len(m.mem) > 0 {
		m.cur, m.ok = m.mem[0], true
	}
	srcs = append(srcs, m)

	rank := 1
	addTable := func(t *sstable) error {
		it, err := t.iterFrom(start)
		if err != nil {
			return err
		}
		s := &src{rank: rank, it: it}
		if it.ok {
			s.cur, s.ok = it.cur, true
		}
		srcs = append(srcs, s)
		rank++
		return nil
	}
	for i := len(db.l0) - 1; i >= 0; i-- { // newest first
		if err := addTable(db.l0[i]); err != nil {
			closeAll()
			return nil, err
		}
	}
	for _, t := range db.l1 {
		if err := addTable(t); err != nil {
			closeAll()
			return nil, err
		}
	}
	defer closeAll()

	out := []Entry{}
	lastKey := ""
	haveLast := false
	for {
		best := -1
		for i, s := range srcs {
			if !s.ok {
				continue
			}
			if best == -1 || s.cur.Key < srcs[best].cur.Key ||
				(s.cur.Key == srcs[best].cur.Key && s.rank < srcs[best].rank) {
				best = i
			}
		}
		if best == -1 {
			break
		}
		e := srcs[best].cur
		advance(srcs[best])
		if haveLast && e.Key == lastKey {
			continue // shadowed by a newer source
		}
		haveLast, lastKey = true, e.Key
		if e.Tomb {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// flushLocked writes the memtable to a new L0 SSTable and resets the WAL.
func (db *DB) flushLocked() error {
	if db.mem.n == 0 {
		return nil
	}
	entries := make([]Entry, 0, db.mem.n)
	db.mem.iter(func(k string, v []byte, tomb bool) {
		entries = append(entries, Entry{Key: k, Val: v, Tomb: tomb})
	})
	name := fmt.Sprintf("L0-%06d.sst", db.nextFile)
	db.nextFile++
	t, err := writeSSTable(filepath.Join(db.dir, name), entries)
	if err != nil {
		return err
	}
	db.l0 = append(db.l0, t)
	db.mem = newSkiplist(42)
	if err := resetWAL(filepath.Join(db.dir, "wal.log")); err != nil {
		return err
	}
	db.Stats.Flushes++
	if err := db.saveManifestLocked(); err != nil {
		return err
	}
	if len(db.l0) >= l0CompactionTrigger {
		return db.compactLocked()
	}
	return nil
}

// compactLocked k-way merges all L0 files plus existing L1 into one new L1
// table, dropping shadowed keys and tombstones (they've done their job).
func (db *DB) compactLocked() error {
	merged := map[string]Entry{}
	for _, t := range db.l1 { // oldest first
		t.scanAll(func(e Entry) { merged[e.Key] = e })
	}
	for _, t := range db.l0 {
		t.scanAll(func(e Entry) { merged[e.Key] = e })
	}
	entries := make([]Entry, 0, len(merged))
	for _, e := range merged {
		if !e.Tomb {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	name := fmt.Sprintf("L1-%06d.sst", db.nextFile)
	db.nextFile++
	t, err := writeSSTable(filepath.Join(db.dir, name), entries)
	if err != nil {
		return err
	}
	old := append(append([]*sstable{}, db.l0...), db.l1...)
	db.l0 = nil
	db.l1 = []*sstable{t}
	for _, o := range old {
		os.Remove(o.path)
	}
	db.Stats.Compactions++
	return db.saveManifestLocked()
}

func (db *DB) saveManifestLocked() error {
	m := manifest{NextFile: db.nextFile}
	for _, t := range db.l0 {
		m.L0 = append(m.L0, filepath.Base(t.path))
	}
	for _, t := range db.l1 {
		m.L1 = append(m.L1, filepath.Base(t.path))
	}
	data, _ := json.Marshal(m)
	return os.WriteFile(filepath.Join(db.dir, "manifest.json"), data, 0o644)
}

// Flush forces a memtable flush (used by tests and the dashboard).
func (db *DB) Flush() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.flushLocked()
}

// Compact forces an L0+L1 compaction.
func (db *DB) Compact() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if len(db.l0) == 0 && len(db.l1) == 0 {
		return nil
	}
	return db.compactLocked()
}

// LevelInfo describes on-disk layout for the dashboard.
type LevelInfo struct {
	L0Files   int `json:"l0_files"`
	L1Files   int `json:"l1_files"`
	L0Entries int `json:"l0_entries"`
	L1Entries int `json:"l1_entries"`
}

func (db *DB) Levels() LevelInfo {
	db.mu.RLock()
	defer db.mu.RUnlock()
	info := LevelInfo{L0Files: len(db.l0), L1Files: len(db.l1)}
	for _, t := range db.l0 {
		info.L0Entries += t.count
	}
	for _, t := range db.l1 {
		info.L1Entries += t.count
	}
	return info
}

func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	return db.wal.close()
}
