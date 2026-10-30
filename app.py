"""LLM Bench web API: start/stop runs, history, reports, datasets."""
import asyncio
import json
import os
import re
import secrets
from datetime import datetime
from pathlib import Path
from typing import Literal

import plotly
from fastapi import FastAPI, HTTPException, UploadFile
from fastapi.responses import FileResponse, HTMLResponse
from pydantic import BaseModel, Field, field_validator

import bench
import report

DATA = Path(os.environ.get("DATA_DIR", "data"))
RUNS, DATASETS = DATA / "runs", DATA / "datasets"
RUNS.mkdir(parents=True, exist_ok=True)
DATASETS.mkdir(parents=True, exist_ok=True)
STATIC = Path(__file__).parent / "static"
PLOTLY_JS = Path(plotly.__file__).parent / "package_data" / "plotly.min.js"
SAFE_ID = re.compile(r"^[\w.-]{1,80}$")


class Endpoint(BaseModel):
    type: Literal["litellm", "service", "route", "pod", "url"]
    url: str = ""
    name: str = ""
    namespace: str = ""
    port: int = Field(8080, ge=1, le=65535)
    scheme: Literal["http", "https"] = "http"
    api_key: str = ""
    verify_tls: bool = True
    metrics_url: str = ""


class Dataset(BaseModel):
    type: Literal["synthetic", "sharegpt", "custom"] = "synthetic"
    custom_name: str = ""
    input_tokens: int = Field(512, ge=1, le=1_000_000)
    input_std: float = Field(0, ge=0)
    output_tokens: int = Field(256, ge=1, le=131_072)
    output_std: float = Field(0, ge=0)
    shared_prefix_tokens: int = Field(0, ge=0, le=1_000_000)


class Load(BaseModel):
    mode: Literal["concurrency", "rate"] = "concurrency"
    levels: list[float] = Field([1, 2, 4, 8, 16, 32, 64], min_length=1, max_length=50)
    requests_per_stage: int = Field(100, ge=1, le=100_000)
    max_concurrency: int = Field(256, ge=1, le=4096)
    burstiness: float = Field(1.0, gt=0, le=100)
    warmup: int = Field(4, ge=0, le=512)
    timeout_s: float = Field(300, gt=0, le=3600)

    @field_validator("levels")
    @classmethod
    def positive(cls, v):
        if any(not 0 < x <= 4096 for x in v):
            raise ValueError("seviyeler 0 ile 4096 arasında olmalı")
        return v


class SLO(BaseModel):
    ttft_ms: float = Field(2000, gt=0)
    tpot_ms: float = Field(100, gt=0)


class Prometheus(BaseModel):
    enabled: bool = True
    url: str = ""
    gpu_selector: str = Field("", pattern=r"^[^{}]*$")


class Config(BaseModel):
    name: str = Field("", max_length=100)
    endpoint: Endpoint
    model: str = ""
    dataset: Dataset = Dataset()
    load: Load = Load()
    ignore_eos: bool = True
    slo: SLO = SLO()
    prometheus: Prometheus = Prometheus()


class ResolveReq(BaseModel):
    endpoint: Endpoint
    model: str = ""


app = FastAPI(title="LLM Bench")
current: bench.Runner | None = None  # ponytail: one run at a time on purpose; parallel runs would skew each other


def save(run):
    tmp = RUNS / f"{run['id']}.json.tmp"
    tmp.write_text(json.dumps(run))
    tmp.replace(RUNS / f"{run['id']}.json")


def load_run(run_id):
    if current and current.data["id"] == run_id:
        return current.data
    if not SAFE_ID.match(run_id) or not (RUNS / f"{run_id}.json").exists():
        raise HTTPException(404, "Test bulunamadı")
    return json.loads((RUNS / f"{run_id}.json").read_text())


# runs left "running" by a crashed/restarted pod
for f in RUNS.glob("*.json"):
    r = json.loads(f.read_text())
    if r["status"] == "running":
        r |= {"status": "failed", "error": "Pod yeniden başladı, test yarıda kaldı"}
        save(r)


@app.get("/healthz")
def healthz():
    return {"ok": True}


@app.get("/")
def index():
    return FileResponse(STATIC / "index.html")


@app.get("/static/plotly.min.js")
def plotly_js():
    return FileResponse(PLOTLY_JS, media_type="application/javascript")


@app.post("/api/resolve")
async def resolve(req: ResolveReq):
    try:
        return await bench.probe(req.endpoint.model_dump(), req.endpoint.api_key)
    except ValueError as e:
        raise HTTPException(400, str(e))


@app.post("/api/runs")
async def start(cfg: Config):
    global current
    if current and current.data["status"] == "running":
        raise HTTPException(409, f"Zaten çalışan bir test var: {current.data['name']}")
    if cfg.endpoint.type == "litellm" and not cfg.model:
        raise HTTPException(400, "LiteLLM için model adı gerekli")
    d = cfg.model_dump()
    api_key = d["endpoint"].pop("api_key")  # never persisted
    run_id = datetime.now().strftime("%Y%m%d-%H%M%S-") + secrets.token_hex(2)
    current = bench.Runner(run_id, d, api_key, save)
    save(current.data)
    current.task = asyncio.create_task(current.run())
    return {"id": run_id}


@app.get("/api/runs")
def runs():
    out = []
    for f in sorted(RUNS.glob("*.json"), reverse=True):
        r = load_run(f.stem)
        st = [s for s in r["stages"] if s.get("ttft")]
        out.append({"id": r["id"], "name": r["name"], "status": r["status"], "created": r["created"],
                    "model": (r.get("resolved") or {}).get("model") or r["config"]["model"],
                    "stages_done": len(r["stages"]), "stages_total": len(r["config"]["load"]["levels"]),
                    "peak_out_tok_per_s": max((s["out_tok_per_s"] for s in st), default=None),
                    "best_ttft_p50_ms": min((s["ttft"]["p50"] for s in st), default=None)})
    return out


@app.get("/api/runs/{run_id}")
def get_run(run_id: str):
    return load_run(run_id)


@app.post("/api/runs/{run_id}/stop")
def stop(run_id: str):
    if not (current and current.data["id"] == run_id and current.data["status"] == "running"):
        raise HTTPException(409, "Bu test çalışmıyor")
    current.task.cancel()
    return {"ok": True}


@app.delete("/api/runs/{run_id}")
def delete(run_id: str):
    if current and current.data["id"] == run_id and current.data["status"] == "running":
        raise HTTPException(409, "Çalışan test silinemez, önce durdurun")
    load_run(run_id)
    (RUNS / f"{run_id}.json").unlink()
    return {"ok": True}


@app.get("/api/runs/{run_id}/report", response_class=HTMLResponse)
def run_report(run_id: str):
    return report.render([load_run(run_id)])


@app.get("/api/compare", response_class=HTMLResponse)
def compare(ids: str):
    return report.render([load_run(i) for i in ids.split(",") if i][:8])


@app.get("/api/datasets")
def datasets():
    return [{"name": f.name, "count": sum(1 for l in f.open() if l.strip())} for f in sorted(DATASETS.glob("*.jsonl"))]


@app.post("/api/datasets")
async def upload(file: UploadFile):
    name = re.sub(r"[^\w.-]", "_", Path(file.filename or "data").stem)[:60] + ".jsonl"
    raw = await file.read(50_000_001)
    if len(raw) > 50_000_000:
        raise HTTPException(413, "En fazla 50 MB")
    lines = [l for l in raw.decode("utf-8", errors="replace").splitlines() if l.strip()]
    for i, line in enumerate(lines, 1):
        try:
            row = json.loads(line)
            assert isinstance(row.get("prompt"), str) or isinstance(row.get("messages"), list)
        except (ValueError, AssertionError, AttributeError):
            raise HTTPException(400, f"Satır {i}: 'prompt' (metin) veya 'messages' (liste) alanı olan JSON olmalı")
    if not lines:
        raise HTTPException(400, "Dosya boş")
    (DATASETS / name).write_text("\n".join(lines) + "\n")
    return {"name": name, "count": len(lines)}
