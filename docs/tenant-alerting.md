# Tenant resource alerting

This document is the contract between the observability stack, `vtafarm-api`, and the Portal. It covers the first alerting phase only; paid plan selection and automated capacity upgrades are intentionally separate.

## Data flow

1. Prometheus evaluates the VTA Farm rules every 15 seconds.
2. Alertmanager groups alerts by cluster, session, component, alert name, and resource.
3. Alertmanager sends firing and resolved notifications to the API webhook.
4. The API validates the shared bearer token, allowlists the alert name, maps the internal session label to its database session, and persists an incident.
5. The Portal reads tenant-isolated incidents from the API. Related log downloads query only the affected component and the incident window.

Alertmanager is responsible for evaluation delivery, grouping, inhibition, and retries. The API remains the source of truth for user-visible incident history, read state, access control, and log downloads.

## Alert catalogue

| Alert name | Severity | Condition | Pending period | User impact |
| --- | --- | --- | --- | --- |
| `VTAFarmCPUUsageHigh` | warning | CPU usage is at least 75% but below 90% of its limit | 10 minutes | Sustained load; approaching throttling |
| `VTAFarmCPUUsageCritical` | critical | CPU usage is at least 90% of its limit | 5 minutes | CPU throttling is likely |
| `VTAFarmMemoryUsageHigh` | warning | working-set memory is at least 75% but below 90% of its limit | 10 minutes | Reduced memory headroom |
| `VTAFarmMemoryUsageCritical` | critical | working-set memory is at least 90% of its limit | 5 minutes | OOMKill risk |
| `VTAFarmStorageUsageHigh` | warning | persistent-volume usage is at least 75% but below 90% of capacity | 15 minutes | Storage increase should be planned |
| `VTAFarmStorageUsageCritical` | critical | persistent-volume usage is at least 90% of capacity | 5 minutes | Writes may fail soon |
| `VTAFarmMemoryOOMKilled` | critical | a container restarted in the last 5 minutes and its last termination reason is `OOMKilled` | immediate | Component restarted after reaching its memory limit |
| `VTAFarmComponentCrashLooping` | critical | a container remains in `CrashLoopBackOff` | 3 minutes | Component cannot start reliably |
| `VTAFarmComponentUnavailable` | critical | a Deployment has zero available replicas | 2 minutes | Component is unavailable |

CPU and memory percentages use the Kubernetes container limits applied to the workload. Storage uses the PVC requested capacity. A missing limit or capacity produces no percentage alert instead of inventing a denominator.

The table lists the normal-operation defaults. Stack 06 may override the CPU recording window, CPU and memory pending periods, and Alertmanager tenant grouping per cluster. Development uses a 1-minute CPU window, 1-minute warning and critical pending periods, a 10-second initial group wait, and a 30-second group interval for end-to-end testing. Staging and production retain the alert defaults above, use a 30-second initial group wait, and deliver subsequent firing or resolved state changes on a 1-minute group interval.

During a rolling upgrade, two pods may coexist. The rules aggregate with `max` by session and component, so limits and usage do not double merely because an old and a new pod overlap.

## Labels and annotations

Every tenant rule sets:

- `audience="tenant"`: required by both the Alertmanager route and API allowlist.
- `severity="warning"` or `severity="critical"`.
- `category="resource"` or `category="availability"`.
- `resource`: `cpu`, `memory`, `storage`, `runtime`, or `availability`.
- `cluster`: supplied by Prometheus external labels.
- `label_session_id`: internal numeric setup-session ID propagated from the workload label.
- `label_app`: workload component label, mapped by the API to `vta`, `mediator`, `dids`, `vtc`, or `trust-registry`.

Rules provide user-facing `summary` and `description` annotations. Percentage rules also provide `usage_percent`. The API never accepts arbitrary alert names from this endpoint; extending the rule catalogue requires extending its allowlist as part of the same release.

## Routing, grouping, and inhibition

The Alertmanager default receiver remains `null`. Only alerts with `audience="tenant"` are routed to the VTA Farm API webhook.

Tenant alerts are grouped by `cluster`, `label_session_id`, `label_app`, `alertname`, and `resource`. A critical alert inhibits its warning counterpart for the same cluster, session, component, and resource. Alertmanager sends resolved notifications so the API can record the end of the incident and calculate the observed incident duration.

The API suppresses user visibility when the session is not running or has an active upgrade task. Suppressed incidents remain available to administrators for diagnosis and never become tenant-visible later just because the upgrade completed.

## Incident lifecycle and deduplication

The incident identity is the tuple `(cluster, fingerprint, starts_at)`. Repeated Alertmanager deliveries update `last_seen_at` rather than creating duplicate rows. A resolved event closes the matching incident; a late firing delivery cannot reopen an already resolved incident.

`downtime_seconds` is the elapsed time between `started_at` and `resolved_at`. It is an observed alert window, not an SLA calculation. For resource-pressure alerts it describes how long the alert condition remained active; for component-unavailable alerts it approximates observed downtime after the two-minute pending period.

## HTTP API

The full schemas are in `/openapi.yaml` and the interactive reference at `/docs`.

### Internal delivery

- `POST /api/v1/internal/alertmanager/webhook`
  - Requires `Authorization: Bearer <shared-token>`.
  - Accepts the standard Alertmanager webhook payload.
  - Returns `202` with accepted and ignored counts.
  - Has a 1 MiB request limit.

### User endpoints

- `GET /api/v1/user/incidents`: incidents across all sessions owned by the current user.
- `GET /api/v1/setup/{id}/incidents`: incidents for one owned session.
- `GET /api/v1/setup/{id}/incidents/{incident_id}`: incident detail.
- `PATCH /api/v1/setup/{id}/incidents/{incident_id}` with `{ "read": true | false }`: update read state.
- `GET /api/v1/setup/{id}/incidents/{incident_id}/logs`: download related retained logs.

List endpoints support `status=firing|resolved`, `unread=true`, and `limit=1..100`.

### Admin endpoints

- `GET /api/v1/admin/incidents`
- `GET /api/v1/admin/setup-sessions/{id}/incidents`
- `GET /api/v1/admin/setup-sessions/{id}/incidents/{incident_id}`
- `GET /api/v1/admin/setup-sessions/{id}/incidents/{incident_id}/logs`

Admin lists include suppressed incidents.

## Related log window

The incident log archive contains Loki logs for only the affected component. Its query window begins five minutes before `started_at`; for a resolved incident it ends five minutes after `resolved_at`, capped at the current time. For an active incident it ends at the current time. Session creation time and the user's log-retention entitlement still bound the query.

## Secret and deployment activation

Use one randomly generated token value per cluster. Put the same value and revision in that cluster's stack 05 and stack 06 local `terraform.tfvars` files:

```hcl
alertmanager_webhook_enabled        = true
alertmanager_webhook_token          = "<output of openssl rand -hex 32>"
alertmanager_webhook_token_revision = 1
```

Stack 05 creates `vtafarm-alertmanager-webhook` in the API namespace. Stack 06 creates the same-named Secret in the monitoring namespace. Both contain the `ALERTMANAGER_WEBHOOK_TOKEN` key. The token variables are ephemeral and the Kubernetes provider receives them through its write-only `data_wo` field, so the values are not persisted in OpenTofu plan or state. Cluster tfvars are local, ignored files and must never be committed.

To rotate a token, replace it in both stack files and increment both revisions to the same new integer. The revision changes the API and Alertmanager Pod templates so both workloads roll and load the new value.

Activation order:

1. Publish the API and Portal releases containing the incident feature.
2. Put those release versions in stack 05 and apply the database migration according to the release procedure.
3. Put the same generated token, enabled flag, and revision in the cluster's stack 05 and stack 06 tfvars.
4. Apply stack 05. It creates the API Secret and deploys the API and Portal releases.
5. Apply stack 06. It creates the monitoring Secret and enables Alertmanager delivery.
6. Confirm the Alertmanager configuration and fire a controlled test alert.

Keep stack 06 `alertmanager_webhook_enabled` false until the API migration and release are ready. Alert rules can exist before webhook routing is enabled; their default receiver is `null`.

## Verification

After deployment:

1. Confirm the Prometheus rules page shows all nine `VTAFarm*` rules without an evaluation error.
2. Confirm Alertmanager status shows the `vtafarm-api` receiver and the tenant route.
3. Send a valid controlled alert or temporarily load a test workload past a threshold.
4. Confirm one incident is created, repeated deliveries do not duplicate it, and a resolved notification closes it.
5. Confirm another user cannot read the incident or download its logs.
6. Confirm an alert produced during an upgrade is visible to an admin as suppressed and invisible to the tenant.
7. Confirm the Portal can open Metrics and download the related log archive from the incident.

Do not lower production thresholds solely to test delivery. A dedicated synthetic rule with the tenant labels is safer, and should be removed immediately after validation.
