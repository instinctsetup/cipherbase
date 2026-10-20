package cb

import (
	"bufio"
	"encoding/binary"
	"os"
	"sort"
)

// Sorted String Table: an immutable sorted file. Layout:
//   data:   [klen u16][vlen u32][tomb u8][key][value] ... (sorted by key)
//   index:  [klen u16][offset u64][key] ...               (sparse: every 16th key)
//   bloom:  marshal()
//   footer: [indexOff u64][bloomOff u64][count u64]

const indexEvery = 16

type Entry struct {
	Key  string
	Val  []byte
	Tomb bool
}

type indexEntry struct {
	key    string
	offset uint64
}

type sstable struct {
	path     string
	index    []indexEntry
	bloom    *bloom
	first    string
	last     string
	count    int
	indexOff uint64 // data section ends here
}

func writeSSTable(path string, entries []Entry) (*sstable, error) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	bl := newBloom(len(entries), 0.01)
	var index []indexEntry
	offset := uint64(0)
	for i, e := range entries {
		bl.add(e.Key)
		if i%indexEvery == 0 {
			index = append(index, indexEntry{key: e.Key, offset: offset})
		}
		var hdr [7]byte
		binary.LittleEndian.PutUint16(hdr[0:2], uint16(len(e.Key)))
		binary.LittleEndian.PutUint32(hdr[2:6], uint32(len(e.Val)))
		if e.Tomb {
			hdr[6] = 1
		}
		w.Write(hdr[:])
		w.WriteString(e.Key)
		w.Write(e.Val)
		offset += uint64(7 + len(e.Key) + len(e.Val))
	}
	indexOff := offset
	for _, ie := range index {
		var hdr [10]byte
		binary.LittleEndian.PutUint16(hdr[0:2], uint16(len(ie.key)))
		binary.LittleEndian.PutUint64(hdr[2:10], ie.offset)
		w.Write(hdr[:])
		w.WriteString(ie.key)
		offset += uint64(10 + len(ie.key))
	}
	bloomOff := offset
	bdata := bl.marshal()
	w.Write(bdata)
	offset += uint64(len(bdata))
	var footer [24]byte
	binary.LittleEndian.PutUint64(footer[0:8], indexOff)
	binary.LittleEndian.PutUint64(footer[8:16], bloomOff)
	binary.LittleEndian.PutUint64(footer[16:24], uint64(len(entries)))
	w.Write(footer[:])
	if err := w.Flush(); err != nil {
		return nil, err
	}
	t := &sstable{path: path, index: index, bloom: bl, count: len(entries), indexOff: indexOff}
	if len(entries) > 0 {
		t.first = entries[0].Key
		t.last = entries[len(entries)-1].Key
	}
	return t, nil
}

func openSSTable(path string) (*sstable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var footer [24]byte
	if _, err := f.ReadAt(footer[:], st.Size()-24); err != nil {
		return nil, err
	}
	indexOff := int64(binary.LittleEndian.Uint64(footer[0:8]))
	bloomOff := int64(binary.LittleEndian.Uint64(footer[8:16]))
	count := int(binary.LittleEndian.Uint64(footer[16:24]))

	index := []indexEntry{}
	r := bufio.NewReader(f)
	if _, err := f.Seek(indexOff, 0); err != nil {
		return nil, err
	}
	r.Reset(f)
	pos := indexOff
	for pos < bloomOff {
		var hdr [10]byte
		if _, err := readFullAt(r, hdr[:]); err != nil {
			return nil, err
		}
		klen := int(binary.LittleEndian.Uint16(hdr[0:2]))
		off := binary.LittleEndian.Uint64(hdr[2:10])
		key := make([]byte, klen)
		if _, err := readFullAt(r, key); err != nil {
			return nil, err
		}
		index = append(index, indexEntry{key: string(key), offset: off})
		pos += int64(10 + klen)
	}
	bdata := make([]byte, st.Size()-24-bloomOff)
	if _, err := f.ReadAt(bdata, bloomOff); err != nil {
		return nil, err
	}
	return &sstable{path: path, index: index, bloom: unmarshalBloom(bdata), count: count, indexOff: uint64(indexOff)}, nil
}

func readFullAt(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// get: bloom check -> sparse index -> one ranged scan of the data block.
func (t *sstable) get(key string) (val []byte, tomb, found bool, err error) {
	if t.count == 0 || !t.bloom.maybeContains(key) {
		return nil, false, false, nil
	}
	if key < t.firstKey() {
		return nil, false, false, nil
	}
	if last := t.lastKey(); last != "" && key > last {
		return nil, false, false, nil
	}
	// binary search the sparse index for the last entry <= key
	lo := 0
	for lo+1 < len(t.index) && t.index[lo+1].key <= key {
		lo++
	}
	f, err := os.Open(t.path)
	if err != nil {
		return nil, false, false, err
	}
	defer f.Close()
	if _, err := f.Seek(int64(t.index[lo].offset), 0); err != nil {
		return nil, false, false, err
	}
	r := bufio.NewReader(f)
	pos := t.index[lo].offset
	for pos < t.indexOff {
		var hdr [7]byte
		if _, err := readFullAt(r, hdr[:]); err != nil {
			return nil, false, false, nil // ran off the block
		}
		pos += 7
		klen := int(binary.LittleEndian.Uint16(hdr[0:2]))
		vlen := int(binary.LittleEndian.Uint32(hdr[2:6]))
		isTomb := hdr[6] == 1
		kbuf := make([]byte, klen)
		if _, err := readFullAt(r, kbuf); err != nil {
			return nil, false, false, err
		}
		pos += uint64(klen)
		k := string(kbuf)
		if k > key {
			return nil, false, false, nil
		}
		vbuf := make([]byte, vlen)
		if _, err := readFullAt(r, vbuf); err != nil {
			return nil, false, false, err
		}
		pos += uint64(vlen)
		if k == key {
			return vbuf, isTomb, true, nil
		}
	}
	return nil, false, false, nil
}

func (t *sstable) firstKey() string {
	if t.first != "" {
		return t.first
	}
	if len(t.index) > 0 {
		return t.index[0].key
	}
	return ""
}

func (t *sstable) lastKey() string {
	// "" means unknown (table reopened from disk); bloom guards those calls.
	return t.last
}

// scanAll streams every entry in order (used by compaction).
func (t *sstable) scanAll(fn func(Entry)) error {
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	read := int64(0)
	limit := int64(t.indexOff)
	for read < limit {
		var hdr [7]byte
		n, err := readFullAt(r, hdr[:])
		read += int64(n)
		if err != nil {
			return err
		}
		klen := int(binary.LittleEndian.Uint16(hdr[0:2]))
		vlen := int(binary.LittleEndian.Uint32(hdr[2:6]))
		isTomb := hdr[6] == 1
		kbuf := make([]byte, klen)
		n, err = readFullAt(r, kbuf)
		read += int64(n)
		if err != nil {
			return err
		}
		vbuf := make([]byte, vlen)
		n, err = readFullAt(r, vbuf)
		read += int64(n)
		if err != nil {
			return err
		}
		fn(Entry{Key: string(kbuf), Val: vbuf, Tomb: isTomb})
	}
	return nil
}

// sstIter streams entries from an SSTable starting at the first key >= start,
// seeking via the sparse index instead of scanning from the top of the file.
type sstIter struct {
	t   *sstable
	f   *os.File
	r   *bufio.Reader
	pos uint64
	cur Entry
	ok  bool
}

func (t *sstable) iterFrom(start string) (*sstIter, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	it := &sstIter{t: t, f: f}
	lo := -1
	for lo+1 < len(t.index) && t.index[lo+1].key <= start {
		lo++
	}
	if lo >= 0 {
		it.pos = t.index[lo].offset
	}
	if _, err := f.Seek(int64(it.pos), 0); err != nil {
		f.Close()
		return nil, err
	}
	it.r = bufio.NewReader(f)
	for it.next() {
		if it.cur.Key >= start {
			return it, nil
		}
	}
	return it, nil // exhausted: it.ok == false
}

func (it *sstIter) next() bool {
	if it.pos >= it.t.indexOff {
		it.ok = false
		return false
	}
	var hdr [7]byte
	if _, err := readFullAt(it.r, hdr[:]); err != nil {
		it.ok = false
		return false
	}
	it.pos += 7
	klen := int(binary.LittleEndian.Uint16(hdr[0:2]))
	vlen := int(binary.LittleEndian.Uint32(hdr[2:6]))
	tomb := hdr[6] == 1
	kbuf := make([]byte, klen)
	if _, err := readFullAt(it.r, kbuf); err != nil {
		it.ok = false
		return false
	}
	it.pos += uint64(klen)
	vbuf := make([]byte, vlen)
	if _, err := readFullAt(it.r, vbuf); err != nil {
		it.ok = false
		return false
	}
	it.pos += uint64(vlen)
	it.cur = Entry{Key: string(kbuf), Val: vbuf, Tomb: tomb}
	it.ok = true
	return true
}

func (it *sstIter) close() {
	if it.f != nil {
		it.f.Close()
	}
}
