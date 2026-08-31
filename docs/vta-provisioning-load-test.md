# VTA provisioning load test

The admin load-testing page provisions a batch of ordinary `vta_only` sessions
through the same Cloudflare, database, Vault, Kubernetes, DID-hosting, and
orchestrator path used by the user portal.

## Run lifecycle

`POST /api/v1/admin/load-tests` accepts a count (1–50), one VTA image, and one
`did:key` admin DID. Members use the existing `platform` system account and are
named `load-<run-id>-NNN`. Up to ten member records are created in parallel;
each recorded session then runs independently in the ordinary orchestrator.

Only one run may own active resources at a time. A partial run remains active
until it is deleted so its successfully created members cannot be forgotten.
The database enforces this across API replicas.

Transient `creating` or `deleting` states older than five minutes are treated
as interrupted. Listing or starting a run reconciles them to a state the admin
can inspect or retry; session provisioning itself is recovered by the existing
orchestrator resume logic.

## Online check

`POST /api/v1/admin/load-tests/{id}/check` requires both:

1. the session row is `running`; and
2. the VTA Deployment currently has a Ready replica.

The second condition is the live result of the VTA container's `/health`
readiness probe. A Deployment that was manually scaled to zero therefore
reports offline even if its database status is still `running`.

## Cleanup

`DELETE /api/v1/admin/load-tests/{id}` asynchronously sends every member
through the normal VTA-only teardown. It removes DNS, the hosted DID and ACL,
Kubernetes resources, Vault seed, and the setup row. The platform account,
namespace, and Vault access remain because they are also owned by the platform
stack. A failed cleanup is retryable from the same button. Cleanup of runs made
by older releases also removes their legacy `load-test-*` account.

Deleted runs remain as lightweight database history but are omitted from the
admin run list.
