package cb

import (
	"bufio"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
)

// Write-ahead log: every mutation is appended (with a CRC) before it touches
// the memtable, so a crash loses nothing - replay rebuilds state on startup.
//
// Record layout: [crc32 u32][op u8][keyLen u16][valLen u32][key][value]

const (
	opPut = byte(1)
	opDel = byte(2)
)

type wal struct {
	f  *os.File
	bw *bufio.Writer
}

func openWAL(path string) (*wal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	return &wal{f: f, bw: bufio.NewWriter(f)}, nil
}

func (w *wal) append(op byte, key string, val []byte) error {
	var hdr [11]byte
	hdr[0] = 0 // placeholder
	binary.LittleEndian.PutUint16(hdr[5:7], uint16(len(key)))
	binary.LittleEndian.PutUint32(hdr[7:11], uint32(len(val)))
	hdr[4] = op
	payload := make([]byte, 0, len(key)+len(val))
	payload = append(payload, key...)
	payload = append(payload, val...)
	crc := crc32.ChecksumIEEE(append(hdr[4:], payload...))
	binary.LittleEndian.PutUint32(hdr[0:4], crc)
	if _, err := w.bw.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.bw.Write(payload); err != nil {
		return err
	}
	return w.bw.Flush()
}

var errCorrupt = errors.New("wal: corrupt record")

// replay calls fn for each intact record and stops at the first corrupt or
// torn tail (the expected outcome of a crash mid-append).
func replayWAL(path string, fn func(op byte, key string, val []byte)) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	for {
		var hdr [11]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return nil // clean EOF or torn header: stop replay
		}
		crc := binary.LittleEndian.Uint32(hdr[0:4])
		op := hdr[4]
		klen := int(binary.LittleEndian.Uint16(hdr[5:7]))
		vlen := int(binary.LittleEndian.Uint32(hdr[7:11]))
		if klen > 1<<20 || vlen > 1<<30 {
			return nil // implausible lengths: torn record
		}
		payload := make([]byte, klen+vlen)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil // torn tail
		}
		if crc32.ChecksumIEEE(append(hdr[4:], payload...)) != crc {
			return errCorruptTail
		}
		fn(op, string(payload[:klen]), payload[klen:])
	}
}

var errCorruptTail = errors.New("wal: corrupt tail (crash stop)")

func (w *wal) close() error { return w.f.Close() }

func resetWAL(path string) error { return os.Truncate(path, 0) }
