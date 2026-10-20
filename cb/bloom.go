package cb

import (
	"encoding/binary"
	"math"
)

// Bloom filter: answers "definitely not here" so reads skip SSTables that
// cannot contain the key - the classic LSM read-path optimization.
//
// Optimal sizing: m = -n*ln(p) / (ln2)^2 bits, k = m/n * ln2 hashes.

type bloom struct {
	bits []uint64
	k    int
}

func newBloom(expected int, falsePositiveRate float64) *bloom {
	if expected < 1 {
		expected = 1
	}
	m := int(-float64(expected) * math.Log(falsePositiveRate) / (math.Ln2 * math.Ln2))
	k := int(float64(m) / float64(expected) * math.Ln2)
	if k < 1 {
		k = 1
	}
	words := (m + 63) / 64
	return &bloom{bits: make([]uint64, words), k: k}
}

// double hashing (Kirsch-Mitzenmacher): h_i = h1 + i*h2, FNV-1a split.
func (b *bloom) hashes(key string) (uint64, uint64) {
	var h1 uint64 = 1469598103934665603
	for i := 0; i < len(key); i++ {
		h1 ^= uint64(key[i])
		h1 *= 1099511628211
	}
	h2 := h1>>33 | h1<<31
	return h1, h2
}

func (b *bloom) add(key string) {
	h1, h2 := b.hashes(key)
	n := uint64(len(b.bits) * 64)
	for i := 0; i < b.k; i++ {
		h := (h1 + uint64(i)*h2) % n
		b.bits[h/64] |= 1 << (h % 64)
	}
}

func (b *bloom) maybeContains(key string) bool {
	h1, h2 := b.hashes(key)
	n := uint64(len(b.bits) * 64)
	for i := 0; i < b.k; i++ {
		h := (h1 + uint64(i)*h2) % n
		if b.bits[h/64]&(1<<(h%64)) == 0 {
			return false
		}
	}
	return true
}

func (b *bloom) marshal() []byte {
	out := make([]byte, 8+8*len(b.bits))
	binary.LittleEndian.PutUint64(out, uint64(b.k))
	for i, w := range b.bits {
		binary.LittleEndian.PutUint64(out[8+8*i:], w)
	}
	return out
}

func unmarshalBloom(data []byte) *bloom {
	b := &bloom{k: int(binary.LittleEndian.Uint64(data))}
	b.bits = make([]uint64, (len(data)-8)/8)
	for i := range b.bits {
		b.bits[i] = binary.LittleEndian.Uint64(data[8+8*i:])
	}
	return b
}
