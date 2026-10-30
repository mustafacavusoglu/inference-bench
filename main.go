// ibench — Inference Bench: load-test and measure LLM inference endpoints (vLLM / OpenAI-compatible).
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"
)

//go:embed web
var webFS embed.FS

//go:embed assets/sharegpt.jsonl
var sharegptData []byte

var version = "dev" // set with -ldflags "-X main.version=..."

func mustRead(p string) []byte {
	b, err := webFS.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return b
}

func itoa(n int) string { return strconv.Itoa(n) }

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

const usage = `ibench — Inference Bench

  ibench serve  [-addr :8080] [-data ./ibench-data]      web UI + API (DATA_DIR env also works)
  ibench run    -f config.json [-o run.json] [-html report.html]
  ibench report run.json [run2.json ...] > report.html    one run, or a comparison
  ibench mock   [-addr :8000] [-model mock-model] [-speed 1]   fake vLLM server for demos/tests
  ibench version

API key for "run": "endpoint.api_key" in the config or IBENCH_API_KEY env.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	switch os.Args[1] {
	case "serve":
		addr := fs.String("addr", ":8080", "listen address")
		dir := fs.String("data", envOr("DATA_DIR", "ibench-data"), "data directory (runs, datasets)")
		fs.Parse(os.Args[2:])
		s, err := newServer(*dir)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("ibench %s serving on %s (data: %s)", version, *addr, *dir)
		srv := &http.Server{Addr: *addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
		log.Fatal(srv.ListenAndServe())
	case "run":
		cfgPath := fs.String("f", "", "config JSON file (required)")
		out := fs.String("o", "", "run JSON output (default run-<id>.json)")
		html := fs.String("html", "", "also write an HTML report")
		fs.Parse(os.Args[2:])
		os.Exit(runCLI(*cfgPath, *out, *html))
	case "report":
		fs.Parse(os.Args[2:])
		if fs.NArg() == 0 {
			log.Fatal("en az bir run JSON dosyası verin")
		}
		var runs [][]byte
		for _, f := range fs.Args() {
			b, err := os.ReadFile(f)
			if err != nil {
				log.Fatal(err)
			}
			runs = append(runs, b)
		}
		os.Stdout.Write(renderReport(runs))
	case "mock":
		addr := fs.String("addr", ":8000", "listen address")
		model := fs.String("model", "mock-model", "model id")
		speed := fs.Float64("speed", 1, "slowdown factor (>1 = slower GPU)")
		fs.Parse(os.Args[2:])
		log.Printf("ibench mock %q on %s (speed %g)", *model, *addr, *speed)
		log.Fatal(http.ListenAndServe(*addr, newMock(*model, *speed).handler()))
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func runCLI(cfgPath, out, html string) int {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		log.Print("config: ", err)
		return 2
	}
	cfg, err := parseConfig(b)
	if err != nil {
		log.Print(err)
		return 2
	}
	if cfg.Endpoint.APIKey == "" {
		cfg.Endpoint.APIKey = os.Getenv("IBENCH_API_KEY")
	}
	id := newRunID()
	if out == "" {
		out = "run-" + id + ".json"
	}
	rn := NewRunner(id, cfg, envOr("DATA_DIR", "ibench-data"), saveRun(out))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				var p Progress
				rn.run.update(func() { p = rn.run.Progress })
				log.Printf("%s  aşama %d/%d  seviye %g  %d/%d", p.Phase, p.Stage, p.Total, p.Level, p.Done, p.Of)
			}
		}
	}()
	rn.Run(ctx)
	close(done)

	r := rn.run
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "seviye\tistek/s\tçıkış tok/s\tTTFT p50\tTTFT p95\tTPOT p50\tTPOT p95\tgoodput\thata\t")
	rec := recommend(r)
	for _, s := range r.Stages {
		if s.TTFT == nil {
			fmt.Fprintf(tw, "%g\t-\t-\t-\t-\t-\t-\t-\t%d\t\n", s.Level, s.Errors)
			continue
		}
		tpot50, tpot95 := 0.0, 0.0
		if s.TPOT != nil {
			tpot50, tpot95 = s.TPOT.P50, s.TPOT.P95
		}
		mark := ""
		if rec != nil && rec.Level == s.Level {
			mark = " ← SLO içi en yüksek"
		}
		fmt.Fprintf(tw, "%g\t%.2f\t%.0f\t%.0f\t%.0f\t%.1f\t%.1f\t%.0f%%\t%d\t%s\n", s.Level, s.ReqPerS, s.OutTokPerS,
			s.TTFT.P50, s.TTFT.P95, tpot50, tpot95, s.GoodputPct, s.Errors, mark)
	}
	tw.Flush()
	for _, w := range r.Warnings {
		log.Print("uyarı: ", w)
	}
	log.Printf("durum: %s → %s", r.Status, out)
	if html != "" {
		if err := os.WriteFile(html, renderReport([][]byte{r.JSON()}), 0o644); err != nil {
			log.Print(err)
		}
	}
	if r.Status != "done" {
		if r.Error != nil {
			log.Print("hata: ", *r.Error)
		}
		return 1
	}
	return 0
}
