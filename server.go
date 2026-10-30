package main

// HTTP API + embedded web UI. One benchmark at a time on purpose: parallel runs would skew each other.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var safeID = regexp.MustCompile(`^[\w.-]{1,80}$`)

type server struct {
	dir     string
	mu      sync.Mutex
	current *Runner
}

func (s *server) runsDir() string     { return filepath.Join(s.dir, "runs") }
func (s *server) datasetsDir() string { return filepath.Join(s.dir, "datasets") }

func saveRun(path string) func(*Run) {
	return func(r *Run) {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, r.JSON(), 0o644) == nil {
			os.Rename(tmp, path)
		}
	}
}

func newServer(dir string) (*server, error) {
	s := &server{dir: dir}
	for _, d := range []string{s.runsDir(), s.datasetsDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// runs left "running" by a crashed/restarted pod
	files, _ := filepath.Glob(filepath.Join(s.runsDir(), "*.json"))
	for _, f := range files {
		if r, err := readRun(f); err == nil && r.Status == "running" {
			r.Status, r.Error = "failed", ptr("Pod yeniden başladı, test yarıda kaldı")
			saveRun(f)(r)
		}
	}
	return s, nil
}

func readRun(path string) (*Run, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &Run{}
	return r, json.Unmarshal(b, r)
}

func (s *server) active() *Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

func (s *server) load(id string) (*Run, error) {
	if cur := s.active(); cur != nil && cur.run.ID == id {
		return cur.run, nil
	}
	if !safeID.MatchString(id) {
		return nil, errNotFound
	}
	r, err := readRun(filepath.Join(s.runsDir(), id+".json"))
	if err != nil {
		return nil, errNotFound
	}
	return r, nil
}

var errNotFound = errors.New("test bulunamadı")

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"detail": msg})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if b, ok := v.([]byte); ok {
		w.Write(b)
		return
	}
	json.NewEncoder(w).Encode(v)
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { reply(w, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(mustRead("web/index.html"))
	})
	mux.HandleFunc("GET /static/plotly.min.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(mustRead("web/plotly.min.js"))
	})
	mux.HandleFunc("POST /api/resolve", s.resolve)
	mux.HandleFunc("POST /api/runs", s.start)
	mux.HandleFunc("GET /api/runs", s.list)
	mux.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		run, err := s.load(r.PathValue("id"))
		if err != nil {
			fail(w, 404, err.Error())
			return
		}
		reply(w, run.JSON())
	})
	mux.HandleFunc("POST /api/runs/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		cur := s.active()
		if cur == nil || cur.run.ID != r.PathValue("id") || cur.run.status() != "running" {
			fail(w, 409, "Bu test çalışmıyor")
			return
		}
		cur.Stop()
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("DELETE /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if cur := s.active(); cur != nil && cur.run.ID == id && cur.run.status() == "running" {
			fail(w, 409, "Çalışan test silinemez, önce durdurun")
			return
		}
		if _, err := s.load(id); err != nil {
			fail(w, 404, err.Error())
			return
		}
		os.Remove(filepath.Join(s.runsDir(), id+".json"))
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/runs/{id}/report", func(w http.ResponseWriter, r *http.Request) {
		s.report(w, []string{r.PathValue("id")})
	})
	mux.HandleFunc("GET /api/compare", func(w http.ResponseWriter, r *http.Request) {
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		s.report(w, ids[:min(len(ids), 8)])
	})
	mux.HandleFunc("GET /api/datasets", s.datasets)
	mux.HandleFunc("POST /api/datasets", s.upload)
	return mux
}

func (s *server) resolve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint Endpoint `json:"endpoint"`
	}
	req.Endpoint = Endpoint{Port: 8080, Scheme: "http", VerifyTLS: true}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, 400, "geçersiz JSON")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := probe(ctx, req.Endpoint)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	reply(w, res)
}

func (s *server) start(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	cfg, err := parseConfig(body)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && s.current.run.status() == "running" {
		fail(w, 409, "Zaten çalışan bir test var: "+s.current.run.Name)
		return
	}
	id := newRunID()
	s.current = NewRunner(id, cfg, s.dir, saveRun(filepath.Join(s.runsDir(), id+".json")))
	s.current.save(s.current.run)
	go s.current.Run(context.Background())
	reply(w, map[string]string{"id": id})
}

func newRunID() string {
	b := make([]byte, 2)
	rand.Read(b)
	return time.Now().Format("20060102-150405-") + hex.EncodeToString(b)
}

func (s *server) list(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Status      string   `json:"status"`
		Created     string   `json:"created"`
		Model       string   `json:"model"`
		StagesDone  int      `json:"stages_done"`
		StagesTotal int      `json:"stages_total"`
		PeakOut     *float64 `json:"peak_out_tok_per_s"`
		BestTTFT    *float64 `json:"best_ttft_p50_ms"`
	}
	files, _ := filepath.Glob(filepath.Join(s.runsDir(), "*.json"))
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	out := []item{}
	for _, f := range files {
		run, err := s.load(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil {
			continue
		}
		var r Run
		json.Unmarshal(run.JSON(), &r) // consistent snapshot of a possibly-running run
		it := item{ID: r.ID, Name: r.Name, Status: r.Status, Created: r.Created, Model: r.Config.Model,
			StagesDone: len(r.Stages), StagesTotal: len(r.Config.Load.Levels)}
		if r.Resolved != nil {
			it.Model = r.Resolved.Model
		}
		for _, st := range r.Stages {
			if st.TTFT == nil {
				continue
			}
			if it.PeakOut == nil || st.OutTokPerS > *it.PeakOut {
				it.PeakOut = ptr(st.OutTokPerS)
			}
			if it.BestTTFT == nil || st.TTFT.P50 < *it.BestTTFT {
				it.BestTTFT = ptr(st.TTFT.P50)
			}
		}
		out = append(out, it)
	}
	reply(w, out)
}

func (s *server) report(w http.ResponseWriter, ids []string) {
	var runs [][]byte
	for _, id := range ids {
		if id == "" {
			continue
		}
		run, err := s.load(id)
		if err != nil {
			fail(w, 404, err.Error())
			return
		}
		runs = append(runs, run.JSON())
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(renderReport(runs))
}

func (s *server) datasets(w http.ResponseWriter, _ *http.Request) {
	type ds struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	files, _ := filepath.Glob(filepath.Join(s.datasetsDir(), "*.jsonl"))
	out := []ds{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		out = append(out, ds{filepath.Base(f), bytes.Count(bytes.TrimSpace(b), []byte("\n")) + 1})
	}
	reply(w, out)
}

var unsafeChars = regexp.MustCompile(`[^\w.-]`)

func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	const limit = 50 << 20
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		fail(w, 400, "dosya okunamadı (en fazla 50 MB)")
		return
	}
	defer file.Close()
	name := unsafeChars.ReplaceAllString(strings.TrimSuffix(filepath.Base(hdr.Filename), filepath.Ext(hdr.Filename)), "_")
	if name == "" {
		name = "data"
	}
	name = name[:min(len(name), 60)] + ".jsonl"
	var buf bytes.Buffer
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1<<20), limit)
	n := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		n++
		var row struct {
			Prompt   *string `json:"prompt"`
			Messages []any   `json:"messages"`
		}
		if json.Unmarshal(line, &row) != nil || (row.Prompt == nil && len(row.Messages) == 0) {
			fail(w, 400, "Satır "+itoa(n)+": 'prompt' (metin) veya 'messages' (liste) alanı olan JSON olmalı")
			return
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if sc.Err() != nil || n == 0 {
		fail(w, 400, "Dosya boş veya okunamadı")
		return
	}
	if err := os.WriteFile(filepath.Join(s.datasetsDir(), name), buf.Bytes(), 0o644); err != nil {
		fail(w, 500, err.Error())
		return
	}
	reply(w, map[string]any{"name": name, "count": n})
}
