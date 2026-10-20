package cb

import "math/rand"

// Skip list memtable: expected O(log n) insert/lookup, ordered iteration
// for free - exactly why real LSM engines (LevelDB, RocksDB) use one.

const maxLevel = 20

type slNode struct {
	key  string
	val  []byte
	tomb bool
	next []*slNode
}

type skiplist struct {
	head  *slNode
	level int
	size  int // bytes, approx
	n     int
	rnd   *rand.Rand
}

func newSkiplist(seed int64) *skiplist {
	return &skiplist{head: &slNode{next: make([]*slNode, maxLevel)}, rnd: rand.New(rand.NewSource(seed))}
}

func (s *skiplist) randomLevel() int {
	lvl := 1
	for lvl < maxLevel && s.rnd.Int31n(4) == 0 { // p = 1/4
		lvl++
	}
	return lvl
}

// set inserts or replaces. Returns approximate entry size.
func (s *skiplist) set(key string, val []byte, tomb bool) {
	update := make([]*slNode, maxLevel)
	x := s.head
	for i := s.level - 1; i >= 0; i-- {
		for x.next[i] != nil && x.next[i].key < key {
			x = x.next[i]
		}
		update[i] = x
	}
	next := x.next[0]
	if next != nil && next.key == key {
		s.size += len(val) - len(next.val)
		next.val, next.tomb = val, tomb
		return
	}
	lvl := s.randomLevel()
	if lvl > s.level {
		for i := s.level; i < lvl; i++ {
			update[i] = s.head
		}
		s.level = lvl
	}
	node := &slNode{key: key, val: val, tomb: tomb, next: make([]*slNode, lvl)}
	for i := 0; i < lvl; i++ {
		node.next[i] = update[i].next[i]
		update[i].next[i] = node
	}
	s.n++
	s.size += len(key) + len(val) + 24
}

func (s *skiplist) get(key string) (val []byte, tomb, found bool) {
	x := s.head
	for i := s.level - 1; i >= 0; i-- {
		for x.next[i] != nil && x.next[i].key < key {
			x = x.next[i]
		}
	}
	x = x.next[0]
	if x != nil && x.key == key {
		return x.val, x.tomb, true
	}
	return nil, false, false
}

// iter visits entries in sorted key order.
func (s *skiplist) iter(fn func(key string, val []byte, tomb bool)) {
	for x := s.head.next[0]; x != nil; x = x.next[0] {
		fn(x.key, x.val, x.tomb)
	}
}
