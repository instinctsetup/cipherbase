// cmd/bench measures sequential Put/Get/Scan throughput on a local DB.
// Method note: single goroutine, sequential calls, direct engine API (no HTTP),
// measured with time.Since on an Intel Xeon sandbox (Go 1.23 linux/amd64).
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"runtime"
	"time"

	"github.com/instinctsetup/cipherbase/cb"
)

const (
	flushMB = 1
	nWrite  = 200_000
	keySize = 16
	valSize = 100
	nRead   = 200_000
	nScan   = 1_000
	scanLen = 100
)

func fmtDurPerOp(total time.Duration, n int) string {
	return fmt.Sprintf("%.0f ns/op", float64(total.Nanoseconds())/float64(n))
}

func main() {
	dir, err := os.MkdirTemp("", "cipherbase-bench-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := cb.Open(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// random keys, fixed seed for reproducibility
	rng := rand.New(rand.NewPCG(42, 0))
	keys := make([]string, nWrite)
	for i := range keys {
		k := make([]byte, keySize)
		const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
		for j := range k {
			k[j] = alpha[rng.IntN(len(alpha))]
		}
		keys[i] = string(k)
	}
	val := make([]byte, valSize)
	for j := range val {
		val[j] = byte(rng.Uint32())
	}
	v := val

	// --- Write benchmark ---
	start := time.Now()
	for _, k := range keys {
		db.Put(k, v)
	}
	wdur := time.Since(start)
	runtime.GC()

	// --- Point-read benchmark (memtable + SSTable hit path) ---
	start = time.Now()
	for i := 0; i < nRead; i++ {
		db.Get(keys[rng.IntN(len(keys))])
	}
	rdur := time.Since(start)

	// --- Scan benchmark (100-key ranges) ---
	start = time.Now()

	for i := 0; i < nScan; i++ {
		from := keys[rng.IntN(len(keys))]
		ents, _ := db.Scan(from, scanLen)
		for _, e := range ents {
			_ = e

		}
	}
	sdur := time.Since(start)

	res := map[string]any{
		"platform":    runtime.GOOS + "/" + runtime.GOARCH + ", Go " + runtime.Version(),
		"method":      "single goroutine, sequential calls, in-process engine API (no HTTP), fixed seed",
		"write_total": fmt.Sprintf("%.2fs for %s keys (16B key, 100B value)", wdur.Seconds(), human(nWrite)),
		"write_ops":   fmt.Sprintf("%.0f writes/s", float64(nWrite)/wdur.Seconds()),
		"write_ns":    fmtDurPerOp(wdur, nWrite),
		"read_total":  fmt.Sprintf("%.2fs for %s point reads", rdur.Seconds(), human(nRead)),
		"read_ops":    fmt.Sprintf("%.0f reads/s", float64(nRead)/rdur.Seconds()),
		"read_ns":     fmtDurPerOp(rdur, nRead),
		"scan_total":  fmt.Sprintf("%.2fs for %d range scans of up to %d keys", sdur.Seconds(), nScan, scanLen),
		"scan_ns":     fmtDurPerOp(sdur, nScan),
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	if err := os.WriteFile("benchmarks.json", out, 0644); err != nil {
		log.Print(err)
	}
}

func human(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
