"""End-to-end self-check against mock_vllm: python test_bench.py [report.html]"""
import asyncio
import os
import sys
import tempfile
import threading
import time

import uvicorn

os.environ.setdefault("DATA_DIR", tempfile.mkdtemp())
import bench  # noqa: E402
import report  # noqa: E402
from app import Config  # noqa: E402

PORT = 18901


def serve():
    uvicorn.run("mock_vllm:app", port=PORT, log_level="warning")


async def main():
    assert bench.parse_metrics('vllm:x{a="1"} 2\nvllm:x{a="2"} 3\n# c\n')["vllm:x"] == [2.0, 3.0]
    assert bench.dist([1, 2, 3, 4])["p50"] == 2.5
    cfg = Config(endpoint={"type": "url", "url": f"http://127.0.0.1:{PORT}/v1"},
                 dataset={"input_tokens": 400, "output_tokens": 40},
                 load={"levels": [1, 8, 64], "requests_per_stage": 40, "warmup": 2},
                 slo={"ttft_ms": 300, "tpot_ms": 15}, prometheus={"enabled": False}).model_dump()
    saved = []
    r = bench.Runner("t1", cfg, "", saved.append)
    await r.run()
    d = r.data
    assert d["status"] == "done", d["error"]
    assert d["resolved"]["model"] == "mock-model"
    assert 1.2 < d["calibration"]["tokens_per_word"] < 1.4, d["calibration"]
    s1, s8, s64 = d["stages"]
    assert s1["ok"] == 40 and s1["out_tokens_mean"] == 40, s1  # ignore_eos honoured
    assert 350 < s1["in_tokens_mean"] < 450, s1["in_tokens_mean"]  # synthetic ISL hits target
    assert s8["out_tok_per_s"] > 3 * s1["out_tok_per_s"]  # batching scales throughput
    assert s64["server"]["queue_ms"] > 10 and s64["server"]["waiting_max"] > 0  # 64 > MAX_SEQS=32 queues
    assert s64["ttft"]["p95"] > s1["ttft"]["p95"]
    assert bench.recommend(d)["level"] in (1, 8), [(s["level"], s["ttft"]["p95"], s["tpot"]["p95"]) for s in d["stages"]]
    html = report.render([d])
    assert "SLO içi kapasite" in html and "Kırılma noktası" in html
    d2 = d | {"name": "ikinci", "id": "t2"}
    assert "Model karşılaştırma" in report.render([d, d2])
    out = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.environ["DATA_DIR"], "report.html")
    open(out, "w").write(html)
    for s in d["stages"]:
        print(f"c={s['level']:<3} tok/s={s['out_tok_per_s']:>7} ttft95={s['ttft']['p95']:>7} tpot95={s['tpot']['p95']:>6} "
              f"queue={s['server']['queue_ms']} kv={s['server']['kv_cache_max_pct']}")
    print("OK ->", out)


if __name__ == "__main__":
    threading.Thread(target=serve, daemon=True).start()
    time.sleep(1.5)
    asyncio.run(main())
