package main

// Fake vLLM OpenAI server for demos and tests: streaming chat, batch-size slowdown,
// a scheduler queue (max_num_seqs / KV cache) and vLLM-style /metrics.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type mockServer struct {
	model  string
	speed  float64 // >1 = slower GPU
	maxSeq int
	kvCap  int

	mu               sync.Mutex
	cond             *sync.Cond
	running, waiting int
	kv               int
	hist             map[string][2]float64 // name -> sum, count
}

func newMock(model string, speed float64) *mockServer {
	m := &mockServer{model: model, speed: speed, maxSeq: 32, kvCap: 60_000, hist: map[string][2]float64{}}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *mockServer) observe(name string, d time.Duration) {
	h := m.hist[name]
	m.hist[name] = [2]float64{h[0] + d.Seconds(), h[1] + 1}
}

func (m *mockServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": m.model, "max_model_len": 8192}}})
	})
	mux.HandleFunc("POST /v1/chat/completions", m.chat)
	mux.HandleFunc("GET /metrics", m.metrics)
	return mux
}

func (m *mockServer) chat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages  []struct{ Content string } `json:"messages"`
		MaxTokens int                        `json:"max_tokens"`
		IgnoreEOS bool                       `json:"ignore_eos"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MaxTokens < 1 {
		http.Error(w, `{"error":"bad request"}`, 400)
		return
	}
	words := 0
	for _, msg := range body.Messages {
		words += len(strings.Fields(msg.Content))
	}
	pTok := int(float64(words)*1.3) + 10
	n := body.MaxTokens
	if !body.IgnoreEOS {
		n = max(1, n/2)
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")

	t0 := time.Now()
	m.mu.Lock()
	m.waiting++
	for m.running >= m.maxSeq || m.kv+pTok+n > m.kvCap {
		m.cond.Wait()
	}
	m.waiting--
	m.running++
	m.kv += pTok + n
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running--
		m.kv -= pTok + n
		m.cond.Broadcast()
		m.mu.Unlock()
	}()
	tq := time.Now()
	step := func(base float64) time.Duration { // slows down as the batch grows, like a real GPU
		m.mu.Lock()
		f := 1 + float64(m.running)/8
		m.mu.Unlock()
		return time.Duration(m.speed * base * f * float64(time.Second))
	}
	time.Sleep(step(0.00005 * float64(pTok))) // prefill
	tp := time.Now()
	for i := 0; i < n; i++ {
		if i > 0 {
			time.Sleep(step(0.004)) // decode
		}
		if r.Context().Err() != nil {
			return
		}
		fmt.Fprintf(w, `data: {"id":"chatcmpl-mock","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"content":"tok "},"finish_reason":null}]}`+"\n\n", m.model)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprintf(w, `data: {"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`+"\n\ndata: [DONE]\n\n", pTok, n)
	te := time.Now()
	m.mu.Lock()
	m.observe("request_queue_time_seconds", tq.Sub(t0))
	m.observe("request_prefill_time_seconds", tp.Sub(tq))
	m.observe("request_decode_time_seconds", te.Sub(tp))
	m.observe("time_to_first_token_seconds", tp.Sub(t0))
	m.observe("e2e_request_latency_seconds", te.Sub(t0))
	m.mu.Unlock()
}

func (m *mockServer) metrics(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lab := fmt.Sprintf(`{model_name=%q}`, m.model)
	fmt.Fprintf(w, "vllm:num_requests_running%s %d\nvllm:num_requests_waiting%s %d\nvllm:kv_cache_usage_perc%s %g\n",
		lab, m.running, lab, m.waiting, lab, float64(m.kv)/float64(m.kvCap))
	for name, h := range m.hist {
		fmt.Fprintf(w, "vllm:%s_sum%s %g\nvllm:%s_count%s %g\n", name, lab, h[0], name, lab, h[1])
	}
}
