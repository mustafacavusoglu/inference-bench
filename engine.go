package main

// Benchmark engine: config, endpoint resolution, prompts, load generation,
// vLLM /metrics + DCGM scraping and statistics.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	saDir   = "/var/run/secrets/kubernetes.io/serviceaccount"
	kubeAPI = "https://kubernetes.default.svc"
	thanos  = "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"
)

var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// ---------------------------------------------------------------- config

type Endpoint struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Port       int    `json:"port"`
	Scheme     string `json:"scheme"`
	APIKey     string `json:"api_key,omitempty"`
	VerifyTLS  bool   `json:"verify_tls"`
	MetricsURL string `json:"metrics_url"`
}

type DatasetCfg struct {
	Type               string  `json:"type"`
	CustomName         string  `json:"custom_name"`
	InputTokens        int     `json:"input_tokens"`
	InputStd           float64 `json:"input_std"`
	OutputTokens       int     `json:"output_tokens"`
	OutputStd          float64 `json:"output_std"`
	SharedPrefixTokens int     `json:"shared_prefix_tokens"`
}

type LoadCfg struct {
	Mode             string    `json:"mode"`
	Levels           []float64 `json:"levels"`
	RequestsPerStage int       `json:"requests_per_stage"`
	MaxConcurrency   int       `json:"max_concurrency"`
	Burstiness       float64   `json:"burstiness"`
	Warmup           int       `json:"warmup"`
	TimeoutS         float64   `json:"timeout_s"`
}

type SLO struct {
	TTFTms float64 `json:"ttft_ms"`
	TPOTms float64 `json:"tpot_ms"`
}

type PromCfg struct {
	Enabled     bool   `json:"enabled"`
	URL         string `json:"url"`
	GPUSelector string `json:"gpu_selector"`
}

type Config struct {
	Name       string     `json:"name"`
	Endpoint   Endpoint   `json:"endpoint"`
	Model      string     `json:"model"`
	Dataset    DatasetCfg `json:"dataset"`
	Load       LoadCfg    `json:"load"`
	IgnoreEOS  bool       `json:"ignore_eos"`
	SLO        SLO        `json:"slo"`
	Prometheus PromCfg    `json:"prometheus"`
}

// parseConfig unmarshals over defaults, so omitted fields keep sensible values.
func parseConfig(b []byte) (Config, error) {
	c := Config{
		Endpoint:   Endpoint{Port: 8080, Scheme: "http", VerifyTLS: true},
		Dataset:    DatasetCfg{Type: "synthetic", InputTokens: 512, OutputTokens: 256},
		Load:       LoadCfg{Mode: "concurrency", Levels: []float64{1, 2, 4, 8, 16, 32, 64}, RequestsPerStage: 100, MaxConcurrency: 256, Burstiness: 1, Warmup: 4, TimeoutS: 300},
		IgnoreEOS:  true,
		SLO:        SLO{TTFTms: 2000, TPOTms: 100},
		Prometheus: PromCfg{Enabled: true},
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("config JSON: %w", err)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	in := func(v string, ok ...string) bool {
		for _, o := range ok {
			if v == o {
				return true
			}
		}
		return false
	}
	d, l := c.Dataset, c.Load
	switch {
	case !in(c.Endpoint.Type, "litellm", "service", "route", "pod", "url"):
		return errors.New("endpoint.type: litellm | service | route | pod | url")
	case c.Endpoint.Type == "litellm" && c.Model == "":
		return errors.New("LiteLLM için model adı gerekli")
	case c.Endpoint.Port < 1 || c.Endpoint.Port > 65535:
		return errors.New("endpoint.port 1-65535 olmalı")
	case !in(c.Endpoint.Scheme, "http", "https"):
		return errors.New("endpoint.scheme: http | https")
	case len(c.Name) > 100:
		return errors.New("name en fazla 100 karakter")
	case !in(d.Type, "synthetic", "sharegpt", "custom"):
		return errors.New("dataset.type: synthetic | sharegpt | custom")
	case d.InputTokens < 1 || d.InputTokens > 1_000_000 || d.OutputTokens < 1 || d.OutputTokens > 131_072:
		return errors.New("token sayıları aralık dışında")
	case d.InputStd < 0 || d.OutputStd < 0 || d.SharedPrefixTokens < 0 || d.SharedPrefixTokens > 1_000_000:
		return errors.New("sapma/önek negatif olamaz")
	case !in(l.Mode, "concurrency", "rate"):
		return errors.New("load.mode: concurrency | rate")
	case len(l.Levels) < 1 || len(l.Levels) > 50:
		return errors.New("1-50 arası seviye girin")
	case l.RequestsPerStage < 1 || l.RequestsPerStage > 100_000:
		return errors.New("requests_per_stage 1-100000 olmalı")
	case l.MaxConcurrency < 1 || l.MaxConcurrency > 4096:
		return errors.New("max_concurrency 1-4096 olmalı")
	case l.Burstiness <= 0 || l.Burstiness > 100 || l.Warmup < 0 || l.Warmup > 512:
		return errors.New("burstiness (0,100], warmup 0-512 olmalı")
	case l.TimeoutS <= 0 || l.TimeoutS > 3600:
		return errors.New("timeout_s (0,3600] olmalı")
	case c.SLO.TTFTms <= 0 || c.SLO.TPOTms <= 0:
		return errors.New("SLO değerleri pozitif olmalı")
	case strings.ContainsAny(c.Prometheus.GPUSelector, "{}"):
		return errors.New("gpu_selector süslü parantez içeremez")
	}
	for _, v := range l.Levels {
		if v <= 0 || v > 4096 {
			return errors.New("seviyeler 0 ile 4096 arasında olmalı")
		}
	}
	return nil
}

// ---------------------------------------------------------------- run document (JSON schema shared with the UI/report)

type Dist struct {
	Mean float64 `json:"mean"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
}

type ServerStats struct {
	QueueMs     *float64 `json:"queue_ms"`
	PrefillMs   *float64 `json:"prefill_ms"`
	DecodeMs    *float64 `json:"decode_ms"`
	TTFTms      *float64 `json:"ttft_ms"`
	E2Ems       *float64 `json:"e2e_ms"`
	PrefixHit   *float64 `json:"prefix_hit_pct"`
	Preemptions *float64 `json:"preemptions"`
	KVMax       *float64 `json:"kv_cache_max_pct"`
	KVMean      *float64 `json:"kv_cache_mean_pct"`
	WaitingMax  *float64 `json:"waiting_max"`
	RunningMax  *float64 `json:"running_max"`
}

type GPUStats struct {
	UtilMean  float64  `json:"util_mean"`
	UtilMax   float64  `json:"util_max"`
	MemMaxGB  *float64 `json:"mem_used_gb_max"`
	PowerMean *float64 `json:"power_w_mean"`
}

type ErrCount struct {
	Error string `json:"error"`
	Count int    `json:"count"`
}

type Stage struct {
	Level         float64      `json:"level"`
	Requests      int          `json:"requests"`
	OK            int          `json:"ok"`
	Errors        int          `json:"errors"`
	ErrorSamples  []ErrCount   `json:"error_samples"`
	DurationS     float64      `json:"duration_s"`
	ReqPerS       float64      `json:"req_per_s"`
	OutTokPerS    float64      `json:"out_tok_per_s"`
	SteadyOutTokS *float64     `json:"steady_out_tok_per_s"`
	InTokPerS     float64      `json:"in_tok_per_s"`
	TotalTokPerS  float64      `json:"total_tok_per_s"`
	InTokensMean  float64      `json:"in_tokens_mean"`
	OutTokensMean float64      `json:"out_tokens_mean"`
	TTFT          *Dist        `json:"ttft"`
	TTFO          *Dist        `json:"ttfo"`
	TPOT          *Dist        `json:"tpot"`
	ITL           *Dist        `json:"itl"`
	E2E           *Dist        `json:"e2e"`
	Decode        *Dist        `json:"decode"`
	ClientQueue   *Dist        `json:"client_queue"`
	UserTokPerS   *float64     `json:"user_tok_per_s"`
	GoodputPct    float64      `json:"goodput_pct"`
	GoodputRPS    float64      `json:"goodput_rps"`
	Samples       [][]any      `json:"samples"`
	TStart        float64      `json:"t_start"`
	TEnd          float64      `json:"t_end"`
	Server        *ServerStats `json:"server"`
	GPU           *GPUStats    `json:"gpu"`
}

type TSPoint struct {
	T        float64  `json:"t"`
	Inflight int64    `json:"inflight"`
	Running  *float64 `json:"running"`
	Waiting  *float64 `json:"waiting"`
	KV       *float64 `json:"kv"`
}

type GPUPoint struct {
	T      float64  `json:"t"`
	Util   *float64 `json:"util"`
	MemGB  *float64 `json:"mem_gb"`
	PowerW *float64 `json:"power_w"`
}

type Resolved struct {
	BaseURL     string  `json:"base_url"`
	MetricsURL  *string `json:"metrics_url"`
	Model       string  `json:"model"`
	MaxModelLen *int    `json:"max_model_len"`
	GPUs        *int    `json:"gpus,omitempty"`
}

type Calibration struct {
	TokensPerWord  float64 `json:"tokens_per_word"`
	OverheadTokens float64 `json:"overhead_tokens"`
}

type Progress struct {
	Phase string  `json:"phase"`
	Stage int     `json:"stage"`
	Total int     `json:"total"`
	Level float64 `json:"level"`
	Done  int     `json:"done"`
	Of    int     `json:"of"`
}

type Run struct {
	mu          sync.Mutex
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Status      string       `json:"status"`
	Created     string       `json:"created"`
	Finished    *string      `json:"finished"`
	Error       *string      `json:"error"`
	Warnings    []string     `json:"warnings"`
	Config      Config       `json:"config"`
	Resolved    *Resolved    `json:"resolved"`
	Calibration *Calibration `json:"calibration"`
	Progress    Progress     `json:"progress"`
	Stages      []Stage      `json:"stages"`
	Timeseries  []TSPoint    `json:"timeseries"`
	GPUSeries   []GPUPoint   `json:"gpu_series"`
}

// JSON snapshots the run under its lock (the runner mutates it concurrently).
func (r *Run) JSON() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := json.Marshal(r)
	return b
}

func (r *Run) status() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Status
}

func (r *Run) update(f func()) {
	r.mu.Lock()
	f()
	r.mu.Unlock()
}

func nowISO() string    { return time.Now().UTC().Format("2006-01-02T15:04:05+00:00") }
func ptr[T any](v T) *T { return &v }
func round(v float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(v*p) / p
}

// ---------------------------------------------------------------- endpoint resolution

func saToken() string {
	b, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func caClient(caFile string, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if pem, err := os.ReadFile(caFile); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func kubeGet(ctx context.Context, path string, out any) error {
	tok := saToken()
	if tok == "" {
		return errors.New("küme dışında çalışıyor: pod/route adı çözülemez, URL girin")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", kubeAPI+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := caClient(saDir+"/ca.crt", 10*time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		return json.NewDecoder(resp.Body).Decode(out)
	case 403:
		return fmt.Errorf("yetki yok (%s): ServiceAccount RBAC ayarını kontrol edin", path)
	case 404:
		return fmt.Errorf("bulunamadı: %s", path)
	}
	return fmt.Errorf("kubernetes API HTTP %d (%s)", resp.StatusCode, path)
}

var trailingV1 = regexp.MustCompile(`/(v1/?)?$`)

// resolve turns an endpoint config into (root URL without /v1, metrics URL or "").
func resolve(ctx context.Context, ep Endpoint) (root, metrics string, err error) {
	byName := ep.Type == "service" || ep.Type == "route" || (ep.Type == "pod" && ep.URL == "")
	if byName && (!dnsName.MatchString(ep.Name) || !dnsName.MatchString(ep.Namespace)) {
		return "", "", fmt.Errorf("geçersiz ad/namespace: %q / %q", ep.Name, ep.Namespace)
	}
	switch {
	case ep.Type == "service":
		root = fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", ep.Scheme, ep.Name, ep.Namespace, ep.Port)
	case ep.Type == "route":
		var rt struct {
			Spec struct {
				Host string          `json:"host"`
				Path string          `json:"path"`
				TLS  json.RawMessage `json:"tls"`
			} `json:"spec"`
		}
		if err = kubeGet(ctx, fmt.Sprintf("/apis/route.openshift.io/v1/namespaces/%s/routes/%s", ep.Namespace, ep.Name), &rt); err != nil {
			return
		}
		scheme := "http"
		if len(rt.Spec.TLS) > 0 && string(rt.Spec.TLS) != "null" {
			scheme = "https"
		}
		root = scheme + "://" + rt.Spec.Host + rt.Spec.Path
	case byName: // pod
		var pod struct {
			Status struct {
				PodIP string `json:"podIP"`
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if err = kubeGet(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ep.Namespace, ep.Name), &pod); err != nil {
			return
		}
		if pod.Status.PodIP == "" {
			return "", "", fmt.Errorf("pod IP yok (durum: %s)", pod.Status.Phase)
		}
		root = fmt.Sprintf("%s://%s", ep.Scheme, net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(ep.Port)))
	default:
		u, perr := url.Parse(strings.TrimSpace(ep.URL))
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", "", errors.New("URL http:// veya https:// ile başlamalı")
		}
		root = strings.TrimSpace(ep.URL)
	}
	root = trailingV1.ReplaceAllString(strings.TrimRight(root, "/"), "")
	metrics = ep.MetricsURL
	if metrics == "" && ep.Type != "litellm" {
		metrics = root + "/metrics"
	}
	return
}

func newClient(ep Endpoint, maxConns int, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        0,
		MaxIdleConnsPerHost: maxConns + 16,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: !ep.VerifyTLS},
		// HTTP/1.1 only: one stream per connection, like real independent clients.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func withAuth(req *http.Request, key string) *http.Request {
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req
}

func listModels(ctx context.Context, c *http.Client, root, key string) ([]string, *int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", root+"/v1/models", nil)
	resp, err := c.Do(withAuth(req, key))
	if err != nil {
		return nil, nil, fmt.Errorf("%s erişilemiyor: %w", root, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("/v1/models HTTP %d: %.200s", resp.StatusCode, body)
	}
	var m struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen *int   `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, fmt.Errorf("/v1/models yanıtı okunamadı: %w", err)
	}
	ids := []string{}
	for _, d := range m.Data {
		ids = append(ids, d.ID)
	}
	if len(m.Data) == 0 {
		return ids, nil, nil
	}
	return ids, m.Data[0].MaxModelLen, nil
}

type ProbeResult struct {
	BaseURL     string   `json:"base_url"`
	MetricsURL  *string  `json:"metrics_url"`
	MetricsOK   bool     `json:"metrics_ok"`
	Models      []string `json:"models"`
	MaxModelLen *int     `json:"max_model_len"`
}

func probe(ctx context.Context, ep Endpoint) (ProbeResult, error) {
	root, metrics, err := resolve(ctx, ep)
	if err != nil {
		return ProbeResult{}, err
	}
	c := newClient(ep, 4, 15*time.Second)
	models, maxLen, err := listModels(ctx, c, root, ep.APIKey)
	if err != nil {
		return ProbeResult{}, err
	}
	res := ProbeResult{BaseURL: root, Models: models, MaxModelLen: maxLen}
	if metrics != "" {
		res.MetricsURL = &metrics
		m, _ := scrape(ctx, c, metrics, ep.APIKey)
		_, res.MetricsOK = m["vllm:num_requests_running"]
	}
	return res, nil
}

// ---------------------------------------------------------------- vLLM /metrics

func parseMetrics(text string) map[string][]float64 {
	out := map[string][]float64{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.IndexAny(line, "{ ")
		if i <= 0 {
			continue
		}
		name, rest := line[:i], line[i:]
		if rest[0] == '{' {
			j := strings.LastIndexByte(rest, '}')
			if j < 0 {
				continue
			}
			rest = rest[j+1:]
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			out[name] = append(out[name], v)
		}
	}
	return out
}

func scrape(ctx context.Context, c *http.Client, u, key string) (map[string][]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := c.Do(withAuth(req, key))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return parseMetrics(string(b)), err
}

func sum(xs []float64) (s float64) {
	for _, x := range xs {
		s += x
	}
	return
}
func mean(xs []float64) float64 { return sum(xs) / float64(len(xs)) }

// mget returns the first present metric among names (vLLM renamed several across versions), aggregated over label sets.
func mget(m map[string][]float64, agg func([]float64) float64, names ...string) *float64 {
	for _, n := range names {
		if v, ok := m[n]; ok && len(v) > 0 {
			return ptr(agg(v))
		}
	}
	return nil
}

var (
	mRunning = []string{"vllm:num_requests_running"}
	mWaiting = []string{"vllm:num_requests_waiting"}
	mKV      = []string{"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"}
)

func serverDelta(a, b map[string][]float64) *ServerStats {
	if a == nil || b == nil {
		return nil
	}
	delta := func(names ...string) float64 {
		x, y := mget(a, sum, names...), mget(b, sum, names...)
		if x == nil || y == nil {
			return 0
		}
		return *y - *x
	}
	histMean := func(base string) *float64 {
		if dc := delta(base + "_count"); dc > 0 {
			return ptr(round(delta(base+"_sum")/dc*1000, 1))
		}
		return nil
	}
	s := &ServerStats{
		QueueMs:   histMean("vllm:request_queue_time_seconds"),
		PrefillMs: histMean("vllm:request_prefill_time_seconds"),
		DecodeMs:  histMean("vllm:request_decode_time_seconds"),
		TTFTms:    histMean("vllm:time_to_first_token_seconds"),
		E2Ems:     histMean("vllm:e2e_request_latency_seconds"),
	}
	if dq := delta("vllm:prefix_cache_queries_total", "vllm:prefix_cache_queries"); dq > 0 {
		s.PrefixHit = ptr(round(delta("vllm:prefix_cache_hits_total", "vllm:prefix_cache_hits")/dq*100, 1))
	} else if v := mget(b, mean, "vllm:gpu_prefix_cache_hit_rate"); v != nil {
		s.PrefixHit = ptr(round(*v*100, 1))
	}
	s.Preemptions = ptr(delta("vllm:num_preemptions_total", "vllm:num_preemptions"))
	return s
}

// ---------------------------------------------------------------- statistics

func dist(xs []float64) *Dist {
	if len(xs) == 0 {
		return nil
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	q := func(p float64) float64 { // linear interpolation, same as numpy default
		k := p / 100 * float64(len(s)-1)
		f := math.Floor(k)
		c := math.Min(f+1, float64(len(s)-1))
		return s[int(f)] + (s[int(c)]-s[int(f)])*(k-f)
	}
	return &Dist{Mean: round(mean(s), 2), Min: round(s[0], 2), Max: round(s[len(s)-1], 2),
		P50: round(q(50), 2), P90: round(q(90), 2), P95: round(q(95), 2), P99: round(q(99), 2)}
}

type Result struct {
	OK                 bool
	Err                string
	Arrival            time.Time // scheduled send time (rate mode), else = Start
	Start, First, Last time.Time
	FirstOut           time.Time // first non-reasoning token
	ITL                []float64 // ms between content chunks
	In                 int       // -1 when the server sent no usage
	Out                int
	SawReasoning       bool
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

func summarize(level float64, res []Result, slo SLO) Stage {
	s := Stage{Level: level, Requests: len(res), ErrorSamples: []ErrCount{}, Samples: [][]any{}}
	errs := map[string]int{}
	var ok []Result
	for _, r := range res {
		if r.OK {
			ok = append(ok, r)
		} else {
			errs[r.Err]++
		}
	}
	for e, n := range errs {
		s.ErrorSamples = append(s.ErrorSamples, ErrCount{e, n})
	}
	sort.Slice(s.ErrorSamples, func(i, j int) bool { return s.ErrorSamples[i].Count > s.ErrorSamples[j].Count })
	if len(s.ErrorSamples) > 5 {
		s.ErrorSamples = s.ErrorSamples[:5]
	}
	s.OK, s.Errors = len(ok), len(res)-len(ok)
	if len(ok) == 0 {
		return s
	}
	t0, t1, lastStart := res[0].Start, res[0].Last, res[0].Start
	for _, r := range res {
		if r.Start.Before(t0) {
			t0 = r.Start
		}
		if r.Start.After(lastStart) {
			lastStart = r.Start
		}
		end := r.Last
		if !r.OK {
			end = r.Start // failed requests: no token timeline
		}
		if end.After(t1) {
			t1 = end
		}
	}
	dur := t1.Sub(t0).Seconds()
	var outTok, inTok, good float64
	var ttft, ttfo, tpot, itl, e2e, dec, queue []float64
	var steadyTok float64
	for _, r := range ok {
		outTok += float64(r.Out)
		if r.In >= 0 {
			inTok += float64(r.In)
		}
		ft, e := ms(r.First.Sub(r.Start)), ms(r.Last.Sub(r.Start))
		ttft, e2e, dec = append(ttft, ft), append(e2e, e), append(dec, e-ft)
		itl = append(itl, r.ITL...)
		var tp any
		if r.Out > 1 {
			v := ms(r.Last.Sub(r.First)) / float64(r.Out-1)
			tpot = append(tpot, v)
			tp = round(v, 2)
		}
		if r.SawReasoning && !r.FirstOut.IsZero() {
			ttfo = append(ttfo, ms(r.FirstOut.Sub(r.Start)))
		}
		if q := ms(r.Start.Sub(r.Arrival)); q > 0 {
			queue = append(queue, q)
		}
		if ft <= slo.TTFTms && (r.Out <= 1 || ms(r.Last.Sub(r.First))/float64(r.Out-1) <= slo.TPOTms) {
			good++
		}
		// tokens produced inside the steady window [t0, lastStart], spread evenly over [First, Last]
		lo, hi := maxT(r.First, t0), minT(r.Last, lastStart)
		if span := r.Last.Sub(r.First); span > 0 && hi.After(lo) {
			steadyTok += float64(r.Out) * float64(hi.Sub(lo)) / float64(span)
		}
		if len(s.Samples) < 2000 { // ponytail: samples capped to keep run JSON small
			var in any
			if r.In >= 0 {
				in = r.In
			}
			s.Samples = append(s.Samples, []any{round(ft, 1), tp, round(e, 1), in, r.Out})
		}
	}
	n := float64(len(ok))
	s.DurationS = round(dur, 2)
	s.ReqPerS = round(n/dur, 3)
	s.OutTokPerS = round(outTok/dur, 1)
	s.InTokPerS = round(inTok/dur, 1)
	s.TotalTokPerS = round((inTok+outTok)/dur, 1)
	s.InTokensMean = round(inTok/n, 1)
	s.OutTokensMean = round(outTok/n, 1)
	if w := lastStart.Sub(t0).Seconds(); w > 0 && w >= 0.25*dur {
		s.SteadyOutTokS = ptr(round(steadyTok/w, 1))
	}
	s.TTFT, s.TTFO, s.TPOT, s.ITL, s.E2E, s.Decode = dist(ttft), dist(ttfo), dist(tpot), dist(itl), dist(e2e), dist(dec)
	s.ClientQueue = dist(queue)
	if s.TPOT != nil && s.TPOT.P50 > 0 {
		s.UserTokPerS = ptr(round(1000/s.TPOT.P50, 1))
	}
	s.GoodputPct = round(good/float64(len(res))*100, 1)
	s.GoodputRPS = round(good/dur, 3)
	return s
}

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func minT(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// recommend returns the highest load level whose p95 TTFT/TPOT meet the SLO with <1% errors.
func recommend(r *Run) *Stage {
	var best *Stage
	for i := range r.Stages {
		s := &r.Stages[i]
		if s.TTFT == nil || float64(s.Errors) > 0.01*float64(s.Requests) {
			continue
		}
		if s.TTFT.P95 <= r.Config.SLO.TTFTms && (s.TPOT == nil || s.TPOT.P95 <= r.Config.SLO.TPOTms) {
			best = s
		}
	}
	return best
}

// ---------------------------------------------------------------- prompts

type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type Req struct {
	Messages  []Message
	MaxTokens int
}

type shareRow struct {
	Prompt      string `json:"prompt"`
	OutputWords int    `json:"output_words"`
}

var fallbackWords = strings.Fields(`system model request latency token server cluster memory network data value result process
function stream batch queue cache layer weight matrix vector input output signal answer question history river mountain
city market garden window letter number simple complex quick slow bright dark happy careful modern ancient public private`)

func sharegptRows() []shareRow {
	var rows []shareRow
	for _, line := range bytes.Split(sharegptData, []byte("\n")) {
		var r shareRow
		if json.Unmarshal(line, &r) == nil && r.Prompt != "" {
			rows = append(rows, r)
		}
	}
	return rows
}

func vocabulary() []string {
	seen := map[string]bool{}
	rows := sharegptRows()
	for i := 0; i < len(rows) && i < 400; i++ {
		for _, w := range strings.Fields(rows[i].Prompt) {
			w = strings.ToLower(w)
			if len(w) < 14 && strings.IndexFunc(w, func(r rune) bool { return r < 'a' || r > 'z' }) < 0 {
				seen[w] = true
			}
		}
	}
	if len(seen) < 500 {
		return fallbackWords
	}
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- runner

type Runner struct {
	run     *Run
	cfg     Config
	apiKey  string
	dataDir string
	save    func(*Run)

	client           *http.Client
	root, metricsURL string
	model            string
	ignoreEOS        bool
	rnd              *rand.Rand
	vocab            []string
	inflight         atomic.Int64
	metricsMu        sync.Mutex
	metrics          map[string][]float64
	prom             *http.Client
	promURL          string
	t0               time.Time
	cancel           context.CancelFunc
}

func NewRunner(id string, cfg Config, dataDir string, save func(*Run)) *Runner {
	key := cfg.Endpoint.APIKey
	cfg.Endpoint.APIKey = "" // never persisted
	name := cfg.Name
	if name == "" {
		name = id
	}
	run := &Run{ID: id, Name: name, Status: "running", Created: nowISO(), Warnings: []string{}, Config: cfg,
		Progress: Progress{Phase: "başlıyor"}, Stages: []Stage{}, Timeseries: []TSPoint{}, GPUSeries: []GPUPoint{}}
	return &Runner{run: run, cfg: cfg, apiKey: key, dataDir: dataDir, save: save}
}

func (rn *Runner) warn(msg string) {
	rn.run.update(func() { rn.run.Warnings = append(rn.run.Warnings, msg) })
}
func (rn *Runner) Stop() { rn.cancel() }

func (rn *Runner) Run(ctx context.Context) {
	ctx, rn.cancel = context.WithCancel(ctx)
	defer rn.cancel()
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v", p)
			}
		}()
		return rn.execute(ctx)
	}()
	rn.run.update(func() {
		switch {
		case err == nil:
			rn.run.Status = "done"
		case ctx.Err() != nil:
			rn.run.Status = "stopped"
		default:
			rn.run.Status = "failed"
			rn.run.Error = ptr(err.Error())
		}
		rn.run.Finished = ptr(nowISO())
	})
	rn.save(rn.run)
}

func (rn *Runner) send(ctx context.Context, rq Req, ignoreEOS bool, arrival time.Time) (r Result) {
	body := map[string]any{"model": rn.model, "messages": rq.Messages, "max_tokens": rq.MaxTokens,
		"stream": true, "stream_options": map[string]any{"include_usage": true}}
	if ignoreEOS {
		body["ignore_eos"], body["min_tokens"] = true, rq.MaxTokens
	}
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(rn.cfg.Load.TimeoutS*float64(time.Second)))
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", rn.root+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	withAuth(req, rn.apiKey)

	rn.inflight.Add(1)
	defer rn.inflight.Add(-1)
	r.In = -1
	r.Start = time.Now()
	r.Arrival = r.Start
	if !arrival.IsZero() {
		r.Arrival = arrival
	}
	resp, err := rn.client.Do(req)
	if err != nil {
		r.Err = shortErr(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 160))
		r.Err = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, eb)
		return
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var ch struct {
		Choices []struct {
			Delta struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				Reasoning        string          `json:"reasoning"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	chunks := 0
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		now := time.Now()
		ch.Choices, ch.Usage, ch.Error = nil, nil, nil
		if err := json.Unmarshal(payload, &ch); err != nil {
			r.Err = "stream JSON: " + err.Error()
			return
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			r.Err = fmt.Sprintf("stream: %.160s", ch.Error)
			return
		}
		if ch.Usage != nil {
			r.In, r.Out = ch.Usage.PromptTokens, ch.Usage.CompletionTokens
		}
		for _, c := range ch.Choices {
			d := c.Delta
			answer := d.Content != "" || (len(d.ToolCalls) > 0 && string(d.ToolCalls) != "null")
			thinking := d.ReasoningContent != "" || d.Reasoning != ""
			if !answer && !thinking {
				continue // role-only / empty chunks don't count as tokens
			}
			if r.First.IsZero() {
				r.First = now
			} else {
				r.ITL = append(r.ITL, ms(now.Sub(r.Last)))
			}
			if answer && r.FirstOut.IsZero() {
				r.FirstOut = now
			}
			r.SawReasoning = r.SawReasoning || thinking
			r.Last = now
			chunks++
		}
	}
	if err := sc.Err(); err != nil {
		r.Err = shortErr(err)
		return
	}
	if r.First.IsZero() {
		r.Err = "boş yanıt (token gelmedi)"
		return
	}
	if r.Out == 0 {
		r.Out = chunks
	}
	r.OK = true
	return
}

func shortErr(err error) string {
	s := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		s = "zaman aşımı (timeout_s)"
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (rn *Runner) wordsFor(tokens int) int {
	c := rn.run.Calibration
	return max(1, int(math.Round((float64(tokens)-c.OverheadTokens)/c.TokensPerWord)))
}

func (rn *Runner) words(n int) string {
	w := make([]string, n)
	for i := range w {
		w[i] = rn.vocab[rn.rnd.Intn(len(rn.vocab))]
	}
	return strings.Join(w, " ")
}

func (rn *Runner) buildPool(n int) ([]Req, error) {
	ds := rn.cfg.Dataset
	pool := make([]Req, 0, n)
	switch ds.Type {
	case "synthetic":
		var prefix []Message
		if ds.SharedPrefixTokens > 0 {
			prefix = []Message{{"system", rn.words(rn.wordsFor(ds.SharedPrefixTokens))}}
		}
		for i := 0; i < n; i++ {
			isl := max(1, int(math.Round(rn.rnd.NormFloat64()*ds.InputStd+float64(ds.InputTokens))))
			osl := max(1, int(math.Round(rn.rnd.NormFloat64()*ds.OutputStd+float64(ds.OutputTokens))))
			msgs := append(append([]Message{}, prefix...), Message{"user", rn.words(rn.wordsFor(isl))})
			pool = append(pool, Req{msgs, osl})
		}
		return pool, nil
	case "sharegpt":
		rows := sharegptRows()
		if len(rows) == 0 {
			return nil, errors.New("ShareGPT veri seti bu binary'de yok; sentetik ya da özel veri seti kullanın")
		}
		tpw := rn.run.Calibration.TokensPerWord
		for i := 0; i < n; i++ {
			r := rows[rn.rnd.Intn(len(rows))]
			mt := min(ds.OutputTokens, max(4, int(math.Round(float64(r.OutputWords)*tpw))))
			pool = append(pool, Req{[]Message{{"user", r.Prompt}}, mt})
		}
		return pool, nil
	}
	path := filepath.Join(rn.dataDir, "datasets", ds.CustomName)
	if ds.CustomName == "" || filepath.Base(ds.CustomName) != ds.CustomName {
		return nil, fmt.Errorf("veri seti bulunamadı: %q", ds.CustomName)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("veri seti okunamadı: %w", err)
	}
	var items []Req
	for _, line := range bytes.Split(b, []byte("\n")) {
		var row struct {
			Prompt    string    `json:"prompt"`
			Messages  []Message `json:"messages"`
			MaxTokens int       `json:"max_tokens"`
		}
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &row) != nil {
			continue
		}
		msgs := row.Messages
		if len(msgs) == 0 {
			msgs = []Message{{"user", row.Prompt}}
		}
		mt := row.MaxTokens
		if mt <= 0 {
			mt = ds.OutputTokens
		}
		items = append(items, Req{msgs, mt})
	}
	if len(items) == 0 {
		return nil, errors.New("veri setinde geçerli satır yok")
	}
	for i := 0; i < n; i++ {
		pool = append(pool, items[rn.rnd.Intn(len(items))])
	}
	return pool, nil
}

func (rn *Runner) scrapeNow(ctx context.Context) map[string][]float64 {
	if rn.metricsURL == "" {
		return nil
	}
	m, err := scrape(ctx, rn.client, rn.metricsURL, rn.apiKey)
	rn.metricsMu.Lock()
	defer rn.metricsMu.Unlock()
	if err == nil {
		rn.metrics = m
	}
	return rn.metrics
}

func (rn *Runner) telemetryLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		m := rn.scrapeNow(ctx)
		p := TSPoint{T: round(time.Since(rn.t0).Seconds(), 1), Inflight: rn.inflight.Load(),
			Running: mget(m, sum, mRunning...), Waiting: mget(m, sum, mWaiting...)}
		if kv := mget(m, mean, mKV...); kv != nil {
			p.KV = ptr(round(*kv*100, 1))
		}
		rn.run.update(func() { rn.run.Timeseries = append(rn.run.Timeseries, p) })
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (rn *Runner) promQuery(ctx context.Context, path string, params url.Values) ([]struct {
	Value  []any   `json:"value"`
	Values [][]any `json:"values"`
}, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", rn.promURL+path+"?"+params.Encode(), nil)
	withAuth(req, saToken())
	resp, err := rn.prom.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("prometheus HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Data struct {
			Result []struct {
				Value  []any   `json:"value"`
				Values [][]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out.Data.Result, err
}

func (rn *Runner) setupPrometheus(ctx context.Context) {
	p := rn.cfg.Prometheus
	if !p.Enabled {
		return
	}
	if p.URL == "" && saToken() == "" {
		rn.warn("GPU telemetrisi atlandı: küme içinde değil ve Prometheus URL'i verilmedi")
		return
	}
	rn.promURL = strings.TrimRight(p.URL, "/")
	if rn.promURL == "" {
		rn.promURL = thanos
		rn.prom = caClient(saDir+"/service-ca.crt", 15*time.Second)
	} else {
		rn.prom = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := rn.promQuery(ctx, "/api/v1/query", url.Values{"query": {fmt.Sprintf("count(DCGM_FI_DEV_GPU_UTIL{%s})", p.GPUSelector)}})
	if err == nil && len(res) == 0 {
		err = errors.New("seçiciye uyan DCGM_FI_DEV_GPU_UTIL serisi yok")
	}
	if err != nil {
		rn.warn("GPU telemetrisi kapalı: " + err.Error())
		rn.prom = nil
		return
	}
	if len(res[0].Value) == 2 {
		if s, ok := res[0].Value[1].(string); ok {
			n, _ := strconv.ParseFloat(s, 64)
			rn.run.update(func() { rn.run.Resolved.GPUs = ptr(int(n)) })
		}
	}
}

func (rn *Runner) gpuStage(ctx context.Context, start, end time.Time) *GPUStats {
	sel := rn.cfg.Prometheus.GPUSelector
	queries := []string{
		fmt.Sprintf("avg(DCGM_FI_DEV_GPU_UTIL{%s})", sel),
		fmt.Sprintf("sum(DCGM_FI_DEV_FB_USED{%s})/1024", sel),
		fmt.Sprintf("sum(DCGM_FI_DEV_POWER_USAGE{%s})", sel),
	}
	series := make([]map[float64]float64, 3)
	for i, q := range queries {
		res, err := rn.promQuery(ctx, "/api/v1/query_range", url.Values{"query": {q}, "step": {"5"},
			"start": {strconv.FormatInt(start.Unix(), 10)}, "end": {strconv.FormatInt(end.Unix()+1, 10)}})
		if err != nil {
			rn.warn("GPU metrikleri okunamadı: " + err.Error())
			rn.prom = nil
			return nil
		}
		series[i] = map[float64]float64{}
		if len(res) > 0 {
			for _, v := range res[0].Values {
				t, _ := v[0].(float64)
				s, _ := v[1].(string)
				f, err := strconv.ParseFloat(s, 64)
				if err == nil {
					series[i][t] = f
				}
			}
		}
	}
	if len(series[0]) == 0 {
		return nil
	}
	vals := func(m map[float64]float64) (xs []float64) {
		for _, v := range m {
			xs = append(xs, v)
		}
		return
	}
	ts := make([]float64, 0, len(series[0]))
	for t := range series[0] {
		ts = append(ts, t)
	}
	sort.Float64s(ts)
	pts := make([]GPUPoint, 0, len(ts))
	get := func(m map[float64]float64, t float64) *float64 {
		if v, ok := m[t]; ok {
			return ptr(round(v, 1))
		}
		return nil
	}
	for _, t := range ts {
		pts = append(pts, GPUPoint{T: round(t-float64(rn.t0.UnixNano())/1e9, 1), Util: get(series[0], t), MemGB: get(series[1], t), PowerW: get(series[2], t)})
	}
	rn.run.update(func() { rn.run.GPUSeries = append(rn.run.GPUSeries, pts...) })
	u := vals(series[0])
	g := &GPUStats{UtilMean: round(mean(u), 1), UtilMax: round(maxOf(u), 1)}
	if m := vals(series[1]); len(m) > 0 {
		g.MemMaxGB = ptr(round(maxOf(m), 1))
	}
	if p := vals(series[2]); len(p) > 0 {
		g.PowerMean = ptr(round(mean(p), 1))
	}
	return g
}

func maxOf(xs []float64) float64 {
	m := math.Inf(-1)
	for _, x := range xs {
		m = math.Max(m, x)
	}
	return m
}

// calibrate measures tokens/word with two tiny requests (no tokenizer download) and checks ignore_eos support.
func (rn *Runner) calibrate(ctx context.Context) error {
	mk := func(words, maxTok int) Req { return Req{[]Message{{"user", rn.words(words)}}, maxTok} }
	r1 := rn.send(ctx, mk(100, 16), rn.ignoreEOS, time.Time{})
	if !r1.OK && rn.ignoreEOS {
		if r1 = rn.send(ctx, mk(100, 16), false, time.Time{}); r1.OK {
			rn.ignoreEOS = false
			rn.warn("Endpoint ignore_eos/min_tokens parametrelerini reddetti; çıkış uzunlukları modele bağlı olacak")
		}
	}
	if !r1.OK {
		return fmt.Errorf("ilk istek başarısız: %s", r1.Err)
	}
	if rn.ignoreEOS && r1.In >= 0 && r1.Out != 16 {
		rn.warn(fmt.Sprintf("ignore_eos etkisiz görünüyor (16 yerine %d token üretildi)", r1.Out))
	}
	r2 := rn.send(ctx, mk(600, 1), false, time.Time{})
	cal := &Calibration{TokensPerWord: 1.3}
	if r1.In > 0 && r2.OK && r2.In > r1.In {
		a := float64(r2.In-r1.In) / 500
		cal = &Calibration{TokensPerWord: round(a, 3), OverheadTokens: math.Round(float64(r1.In) - 100*a)}
	} else {
		rn.warn("Endpoint usage bilgisi döndürmüyor; token sayıları yaklaşık")
	}
	rn.run.update(func() { rn.run.Calibration = cal })
	return nil
}

// prewarm opens n keep-alive connections with cheap GETs so TCP/TLS setup doesn't land in TTFT.
func (rn *Runner) prewarm(ctx context.Context, n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", rn.root+"/v1/models", nil)
			if resp, err := rn.client.Do(withAuth(req, rn.apiKey)); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

func gammaSample(r *rand.Rand, k, theta float64) float64 { // Marsaglia–Tsang
	if k < 1 {
		return gammaSample(r, k+1, theta) * math.Pow(r.Float64(), 1/k)
	}
	d := k - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := r.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := r.Float64()
		if u < 1-0.0331*x*x*x*x || math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v * theta
		}
	}
}

func (rn *Runner) runStage(ctx context.Context, level float64, reqs []Req) []Result {
	var mu sync.Mutex
	results := make([]Result, 0, len(reqs))
	done := func(r Result) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
		rn.run.update(func() { rn.run.Progress.Done++ })
	}
	var wg sync.WaitGroup
	if rn.cfg.Load.Mode == "concurrency" {
		jobs := make(chan Req)
		for i := 0; i < int(level); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for rq := range jobs {
					done(rn.send(ctx, rq, rn.ignoreEOS, time.Time{}))
				}
			}()
		}
	feed:
		for _, rq := range reqs {
			select {
			case jobs <- rq:
			case <-ctx.Done():
				break feed
			}
		}
		close(jobs)
	} else {
		// absolute schedule (no drift); requests waiting on the cap record client queue time
		sem := make(chan struct{}, rn.cfg.Load.MaxConcurrency)
		b := rn.cfg.Load.Burstiness
		next := time.Now()
		for _, rq := range reqs {
			select {
			case <-time.After(time.Until(next)):
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(rq Req, arrival time.Time) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				done(rn.send(ctx, rq, rn.ignoreEOS, arrival))
			}(rq, next)
			next = next.Add(time.Duration(gammaSample(rn.rnd, b, 1/(level*b)) * float64(time.Second)))
		}
	}
	wg.Wait()
	return results
}

func (rn *Runner) execute(ctx context.Context) error {
	cfg, load := rn.cfg, rn.cfg.Load
	rn.rnd = rand.New(rand.NewSource(0))
	rn.vocab = vocabulary()
	rn.ignoreEOS = cfg.IgnoreEOS
	var err error
	if rn.root, rn.metricsURL, err = resolve(ctx, cfg.Endpoint); err != nil {
		return err
	}
	top := load.MaxConcurrency
	if load.Mode == "concurrency" {
		top = int(maxOf(load.Levels))
	}
	rn.client = newClient(cfg.Endpoint, max(top, load.Warmup), 0)
	models, maxLen, err := listModels(ctx, rn.client, rn.root, rn.apiKey)
	if err != nil {
		return err
	}
	rn.model = cfg.Model
	if rn.model == "" && len(models) > 0 {
		rn.model = models[0]
	}
	if rn.model == "" {
		return errors.New("model adı yok ve /v1/models boş döndü")
	}
	res := &Resolved{BaseURL: rn.root, Model: rn.model, MaxModelLen: maxLen}
	if rn.metricsURL != "" {
		if _, ok := rn.scrapeNow(ctx)["vllm:num_requests_running"]; ok {
			res.MetricsURL = ptr(rn.metricsURL)
		} else {
			rn.warn(fmt.Sprintf("vLLM /metrics okunamadı (%s); sunucu tarafı prefill/decode/KV cache verisi olmayacak", rn.metricsURL))
			rn.metricsURL = ""
		}
	}
	rn.run.update(func() { rn.run.Resolved = res; rn.run.Progress = Progress{Phase: "kalibrasyon"} })
	rn.setupPrometheus(ctx)
	if err := rn.calibrate(ctx); err != nil {
		return err
	}
	ds := cfg.Dataset
	if need := ds.InputTokens + ds.SharedPrefixTokens + ds.OutputTokens; maxLen != nil && ds.Type == "synthetic" && need > *maxLen {
		rn.warn(fmt.Sprintf("Giriş+çıkış (%d) max_model_len (%d) değerini aşıyor; istekler reddedilebilir", need, *maxLen))
	}

	rn.t0 = time.Now()
	tctx, stopTele := context.WithCancel(ctx)
	defer stopTele()
	go rn.telemetryLoop(tctx)

	if load.Warmup > 0 {
		rn.run.update(func() { rn.run.Progress = Progress{Phase: "ısınma", Total: len(load.Levels), Of: load.Warmup} })
		pool, err := rn.buildPool(load.Warmup)
		if err != nil {
			return err
		}
		var wg sync.WaitGroup
		for _, rq := range pool {
			wg.Add(1)
			go func(rq Req) { defer wg.Done(); rn.send(ctx, rq, rn.ignoreEOS, time.Time{}) }(rq)
		}
		wg.Wait()
	}
	for i, level := range load.Levels {
		if load.Mode == "concurrency" {
			level = math.Max(1, math.Round(level))
		}
		n := load.RequestsPerStage
		if load.Mode == "concurrency" {
			n = max(n, int(level)*2)
		}
		reqs, err := rn.buildPool(n)
		if err != nil {
			return err
		}
		rn.run.update(func() {
			rn.run.Progress = Progress{Phase: "ölçüm", Stage: i + 1, Total: len(load.Levels), Level: level, Of: n}
		})
		warm := int(level) // ponytail: rate mode pre-opens ~2s of arrivals worth of connections; tune if bursts are larger
		if load.Mode == "rate" {
			warm = min(load.MaxConcurrency, max(8, int(math.Ceil(level*2))))
		}
		rn.prewarm(ctx, warm)
		m0 := rn.scrapeNow(ctx)
		w0 := time.Now()
		results := rn.runStage(ctx, level, reqs)
		w1 := time.Now()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m1 := rn.scrapeNow(ctx)
		s := summarize(level, results, cfg.SLO)
		s.TStart, s.TEnd = round(w0.Sub(rn.t0).Seconds(), 1), round(w1.Sub(rn.t0).Seconds(), 1)
		if s.Server = serverDelta(m0, m1); s.Server != nil {
			rn.run.update(func() {
				var kv, wait, runn []float64
				for _, p := range rn.run.Timeseries {
					if p.T < s.TStart || p.T > s.TEnd {
						continue
					}
					if p.KV != nil {
						kv = append(kv, *p.KV)
					}
					if p.Waiting != nil {
						wait = append(wait, *p.Waiting)
					}
					if p.Running != nil {
						runn = append(runn, *p.Running)
					}
				}
				if len(kv) > 0 {
					s.Server.KVMax, s.Server.KVMean = ptr(maxOf(kv)), ptr(round(mean(kv), 1))
				}
				if len(wait) > 0 {
					s.Server.WaitingMax = ptr(maxOf(wait))
				}
				if len(runn) > 0 {
					s.Server.RunningMax = ptr(maxOf(runn))
				}
			})
		}
		if rn.prom != nil {
			s.GPU = rn.gpuStage(ctx, w0, w1)
		}
		rn.run.update(func() { rn.run.Stages = append(rn.run.Stages, s) })
		rn.save(rn.run)
	}
	rn.run.update(func() { rn.run.Progress.Phase = "bitti" })
	return nil
}
