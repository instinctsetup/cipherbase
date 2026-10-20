package cb

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return db, dir
}

func TestPutGetDelete(t *testing.T) {
	db, _ := openTemp(t)
	defer db.Close()
	db.Put("name", []byte("cipherbase"))
	v, ok, _ := db.Get("name")
	if !ok || string(v) != "cipherbase" {
		t.Fatalf("got %q %v", v, ok)
	}
	db.Delete("name")
	if _, ok, _ := db.Get("name"); ok {
		t.Fatal("deleted key must not read back")
	}
}

func TestOverwriteNewestWins(t *testing.T) {
	db, _ := openTemp(t)
	defer db.Close()
	db.Put("k", []byte("old"))
	db.Flush()
	db.Put("k", []byte("new"))
	v, ok, _ := db.Get("k")
	if !ok || string(v) != "new" {
		t.Fatalf("memtable must shadow SSTable, got %q", v)
	}
	db.Flush()
	v, _, _ = db.Get("k")
	if string(v) != "new" {
		t.Fatalf("newer SSTable must shadow older, got %q", v)
	}
}

func TestCrashRecoveryReplaysWAL(t *testing.T) {
	db, dir := openTemp(t)
	for i := 0; i < 100; i++ {
		db.Put(fmt.Sprintf("key-%03d", i), []byte(fmt.Sprintf("val-%d", i)))
	}
	// simulate a crash: do NOT Close (no clean shutdown), just drop the handle
	db2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	v, ok, _ := db2.Get("key-042")
	if !ok || string(v) != "val-42" {
		t.Fatalf("WAL replay lost the write: %q %v", v, ok)
	}
}

func TestTornWALTailTolerated(t *testing.T) {
	db, dir := openTemp(t)
	db.Put("good", []byte("data"))
	db.Close()
	// corrupt the tail as a crash mid-append would
	walPath := filepath.Join(dir, "wal.log")
	st, _ := os.Stat(walPath)
	os.Truncate(walPath, st.Size()-3)
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("torn WAL tail must not prevent recovery: %v", err)
	}
	defer db2.Close()
}

func TestFlushCompactionAndShadowing(t *testing.T) {
	db, _ := openTemp(t)
	defer db.Close()
	// 4 L0 files trigger compaction into L1
	for round := 0; round < 4; round++ {
		for i := 0; i < 10; i++ {
			db.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf("r%d", round)))
		}
		if err := db.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	lv := db.Levels()
	if lv.L0Files != 0 || lv.L1Files != 1 {
		t.Fatalf("expected compaction into one L1 file, got %+v", lv)
	}
	if lv.L1Entries != 10 {
		t.Fatalf("compaction must drop shadowed keys: %d entries", lv.L1Entries)
	}
	for i := 0; i < 10; i++ {
		v, ok, _ := db.Get(fmt.Sprintf("k%d", i))
		if !ok || string(v) != "r3" {
			t.Fatalf("k%d: want r3 got %q", i, v)
		}
	}
}

func TestTombstonesDroppedAtCompaction(t *testing.T) {
	db, dir := openTemp(t)
	defer db.Close()
	db.Put("dead", []byte("x"))
	db.Flush()
	db.Delete("dead")
	db.Flush()
	db.Put("alive", []byte("y"))
	db.Flush()
	db.Put("alive2", []byte("z"))
	db.Flush() // 4th L0 -> compact
	ents, _ := db.Scan("", 0)
	for _, e := range ents {
		if e.Key == "dead" {
			t.Fatal("tombstoned key survived compaction")
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.sst"))
	if len(files) != 1 {
		t.Fatalf("old SSTables must be removed, found %d", len(files))
	}
}

func TestScanOrder(t *testing.T) {
	db, _ := openTemp(t)
	defer db.Close()
	keys := []string{"pear", "apple", "kiwi", "banana"}
	for _, k := range keys {
		db.Put(k, []byte(k))
	}
	db.Flush()
	db.Put("apricot", []byte("apricot"))
	ents, _ := db.Scan("a", 0)
	got := []string{}
	for _, e := range ents {
		got = append(got, e.Key)
	}
	want := []string{"apple", "apricot", "banana", "kiwi", "pear"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("scan order: got %v want %v", got, want)
	}
}

func TestBloomFilterFalsePositiveRate(t *testing.T) {
	b := newBloom(10000, 0.01)
	for i := 0; i < 10000; i++ {
		b.add(fmt.Sprintf("present-%d", i))
	}
	// no false negatives allowed, ever
	for i := 0; i < 10000; i++ {
		if !b.maybeContains(fmt.Sprintf("present-%d", i)) {
			t.Fatal("false negative: bloom filters must never do that")
		}
	}
	fp := 0
	const probes = 20000
	for i := 0; i < probes; i++ {
		if b.maybeContains(fmt.Sprintf("absent-%d", i)) {
			fp++
		}
	}
	rate := float64(fp) / probes
	if rate > 0.03 {
		t.Fatalf("false positive rate %.2f%% exceeds 3%% (target 1%%)", rate*100)
	}
	t.Logf("measured false positive rate: %.2f%%", rate*100)
}

func TestSkiplistOrderingAndScale(t *testing.T) {
	s := newSkiplist(7)
	for i := 9999; i >= 0; i-- {
		s.set(fmt.Sprintf("k%05d", i), []byte("v"), false)
	}
	prev := ""
	count := 0
	s.iter(func(k string, v []byte, tomb bool) {
		if k < prev {
			t.Fatalf("iteration out of order at %q after %q", k, prev)
		}
		prev = k
		count++
	})
	if count != 10000 {
		t.Fatalf("want 10000 entries, got %d", count)
	}
	if _, _, found := s.get("k05000"); !found {
		t.Fatal("lookup failed")
	}
}

func TestWritesAfterCorruptTailRecoverySurviveSecondCrash(t *testing.T) {
	db, dir := openTemp(t)
	db.Put("before", []byte("1"))
	db.Close()
	// corrupt the tail as a crash mid-append would
	walPath := filepath.Join(dir, "wal.log")
	st, _ := os.Stat(walPath)
	os.Truncate(walPath, st.Size()-3)
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("torn WAL tail must not prevent recovery: %v", err)
	}
	// write more after recovery, then crash again without Close
	db2.Put("after", []byte("2"))
	db3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	if v, ok, _ := db3.Get("after"); !ok || string(v) != "2" {
		t.Fatalf("post-recovery write lost after second crash: %q %v", v, ok)
	}
}
