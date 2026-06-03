# Datadog monitors

Alerting for Harbor Reef proxy-cache outages — the failure mode where a proxy
cache (e.g. `proxy-nvcr`) goes **unhealthy** because Harbor cannot authenticate
to the upstream, causing every pull through the cache to return `not found`.

Harbor's own exporter (`harbor.exporter.harbor_up`) only covers the
core/registry/jobservice components — it does **not** expose per-proxy-cache
endpoint health, so these two monitors fill that gap.

| File | Type | Fires when | Notes |
|------|------|-----------|-------|
| `monitor-proxycache-unhealthy-logs.json` | log alert | harbor-core logs `current registry is unhealthy` (> 10 / 10m per cluster) | Works today. Keys on message text because these lines are ingested as `status:info`, not `status:error`. |
| `monitor-proxycache-healthy-metric.json` | metric alert | `harbor_reef_operator.proxycache_healthy < 1` | Requires harbor-reef-operator >= 1.2.0. `notify_no_data:false` so it stays quiet until the gauge is being emitted. |

## Apply

Requires a Datadog API key and an application key for the `nv-prodsec` org
(US1, `api.datadoghq.com`):

```bash
export DD_API_KEY=...      # nv-prodsec API key
export DD_APP_KEY=...       # application key with monitors_write

for f in monitor-proxycache-unhealthy-logs.json monitor-proxycache-healthy-metric.json; do
  curl -sS -X POST "https://api.datadoghq.com/api/v1/monitor" \
    -H "DD-API-KEY: ${DD_API_KEY}" \
    -H "DD-APPLICATION-KEY: ${DD_APP_KEY}" \
    -H "Content-Type: application/json" \
    -d @"${f}" | jq '{id, name}'
done
```

To update an existing monitor instead of creating a duplicate, capture the `id`
from the create response and `PUT https://api.datadoghq.com/api/v1/monitor/<id>`
with the same body.

These carry `integration:harbor` + `team:platform`, so they appear in the
"Harbor Monitor Summary" widget on the
[Harbor Reef Image Cache dashboard](https://nv-prodsec.datadoghq.com/dashboard/4un-bit-6gu).

Fill in a notification handle (e.g. `@slack-...` / `@pagerduty-...`) in each
`message` before applying if you want routed alerts; existing Harbor monitors
in this org do not embed one.
