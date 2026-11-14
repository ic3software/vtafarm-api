# Tenant Metrics and Live Monitoring — Design

Status: design approved for planning; not implemented

This document defines the tenant-facing metrics experience for VTA-only and
Full Stack sessions. It covers product entitlements, Prometheus/Thanos query
boundaries, the historical API, the Live SSE protocol, frontend behavior,
security, rollout, and verification.

The first implementation must follow this design unless a later review updates
the document. Nothing described here should be treated as shipped until the
corresponding API, infrastructure, and frontend changes land.

## 1. Decisions

The following product and architecture decisions are settled:

- Free users can query the previous **7 days**.
- Premium users can query the previous **30 days**.
- **Temporary plan mapping:** for the first version,
  `users.fullstack_access` is the source of truth for plan status. `false`
  means Free; `true` means Premium. This is an initial compatibility decision,
  not a permanent product-model guarantee, and may be replaced by a dedicated
  plan or entitlement later.
- The API exposes derived `plan` and `metrics_retention_days` values so the
  frontend does not reinterpret `fullstack_access` itself.
- Both plans receive the same metric types. Premium gates longer history, not
  basic operational visibility.
- The default historical view is the previous **24 hours**.
- Both plans receive a **Live** view.
- Historical data uses bounded HTTP range queries. Live data uses
  **Server-Sent Events (SSE)**.
- Prometheus, Thanos Query, and Grafana remain private ClusterIP services.
  Browsers never receive their URLs and never submit PromQL.
- The API authorizes the session in PostgreSQL, selects from fixed query
  templates, and returns only tenant-safe fields.

`fullstack_access` currently also grants Full Stack creation and removes the
two-session VTA limit. Reusing it for Free/Premium classification is a temporary
coupling accepted for the first version. All metrics code must nevertheless
call one entitlement helper rather than reading the field throughout the
codebase, so a future billing model can split those capabilities without
changing the public metrics API.

## 2. Goals

- Let an owner inspect every workload component and current Pod belonging to
  one of their sessions.
- Make CPU and memory the primary diagnostic experience.
- Provide health, restart, network, and persistent-storage context without
  exposing Kubernetes or Prometheus internals.
- Keep Full Stack graphs continuous across Pod replacements by making a stable
  component the default target and an individual Pod an optional drill-down.
- Support useful near-real-time monitoring without scraping more frequently
  than the monitoring stack can sustain.
- Bound query cost and response size for both 7-day and 30-day ranges.
- Make missing, delayed, or partial data visible rather than presenting it as
  a healthy zero.

## 3. Non-goals

- Arbitrary PromQL, a tenant Grafana login, or direct access to Prometheus or
  Thanos.
- Log aggregation. Existing setup and Pod log streams remain separate.
- Distributed tracing.
- Application-level request rate, status code, or latency metrics until the
  VTA, mediator, DID-hosting daemon, VTC, and Trust Registry expose deliberate
  application metrics through ServiceMonitors.
- Per-second telemetry. Live is bounded by the Prometheus scrape interval.
- Billing or payment-provider integration. This design consumes the existing
  admin-managed `fullstack_access` grant.
- An admin-facing cluster metrics UI. The first delivery is scoped to the
  authenticated owner of a user session.

## 4. Existing monitoring foundation

The optional observability stack already provides the required base:

- kubelet/cAdvisor container usage metrics;
- kube-state-metrics object state and resource declarations;
- node-exporter host metrics for operators;
- Prometheus recording rules labelled by `label_session_id` and `label_app`;
- a private Thanos Query endpoint combining current Prometheus data and object
  storage history;
- 30-second scrape and rule-evaluation intervals by default;
- 30-day Prometheus and Thanos time-retention settings by default;
- existing recording rules for CPU usage, memory working set, CPU and memory
  requests, memory limits, network receive/transmit, and PVC usage/capacity.

The observability stack already exports a private `metrics_query_url` intended
for vtafarm-api. The API does not yet consume it. Thanos Query Frontend is also
disabled today and should be enabled before broad 30-day query usage.

Prometheus is configured with both time and size retention. Whichever limit is
reached first wins, so the API must use Thanos Query—not local Prometheus—as its
normal query backend. The UI must report the actual earliest available sample;
it must not imply that data predating monitoring installation or recording-rule
creation exists.

## 5. Entitlements

### 5.1 Source of truth

The following mapping is **temporary** and applies only until the product gains
an independent plan or subscription entitlement. No database migration is
required for the first implementation:

| `users.fullstack_access` | Plan | Maximum metrics history |
| --- | --- | --- |
| `false` | `free` | 7 days |
| `true` | `premium` | 30 days |

In other words, the current provisional rule is:

```text
No Full Stack Access  -> Free plan
Full Stack Access     -> Premium plan
```

This document does not define Full Stack Access and Premium as permanently
equivalent. It defines the mapping the first implementation will use while
preserving a migration path to separate them later.

Add one domain-level helper and use it in every metadata, range, and stream
handler:

```go
type MetricsEntitlement struct {
    Plan          string
    RetentionDays int
}

func MetricsEntitlementFor(user model.User) MetricsEntitlement {
    if user.FullstackAccess {
        return MetricsEntitlement{Plan: "premium", RetentionDays: 30}
    }
    return MetricsEntitlement{Plan: "free", RetentionDays: 7}
}
```

The exact package and names may change during implementation, but the single
derivation point is required.

### 5.2 Profile response

Extend `GET /api/v1/user/me` with derived fields while preserving the existing
ones:

```json
{
  "id": "alice",
  "fullstack_access": true,
  "plan": "premium",
  "metrics_retention_days": 30,
  "vta_count": 3,
  "vta_limit": null
}
```

The API, not the browser, enforces the maximum range. Hiding a 30-day button is
only presentation. A Free request exceeding 7 days returns `403` with a stable
error code and the current entitlement:

```json
{
  "error": "requested metrics range exceeds plan retention",
  "code": "metrics_range_not_allowed",
  "plan": "free",
  "metrics_retention_days": 7
}
```

An upgrade immediately unlocks up to the previous 30 days already present in
Thanos. A downgrade immediately limits new range requests to 7 days; it does not
delete stored samples. Existing sessions remain visible after
`fullstack_access` is revoked, consistent with the current grant semantics.

## 6. Target model and UI

Add a `Metrics` tab to the session detail page. Show it for a session once a
workload target may exist; a provisioning or failed session may legitimately
show no data yet.

The control row contains:

1. target scope;
2. component;
3. Pod, when the user chooses Pod scope;
4. time range.

The target options are derived from the database session mode and the supported
workload definitions, not discovered from arbitrary Prometheus labels:

| Mode | Stable targets |
| --- | --- |
| VTA-only | VTA |
| Full Stack | Entire Stack, VTA, Mediator, DID Hosting, VTC |
| Full Stack with active Trust Registry | the above plus Trust Registry |

The label mapping is fixed:

| UI component | `label_app` |
| --- | --- |
| VTA-only VTA | `vta` |
| Full Stack VTA | `fs-vta` |
| Mediator | `fs-mediator` |
| DID Hosting | `fs-dids` |
| VTC | `fs-vtc` |
| Trust Registry | `fs-trust-registry` |

Stable component views are the default because Deployment rollouts replace Pod
names. Pod drill-down is for diagnosing a current instance. The metadata API
returns current Pods, and range results may name historical Pods observed in
that result. The frontend may place historical names under a `Previous Pods`
group, but it must not make a Pod the default long-term identity.

### 6.1 Time controls

Display:

```text
Live · 1h · 6h · 24h · 7d · 30d
```

- The default is `1h`.
- Free users see `30d` as locked with an upgrade explanation.
- Premium users can select every option.
- Live is available to both plans.

Use fixed presets in the first release rather than arbitrary start/end input.
This keeps query cost predictable and the API contract small.

### 6.2 Dashboard order

Show:

1. status cards: Ready, restarts in range, last termination reason;
2. CPU current/average/peak and a usage chart with request and limit lines;
3. memory current/peak and a working-set chart with request and limit lines;
4. network receive/transmit;
5. persistent-volume used/capacity, when present.

Requests are scheduler reservations, not caps. The UI must say `% of request`
or `% of limit`; it must never label request utilization as generic capacity.
CPU usage above 100% of request is valid. A memory limit is a hard limit and is
the preferred denominator when present.

## 7. Tenant metric catalog

Public metric names are API vocabulary, not Prometheus recording-rule names.
This prevents a storage rename from becoming a frontend breaking change.

| Public metric | Unit | Source | Delivery |
| --- | --- | --- | --- |
| `cpu_usage_cores` | cores | new 1m rule for Live; existing 5m rule for history | summary + series |
| `cpu_request_cores` | cores | existing rule | summary + reference line |
| `cpu_limit_cores` | cores | new rule | summary + reference line |
| `memory_working_set_bytes` | bytes | existing rule | summary + series |
| `memory_request_bytes` | bytes | existing rule | summary + reference line |
| `memory_limit_bytes` | bytes | existing rule | summary + reference line |
| `network_receive_bytes_per_second` | bytes/second | existing rule | series |
| `network_transmit_bytes_per_second` | bytes/second | existing rule | series |
| `pvc_used_bytes` | bytes | existing rule | summary + series per claim |
| `pvc_capacity_bytes` | bytes | existing rule | summary + reference line per claim |
| `ready` | boolean | new recording rule from kube-state-metrics | status + series |
| `restart_total` | count | new recording rule from kube-state-metrics | status |
| `last_terminated_reason` | enum/string | fixed query from kube-state-metrics | status |
| `pod_start_time_seconds` | Unix seconds | fixed query from kube-state-metrics | metadata/status |

Add recording rules under the existing `vtafarm.pod-resources` rule group for
CPU limit, Ready, and restart total. Add
`vtafarm:pod_cpu_usage_cores:rate1m` for Live while keeping the existing 5m CPU
rule for historical views. A 30-second scrape interval gives the 1m rate enough
samples while avoiding a misleading per-second graph.

Network error/drop counters can be added after the core catalog. HTTP request
rate, error rate, and latency remain out of scope until the applications expose
intentional metrics.

## 8. Aggregation rules

Every API query starts from the authorized session's internal numeric ID and
fixed label mapping.

- Pod scope preserves `pod`.
- Component scope sums CPU, memory, requests, limits, and network across all
  selected Pods.
- Entire Stack scope sums those values across supported Full Stack components.
- Readiness is true only when every selected current Pod is ready. An empty
  selection is `unknown`, never true.
- Restarts in a displayed range are derived from the increase in the restart
  counter and summed across selected containers/Pods.
- Persistent volumes are deduplicated by PVC before aggregation. A shared PVC
  mounted by more than one Pod must not be counted twice.
- Missing requests or limits are represented as `null`, not zero.
- Gaps remain gaps. The API and frontend must not interpolate a missing workload
  into a healthy zero.

The API strips `namespace`, the internal `label_session_id`, node name, Pod IP,
container ID, image ID, and all unrelated Prometheus labels before returning a
response. Component and Pod names are the only workload identity labels needed
by the tenant UI.

## 9. Configuration

Add a metrics configuration section to `internal/config` and the Helm chart.
The private service URL is configuration, not a browser-visible value:

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `METRICS_QUERY_URL` | empty | Prometheus-compatible Thanos Query base URL; empty disables tenant metrics |
| `METRICS_QUERY_TIMEOUT` | `10s` | per upstream request timeout |
| `METRICS_LIVE_INTERVAL` | `15s` | Live sample interval; must not be below the scrape interval |
| `METRICS_LIVE_WINDOW` | `5m` | initial Live snapshot window |
| `METRICS_MAX_POINTS` | `2000` | maximum points per returned series |
| `METRICS_CACHE_TTL` | `25s` | short-lived instant-query cache |

Production points `METRICS_QUERY_URL` at the private Thanos Query or Query
Frontend service. The URL must not contain credentials. An empty or unreachable
backend makes metrics endpoints return `503 metrics_unavailable`; it must not
prevent the rest of vtafarm-api from starting.

## 10. Historical HTTP API

Every new route must also be added to `internal/apidocs/openapi.yaml`.

### 10.1 Metadata

```text
GET /api/v1/setup/:id/metrics/meta?range=1h
```

The handler resolves `vta_name = :id AND user_id = authenticated user`, derives
the entitlement, determines supported components, and returns current Pods.
When the backend is available it also returns data availability for the selected
range.

Example:

```json
{
  "plan": "free",
  "metrics_retention_days": 7,
  "default_range": "1h",
  "allowed_ranges": ["1h", "6h", "24h", "7d"],
  "live": {
    "enabled": true,
    "interval_seconds": 15,
    "window_seconds": 300
  },
  "components": [
    {"id": "vta", "label": "VTA"},
    {"id": "mediator", "label": "Mediator"},
    {"id": "dids", "label": "DID Hosting"},
    {"id": "vtc", "label": "VTC"}
  ],
  "pods": [
    {"name": "fs-vta-42-7bcf8d9f86-q8pz2", "component": "vta", "current": true}
  ],
  "available_from": "2026-11-01T12:00:00Z",
  "generated_at": "2026-11-14T20:30:00Z"
}
```

`available_from` is nullable when no samples exist. The range parameter is
validated against the entitlement before it is used to discover historical
Pods or availability.

### 10.2 Range query

```text
GET /api/v1/setup/:id/metrics/range
    ?scope=stack|component|pod
    &component=vta
    &pod=fs-vta-42-7bcf8d9f86-q8pz2
    &range=24h
```

Rules:

- `stack` is valid only for Full Stack sessions.
- `component` is required for component scope and must be supported by the
  session.
- `pod` and `component` are required for Pod scope. The fixed query must prove
  that the Pod belongs to the authorized session/component.
- `range` is one of the plan's allowed presets.
- The server chooses start, end, and step; clients do not submit PromQL or a
  step.

Recommended steps:

| Range | Step | Maximum approximate points |
| --- | --- | --- |
| `1h` | 30s | 120 |
| `6h` | 1m | 360 |
| `24h` | 1m | 1,440 |
| `7d` | 5m | 2,016; trim/alignment must keep the hard limit at 2,000 |
| `30d` | 30m | 1,440 |

The implementation may align the 7-day step slightly above 5 minutes to honor
`METRICS_MAX_POINTS`. The response reports the actual step.

Example shape:

```json
{
  "target": {"scope": "component", "component": "vta"},
  "start": "2026-11-13T20:30:00Z",
  "end": "2026-11-14T20:30:00Z",
  "step_seconds": 60,
  "generated_at": "2026-11-14T20:30:03Z",
  "summary": {
    "ready": true,
    "restarts": 0,
    "cpu_current_cores": 0.041,
    "cpu_average_cores": 0.036,
    "cpu_peak_cores": 0.091,
    "memory_current_bytes": 94371840,
    "memory_peak_bytes": 112197632
  },
  "series": [
    {
      "metric": "cpu_usage_cores",
      "unit": "cores",
      "labels": {"component": "vta"},
      "points": [[1794697740, 0.039], [1794697800, 0.041]]
    }
  ],
  "warnings": []
}
```

Thanos partial responses may be used for availability, but every upstream
warning must be returned in `warnings` and displayed as `Data may be incomplete`.
Partial data must never be silently presented as complete.

## 11. Live SSE API

```text
GET /api/v1/setup/:id/metrics/live
    ?scope=stack|component|pod
    &component=vta
    &pod=...
```

It uses the same ownership, target validation, query templates, field stripping,
and entitlement service as the historical API. Live is available to both plans,
so plan status does not change its sample frequency.

SSE is the deliberate transport choice rather than an interim polling design.
Metrics flow only from server to browser, so WebSocket's bidirectional protocol
adds no useful capability. SSE fits the existing cookie-authenticated log-stream
path, has native browser reconnection, remains ordinary HTTP through Traefik,
and lets the server fan out one cached observation to multiple clients. It does
not make Prometheus more current: the 30-second scrape interval remains the
source-of-truth freshness boundary.

### 11.1 Connection behavior

- Authenticate with the existing user cookie. Native browser `EventSource`
  cannot attach an arbitrary Authorization header; cookie authentication is the
  supported portal path.
- Set `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
  `Connection: keep-alive`, and `X-Accel-Buffering: no`.
- Send `retry: 3000` when the stream opens.
- Send a `snapshot` event containing the previous 15 minutes.
- Send a `sample` event after each new 30-second observation. Do not emit a
  duplicate when the upstream observation timestamp has not advanced.
- Send a `status` event when readiness, restart count, or termination reason
  changes.
- Send an SSE comment heartbeat (`: keepalive`) every 15 seconds so proxies do
  not close an otherwise quiet stream.
- Assign a timestamp-based SSE `id` to data events for diagnostics. Reconnects
  always receive a fresh snapshot; the server does not need an event replay log.
- Close each connection after at most 60 minutes with a `reconnect` event. The
  browser reconnects and therefore re-runs authentication, ownership, and
  current grant checks.
- Stop immediately when the request context is cancelled.

Example:

```text
retry: 3000

event: snapshot
id: 1794697800000
data: {"window_seconds":300,"step_seconds":15,"series":[...]}

: keepalive

event: sample
id: 1794697830000
data: {"observed_at":"2026-11-14T20:30:30Z","values":{"cpu_usage_cores":0.041,"memory_working_set_bytes":94371840}}

event: status
id: 1794697830001
data: {"ready":true,"restart_total":0,"last_terminated_reason":null}
```

Use a named `problem` event for recoverable upstream failures rather than
closing the stream:

```text
event: problem
data: {"code":"metrics_backend_unavailable","message":"Live metrics are temporarily unavailable","retryable":true}
```

When the backend recovers, send a fresh snapshot before resuming samples.

### 11.2 Browser lifecycle

- Complete the metadata request immediately before opening Live. This makes
  initial ownership, entitlement, and backend failures visible as ordinary HTTP
  responses before native EventSource takes over.
- Opening Live creates one EventSource.
- Changing session, scope, component, or Pod closes the previous source before
  opening another.
- Leaving the Metrics tab or putting the document in the background closes the
  source. Returning to the visible tab reconnects and receives a fresh snapshot.
- Pause closes the source; Resume creates a new one.
- The UI shows `Last updated` and changes to a stale state after two missed Live
  intervals.
- Browser network errors use EventSource's normal reconnect behavior. Native
  EventSource does not expose an initial HTTP error status or JSON body to
  JavaScript. After repeated connection errors, the UI calls the metadata route
  again: a terminal `401`, `403`, or `404` there stops the retry loop and shows
  the actionable state; a successful metadata response keeps the bounded
  reconnect behavior.

### 11.3 Server fan-out and backpressure

SSE must not multiply upstream load by the number of browser tabs.

- Cache Live observations by internal session ID, scope, component, and Pod.
- Coalesce concurrent cache misses with `singleflight` or an equivalent
  per-key mechanism.
- A key can query upstream at most once per Live interval.
- Multiple connected clients reuse that observation.
- Use bounded subscriber channels. When a client is slow, discard the older
  pending observation and retain the newest one; metrics are state, not an
  audit log.
- Do not hold an upstream HTTP response body or DB transaction for the lifetime
  of the SSE connection.

The initial deployment runs one API replica, so an in-process cache and fan-out
registry are sufficient. If API replicas increase, duplicate queries across
replicas remain bounded by Thanos Query Frontend caching. Redis or another
cross-replica broker is not required until measured load justifies it.

## 12. Query service and cost controls

Implement one internal metrics service used by metadata, range, and Live
handlers. HTTP handlers must not construct PromQL themselves.

The service owns:

- public metric to recording-rule mapping;
- target matcher construction;
- label escaping;
- fixed aggregation templates;
- step selection and point limits;
- upstream timeouts and response-size limits;
- conversion from Prometheus matrix/vector results to public response types;
- stripping private labels;
- warning propagation;
- short-lived instant-query cache and request coalescing.

Prefer recording rules for tenant endpoints so the API does not repeatedly run
large raw cAdvisor joins. Range queries go through Thanos Query Frontend with
range splitting and result caching enabled before general Premium rollout.

Apply per-user and per-IP rate limits to metadata, range, and stream connection
creation. Suggested starting limits are deliberately conservative and should be
load-tested:

- 30 range or metadata requests per minute per user;
- 5 simultaneous Live streams per user;
- 2 simultaneous Live streams for the same session/target;
- 10-second upstream timeout;
- 2,000 points per series;
- a fixed maximum number of returned series and response bytes.

The API should return `429 metrics_rate_limited` with `Retry-After` when a
limit is exceeded.

## 13. Security boundaries

The following are release requirements:

- Resolve every user route with both public session name and authenticated
  `user_id`, matching the existing owner-route pattern.
- Load the user fresh from PostgreSQL for entitlement decisions. Do not put the
  plan into the JWT; admins can change `fullstack_access` without requiring a
  new login.
- Never accept PromQL, a raw label matcher, a namespace, or an internal numeric
  session ID from the browser.
- Validate component values against the session mode and Trust Registry state.
- Validate a requested Pod through a fixed session/component query or the
  Kubernetes workload inventory before using it.
- Escape all generated PromQL label values even after validation.
- Do not expose Prometheus, Thanos, or Grafana outside the cluster.
- Do not include private infrastructure labels in the response.
- Do not log full upstream responses or query URLs containing tenant selectors.
  Structured logs may include internal session ID, public metric name, range,
  duration, series count, cache status, and result status.
- Query failures, absent data, and zero usage are distinct states.

## 14. Failure semantics

Use stable machine-readable codes:

| HTTP/status | Code | Meaning |
| --- | --- | --- |
| `400` | `invalid_metrics_target` | scope/component/Pod combination is invalid |
| `400` | `invalid_metrics_range` | unsupported range preset |
| `401` | existing auth error | missing/expired login |
| `403` | `metrics_range_not_allowed` | requested history exceeds plan |
| `404` | `session_not_found` | session is absent or not owned by caller |
| `429` | `metrics_rate_limited` | query or stream connection limit reached |
| `503` | `metrics_unavailable` | metrics disabled or backend unavailable |
| `504` | `metrics_query_timeout` | upstream exceeded the bounded timeout |

An empty successful result is `200` with `available_from: null` or empty series;
it is not a backend failure. The UI copy should explain that monitoring may
have been installed recently or the workload may not have existed in that
period.

## 15. Implementation plan

Implementation happens only after this design is reviewed.

### Phase 1 — monitoring rules and query path

Repository: `vtafarm-k8s`

- Add CPU 1m, CPU limit, Ready, and restart recording rules. Query the bounded
  termination-reason metric through a fixed API template.
- Add rule-expression tests, not only record-name presence tests.
- Enable and size Thanos Query Frontend for range splitting and result caching.
- Keep Query/Frontend private.
- Verify every tenant rule includes `namespace`, `pod`, `label_session_id`, and
  `label_app` as appropriate.
- Update the observability verification runbook.

### Phase 2 — API configuration and entitlement seam

Repository: `vtafarm-api`

- Add `MetricsConfig` and Helm values/environment variables.
- Add the centralized entitlement helper derived from `fullstack_access`.
- Extend `/user/me` with `plan` and `metrics_retention_days`.
- Add public response types and a Prometheus-compatible HTTP client.
- Add fake-upstream tests covering vectors, matrices, warnings, gaps, malformed
  data, timeouts, and response limits.

### Phase 3 — historical APIs

Repository: `vtafarm-api`

- Implement target validation and fixed query builders.
- Implement metadata and range endpoints.
- Enforce ownership, plan ranges, step selection, timeouts, rate limits, and
  field stripping.
- Document both routes in OpenAPI.

### Phase 4 — Live SSE

Repository: `vtafarm-api`

- Implement the observation cache, request coalescing, and bounded subscriber
  fan-out.
- Implement snapshot/sample/status/problem/reconnect events and heartbeats.
- Add stream limits, 60-minute reauthentication rollover, cancellation, and
  upstream recovery behavior.
- Document the stream in OpenAPI.

### Phase 5 — portal UI

Repository: `vtafarm`

- Add the Metrics tab, target selectors, plan-aware range controls, summary
  cards, and charts.
- Implement historical fetch cancellation when controls change.
- Implement exactly one EventSource for the active Live target.
- Close Live on pause, tab navigation, document backgrounding, and unmount.
- Display gaps, staleness, warnings, partial data, empty history, and upgrade
  messaging explicitly.
- Keep mobile layouts usable without hiding metric units or target identity.

### Phase 6 — measured rollout

- Deploy rules first and allow at least two scrape/evaluation intervals before
  enabling the UI.
- Confirm Sidecar uploads and Store Gateway queries before advertising 30 days.
- Enable the API with metrics unavailable gracefully when the stack is absent.
- Roll out the portal after API and recording-rule verification.
- Measure query duration, series count, cache hit rate, SSE connections,
  upstream errors, object-store growth, and API memory.
- Adjust cache and rate limits from measured data rather than removing bounds.

## 16. Verification and acceptance criteria

### Authorization and entitlement

- An owner can read their own session metrics.
- A user receives `404`, not existence disclosure, for another user's session.
- Free users can query 7 days and receive `403 metrics_range_not_allowed` for
  30 days.
- Premium users can query 30 days.
- Changing `fullstack_access` affects the next metadata/range request without a
  new login.
- No endpoint accepts arbitrary PromQL or namespace selectors.

### Metric correctness

- VTA-only shows one stable VTA component and its current Pod.
- Full Stack exposes only the components it actually runs.
- Component graphs remain continuous across a Pod rollout.
- Pod scope contains only the selected Pod.
- Entire Stack equals the sum of supported components for additive metrics.
- A shared PVC is counted once.
- Missing request/limit is `null`; missing usage is a gap, not zero.
- CPU Live uses the 1m rule; historical CPU uses the 5m rule.
- Values and units agree with the operator Grafana/Prometheus view for the same
  fixed time and target.

### Live behavior

- A Live connection receives a snapshot, then samples no faster than the
  configured interval.
- Heartbeats keep the connection open through Traefik and external proxies.
- Multiple clients for one target result in at most one upstream query per
  interval per API replica.
- Slow clients do not grow unbounded memory.
- Closing the browser, changing target, pausing, or backgrounding cancels the
  stream promptly.
- An upstream outage sends `problem`; recovery sends a fresh snapshot and
  resumes without a page reload.
- The 60-minute rollover reconnects and reauthorizes successfully.

### Performance and resilience

- Every range series stays within the configured point limit.
- A timed-out or oversized upstream response cannot exhaust an API worker.
- Thanos warnings are visible in the response and UI.
- Metrics being disabled or unavailable does not affect non-metrics routes.
- A rolling API deployment is recovered by normal EventSource reconnection.

## 17. Future separation of Premium

If product requirements later allow Premium without Full Stack Access, or Full
Stack Access without Premium history, introduce a dedicated plan or entitlement
column/table and change only the centralized entitlement helper. Preserve the
public values:

```text
plan: free | premium
metrics_retention_days: 7 | 30
```

Do not make the frontend depend on the future storage model. This seam is the
reason the first version can safely reuse `fullstack_access` without making that
coupling permanent.
