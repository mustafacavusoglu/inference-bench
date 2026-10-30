"""Fake vLLM OpenAI server for local testing: streaming chat, batching slowdown, KV cache and /metrics.
Run: uvicorn mock_vllm:app --port 8001"""
import asyncio
import json
import time

from fastapi import FastAPI, Request
from fastapi.responses import PlainTextResponse, StreamingResponse

app = FastAPI()
MAX_SEQS, KV_TOKENS = 32, 60_000
st = {"running": 0, "waiting": 0, "kv": 0, "hist": {k: [0.0, 0] for k in ("queue", "prefill", "decode", "ttft", "e2e")}}


def observe(k, v):
    st["hist"][k][0] += v
    st["hist"][k][1] += 1


@app.get("/v1/models")
def models():
    return {"data": [{"id": "mock-model", "max_model_len": 8192}]}


@app.post("/v1/chat/completions")
async def chat(req: Request):
    body = await req.json()
    text = " ".join(m["content"] for m in body["messages"])
    p_tok = int(len(text.split()) * 1.3) + 10
    n = body["max_tokens"] if body.get("ignore_eos") else max(1, body["max_tokens"] // 2)

    async def gen():
        t0 = time.perf_counter()
        st["waiting"] += 1
        while st["running"] >= MAX_SEQS or st["kv"] + p_tok + n > KV_TOKENS:
            await asyncio.sleep(0.005)
        st["waiting"] -= 1
        st["running"] += 1
        st["kv"] += p_tok + n
        tq = time.perf_counter()
        try:
            await asyncio.sleep(0.00005 * p_tok * (1 + st["running"] / 8))  # prefill
            tp = time.perf_counter()
            for i in range(n):
                if i:
                    await asyncio.sleep(0.004 * (1 + st["running"] / 8))  # decode slows with batch size
                yield f"data: {json.dumps({'choices': [{'delta': {'content': 'tok '}}]})}\n\n"
            yield f"data: {json.dumps({'choices': [], 'usage': {'prompt_tokens': p_tok, 'completion_tokens': n}})}\n\n"
            yield "data: [DONE]\n\n"
            te = time.perf_counter()
            for k, v in (("queue", tq - t0), ("prefill", tp - tq), ("decode", te - tp), ("ttft", tp - t0), ("e2e", te - t0)):
                observe(k, v)
        finally:
            st["running"] -= 1
            st["kv"] -= p_tok + n

    return StreamingResponse(gen(), media_type="text/event-stream")


@app.get("/metrics", response_class=PlainTextResponse)
def metrics():
    lab = '{model_name="mock-model"}'
    lines = [f"vllm:num_requests_running{lab} {st['running']}", f"vllm:num_requests_waiting{lab} {st['waiting']}",
             f"vllm:kv_cache_usage_perc{lab} {st['kv'] / KV_TOKENS}"]
    names = {"queue": "request_queue_time_seconds", "prefill": "request_prefill_time_seconds",
             "decode": "request_decode_time_seconds", "ttft": "time_to_first_token_seconds", "e2e": "e2e_request_latency_seconds"}
    for k, (s, c) in st["hist"].items():
        lines += [f"vllm:{names[k]}_sum{lab} {s}", f"vllm:{names[k]}_count{lab} {c}"]
    return "\n".join(lines) + "\n"
