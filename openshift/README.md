# llm-bench - OpenShift kurulumu

UI, OpenShift OAuth (oauth-proxy sidecar) arkasindadir; sadece `llm-bench`
namespace'inde pod okuyabilen kullanicilar girebilir.

```bash
oc new-project llm-bench

# oauth-proxy cookie secret (repoya commit edilmez)
oc create secret generic llm-bench-cookie \
  --from-literal=session_secret="$(openssl rand -base64 24 | tr -d '\n')"

oc apply -k openshift/

# imaji derle (repo kokunden) ve deploy otomatik yuvarlanir
oc start-build llm-bench --from-dir=. --follow

oc get route llm-bench   # HOST/PORT -> https://...
```

## Notlar

- **Air-gapped build:** ShareGPT indirilemiyorsa `openshift/build.yaml` icinde
  `dockerStrategy.buildArgs` altindaki `SKIP_SHAREGPT=1` satirlarini ac. Bos
  dataset ile build olur; JSONL dataset'i UI'dan yukleyebilirsin.
- **GPU telemetri:** NVIDIA GPU Operator'un DCGM exporter'i cluster monitoring
  tarafindan scrape edilmeli (ServiceMonitor, `openshift.io/cluster-monitoring: "true"`
  label'li namespace). Sorgularda secici ornegi: `exported_namespace="my-models"`.
  Uygulama `thanos-querier.openshift-monitoring.svc:9091`'e SA token'i ile gider
  (`cluster-monitoring-view` rbac.yaml'da bagli).
- **En az yetki:** `rbac.yaml`'daki `llm-bench-resolver` ClusterRoleBinding'i
  silip, model namespace'lerinin her birinde ayni ClusterRole'e RoleBinding
  olustur:
  `oc create rolebinding llm-bench-resolver -n my-models --clusterrole=llm-bench-resolver --serviceaccount=llm-bench:llm-bench`
- **crc / OKD:** `registry.redhat.io` imajina pull secret gerekir. Lokalde
  `deployment.yaml` icinde oauth-proxy imajini
  `quay.io/openshift/origin-oauth-proxy:4.16` yap.
- Deployment `Recreate` + tek replika: PVC RWO ve ayni anda tek benchmark.
