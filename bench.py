"""Benchmark engine: endpoint resolution, prompts, load generation, vLLM /metrics + DCGM scraping, stats."""
import asyncio
import json
import math
import os
import random
import re
import statistics
import time
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path

import httpx

SA = Path("/var/run/secrets/kubernetes.io/serviceaccount")
KUBE_API = "https://kubernetes.default.svc"
THANOS = "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"
SHAREGPT = Path(os.environ.get("SHAREGPT_PATH", "data/sharegpt.jsonl"))
DNS_NAME = re.compile(r"^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$")

FALLBACK_WORDS = (
    "system model request latency token server cluster memory network data value result process "
    "function stream batch queue cache layer weight matrix vector input output signal answer question "
    "history river mountain city market garden window letter number simple complex quick slow bright "
    "dark happy careful modern ancient public private open closed early late strong light heavy small"
).split()


def now_iso():
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def _sa_token():
    try:
        return (SA / "token").read_text().strip()
    except OSError:
        return None


# ---------------------------------------------------------------- endpoint resolution

async def kube_get(path):
    token = _sa_token()
    if not token:
        raise ValueError("Küme dışında çalışıyor: pod/route adı çözülemez, URL girin")
    async with httpx.AsyncClient(verify=str(SA / "ca.crt"), timeout=10) as c:
        r = await c.get(KUBE_API + path, headers={"Authorization": f"Bearer {token}"})
    if r.status_code == 403:
        raise ValueError(f"Yetki yok ({path}): ServiceAccount RBAC ayarını kontrol edin")
    if r.status_code == 404:
        raise ValueError(f"Bulunamadı: {path}")
    r.raise_for_status()
    return r.json()


async def resolve(ep):
    """Endpoint config -> (root_url without /v1, metrics_url or None)."""
    t = ep["type"]
    if t in ("service", "route", "pod") and not (t == "pod" and ep.get("url")):
        for k in ("name", "namespace"):
            if not DNS_NAME.match(ep.get(k, "")):
                raise ValueError(f"Geçersiz {k}: '{ep.get(k, '')}'")
    if t == "service":
        root = f"{ep['scheme']}://{ep['name']}.{ep['namespace']}.svc.cluster.local:{ep['port']}"
    elif t == "route":
        rt = await kube_get(f"/apis/route.openshift.io/v1/namespaces/{ep['namespace']}/routes/{ep['name']}")
        spec = rt["spec"]
        root = ("https" if spec.get("tls") else "http") + "://" + spec["host"] + spec.get("path", "")
    elif t == "pod" and not ep.get("url"):
        pod = await kube_get(f"/api/v1/namespaces/{ep['namespace']}/pods/{ep['name']}")
        ip = pod.get("status", {}).get("podIP")
        if not ip:
            raise ValueError(f"Pod IP yok (durum: {pod.get('status', {}).get('phase')})")
        root = f"{ep['scheme']}://{f'[{ip}]' if ':' in ip else ip}:{ep['port']}"
    else:
        root = ep.get("url", "").strip()
        if not re.match(r"^https?://[^/\s]+", root):
            raise ValueError("URL http:// veya https:// ile başlamalı")
    root = re.sub(r"/(v1/?)?$", "", root.rstrip("/"))
    metrics = ep.get("metrics_url") or (None if t == "litellm" else root + "/metrics")
    return root, metrics


def make_client(ep, api_key, max_conn=64, timeout=300):
    headers = {"Authorization": f"Bearer {api_key}"} if api_key else {}
    return httpx.AsyncClient(
        headers=headers, verify=ep.get("verify_tls", True),
        timeout=httpx.Timeout(timeout, connect=10),
        limits=httpx.Limits(max_connections=max_conn + 8, max_keepalive_connections=max_conn + 8),
    )


async def list_models(client, root):
    r = await client.get(root + "/v1/models", timeout=15)
    if r.status_code != 200:
        raise ValueError(f"/v1/models HTTP {r.status_code}: {r.text[:200]}")
    data = r.json().get("data", [])
    return [m["id"] for m in data], (data[0].get("max_model_len") if data else None)


async def probe(ep, api_key):
    root, metrics = await resolve(ep)
    async with make_client(ep, api_key) as c:
        try:
            models, max_len = await list_models(c, root)
        except httpx.HTTPError as e:
            raise ValueError(f"{root} erişilemiyor: {type(e).__name__}: {e}")
        metrics_ok = False
        if metrics:
            try:
                r = await c.get(metrics, timeout=10)
                metrics_ok = r.status_code == 200 and "vllm:" in r.text
            except httpx.HTTPError:
                pass
    return {"base_url": root, "metrics_url": metrics, "metrics_ok": metrics_ok,
            "models": models, "max_model_len": max_len}


# ---------------------------------------------------------------- vLLM /metrics

METRIC_LINE = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{.*\})?\s+(\S+)")


def parse_metrics(text):
    out = {}
    for line in text.splitlines():
        m = METRIC_LINE.match(line)
        if m:
            try:
                out.setdefault(m[1], []).append(float(m[2]))
            except ValueError:
                pass
    return out


def mget(m, *names, agg=sum):
    """First present metric among names (vLLM renamed several across versions), aggregated over label sets."""
    for n in names:
        if m and n in m:
            return agg(m[n])
    return None


RUNNING = ("vllm:num_requests_running",)
WAITING = ("vllm:num_requests_waiting",)
KV = ("vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc")
HISTS = {  # stage field -> histogram base name
    "queue_ms": "vllm:request_queue_time_seconds",
    "prefill_ms": "vllm:request_prefill_time_seconds",
    "decode_ms": "vllm:request_decode_time_seconds",
    "ttft_ms": "vllm:time_to_first_token_seconds",
    "e2e_ms": "vllm:e2e_request_latency_seconds",
}


def server_delta(a, b):
    """Mean of server-side histograms and counter ratios between two /metrics snapshots."""
    if not a or not b:
        return None
    out = {}
    for field, base in HISTS.items():
        ds = (mget(b, base + "_sum") or 0) - (mget(a, base + "_sum") or 0)
        dc = (mget(b, base + "_count") or 0) - (mget(a, base + "_count") or 0)
        out[field] = round(ds / dc * 1000, 1) if dc > 0 else None
    hits = ("vllm:prefix_cache_hits_total", "vllm:prefix_cache_hits")
    queries = ("vllm:prefix_cache_queries_total", "vllm:prefix_cache_queries")
    dq = (mget(b, *queries) or 0) - (mget(a, *queries) or 0)
    dh = (mget(b, *hits) or 0) - (mget(a, *hits) or 0)
    out["prefix_hit_pct"] = round(dh / dq * 100, 1) if dq > 0 else mget(b, "vllm:gpu_prefix_cache_hit_rate", agg=statistics.fmean)
    pre = ("vllm:num_preemptions_total", "vllm:num_preemptions")
    out["preemptions"] = (mget(b, *pre) or 0) - (mget(a, *pre) or 0)
    return out


# ---------------------------------------------------------------- stats

def dist(xs):
    if not xs:
        return None
    xs = sorted(xs)
    if len(xs) == 1:
        q = {p: xs[0] for p in (50, 90, 95, 99)}
    else:
        qs = statistics.quantiles(xs, n=100, method="inclusive")
        q = {p: qs[p - 1] for p in (50, 90, 95, 99)}
    r = {"mean": statistics.fmean(xs), "min": xs[0], "max": xs[-1]} | {f"p{p}": v for p, v in q.items()}
    return {k: round(v, 2) for k, v in r.items()}


def summarize(level, results, slo):
    ok = [r for r in results if r["ok"]]
    errs = Counter(r["error"] for r in results if not r["ok"])
    s = {"level": level, "requests": len(results), "ok": len(ok), "errors": len(results) - len(ok),
         "error_samples": [{"error": e, "count": n} for e, n in errs.most_common(5)]}
    if not ok:
        return s
    t0 = min(r["start"] for r in results)
    dur = max(r["end"] for r in results) - t0
    out_tok = sum(r["out"] for r in ok)
    in_tok = sum(r["in"] or 0 for r in ok)
    good = [r for r in ok if r["ttft"] * 1000 <= slo["ttft_ms"] and (r["tpot"] is None or r["tpot"] * 1000 <= slo["tpot_ms"])]
    tpot = dist([r["tpot"] * 1000 for r in ok if r["tpot"] is not None])
    s |= {
        "duration_s": round(dur, 2),
        "req_per_s": round(len(ok) / dur, 3),
        "out_tok_per_s": round(out_tok / dur, 1),
        "in_tok_per_s": round(in_tok / dur, 1),
        "total_tok_per_s": round((in_tok + out_tok) / dur, 1),
        "in_tokens_mean": round(in_tok / len(ok), 1),
        "out_tokens_mean": round(out_tok / len(ok), 1),
        "ttft": dist([r["ttft"] * 1000 for r in ok]),
        "tpot": tpot,
        "itl": dist([x * 1000 for r in ok for x in r["itl"]]),
        "e2e": dist([r["e2e"] * 1000 for r in ok]),
        "decode": dist([(r["e2e"] - r["ttft"]) * 1000 for r in ok]),
        "user_tok_per_s": round(1000 / tpot["p50"], 1) if tpot and tpot["p50"] else None,
        "goodput_pct": round(len(good) / len(results) * 100, 1),
        "goodput_rps": round(len(good) / dur, 3),
        # ponytail: samples capped at 2000/stage to keep run JSON small
        "samples": [[round(r["ttft"] * 1000, 1), round(r["tpot"] * 1000, 2) if r["tpot"] else None,
                     round(r["e2e"] * 1000, 1), r["in"], r["out"]] for r in ok[:2000]],
    }
    return s


def recommend(run):
    """Highest load level whose p95 TTFT/TPOT meet the SLO with <1% errors."""
    slo = run["config"]["slo"]
    best = None
    for s in run["stages"]:
        if not s.get("ttft") or s["errors"] > 0.01 * s["requests"]:
            continue
        if s["ttft"]["p95"] <= slo["ttft_ms"] and (not s["tpot"] or s["tpot"]["p95"] <= slo["tpot_ms"]):
            best = s
    return best


# ---------------------------------------------------------------- prompts

def _sharegpt_rows():
    if not SHAREGPT.exists() or SHAREGPT.stat().st_size == 0:
        return []
    return [json.loads(l) for l in SHAREGPT.read_text().splitlines() if l.strip()]


def vocabulary():
    words = {w.lower() for row in _sharegpt_rows()[:400] for w in row["prompt"].split() if w.isalpha() and len(w) < 14}
    return sorted(words) if len(words) > 500 else FALLBACK_WORDS


# ---------------------------------------------------------------- runner

class Runner:
    def __init__(self, run_id, cfg, api_key, save):
        self.cfg, self.api_key, self.save = cfg, api_key, save
        self.data = {"id": run_id, "name": cfg["name"] or run_id, "status": "running", "created": now_iso(),
                     "finished": None, "error": None, "warnings": [], "config": cfg, "resolved": None,
                     "calibration": None, "progress": {"phase": "başlıyor"}, "stages": [],
                     "timeseries": [], "gpu_series": []}
        self.inflight = 0
        self.metrics = None  # latest /metrics snapshot
        self.task = None

    def warn(self, msg):
        self.data["warnings"].append(msg)

    # -- requests

    async def send(self, req, ignore_eos=None):
        ignore_eos = self.ignore_eos if ignore_eos is None else ignore_eos
        body = {"model": self.model, "messages": req["messages"], "max_tokens": req["max_tokens"],
                "stream": True, "stream_options": {"include_usage": True}}
        if ignore_eos:
            body |= {"ignore_eos": True, "min_tokens": req["max_tokens"]}
        rec = {"ok": False, "error": None}
        first = last = usage = None
        itl, chunks = [], 0
        self.inflight += 1
        t0 = time.perf_counter()
        try:
            async with self.client.stream("POST", self.root + "/v1/chat/completions", json=body) as r:
                if r.status_code != 200:
                    rec["error"] = f"HTTP {r.status_code}: {(await r.aread())[:160].decode(errors='replace')}"
                    return rec
                async for line in r.aiter_lines():
                    if not line.startswith("data:"):
                        continue
                    payload = line[5:].strip()
                    if payload == "[DONE]":
                        break
                    ch = json.loads(payload)
                    if ch.get("error"):
                        rec["error"] = f"stream: {str(ch['error'])[:160]}"
                        return rec
                    usage = ch.get("usage") or usage
                    for c in ch.get("choices") or []:
                        d = c.get("delta") or {}
                        if d.get("content") or d.get("reasoning_content") or d.get("reasoning"):
                            now = time.perf_counter()
                            if first is None:
                                first = now
                            else:
                                itl.append(now - last)
                            last = now
                            chunks += 1
        except (httpx.HTTPError, json.JSONDecodeError) as e:
            rec["error"] = f"{type(e).__name__}: {e}"[:200]
            return rec
        finally:
            self.inflight -= 1
            rec["start"], rec["end"] = t0, time.perf_counter()
        if first is None:
            rec["error"] = "boş yanıt (token gelmedi)"
            return rec
        out = (usage or {}).get("completion_tokens") or chunks
        rec |= {"ok": True, "ttft": first - t0, "e2e": rec["end"] - t0, "itl": itl, "out": out,
                "in": (usage or {}).get("prompt_tokens"),
                "tpot": (last - first) / (out - 1) if out > 1 else None}
        return rec

    # -- prompt pool

    def words_for(self, tokens):
        a, b = self.data["calibration"]["tokens_per_word"], self.data["calibration"]["overhead_tokens"]
        return max(1, round((tokens - b) / a))

    def build_pool(self, n):
        ds, rnd = self.cfg["dataset"], self.rnd
        cap = ds["output_tokens"]
        if ds["type"] == "synthetic":
            prefix = []
            if ds["shared_prefix_tokens"]:
                prefix = [{"role": "system", "content": " ".join(rnd.choices(self.vocab, k=self.words_for(ds["shared_prefix_tokens"])))}]
            pool = []
            for _ in range(n):
                isl = max(1, round(rnd.gauss(ds["input_tokens"], ds["input_std"])))
                osl = max(1, round(rnd.gauss(ds["output_tokens"], ds["output_std"])))
                body = " ".join(rnd.choices(self.vocab, k=self.words_for(isl)))
                pool.append({"messages": prefix + [{"role": "user", "content": body}], "max_tokens": osl})
            return pool
        if ds["type"] == "sharegpt":
            rows = _sharegpt_rows()
            if not rows:
                raise RuntimeError("ShareGPT veri seti imajda yok (SKIP_SHAREGPT ile derlenmiş). Synthetic ya da özel veri seti kullanın.")
            tpw = self.data["calibration"]["tokens_per_word"]
            items = [{"messages": [{"role": "user", "content": r["prompt"]}],
                      "max_tokens": max(4, min(cap, round(r["output_words"] * tpw)))} for r in rows]
        else:
            path = Path(os.environ.get("DATA_DIR", "data")) / "datasets" / ds["custom_name"]
            if not ds["custom_name"] or not path.is_file():
                raise RuntimeError(f"Veri seti bulunamadı: {ds['custom_name']}")
            items = []
            for line in path.read_text().splitlines():
                if line.strip():
                    row = json.loads(line)
                    msgs = row.get("messages") or [{"role": "user", "content": row["prompt"]}]
                    items.append({"messages": msgs, "max_tokens": int(row.get("max_tokens") or cap)})
        return [rnd.choice(items) for _ in range(n)]

    # -- telemetry

    async def scrape(self):
        try:
            r = await self.client.get(self.metrics_url, timeout=5)
            if r.status_code == 200:
                self.metrics = parse_metrics(r.text)
        except httpx.HTTPError:
            pass
        return self.metrics

    async def telemetry_loop(self):
        while True:
            m = await self.scrape() if self.metrics_url else None
            kv = mget(m, *KV, agg=statistics.fmean)
            self.data["timeseries"].append({
                "t": round(time.time() - self.t_run0, 1), "inflight": self.inflight,
                "running": mget(m, *RUNNING), "waiting": mget(m, *WAITING),
                "kv": round(kv * 100, 1) if kv is not None else None})
            await asyncio.sleep(1)

    async def prom_range(self, query, start, end):
        r = await self.prom.get(self.prom_url + "/api/v1/query_range",
                                params={"query": query, "start": start, "end": end, "step": "5s"})
        r.raise_for_status()
        res = r.json()["data"]["result"]
        return {float(t): float(v) for t, v in res[0]["values"]} if res else {}

    async def gpu_stage(self, start, end):
        sel = self.cfg["prometheus"]["gpu_selector"]
        q = {"util": f"avg(DCGM_FI_DEV_GPU_UTIL{{{sel}}})",
             "mem_gb": f"sum(DCGM_FI_DEV_FB_USED{{{sel}}})/1024",
             "power_w": f"sum(DCGM_FI_DEV_POWER_USAGE{{{sel}}})"}
        try:
            series = {k: await self.prom_range(v, start, end) for k, v in q.items()}
        except (httpx.HTTPError, KeyError, ValueError) as e:
            self.warn(f"GPU metrikleri okunamadı: {e}")
            self.prom = None
            return None
        if not series["util"]:
            return None
        for t in sorted(series["util"]):
            self.data["gpu_series"].append({"t": round(t - self.t_run0, 1)} | {k: round(v[t], 1) if t in v else None for k, v in series.items()})
        vals = lambda k: list(series[k].values())
        return {"util_mean": round(statistics.fmean(vals("util")), 1), "util_max": round(max(vals("util")), 1),
                "mem_used_gb_max": round(max(vals("mem_gb")), 1) if vals("mem_gb") else None,
                "power_w_mean": round(statistics.fmean(vals("power_w")), 1) if vals("power_w") else None}

    async def setup_prometheus(self):
        p = self.cfg["prometheus"]
        self.prom = None
        if not p["enabled"]:
            return
        url = p["url"] or THANOS
        token = _sa_token()
        if not p["url"] and not token:
            self.warn("GPU telemetrisi atlandı: küme içinde değil ve Prometheus URL'i verilmedi")
            return
        ca = SA / "service-ca.crt"
        self.prom_url = url.rstrip("/")
        self.prom = httpx.AsyncClient(headers={"Authorization": f"Bearer {token}"} if token else {},
                                      verify=str(ca) if ca.exists() and not p["url"] else True, timeout=15)
        try:
            r = await self.prom.get(self.prom_url + "/api/v1/query", params={"query": f"count(DCGM_FI_DEV_GPU_UTIL{{{p['gpu_selector']}}})"})
            r.raise_for_status()
            res = r.json()["data"]["result"]
            if not res:
                raise ValueError("seçiciye uyan DCGM_FI_DEV_GPU_UTIL serisi yok")
            self.data["resolved"]["gpus"] = int(float(res[0]["value"][1]))
        except (httpx.HTTPError, KeyError, ValueError) as e:
            self.warn(f"GPU telemetrisi kapalı: {e}")
            await self.prom.aclose()
            self.prom = None

    # -- flow

    async def calibrate(self):
        """Measure tokens/word with two tiny requests and check ignore_eos support."""
        mk = lambda w, mt: {"messages": [{"role": "user", "content": " ".join(self.rnd.choices(self.vocab, k=w))}], "max_tokens": mt}
        r1 = await self.send(mk(100, 16))
        if not r1["ok"] and self.ignore_eos:
            r1 = await self.send(mk(100, 16), ignore_eos=False)
            if r1["ok"]:
                self.ignore_eos = False
                self.warn("Endpoint ignore_eos/min_tokens parametrelerini reddetti; çıkış uzunlukları modele bağlı olacak")
        if not r1["ok"]:
            raise RuntimeError(f"İlk istek başarısız: {r1['error']}")
        if self.ignore_eos and r1["in"] is not None and r1["out"] != 16:
            self.warn(f"ignore_eos etkisiz görünüyor (16 yerine {r1['out']} token üretildi)")
        r2 = await self.send(mk(600, 1), ignore_eos=False)
        if r1["in"] and r2["ok"] and r2["in"]:
            a = (r2["in"] - r1["in"]) / 500
            cal = {"tokens_per_word": round(a, 3), "overhead_tokens": round(r1["in"] - 100 * a)}
        else:
            cal = {"tokens_per_word": 1.3, "overhead_tokens": 0}
            self.warn("Endpoint usage bilgisi döndürmüyor; token sayıları yaklaşık")
        self.data["calibration"] = cal

    async def run_stage(self, level, reqs):
        load = self.cfg["load"]
        results = []
        prog = self.data["progress"]

        async def go(r):
            res = await self.send(r)
            results.append(res)
            prog["done"] += 1

        if load["mode"] == "concurrency":
            it = iter(reqs)

            async def worker():
                for r in it:  # shared iterator: each request taken once
                    await go(r)
            await asyncio.gather(*(worker() for _ in range(int(level))))
        else:
            sem = asyncio.Semaphore(load["max_concurrency"])

            async def limited(r):
                async with sem:
                    await go(r)
            tasks = []
            theta = 1 / (level * load["burstiness"])
            for r in reqs:
                tasks.append(asyncio.create_task(limited(r)))
                await asyncio.sleep(self.rnd.gammavariate(load["burstiness"], theta))
            await asyncio.gather(*tasks)
        return results

    async def _run(self):
        cfg, load = self.cfg, self.cfg["load"]
        self.rnd = random.Random(0)
        self.vocab = vocabulary()
        self.ignore_eos = cfg["ignore_eos"]
        ep = cfg["endpoint"]
        self.root, self.metrics_url = await resolve(ep)
        levels = [int(l) if load["mode"] == "concurrency" else l for l in load["levels"]]
        top = max(levels) if load["mode"] == "concurrency" else load["max_concurrency"]
        self.client = make_client(ep, self.api_key, max_conn=max(top, load["warmup"]), timeout=load["timeout_s"])
        models, max_len = await list_models(self.client, self.root)
        self.model = cfg["model"] or (models[0] if models else "")
        if not self.model:
            raise RuntimeError("Model adı yok ve /v1/models boş döndü")
        self.data["resolved"] = {"base_url": self.root, "metrics_url": self.metrics_url,
                                 "model": self.model, "max_model_len": max_len}
        if self.metrics_url and not (await self.scrape() or {}).get(RUNNING[0]):
            self.warn(f"vLLM /metrics okunamadı ({self.metrics_url}); sunucu tarafı prefill/decode/KV cache verisi olmayacak")
            self.metrics_url = None
            self.data["resolved"]["metrics_url"] = None
        await self.setup_prometheus()

        ds = cfg["dataset"]
        self.data["progress"] = {"phase": "kalibrasyon"}
        await self.calibrate()
        if max_len and ds["type"] == "synthetic" and ds["input_tokens"] + ds["shared_prefix_tokens"] + ds["output_tokens"] > max_len:
            self.warn(f"Giriş+çıkış ({ds['input_tokens'] + ds['shared_prefix_tokens'] + ds['output_tokens']}) max_model_len ({max_len}) değerini aşıyor; istekler reddedilebilir")

        self.t_run0 = time.time()
        tele = asyncio.create_task(self.telemetry_loop())
        try:
            if load["warmup"]:
                self.data["progress"] = {"phase": "ısınma", "stage": 0, "total": len(levels), "done": 0, "of": load["warmup"]}
                await asyncio.gather(*(self.send(r) for r in self.build_pool(load["warmup"])))
            for i, level in enumerate(levels, 1):
                n = max(load["requests_per_stage"], int(level) * 2) if load["mode"] == "concurrency" else load["requests_per_stage"]
                self.data["progress"] = {"phase": "ölçüm", "stage": i, "total": len(levels), "level": level, "done": 0, "of": n}
                reqs = self.build_pool(n)
                m0 = dict(await self.scrape()) if self.metrics_url else None
                w0 = time.time()
                results = await self.run_stage(level, reqs)
                w1 = time.time()
                m1 = await self.scrape() if self.metrics_url else None
                s = summarize(level, results, cfg["slo"])
                s["t_start"], s["t_end"] = round(w0 - self.t_run0, 1), round(w1 - self.t_run0, 1)
                s["server"] = server_delta(m0, m1)
                if s["server"]:
                    win = [p for p in self.data["timeseries"] if s["t_start"] <= p["t"] <= s["t_end"]]
                    pick = lambda k: [p[k] for p in win if p[k] is not None]
                    s["server"] |= {"kv_cache_max_pct": max(pick("kv"), default=None),
                                    "kv_cache_mean_pct": round(statistics.fmean(pick("kv")), 1) if pick("kv") else None,
                                    "waiting_max": max(pick("waiting"), default=None),
                                    "running_max": max(pick("running"), default=None)}
                s["gpu"] = await self.gpu_stage(w0, w1) if self.prom else None
                self.data["stages"].append(s)
                self.save(self.data)
            self.data["progress"]["phase"] = "bitti"
        finally:
            tele.cancel()

    async def run(self):
        try:
            await self._run()
            self.data["status"] = "done"
        except asyncio.CancelledError:
            self.data["status"] = "stopped"
        except Exception as e:  # surface any failure in the UI instead of a silent dead task
            self.data["status"] = "failed"
            self.data["error"] = f"{type(e).__name__}: {e}"
        finally:
            self.data["finished"] = now_iso()
            self.save(self.data)
            for c in ("client", "prom"):
                if getattr(self, c, None):
                    await getattr(self, c).aclose()
