# LLM Bench

OpenShift'te çalışan, arayüzlü vLLM / OpenAI uyumlu model benchmark aracı. Prod'a alınacak modeli
gerçek yük altında ölçer ve grafik ağırlıklı, tek dosyalık (air-gap uyumlu) HTML rapor üretir.

**Ölçtükleri:** TTFT, TPOT, ITL, E2E (p50/p90/p95/p99) · çıkış/giriş tok/s, istek/s · goodput (SLO'yu karşılayan istek oranı) ·
sunucu tarafı kuyruk/prefill/decode süresi, KV cache doluluğu, kuyruktaki istek, prefix cache isabeti, preemption (vLLM `/metrics`) ·
GPU kullanımı, bellek, güç ve tok/J (DCGM, OpenShift monitoring üzerinden).

**Endpoint türleri:** LiteLLM (URL + model adı) · OpenShift Service (ad + namespace + port) · Route (ad + namespace, host API'den çözülür) ·
Pod (ad + namespace, IP API'den çözülür) · düz URL.

**Yük:** eşzamanlılık taraması (closed loop, `1,2,4,…`) veya istek hızı taraması (Poisson/gamma, `burstiness`).
**Prompt'lar:** sentetik (hedef giriş/çıkış token ± sapma, ortak system prompt ile prefix cache testi), ShareGPT (vLLM bench'in standart seti, imaja gömülür), JSONL yükleme.

vllm bench / aiperf'e göre ekledikleri: web arayüzü ve geçmiş, modelleri yan yana karşılaştırma, OpenShift kaynak adından endpoint çözme,
SLO içi kapasite önerisi + "kırılma noktası ve sebebi" yorumu, istemci ölçümünü sunucu metrikleri ve GPU telemetrisiyle aynı zaman çizelgesinde birleştirme.

## Kurulum

[openshift/README.md](openshift/README.md)

## Lokal

```bash
pip install -r requirements.txt
uvicorn mock_vllm:app --port 8001 &          # sahte vLLM (gerçek endpoint yoksa)
DATA_DIR=./data uvicorn app:app --port 8080  # http://localhost:8080 → Doğrudan URL: http://localhost:8001
python test_bench.py                         # uçtan uca kontrol (mock'a karşı)
```

## Notlar

- Ölçüm doğruluğu: token sayıları sunucunun `usage` alanından gelir; sentetik prompt uzunluğu 2 kalibrasyon isteğiyle ayarlanır.
  `ignore_eos` açıkken çıkış uzunluğu sabitlenir (vLLM `ignore_eos` + `min_tokens`; reddedilirse otomatik kapanır, raporda uyarı çıkar).
- LiteLLM üzerinden test ederken sunucu metrikleri için "Gelişmiş → metrics URL" alanına vLLM servisinin `/metrics` adresini verin.
- DCGM genelde 15–30 sn'de bir toplanır; anlamlı GPU verisi için aşama başına ≥ 60 sn sürecek istek sayısı seçin.
- Aynı anda tek test çalışır (paralel testler birbirinin sonucunu bozar). İstemci tek Python süreci: ~birkaç bin eşzamanlı stream'e kadar yeterli.
