package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseAndDist(t *testing.T) {
	m := parseMetrics("# HELP x\nvllm:x{a=\"1\"} 2\nvllm:x{a=\"2\"} 3\nvllm:y 1.5e0\n")
	if fmt.Sprint(m["vllm:x"]) != "[2 3]" || m["vllm:y"][0] != 1.5 {
		t.Fatal(m)
	}
	if d := dist([]float64{1, 2, 3, 4}); d.P50 != 2.5 || d.P99 != 3.97 {
		t.Fatal(d)
	}
	if _, err := parseConfig([]byte(`{"endpoint":{"type":"service","name":"a"},"load":{"levels":[0]}}`)); err == nil {
		t.Fatal("level 0 must be rejected")
	}
}

func runAgainst(t *testing.T, url, cfgJSON string) *Run {
	t.Helper()
	cfg, err := parseConfig([]byte(fmt.Sprintf(cfgJSON, url)))
	if err != nil {
		t.Fatal(err)
	}
	rn := NewRunner("t", cfg, t.TempDir(), func(*Run) {})
	rn.Run(context.Background())
	if rn.run.Status != "done" {
		t.Fatalf("status %s: %v", rn.run.Status, *rn.run.Error)
	}
	return rn.run
}

// End-to-end against the built-in mock: calibration, ignore_eos, queueing, server metrics, report.
func TestEndToEndMock(t *testing.T) {
	srv := httptest.NewServer(newMock("mock-model", 1).handler())
	defer srv.Close()
	r := runAgainst(t, srv.URL, `{"endpoint":{"type":"url","url":"%s/v1"},
		"dataset":{"input_tokens":400,"output_tokens":40},
		"load":{"levels":[1,8,64],"requests_per_stage":40,"warmup":2},
		"slo":{"ttft_ms":300,"tpot_ms":15},"prometheus":{"enabled":false}}`)
	if r.Resolved.Model != "mock-model" {
		t.Fatal(r.Resolved)
	}
	if c := r.Calibration.TokensPerWord; c < 1.2 || c > 1.4 {
		t.Fatal("calibration", c)
	}
	s1, s8, s64 := r.Stages[0], r.Stages[1], r.Stages[2]
	if s1.OK != 40 || s1.OutTokensMean != 40 {
		t.Fatal("ignore_eos not honoured", s1.OK, s1.OutTokensMean)
	}
	if s1.InTokensMean < 350 || s1.InTokensMean > 450 {
		t.Fatal("synthetic ISL off target", s1.InTokensMean)
	}
	if s8.OutTokPerS < 3*s1.OutTokPerS {
		t.Fatal("batching should scale throughput", s1.OutTokPerS, s8.OutTokPerS)
	}
	if *s64.Server.QueueMs < 10 || *s64.Server.WaitingMax == 0 { // 64 > max_num_seqs 32
		t.Fatal("expected server queueing at c=64", *s64.Server.QueueMs)
	}
	if rec := recommend(r); rec == nil || rec.Level == 64 {
		t.Fatal("recommendation", rec)
	}
	if html := string(renderReport([][]byte{r.JSON(), r.JSON()})); !strings.Contains(html, `"stages":[`) || strings.Contains(html, "/*RUNS*/") {
		t.Fatal("report not rendered")
	}
}

// Measurement accuracy: server with exact TTFT 200 ms and ITL 20 ms; the client must not add more than a few ms at c=64.
func TestTimingAccuracy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"exact"}]}`)
			return
		}
		f := w.(http.Flusher)
		next := time.Now().Add(200 * time.Millisecond)
		for i := 0; i < 20; i++ {
			time.Sleep(time.Until(next))
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"tok "}}]}`+"\n\n")
			f.Flush()
			next = next.Add(20 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	r := runAgainst(t, srv.URL, `{"endpoint":{"type":"url","url":"%s"},"ignore_eos":false,
		"load":{"levels":[64],"requests_per_stage":128,"warmup":0},"prometheus":{"enabled":false}}`)
	s := r.Stages[0]
	if s.TTFT.P99 > 215 || s.TTFT.P50 < 199 || s.ITL.P50 < 19 || s.ITL.P50 > 21.5 || s.TPOT.P99 > 22 {
		t.Fatalf("client overhead too high: ttft %+v itl %+v tpot %+v", *s.TTFT, *s.ITL, *s.TPOT)
	}
	if s.SteadyOutTokS == nil || *s.SteadyOutTokS < 0.8*s.OutTokPerS {
		t.Fatal("steady throughput", s.SteadyOutTokS, s.OutTokPerS)
	}
	t.Logf("TTFT p50/p99 %.1f/%.1f ms, ITL p50/p99 %.2f/%.2f ms, steady %.0f vs stage %.0f tok/s",
		s.TTFT.P50, s.TTFT.P99, s.ITL.P50, s.ITL.P99, *s.SteadyOutTokS, s.OutTokPerS)
}
