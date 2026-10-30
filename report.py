"""Self-contained HTML report (single run or comparison) with plotly charts."""
import math
from html import escape

import plotly.graph_objects as go
from plotly.offline import get_plotlyjs
from plotly.subplots import make_subplots

from bench import recommend

# validated categorical order (dataviz reference palette); entity keeps its slot
C = ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"]
SLO_COLOR = "#e34948"


def f(v, d=0, unit=""):
    return "–" if v is None else f"{v:,.{d}f}{unit}"


def g(s, *path):
    for p in path:
        s = (s or {}).get(p)
    return s


def unit(run):
    return "eşzamanlı istek" if run["config"]["load"]["mode"] == "concurrency" else "istek/s"


def lvl_label(run, level):
    return f"c={level:g}" if run["config"]["load"]["mode"] == "concurrency" else f"{level:g} istek/s"


def stages(run):
    return [s for s in run["stages"] if s.get("ttft")]


def html_fig(fig, height=320, xlog_levels=None, title=""):
    fig.update_layout(
        title=dict(text=title, x=0, xanchor="left", font=dict(size=14)), height=height,
        margin=dict(l=56, r=16, t=44, b=40), template="none",
        paper_bgcolor="rgba(0,0,0,0)", plot_bgcolor="rgba(0,0,0,0)",
        font=dict(family="system-ui, -apple-system, Segoe UI, sans-serif", size=12),
        legend=dict(orientation="h", y=1.0, yanchor="bottom", x=1, xanchor="right", bgcolor="rgba(0,0,0,0)"),
        hovermode="x unified")
    fig.update_xaxes(showgrid=True, zeroline=False)
    fig.update_yaxes(showgrid=True, zeroline=False, rangemode="tozero")
    if xlog_levels:
        fig.update_xaxes(type="log", tickvals=xlog_levels, ticktext=[f"{v:g}" for v in xlog_levels])
    return fig.to_html(full_html=False, include_plotlyjs=False, config={"displaylogo": False, "responsive": True})


def line(fig, x, y, name, color, dash=None, text=None, **kw):
    fig.add_trace(go.Scatter(x=x, y=y, name=name, mode="lines+markers", text=text,
                             line=dict(color=color, width=2, dash=dash), marker=dict(size=8), **kw))


def slo_line(fig, y, label):
    fig.add_hline(y=y, line=dict(color=SLO_COLOR, width=1.5, dash="dash"),
                  annotation_text=f"SLO {label}", annotation_position="top left", annotation_font_color=SLO_COLOR)


# ---------------------------------------------------------------- insights

def insights(run):
    st, slo, rec = stages(run), run["config"]["slo"], recommend(run)
    if not st:
        return ["Tamamlanmış ölçüm yok."]
    out = []
    u = unit(run)
    if rec:
        out.append(f"<b>{escape(run['name'])}</b>, p95 TTFT ≤ {slo['ttft_ms']:g} ms ve p95 TPOT ≤ {slo['tpot_ms']:g} ms hedefinde "
                   f"en fazla <b>{rec['level']:g} {u}</b> karşılıyor; bu noktada <b>{f(rec['out_tok_per_s'])} tok/s</b> "
                   f"({f(rec['req_per_s'], 2)} istek/s) üretiyor.")
    else:
        out.append(f"<b>En düşük yük seviyesinde bile SLO karşılanmadı</b> (p95 TTFT {f(st[0]['ttft']['p95'])} ms, "
                   f"p95 TPOT {f(g(st[0], 'tpot', 'p95'))} ms). Daha düşük yük, daha kısa girdi veya daha güçlü donanım gerekli.")
    after = [s for s in st if rec and s["level"] > rec["level"]] or ([] if rec else st[:1])
    if after:
        b = after[0]
        why = []
        if b["ttft"]["p95"] > slo["ttft_ms"]:
            why.append(f"TTFT p95 {f(b['ttft']['p95'])} ms")
        if g(b, "tpot", "p95") and b["tpot"]["p95"] > slo["tpot_ms"]:
            why.append(f"TPOT p95 {f(b['tpot']['p95'])} ms")
        if b["errors"] > 0.01 * b["requests"]:
            why.append(f"hata oranı %{b['errors'] / b['requests'] * 100:.1f}")
        cause = []
        sv = b.get("server") or {}
        if (sv.get("kv_cache_max_pct") or 0) >= 90:
            cause.append(f"KV cache %{sv['kv_cache_max_pct']:.0f} doldu")
        if sv.get("preemptions"):
            cause.append(f"{sv['preemptions']:.0f} preemption (KV cache yetersiz)")
        if (sv.get("waiting_max") or 0) > 0 and sv.get("queue_ms"):
            cause.append(f"istekler sunucu kuyruğunda ortalama {f(sv['queue_ms'])} ms bekledi")
        if g(b, "tpot", "p95") and b["tpot"]["p95"] > slo["tpot_ms"] and not cause:
            cause.append("batch büyüdükçe token üretimi (decode) yavaşladı")
        if why:
            out.append(f"Kırılma noktası <b>{lvl_label(run, b['level'])}</b>: {', '.join(why)}"
                       + (f" — sebep: {'; '.join(cause)}." if cause else "."))
    for prev, s in zip(st, st[1:]):
        if prev["out_tok_per_s"] and s["out_tok_per_s"] < prev["out_tok_per_s"] * 1.1:
            out.append(f"Throughput {lvl_label(run, prev['level'])} sonrasında doyuma ulaşıyor "
                       f"({f(prev['out_tok_per_s'])} → {f(s['out_tok_per_s'])} tok/s); daha fazla yük yalnızca gecikmeyi artırır.")
            break
    ref = rec or st[0]
    total = ref["ttft"]["mean"] + ref["decode"]["mean"]
    out.append(f"{lvl_label(run, ref['level'])} seviyesinde bir isteğin süresinin %{ref['ttft']['mean'] / total * 100:.0f}'i ilk token'a kadar "
               f"(kuyruk + prefill), %{ref['decode']['mean'] / total * 100:.0f}'i token üretimine (decode) gidiyor. "
               f"Ortalama giriş {f(ref['in_tokens_mean'])} / çıkış {f(ref['out_tokens_mean'])} token.")
    if st[0].get("user_tok_per_s"):
        out.append(f"Tek kullanıcı, yanıtı saniyede ~{f(st[0]['user_tok_per_s'])} token hızında görür "
                   f"(insan okuma hızı ~5–10 token/s).")
    errs = sum(s["errors"] for s in run["stages"])
    if errs:
        top = next((e for s in run["stages"] for e in s.get("error_samples", [])), None)
        out.append(f"Toplam {errs} hatalı istek. En sık: <code>{escape(top['error']) if top else ''}</code>")
    return out


def kpis(run):
    st, rec = stages(run), recommend(run)
    if not st:
        return []
    peak = max(st, key=lambda s: s["out_tok_per_s"])
    total = sum(s["requests"] for s in run["stages"])
    errs = sum(s["errors"] for s in run["stages"])
    tiles = [
        ("SLO içi kapasite", f"{rec['level']:g}" if rec else "Yok", unit(run) if rec else "SLO karşılanmadı", "good" if rec else "bad"),
        ("SLO noktasında throughput", f(rec["out_tok_per_s"]) if rec else "–", f"tok/s · {f(rec['req_per_s'], 2)} istek/s" if rec else "", ""),
        ("Tepe throughput", f(peak["out_tok_per_s"]), f"tok/s @ {lvl_label(run, peak['level'])}", ""),
        ("İlk token (tek kullanıcı)", f(st[0]["ttft"]["p50"]), f"ms TTFT p50 @ {lvl_label(run, st[0]['level'])}", ""),
        ("Kullanıcı başına hız", f(st[0].get("user_tok_per_s")), "tok/s (1000 / TPOT p50)", ""),
        ("Hata oranı", f"%{errs / total * 100:.1f}" if total else "–", f"{errs} / {total} istek", "bad" if errs > 0.01 * total else "good"),
    ]
    gpus = [s for s in st if s.get("gpu")]
    if gpus:
        gp = max(gpus, key=lambda s: s["gpu"]["util_max"])
        tiles.append(("GPU kullanımı (tepe)", f(gp["gpu"]["util_max"], 0, "%"), f"bellek {f(gp['gpu']['mem_used_gb_max'], 1)} GB", ""))
        pk = peak.get("gpu") or {}
        if pk.get("power_w_mean"):
            tiles.append(("Enerji verimliliği", f(peak["out_tok_per_s"] / pk["power_w_mean"], 2), f"tok/J @ {f(pk['power_w_mean'])} W", ""))
    return tiles


# ---------------------------------------------------------------- charts

def x_levels(runs):
    return sorted({s["level"] for r in runs for s in stages(r)})


def single_charts(run):
    st, slo = stages(run), run["config"]["slo"]
    x = [s["level"] for s in st]
    xl = x_levels([run])
    xt = f"Yük ({unit(run)})"
    rec = recommend(run)
    out = []

    fig = go.Figure()
    line(fig, x, [s["out_tok_per_s"] for s in st], "Çıkış tok/s", C[0])
    fig.update_layout(showlegend=False)
    if rec:
        fig.add_vline(x=math.log10(rec["level"]),  # shapes use log10 coords on log axes
                      line=dict(color=C[5], width=1.5, dash="dash"),
                      annotation_text="SLO sınırı", annotation_position="top left")
    fig.update_layout(xaxis_title=xt, yaxis_title="token / s")
    out.append(html_fig(fig, xlog_levels=xl, title="Throughput — yük arttıkça sistem ne kadar üretiyor?"))

    fig = go.Figure()
    for i, p in enumerate(["p50", "p95", "p99"]):
        line(fig, x, [s["ttft"][p] for s in st], f"TTFT {p}", C[i])
    slo_line(fig, slo["ttft_ms"], f"{slo['ttft_ms']:g} ms")
    fig.update_layout(xaxis_title=xt, yaxis_title="ms")
    out.append(html_fig(fig, xlog_levels=xl, title="TTFT — ilk token'a kadar bekleme"))

    fig = go.Figure()
    for i, p in enumerate(["p50", "p95"]):
        line(fig, x, [g(s, "tpot", p) for s in st], f"TPOT {p}", C[i])
    line(fig, x, [g(s, "itl", "p99") for s in st], "ITL p99", C[2], dash="dot")
    slo_line(fig, slo["tpot_ms"], f"{slo['tpot_ms']:g} ms")
    fig.update_layout(xaxis_title=xt, yaxis_title="ms / token")
    out.append(html_fig(fig, xlog_levels=xl, title="TPOT / ITL — token başına üretim süresi"))

    fig = go.Figure()
    for i, p in enumerate(["p50", "p95"]):
        line(fig, x, [s["e2e"][p] / 1000 for s in st], f"E2E {p}", C[i])
    fig.update_layout(xaxis_title=xt, yaxis_title="saniye")
    out.append(html_fig(fig, xlog_levels=xl, title="Uçtan uca yanıt süresi"))

    fig = go.Figure()
    fig.add_trace(go.Scatter(x=[s["out_tok_per_s"] for s in st], y=[s.get("user_tok_per_s") for s in st],
                             mode="lines+markers+text", text=[lvl_label(run, s["level"]) for s in st],
                             textposition="top right", line=dict(color=C[0], width=2), marker=dict(size=8),
                             name=run["name"], hovertemplate="%{text}<br>sistem %{x:,.0f} tok/s<br>kullanıcı %{y:.1f} tok/s<extra></extra>"))
    fig.update_layout(xaxis_title="Sistem throughput (tok/s)", yaxis_title="Kullanıcı başına tok/s", hovermode="closest", showlegend=False)
    out.append(html_fig(fig, title="Verim ↔ kullanıcı deneyimi dengesi (sağ üst = iyi)"))

    fig = go.Figure()
    fig.add_trace(go.Bar(x=x, y=[s["goodput_pct"] for s in st], name="Goodput %",
                         marker=dict(color=[C[2] if s["goodput_pct"] >= 90 else C[7] for s in st], cornerradius=4),
                         text=[f"{s['goodput_pct']:.0f}%" for s in st], textposition="outside", cliponaxis=False))
    fig.update_layout(xaxis_title=xt, yaxis_title="% istek", yaxis_range=[0, 110], xaxis_type="category", showlegend=False)
    out.append(html_fig(fig, title="Goodput — SLO'yu karşılayan isteklerin oranı (yeşil ≥ %90)"))

    sv = [s for s in st if s.get("server") and s["server"].get("prefill_ms") is not None]
    fig = go.Figure()
    if sv:
        parts = [("queue_ms", "Kuyruk"), ("prefill_ms", "Prefill"), ("decode_ms", "Decode")]
        src = "sunucu (vLLM /metrics)"
        for i, (k, name) in enumerate(parts):
            fig.add_trace(go.Bar(x=[lvl_label(run, s["level"]) for s in sv], y=[s["server"][k] or 0 for s in sv], name=name,
                                 marker=dict(color=C[i], line=dict(width=2, color="rgba(0,0,0,0)"))))
    else:
        src = "istemci (TTFT = kuyruk + prefill + ağ)"
        for i, (k, name) in enumerate([("ttft", "İlk token (TTFT)"), ("decode", "Decode")]):
            fig.add_trace(go.Bar(x=[lvl_label(run, s["level"]) for s in st], y=[s[k]["mean"] for s in st], name=name, marker_color=C[i]))
    fig.update_layout(barmode="stack", bargap=0.35, yaxis_title="ortalama ms")
    out.append(html_fig(fig, title=f"İstek süresi dağılımı — {src}"))

    fig = go.Figure()
    for s in st:
        fig.add_trace(go.Box(y=[r[0] for r in s.get("samples", [])], name=lvl_label(run, s["level"]),
                             marker_color=C[0], boxpoints=False, line=dict(width=1.5)))
    slo_line(fig, slo["ttft_ms"], f"{slo['ttft_ms']:g} ms")
    fig.update_layout(yaxis_title="TTFT ms", showlegend=False, hovermode="closest")
    out.append(html_fig(fig, title="TTFT dağılımı — kutu: p25–p75, bıyık: uç değerler"))
    return out


def timeline(run):
    ts, gs = run.get("timeseries") or [], run.get("gpu_series") or []
    if not ts:
        return ""
    fig = make_subplots(rows=2, cols=1, shared_xaxes=True, vertical_spacing=0.08,
                        subplot_titles=("İstek sayısı", "Doluluk (%)"))
    t = [p["t"] for p in ts]
    for i, (k, name) in enumerate([("inflight", "İstemcide açık istek"), ("running", "vLLM çalışan"), ("waiting", "vLLM kuyrukta")]):
        if any(p.get(k) is not None for p in ts):
            fig.add_trace(go.Scatter(x=t, y=[p.get(k) for p in ts], name=name, line=dict(color=C[i], width=2)), row=1, col=1)
    if any(p.get("kv") is not None for p in ts):
        fig.add_trace(go.Scatter(x=t, y=[p["kv"] for p in ts], name="KV cache %", line=dict(color=C[3], width=2)), row=2, col=1)
    if gs:
        fig.add_trace(go.Scatter(x=[p["t"] for p in gs], y=[p["util"] for p in gs], name="GPU util %",
                                 line=dict(color=C[6], width=2)), row=2, col=1)
    for i, s in enumerate(run["stages"]):
        if "t_start" in s:
            fig.add_vrect(x0=s["t_start"], x1=s["t_end"], fillcolor="#888", opacity=0.07 if i % 2 else 0.0, line_width=0,
                          annotation_text=lvl_label(run, s["level"]), annotation_position="top left", annotation_font_size=10)
    fig.update_yaxes(range=[0, 105], row=2, col=1)
    fig.update_xaxes(title_text="test başından itibaren saniye", row=2, col=1)
    return html_fig(fig, height=460, title="Zaman çizelgesi — yük, kuyruk, KV cache ve GPU")


def compare_charts(runs):
    xl = x_levels(runs)
    out = []
    specs = [
        ("Throughput (çıkış tok/s)", "token / s", lambda s: s["out_tok_per_s"]),
        ("TTFT p95", "ms", lambda s: s["ttft"]["p95"]),
        ("TPOT p95", "ms / token", lambda s: g(s, "tpot", "p95")),
        ("Goodput %", "% istek", lambda s: s["goodput_pct"]),
    ]
    slo = runs[0]["config"]["slo"]
    for title, yt, fn in specs:
        fig = go.Figure()
        for i, r in enumerate(runs):
            st = stages(r)
            line(fig, [s["level"] for s in st], [fn(s) for s in st], r["name"], C[i % len(C)])
        if title == "TTFT p95":
            slo_line(fig, slo["ttft_ms"], f"{slo['ttft_ms']:g} ms")
        if title == "TPOT p95":
            slo_line(fig, slo["tpot_ms"], f"{slo['tpot_ms']:g} ms")
        fig.update_layout(xaxis_title="Yük", yaxis_title=yt)
        out.append(html_fig(fig, xlog_levels=xl, title=title))
    fig = go.Figure()
    for i, r in enumerate(runs):
        st = stages(r)
        fig.add_trace(go.Scatter(x=[s["out_tok_per_s"] for s in st], y=[s.get("user_tok_per_s") for s in st], name=r["name"],
                                 mode="lines+markers", text=[lvl_label(r, s["level"]) for s in st],
                                 line=dict(color=C[i % len(C)], width=2), marker=dict(size=8),
                                 hovertemplate="%{text}<br>sistem %{x:,.0f} tok/s<br>kullanıcı %{y:.1f} tok/s"))
    fig.update_layout(xaxis_title="Sistem throughput (tok/s)", yaxis_title="Kullanıcı başına tok/s", hovermode="closest")
    out.append(html_fig(fig, title="Verim ↔ kullanıcı deneyimi (sağ üst = iyi)"))
    return out


# ---------------------------------------------------------------- tables

def stage_table(run):
    slo = run["config"]["slo"]
    bad = lambda v, lim: ' class="bad"' if v is not None and v > lim else ""
    rows = []
    for s in run["stages"]:
        if not s.get("ttft"):
            rows.append(f"<tr><td>{lvl_label(run, s['level'])}</td><td colspan=15>tüm istekler başarısız: "
                        f"{escape(s['error_samples'][0]['error']) if s.get('error_samples') else ''}</td></tr>")
            continue
        sv, gp = s.get("server") or {}, s.get("gpu") or {}
        rows.append(
            f"<tr><td>{lvl_label(run, s['level'])}</td><td>{s['ok']}/{s['requests']}</td><td>{f(s['req_per_s'], 2)}</td>"
            f"<td>{f(s['out_tok_per_s'])}</td><td>{f(s['ttft']['p50'])}</td><td{bad(s['ttft']['p95'], slo['ttft_ms'])}>{f(s['ttft']['p95'])}</td>"
            f"<td>{f(s['ttft']['p99'])}</td><td>{f(g(s, 'tpot', 'p50'), 1)}</td><td{bad(g(s, 'tpot', 'p95'), slo['tpot_ms'])}>{f(g(s, 'tpot', 'p95'), 1)}</td>"
            f"<td>{f(g(s, 'itl', 'p99'), 1)}</td><td>{f(s['e2e']['p95'] / 1000, 2)}</td><td>{f(s['goodput_pct'], 0, '%')}</td>"
            f"<td>{f(sv.get('queue_ms'))}</td><td>{f(sv.get('prefill_ms'))}</td><td>{f(sv.get('kv_cache_max_pct'), 0, '%')}</td>"
            f"<td>{f(gp.get('util_mean'), 0, '%')}</td></tr>")
    head = ("Yük", "Başarılı", "İstek/s", "Çıkış tok/s", "TTFT p50", "TTFT p95", "TTFT p99", "TPOT p50", "TPOT p95",
            "ITL p99", "E2E p95 (s)", "Goodput", "Kuyruk ms", "Prefill ms", "KV max", "GPU ort.")
    return f"<div class=tbl><table><thead><tr>{''.join(f'<th>{h}</th>' for h in head)}</tr></thead><tbody>{''.join(rows)}</tbody></table></div>"


def compare_table(runs):
    rows = []
    for i, r in enumerate(runs):
        st, rec = stages(r), recommend(r)
        peak = max(st, key=lambda s: s["out_tok_per_s"]) if st else None
        rows.append(
            f"<tr><td><span class=dot style='background:{C[i % len(C)]}'></span>{escape(r['name'])}</td>"
            f"<td>{escape((r.get('resolved') or {}).get('model') or '')}</td>"
            f"<td>{format(rec['level'], 'g') if rec else 'Yok'}</td><td>{f(rec['out_tok_per_s']) if rec else '–'}</td>"
            f"<td>{f(peak['out_tok_per_s']) if peak else '–'}</td><td>{f(st[0]['ttft']['p50']) if st else '–'}</td>"
            f"<td>{f(st[0].get('user_tok_per_s')) if st else '–'}</td>"
            f"<td>{sum(s['errors'] for s in r['stages'])}</td></tr>")
    head = ("Test", "Model", "SLO içi kapasite", "SLO'da tok/s", "Tepe tok/s", "TTFT p50 (ms, en düşük yük)", "Kullanıcı tok/s", "Hata")
    return f"<div class=tbl><table><thead><tr>{''.join(f'<th>{h}</th>' for h in head)}</tr></thead><tbody>{''.join(rows)}</tbody></table></div>"


def config_block(run):
    c, rs, cal = run["config"], run.get("resolved") or {}, run.get("calibration") or {}
    ds, ld = c["dataset"], c["load"]
    data = {"Endpoint": f"{c['endpoint']['type']} → {rs.get('base_url', '')}", "Model": rs.get("model") or c["model"],
            "max_model_len": rs.get("max_model_len"), "vLLM metrics": rs.get("metrics_url") or "yok",
            "GPU sayısı (DCGM)": rs.get("gpus") or "yok",
            "Veri seti": ds["type"] + (f" · giriş {ds['input_tokens']}±{ds['input_std']:g} / çıkış {ds['output_tokens']}±{ds['output_std']:g} token"
                                        + (f" · ortak önek {ds['shared_prefix_tokens']}" if ds["shared_prefix_tokens"] else "")
                                        if ds["type"] == "synthetic" else f" · max çıkış {ds['output_tokens']}"),
            "Yük": f"{ld['mode']} · seviyeler {', '.join(f'{v:g}' for v in ld['levels'])} · seviye başına {ld['requests_per_stage']} istek"
                   + (f" · burstiness {ld['burstiness']:g}" if ld["mode"] == "rate" else ""),
            "ignore_eos": c["ignore_eos"], "SLO": f"p95 TTFT ≤ {c['slo']['ttft_ms']:g} ms, p95 TPOT ≤ {c['slo']['tpot_ms']:g} ms",
            "Kalibrasyon": f"{cal.get('tokens_per_word')} token/kelime" if cal else "–",
            "Başlangıç / bitiş": f"{run['created']} → {run.get('finished') or '…'}", "Durum": run["status"]}
    items = "".join(f"<dt>{k}</dt><dd>{escape(str(v))}</dd>" for k, v in data.items() if v is not None)
    return f"<dl class=cfg>{items}</dl>"


GLOSSARY = """
<dl class=gloss>
<dt>TTFT</dt><dd>İsteği gönderdikten ilk token gelene kadar geçen süre. Kuyruk bekleme + prefill (girdinin işlenmesi) + ağ. Kullanıcının "cevap başladı" hissi.</dd>
<dt>TPOT</dt><dd>İlk token sonrası token başına ortalama süre: (E2E − TTFT) / (çıkış token − 1). Metnin akış hızı; 1000/TPOT = kullanıcının gördüğü tok/s.</dd>
<dt>ITL</dt><dd>Ardışık iki stream parçası arası süre. p99 değeri akıştaki takılmaları gösterir.</dd>
<dt>Prefill / Decode</dt><dd>Prefill girdinin tek seferde işlendiği, hesaplama ağırlıklı aşama; decode token'ların tek tek üretildiği, bellek bant genişliği ağırlıklı aşama. Sunucu değerleri vLLM /metrics histogramlarından alınır.</dd>
<dt>Goodput</dt><dd>Hem TTFT hem TPOT SLO'sunu karşılayan isteklerin oranı. Ham throughput yerine kapasite planlamasında bu kullanılmalı.</dd>
<dt>SLO içi kapasite</dt><dd>p95 TTFT ve p95 TPOT hedefin altında, hata oranı %1'in altında kalan en yüksek yük seviyesi.</dd>
<dt>KV cache</dt><dd>Aktif isteklerin bağlamını tutan GPU belleği. %100'e yaklaşınca yeni istekler kuyrukta bekler veya preempt edilir.</dd>
</dl>"""

CSS = """
:root{--bg:#fcfcfb;--card:#ffffff;--text:#0b0b0b;--text-2:#52514e;--muted:#8a8984;--line:#e6e5e0;--grid:#ecebe6;--good:#008300;--bad:#c62828}
:root[data-theme=dark]{--bg:#121211;--card:#1a1a19;--text:#f0efec;--text-2:#c3c2b7;--muted:#8f8e86;--line:#2e2e2b;--grid:#2a2a27;--good:#4caf50;--bad:#ef6b6b}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:1240px;margin:0 auto;padding:24px 16px 64px}
header{display:flex;justify-content:space-between;align-items:flex-start;gap:16px;margin-bottom:20px}
h1{font-size:22px;margin:0}h2{font-size:16px;margin:32px 0 12px}.sub{color:var(--text-2);margin-top:4px}
button{background:var(--card);color:var(--text);border:1px solid var(--line);border-radius:8px;padding:6px 12px;cursor:pointer;font:inherit}
.kpis{display:grid;grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:12px}
.kpi{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px 16px}
.kpi .l{color:var(--text-2);font-size:12px}.kpi .v{font-size:26px;font-weight:600;margin:2px 0}.kpi .u{color:var(--muted);font-size:12px}
.kpi.good .v{color:var(--good)}.kpi.bad .v{color:var(--bad)}
.insights{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:12px 20px}.insights li{margin:6px 0}
.grid{display:grid;grid-template-columns:1fr 1fr;gap:12px}.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:8px 8px 0;min-width:0}
.wide{grid-column:1/-1}
.tbl{overflow-x:auto;background:var(--card);border:1px solid var(--line);border-radius:12px}
table{border-collapse:collapse;width:100%;font-size:13px;font-variant-numeric:tabular-nums}
th,td{padding:7px 10px;text-align:right;border-bottom:1px solid var(--line);white-space:nowrap}th{color:var(--text-2);font-weight:500}
td:first-child,th:first-child{text-align:left}td.bad{color:var(--bad);font-weight:600}
.dot{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:6px}
dl{display:grid;grid-template-columns:max-content 1fr;gap:6px 16px;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px 18px;margin:0}
dt{color:var(--text-2)}dd{margin:0;word-break:break-word}.warn{color:var(--bad)}
code{background:var(--grid);padding:1px 5px;border-radius:4px}
@media(max-width:860px){.grid{grid-template-columns:1fr}header{flex-direction:column}dl{grid-template-columns:1fr}}
"""

JS = """
function themeColors(){const cs=getComputedStyle(document.documentElement);return{fg:cs.getPropertyValue('--text-2').trim(),grid:cs.getPropertyValue('--grid').trim()}}
function paint(){const c=themeColors();document.querySelectorAll('.js-plotly-plot').forEach(d=>{const u={'font.color':c.fg};
 Object.keys(d.layout).filter(k=>/^[xy]axis\\d*$/.test(k)).forEach(k=>{u[k+'.gridcolor']=c.grid;u[k+'.linecolor']=c.grid});Plotly.relayout(d,u)})}
function setTheme(t){document.documentElement.dataset.theme=t;try{localStorage.setItem('llmbench-theme',t)}catch(e){}
 document.getElementById('tt').textContent=t==='dark'?'☀ Açık':'☾ Koyu';paint()}
document.getElementById('tt').onclick=()=>setTheme(document.documentElement.dataset.theme==='dark'?'light':'dark');
let t0='light';try{t0=localStorage.getItem('llmbench-theme')||'light'}catch(e){}setTheme(t0);
"""


def page(title, sub, body):
    return f"""<!doctype html><html lang=tr data-theme=light><head><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1"><title>{escape(title)}</title>
<style>{CSS}</style><script>{get_plotlyjs()}</script></head><body><main>
<header><div><h1>{escape(title)}</h1><div class=sub>{sub}</div></div><button id=tt type=button>☾ Koyu</button></header>
{body}</main><script>{JS}</script></body></html>"""


def render(runs):
    if not runs:
        return page("LLM Bench", "Test seçilmedi", "")
    if len(runs) > 1:
        names = ", ".join(escape(r["name"]) for r in runs)
        charts = compare_charts(runs)
        body = (f"<h2>Özet</h2>{compare_table(runs)}"
                f"<h2>Ne anlama geliyor?</h2>" + "".join(f"<div class=insights><ul><li>{insights(r)[0]}</li></ul></div>" for r in runs)
                + "<h2>Karşılaştırma grafikleri</h2><div class=grid>" + "".join(f"<div class=card>{c}</div>" for c in charts) + "</div>"
                + f"<h2>Terimler</h2>{GLOSSARY}")
        return page("Model karşılaştırma", names, body)
    run = runs[0]
    rs = run.get("resolved") or {}
    tiles = "".join(f"<div class='kpi {cls}'><div class=l>{l}</div><div class=v>{escape(v)}</div><div class=u>{escape(u)}</div></div>"
                    for l, v, u, cls in kpis(run))
    warns = "".join(f"<li class=warn>{escape(w)}</li>" for w in run.get("warnings", []))
    if run.get("error"):
        warns += f"<li class=warn>Hata: {escape(run['error'])}</li>"
    charts = single_charts(run) if stages(run) else []
    tl = timeline(run)
    body = (f"<div class=kpis>{tiles}</div>"
            f"<h2>Ne anlama geliyor?</h2><div class=insights><ul>{''.join(f'<li>{i}</li>' for i in insights(run))}{warns}</ul></div>"
            + "<h2>Grafikler</h2><div class=grid>" + "".join(f"<div class=card>{c}</div>" for c in charts)
            + (f"<div class='card wide'>{tl}</div>" if tl else "") + "</div>"
            + f"<h2>Seviye bazında sonuçlar</h2>{stage_table(run)}"
            + f"<h2>Test ayarları</h2>{config_block(run)}<h2>Terimler</h2>{GLOSSARY}")
    sub = f"{escape(rs.get('model') or run['config']['model'])} · {escape(rs.get('base_url', ''))} · {escape(run['created'][:16].replace('T', ' '))}"
    return page(run["name"], sub, body)
