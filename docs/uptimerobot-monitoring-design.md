# UptimeRobot monitoring and public status design

Status: proposed

Observed: 2026-10-24

Scope: production and staging VTA Farm monitors, `status.ic3.dev`, and the
token-gated monitor endpoints in `vtafarm-api`

## 1. Decision

Use two independent monitoring surfaces:

1. **Customer status** answers only whether a customer can use VTA Farm. It
   contains three production monitors: Farm control plane, VTA connectivity,
   and shared platform connectivity.
2. **Operator alerts** report dependencies, unhealthy workloads, capacity,
   Rancher, and staging. They notify the internal team and never appear on a
   public status page.

Public probes are deliberately safe to call without a credential and return no
infrastructure detail. Internal probes use an environment-scoped secret in the
standard `Authorization: Bearer ...` header. Query-string authentication is
removed.

`status.ic3.dev` must select customer monitors explicitly. It must not select
the whole dashboard group, because adding an internal monitor to that group
would silently publish it.

## 2. Current-state findings

The UptimeRobot account currently has this VTA-related layout:

| Dashboard group | Monitor | Public on `status.ic3.dev` |
| --- | --- | --- |
| `VTI DevOps` | `Prod Workloads` | No |
| `VTI DevOps` | `Staging Workloads` | No |
| `VTA Farm` | `Prod Capacity` | Yes |
| `VTA Farm` | `Prod Health` | Yes |
| `VTA Farm` | `Staging Capacity` | Yes |
| `VTA Farm` | `Staging Health` | Yes |
| `VTA Farm` | `VTA Farm Rancher` | Yes |

`ic3 Status` selects the complete `VTA Farm` monitor group. Its **Show the
monitor URL** option is enabled. Consequently, each public monitor detail page
shows the complete target URL.

The existing internal endpoints authenticate with a shared `?token=...` query
parameter. The `Prod Capacity` public detail page therefore exposed the shared
monitor token. Treat that token as compromised; hiding the URL later does not
make it secret again. The same token protects health, workload, and capacity
responses, all of which disclose operator-only state.

There is also a second public page, `VTA Farm Status`, hosted at
`status.firstperson.dev`. It currently contains the same five `VTA Farm`
monitors. Its URL display is disabled, but it still needs to be included in the
migration so that the old internal monitors are not left public through another
page.

The `Prod Workloads` monitor already demonstrates the intended internal model:
it is in `VTI DevOps`, not attached to a public page, and notifies internal
contacts.

## 3. What belongs on the public page

A public status signal should answer a user question, not expose an
implementation symptom.

| Public component | User question | Public name |
| --- | --- | --- |
| Farm control plane | Can I reach VTA Farm and perform core management operations? | `VTA Farm` |
| VTA data plane | Can a VTA be reached through the public network path? | `VTA Connectivity` |
| Shared platform | Can customers reach the shared platform services used by default-connected VTAs? | `Platform Connectivity` |

The following are operator signals and must not be public components:

- pod restarts, pending pods, image-pull failures, or CrashLoopBackOff;
- node CPU, memory, Longhorn capacity, or metrics collection failures;
- database, Kubernetes API, or Vault detail;
- Rancher availability;
- any staging monitor;
- customer names, namespaces, pod names, node names, IP addresses, thresholds,
  or raw upstream error messages.

An internal alert can lead to a public incident when it is confirmed to affect
customers, but it should not automatically become a public component merely
because it is useful to operators.

## 4. Proposed HTTP interface

### 4.1 Public status endpoints

Add three unauthenticated, read-only endpoints:

```text
GET /api/v1/status/farm
GET /api/v1/status/vta
GET /api/v1/status/platform
```

All three use the same small response contract:

```json
{
  "status": "operational",
  "checked_at": "2026-10-24T23:00:00Z"
}
```

When a customer-important check fails, respond with HTTP `503` and only:

```json
{
  "status": "unavailable",
  "checked_at": "2026-10-24T23:00:00Z"
}
```

Contract rules:

- `200` means operational; `503` means unavailable. Unexpected handler errors
  also produce a generic `503`, with detail logged internally.
- Add `Cache-Control: no-store` and `Content-Type: application/json`.
- Do not accept target URLs, hostnames, thresholds, or component selectors from
  the request. That avoids turning the probes into an SSRF or discovery API.
- Do not return component-level failures, deployment state, database state, or
  exception text.
- Run downstream probes concurrently with a fixed three-second per-probe
  timeout and an eight-second overall budget, below UptimeRobot's timeout.
- Use public HTTPS hostnames for downstream connectivity checks, not cluster
  Services. The result must cover DNS, Cloudflare, TLS, Traefik, and the
  application listener.
- Share a limit of 30 requests per minute per client IP across the three public
  status endpoints. This leaves room for UptimeRobot retries and manual support
  checks without reusing the authenticated admin/user limits.

#### Public status rate-limit policy

The `/api/v1/status` route group applies one in-memory sliding-window limit to
all three public endpoints:

```text
GET /api/v1/status/farm
GET /api/v1/status/vta
GET /api/v1/status/platform
```

The rules are:

- one shared bucket per client IP, not 30 requests per endpoint;
- at most 30 requests in the preceding 60 seconds;
- every request reaching the route group counts, including a check that later
  returns `503`;
- request 31 returns HTTP `429 Too Many Requests` without running database,
  Kubernetes, Vault, VTA, or platform probes;
- the three normal one-minute UptimeRobot monitors consume approximately three
  requests per minute from a checking source, leaving headroom for retries and
  manual checks;
- the limiter is per API replica and held in memory. It protects dependencies
  from one noisy source but is not a distributed DDoS control; Cloudflare edge
  rate limiting remains the appropriate additional control for distributed
  traffic.

Client identity comes from Gin's `ClientIP()`, so production proxy trust and
forwarded-IP handling must preserve the real Cloudflare/visitor address rather
than collapsing every request to the ingress address or trusting arbitrary
forwarded headers.

#### `GET /api/v1/status/farm`

The inbound request already proves that the public API hostname, DNS, TLS,
Cloudflare route, ingress, and API process are reachable. The handler should
also perform cheap readiness checks for the dependencies required by core Farm
management operations:

- database reachable;
- Kubernetes API reachable;
- Vault reachable, initialized, and unsealed.

It returns only the generic public contract. The existing internal dependency
endpoint remains the place to learn which dependency failed.

#### `GET /api/v1/status/vta`

Resolve the one `DomainPlatform` session, require it to be in `running` state,
and probe its VTA through the public `/health` URL. The fixed platform VTA uses
the same runtime and public ingress path as other VTAs without tying global
status to a customer-owned session or requiring a second synthetic VTA.

A single broken customer VTA does not make this global component fail; such
failures belong to the internal workload alert and the affected customer's
incident handling.

#### `GET /api/v1/status/platform`

Resolve the one `DomainPlatform` session from the database, require it to be in
`running` state, and probe the shared stack through its public hostnames:

| Component | Probe |
| --- | --- |
| Mediator | `GET https://mediator.<platform-domain>/mediator/v1/readyz` |
| DID hosting | `GET https://dids.<platform-domain>/health` |
| VTC | `GET https://vtc.<platform-domain>/health` |

All required probes must pass. Internally log which component failed; publicly
return only `operational` or `unavailable`.

If product semantics later permit the VTC to be unavailable without affecting
the default VTA connection path, split it into its own public component rather
than silently treating a failed required probe as healthy.

### 4.2 Internal operator endpoints

Keep the existing diagnostic functions, but make their audience explicit:

```text
GET /api/v1/monitor/health
GET /api/v1/monitor/workloads
GET /api/v1/monitor/capacity
```

Authenticate them with:

```http
Authorization: Bearer <environment-scoped-monitor-token>
```

Use UptimeRobot's built-in Bearer authentication where available; its custom
header feature is an equivalent fallback. UptimeRobot supports authentication
and custom request headers for HTTP/API monitors, so the secret does not need
to be placed in the URL. See the official
[API monitoring guide](https://help.uptimerobot.com/en/articles/13628553-uptimerobot-api-monitoring)
and [advanced settings guide](https://help.uptimerobot.com/en/articles/11360863-uptimerobot-advanced-settings-customize-your-monitor-configuration).

Authentication rules:

- Accept the `Bearer` scheme case-insensitively and compare the token in
  constant time.
- Do not accept `token`, `api_key`, or any credential in the query string.
- Return `404` when monitoring is disabled and `401` for a missing or invalid
  credential.
- Use separate random tokens for production and staging. A token grants only
  read access to that environment's monitor endpoints.
- Store the token in the existing external Kubernetes Secret and in
  UptimeRobot's authentication field. Never write it to Helm values, Git,
  monitor names, metadata, examples, logs, or incident text.
- Redact `Authorization` and query strings from application, ingress,
  Cloudflare, and support logs.
- Confirm that Cloudflare and Traefik forward the `Authorization` header before
  removing query support.

Moving the secret to a header prevents URL disclosure through status pages,
browser history, referrers, and ordinary URL logs. It does not make the already
published token safe; that token still must be rotated.

## 5. UptimeRobot organization

### 5.1 Dashboard groups

Use groups for the operator dashboard, not as the public-page security
boundary:

| Group | Monitors | Audience |
| --- | --- | --- |
| `VTA Farm` | the three production public status monitors | Customers and operators |
| `VTI DevOps` | production dependencies, workloads, capacity, Rancher | Internal production on-call |
| `VTI Staging` | staging dependencies, workloads, capacity, and staging surfaces | Internal development team |

Recommended monitor names and targets:

| Monitor | Target | Public page | Notifications |
| --- | --- | --- | --- |
| `VTA Farm` | `/api/v1/status/farm` | Yes | customer-impact on-call |
| `VTA Connectivity` | `/api/v1/status/vta` | Yes | customer-impact on-call |
| `Platform Connectivity` | `/api/v1/status/platform` | Yes | customer-impact on-call |
| `Prod Dependencies` | `/api/v1/monitor/health` + Bearer | No | internal production |
| `Prod Workloads` | `/api/v1/monitor/workloads` + Bearer | No | internal production |
| `Prod Capacity` | `/api/v1/monitor/capacity` + Bearer | No | internal production |
| `VTA Farm Rancher` | existing target | No | internal production |
| `Staging Dependencies` | staging `/monitor/health` + Bearer | No | internal staging |
| `Staging Workloads` | staging `/monitor/workloads` + Bearer | No | internal staging |
| `Staging Capacity` | staging `/monitor/capacity` + Bearer | No | internal staging |

Add tags such as `audience:public`, `audience:internal`, `env:prod`, and
`env:staging` for filtering and audits. Tags are helpful metadata, but the
public page should still be an explicit allowlist of the three monitor IDs.

### 5.2 `status.ic3.dev`

Keep the existing Murmurations selection unchanged unless that product owner
wants a separate redesign. For VTA Farm:

1. Remove the selected `VTA Farm` group from the status page.
2. Add exactly the three new public monitor IDs.
3. Disable **Show the monitor URL** as defense in depth. UptimeRobot supports
   hiding monitored URLs; see its official
   [status page overview](https://uptimerobot.com/status-page/).
4. Keep the details page, uptime charts, percentages, and outage history if
   desired; their targets and responses are now safe for a public audience.
5. Do not include staging, capacity, workload, dependency-detail, or Rancher
   monitors.

Repeat the allowlist change on `status.firstperson.dev`, or retire that page if
it is no longer intentionally customer-facing. Do not leave the old five
monitors attached to either public page.

### 5.3 Polling and notification defaults

Start with:

| Class | Interval | Failure confirmation | Timeout | Recipient |
| --- | --- | --- | --- | --- |
| Public customer signal | 1 minute | 2 consecutive failures | 15 seconds | production on-call |
| Prod dependencies/workloads | 1 minute | 2 consecutive failures | 15 seconds | internal production |
| Prod capacity | 5 minutes | 2 consecutive failures | 15 seconds | internal production |
| Staging | 5 minutes | 2 consecutive failures | 15 seconds | internal staging |

Use at least North America and Europe for customer-facing checks if the plan
supports multiple regions. Enable recovery notifications. Put planned rollout
windows in UptimeRobot maintenance so expected restarts do not create false
incidents.

Capacity is intentionally slower because a transient CPU spike is not an
outage. If storage urgency and CPU/memory noise require different escalation,
split the combined capacity endpoint into separate internal monitors later.

## 6. Token containment and migration order

### P0: contain the published credential

1. Disable **Show the monitor URL** on `ic3 Status` immediately.
2. Detach `Prod Capacity`, `Prod Health`, both staging monitors, and Rancher
   from every public status page. Audit both `status.ic3.dev` and
   `status.firstperson.dev`.
3. Rotate `MONITOR_TOKEN`. The currently published value must never be reused.
4. Verify that requests with the old value fail. Do not paste either value into
   tickets, chat, logs, or this document.

Steps 1 and 2 reduce continued exposure; step 3 is what invalidates the leaked
credential. Search-engine or CDN cache removal is optional cleanup, not a
substitute for rotation.

### Phase 1: move internal monitors to header authentication

1. Implement Bearer-header support and tests while temporarily retaining the
   old query path only for the cutover window.
2. Put the monitors in a maintenance window.
3. Edit each internal UptimeRobot monitor to use a query-free URL and the
   Bearer authentication field.
4. Rotate the server and UptimeRobot value to a new environment-scoped token.
5. Confirm successful checks and recovery notifications.
6. Remove query-token support from code and OpenAPI, redeploy, and verify that
   even a valid current token in `?token=` receives `401`.
7. End the maintenance window.

The temporary compatibility window should be measured in minutes, not releases.

### Phase 2: add customer-status probes

1. Implement the three `/api/v1/status/*` endpoints with generic responses,
   fixed targets, concurrency, and bounded timeouts.
2. Add handler, middleware, timeout, and response-redaction tests.
3. Create the three query-free UptimeRobot monitors.
4. Exercise one controlled failure for each monitor and confirm both down and
   recovery behavior.
5. Replace the public page's group selection with the explicit three-monitor
   allowlist.

### Phase 3: reorganize internal monitors

Move production diagnostics to `VTI DevOps`, staging diagnostics to
`VTI Staging`, and keep only the three customer signals in `VTA Farm`. Prefer
moving and renaming the existing monitors over deleting them so incident
history is retained.

## 7. Acceptance checklist

### Security

- No public status page, monitor detail page, monitor URL, or redirect contains
  a secret.
- The compromised token returns `401` on all internal monitor endpoints.
- The current token succeeds only in the `Authorization` header and fails in a
  query parameter.
- Production and staging use different tokens.
- Public endpoint bodies contain no infrastructure identifiers or raw errors.
- Both public status pages have been audited.

### Customer behavior

- `status.ic3.dev` shows only `VTA Farm`, `VTA Connectivity`, and
  `Platform Connectivity` for this product.
- A Farm dependency failure makes only the customer-relevant Farm component
  unavailable and also triggers the detailed internal dependency alert.
- A platform VTA ingress or health failure changes `VTA Connectivity`.
- A required shared-stack failure changes `Platform Connectivity`.
- A staging failure, isolated pod restart, Rancher outage, or capacity warning
  does not appear on the public page.

### Operations

- Public and internal monitors notify the intended contact groups.
- Controlled failures create an alert only after the configured confirmation
  threshold and send a recovery notification.
- Maintenance windows suppress planned rollout noise.
- Monitor names, group membership, tags, URLs, intervals, regions, contacts,
  and public-page membership are recorded in an operator-owned inventory.

## 8. Follow-up boundary

UptimeRobot remains appropriate for externally visible reachability and the
small set of aggregate HTTP checks above. Detailed pod, node, and storage
telemetry will eventually be easier to route and deduplicate in
Prometheus/Alertmanager or an equivalent Kubernetes-native stack. That is a
separate improvement; it is not required to remove internal signals from the
public page or to fix the token exposure.
