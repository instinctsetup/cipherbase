// cipherbase server: REST API + live dashboard over the storage engine.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/instinctsetup/cipherbase/cb"
)

//go:embed web
var webFS embed.FS

type server struct {
	db *cb.DB
}

func (s *server) handlePut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
		http.Error(w, `{"error":"need key and value"}`, http.StatusBadRequest)
		return
	}
	if err := s.db.Put(body.Key, []byte(body.Value)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	v, ok, err := s.db.Get(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"found": ok, "value": string(v)})
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if err := s.db.Delete(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleScan(w http.ResponseWriter, r *http.Request) {
	ents, err := s.db.Scan(r.URL.Query().Get("start"), 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := []map[string]string{}
	for _, e := range ents {
		out = append(out, map[string]string{"key": e.Key, "value": string(e.Val)})
	}
	writeJSON(w, map[string]any{"entries": out})
}

func (s *server) handleStats(w http.ResponseWriter, _ *http.Request) {
	lv := s.db.Levels()
	writeJSON(w, map[string]any{
		"levels": lv, "puts": s.db.Stats.Puts, "gets": s.db.Stats.Gets,
		"dels": s.db.Stats.Dels, "flushes": s.db.Stats.Flushes,
		"compactions": s.db.Stats.Compactions, "bloom_skips": s.db.Stats.BloomSkips,
	})
}

func (s *server) handleCompact(w http.ResponseWriter, _ *http.Request) {
	if err := s.db.Compact(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleFlush(w http.ResponseWriter, _ *http.Request) {
	if err := s.db.Flush(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleLoad writes N keys then reads M random keys, so the dashboard shows
// the memtable->L0->L1 flow live.
func (s *server) handleLoad(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Writes int `json:"writes"`
		Reads  int `json:"reads"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Writes <= 0 {
		body.Writes = 30000
	}
	if body.Reads <= 0 {
		body.Reads = 20000
	}
	go func() {
		for i := 0; i < body.Writes; i++ {
			k := "user:" + itoa(i%5000)
			s.db.Put(k, []byte("payload-"+itoa(i)))
			time.Sleep(0) // yield
		}
		for i := 0; i < body.Reads; i++ {
			s.db.Get("user:" + itoa((i*7)%7000)) // some hits, some misses
		}
	}()
	writeJSON(w, map[string]any{"started": true, "writes": body.Writes, "reads": body.Reads})
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[n:])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	addr := flag.String("addr", ":8090", "listen address")
	dir := flag.String("dir", "cipherbase-data", "data directory")
	flag.Parse()
	db, err := cb.Open(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	s := &server{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/put", s.handlePut)
	mux.HandleFunc("GET /api/get", s.handleGet)
	mux.HandleFunc("DELETE /api/delete", s.handleDelete)
	mux.HandleFunc("GET /api/scan", s.handleScan)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("POST /api/flush", s.handleFlush)
	mux.HandleFunc("POST /api/compact", s.handleCompact)
	mux.HandleFunc("POST /api/load", s.handleLoad)
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	log.Printf("cipherbase dashboard on http://localhost%s (data in %s)", *addr, *dir)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
