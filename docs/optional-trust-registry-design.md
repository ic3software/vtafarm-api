# Optional Trust Registry for Full Stack — Design

Status: **implemented in the working branch; image publication and staging acceptance pending**.
Headless setup integration checked on 2026-10-22; live staging acceptance remains pending.

Add a per-session Trust Registry to `full_stack` as an opt-in component, both
when creating a stack and when adding it to an existing running stack. Existing
stacks must keep their identities, member data, volumes, URLs, and installation.
Enabling the registry must never require tearing down or re-running VTC setup.

This extends [full-stack-setup-design.md](full-stack-setup-design.md), whose
four-component pipeline describes the currently implemented baseline.

## 1. What the registry adds

The VTC remains responsible for membership, admission policy, roles, and issuing
credentials. The Trust Registry publishes queryable trust records that external
verifiers and peer communities can consult. In this integration the VTC writes
its membership projection; the registry is not the authoritative member store.

The registry can also represent authorization and recognition relationships.
Deploying it does not automatically configure trust between communities, grant
issuer powers, or make another verifier trust this community.

The upstream VTC explicitly supports **no-registry mode**: when neither
`registry.did` nor `registry.url` is configured, it constructs no registry client
and does not start membership synchronization. The VTC continues running. Features
that require registry access, including registry-backed cross-community
recognition, are unavailable. Upstream currently reports the registry status as
`degraded` in this mode; the Farm should display **Not enabled** for an opted-out
component rather than treating the whole stack as broken.

Sources: [VTC runtime client selection][vtc-server],
[runtime configuration][vtc-config], [integration guide][vtc-guide]. These
describe the reviewed upstream revision, not a runtime inspection of production.

## 2. Scope and compatibility

| Scenario | Behavior |
| --- | --- |
| New `full_stack`, option omitted or false | Existing four-component flow |
| New `full_stack`, option true | Existing components plus a dedicated registry |
| Existing running `full_stack` | Owner explicitly enables the add-on in place |
| Existing platform stack | Equivalent action in the platform admin surface |
| `vta_only` | Reject registry provisioning; no change to this mode |

Keep the modes `vta_only` and `full_stack`. Do not introduce another setup mode.
The default is off, including for migrated rows and existing API clients.
Deploying the new Farm API must not enqueue enablement for old sessions.

First delivery includes managed and custom domains, new and existing Full Stacks,
status/progress, retry, image upgrades, backup/export inventory, and teardown.
External registries, shared multi-community registries, public REST ingress,
standalone registry deployments, and a user-facing disable/delete-add-on action
are outside this delivery. Failure recovery must still restore the prior VTC
configuration when possible, without deleting existing stack data.

## 3. Image and version preflight

Use **`ghcr.io/ic3software/trust-registry`**, supplied by the user:
[container package][image-package]. Integrate its immutable tags into the same
GHCR selection mechanism as other components; persist the selected immutable
image reference. Do not hard-code the Dockerfile's default `0.8.0` as the selected
version or automatically use a moving `nightly` tag.

Research found a successful build publishing `0.28.0-c534746` and `nightly` on
2026-10-20 ([build run][image-run]). This is evidence of publication, not an
end-to-end compatibility result or a final version selection.

On 2026-10-22, `0.28.0-be33b15` was pulled and checked locally: its headless
setup flags, phase-1 key-file permissions, recipe validation, and refusal to
overwrite an existing credential match this integration. This was not a live
VTA provisioning test. VTA revision `4df0200f` includes the required template
from PR #2252; no image pins are changed by this integration.

The [image Dockerfile][image-dockerfile] downloads a precompiled
`trust-registry-k8s` binary. Unlike the upstream Dockerfile, this repository does
not compile Rust or declare Cargo features. Check the binary producer and verify
the actual selected image supports `vta`, `storage-fjall`, `secrets-vault`,
`standalone`, and TSP if advertised. Do not infer missing VTA support from the
upstream default build, or infer its presence from a successful image build.

The [image entrypoint][image-entrypoint] currently holds the container when
DIDComm is enabled and `PROFILE_CONFIG` is absent. It does not recognize
`TR_VTA_CREDENTIAL` as an alternative. The Farm will explicitly run
`trust-registry` with its own working directory, as it already does for other
components, bypassing this guard. Do not inject a dummy `PROFILE_CONFIG`.
Align the image entrypoint separately if default-entrypoint support is wanted.

Before enabling an existing stack, inspect its recorded and deployed images for
VTC messaging-based registry writes, VTA provisioning/DID editing, and compatible
mediator protocols. Upgrade only an incompatible component through the existing
upgrade flow. A newer registry image alone cannot add missing VTC capabilities.

## 4. Identity, storage, and configuration

Each enabled session gets its own registry Deployment and PVC, named consistently
with the existing `fs-{sid}-*` resources, proposed `fs-{sid}-trust-registry`.
Use one replica and Recreate updates: fjall must never have concurrent processes
opening one directory. Store records at an absolute path such as
`/work/trust-registry/data` on the registry PVC.

Reuse the session's VTA, mediator, and DID host. Give the registry a dedicated
VTA context and a hosted `did:webvh`, with a deterministic path such as
`{vtc_name}-trust-registry`. The context and DID path are selected once, checked
for collisions, persisted, and reused on retry. Do not reuse the VTC context's
keys or create another community identity.

No new public hostname, Cloudflare record, Ingress, or TLS SAN is needed initially.
Publish the registry's messaging services in its DID document and verify those
advertisements match the actual binary. Clients reach it through the mediator.
A ClusterIP Service may expose port 3232 internally. Bind to `0.0.0.0:3232` for
Pod-IP HTTP probes; the explore guide's `127.0.0.1` bind cannot serve those probes.
Keep internal access subject to the existing namespace/network policy.

Required configuration belongs to the service that consumes it:

| Consumer | Information to add |
| --- | --- |
| Registry environment | VTA credential reference, context ID, mediator DID, storage path, Vault cache settings, enabled transports |
| Registry `ADMIN_DIDS` | The existing **VTC service DID**, which signs record writes |
| VTC runtime `config.toml` | `[registry].did` pointing to the registry DID |
| VTC DID document | A `TrustRegistry` referral to that same registry DID |
| VTA | Registry context, keys, and appropriately scoped provisioning/runtime ACLs |

For example, the VTC runtime change is a merge into its existing TOML:

```toml
[registry]
did = "did:webvh:<scid>:<existing-did-host>:<registry-path>"
```

The runtime setting enables the VTC client. The DID-document referral lets other
clients discover the registry. They are separate changes: setup's `registry_did`
only publishes the referral and does not configure runtime synchronization.
Neither the VTC admin DID nor the owner's PNM DID substitutes for the VTC service
DID in `ADMIN_DIDS`.

Do not regenerate other service configurations. The mediator and DID host are
reused through their normal account/publication operations, with existing policy
changes only if preflight proves they are necessary.

### Credential file and Vault identity cache

The headless `trust-registry setup` command now owns credential creation. It writes
`/work/trust-registry/vta-credential.json` with mode `0600` on the Registry PVC;
the Deployment uses that file directly through `TR_VTA_CREDENTIAL`. The temporary
setup key lives on the same PVC. Farm neither generates private keys nor copies
credential values through Job logs, ConfigMaps, database rows, or Deployment env.
The PVC therefore contains sensitive state and its backups must protect both the
record store and the VTA credential.

The identity bundle cache remains in Vault at
`trust-registry/user-{uid}/session-{sid}/identity-cache`, using Kubernetes auth
and the existing per-user policy. Building with `secrets-vault` is still required.
There is no credential-fetching init container, Vault CLI helper image, or
Farm-minted permanent access DID in the headless flow. Legacy Vault credentials
are left untouched until ordinary session teardown.

Provisioning advertises only DIDComm, with the existing mediator. Farm keeps
`ENABLE_REST=true` at runtime solely for its internal HTTP service and `/health`.
It does not set `TR_PUBLIC_URL`, so the runtime DID builder advertises no REST
endpoint and agrees with the provisioned DID. Loading `vta.env` verbatim would
turn off `/health`; the Deployment supplies this deliberate internal override.

The registry obtains and uses private keys at runtime. The setup guide's phrase
"no private keys of its own" means VTA-managed identity, not remote-only signing.
Sources: [VTA startup integration][tr-vta], [secret backend][tr-secrets],
[credential loader][tr-loaders].

## 5. New-stack provisioning

When opted out, do not add registry resources, Jobs, Vault paths, or setup stages.
When opted in, extend the post-admin-gate flow:

```text
existing bootstrap and admin gate
  -> deploy VTA (mediator and DID host already ready)
  -> trust-registry setup --setup-key-out (Registry PVC)
  -> stop VTA, grant setup DID with --admin-expires 1h --admin-handoff, restart VTA
  -> trust-registry setup --from (running VTA mints/publishes DID and admin credential)
  -> persist registry_did from the public key=value completion block
  -> VTC setup with registry_did, then deploy VTC
  -> configure registry ADMIN_DIDS with the VTC DID
  -> deploy registry and check its messaging/write-path status
  -> merge [registry].did into the VTC runtime config and restart VTC
  -> verify the published referral and finish add-on enablement
  -> running only after Registry activation succeeds
```

Follow [upstream headless setup][tr-headless], using Registry Jobs for both setup
phases and a single offline VTA context grant. No PNM credential or offline
`vta did-mgmt dids create` chain is needed. The Registry image must include
headless setup plus `vta`, `secrets-vault`, `storage-fjall` and `standalone`; the VTA
must include [the trust-registry template][vta-tr-template].

A Registry identity setup failure is recorded while VTC setup proceeds without
a referral, so the baseline components can finish provisioning safely. The
session then stops at the Trust Registry step and reports the Registry error;
it must not be marked running. Preserve the VTC's existing claim/install output;
its setup now follows Registry provisioning.

## 6. Add to an existing stack without rebuilding

Expose an **Enable Trust Registry** action for a running Full Stack. Keep the
parent session running and report add-on progress separately. Never call
`vtc setup`, recreate the VTA seed/context, reissue member credentials, replace
existing PVCs, or re-enroll the existing administrators.

1. Authorize the owner (or the platform admin for the platform stack), acquire
   the session maintenance lock, and check capacity, versions, configuration,
   DID ownership/history, and any active upgrade/recovery operation.
2. Persist the operation and chosen image/identity parameters before mutation.
   Provision only the registry resources and Vault locations.
3. Run Registry phase 1 on its PVC. Stop the VTA only for the context-scoped
   setup grant, with `--admin-expires 1h --admin-handoff`, then restart it.
4. Run Registry phase 2 against the live VTA. It publishes the DID on the
   registered `dids` server and writes the long-term credential locally.
   Reuse successful Job receipts on retry.
5. Set `ADMIN_DIDS` to the existing `session.VtcDid`, start the registry, and
   verify its advertised transport and write path.
6. Append a signed update to the **existing** VTC DID history through the
   **running** VTA, adding the referral while preserving the DID, keys, other
   services, and history. The farm mints a did:key for this one operation, grants
   it as a context-scoped admin of the VTC's context in the same stopped-VTA
   window pattern as step 3 (`vta acl create --role admin --contexts <vtc>
   --expires 15m`), logs in to the VTA over HTTPS as that key
   (`auth/challenge/0.1`, `auth/authenticate/0.2`), reads the VTA's head of the
   log (`vta/webvh/dids/get/1.0`), and sends `vta/webvh/dids/update/1.0` with the
   document plus `expectedVersionId`. The VTA signs the new entry and publishes
   it to the DID host itself, over TSP or DIDComm. The key is never stored; the
   grant expires on its own. A self-hosted VTC log, if present, must also be
   updated using the supported install path.
7. Read the current VTC runtime config immediately before writing, merge only
   `[registry].did`, and restart the VTC using the existing component-config
   backup/write/start/rollback pattern. Do not render a fresh setup config over
   the customized one or discard other keys already in `[registry]`.
8. Verify the published signed referral and the restarted VTC. Mark the add-on
   active after deployment/configuration checks. This state does not assert that
   membership writes have succeeded: staging acceptance must verify the VTC's
   selected transport, correlated replies, authorization, sync diagnostics, and
   existing-member publication (§8).

Upstream explicitly supports changing the referral after initial mint via
VTA-side DID editing ([setup schema][vtc-setup]). This requires a signed DID-log
update, not a change to a locally served JSON file alone.

**Why the edit goes through the running VTA and not the offline CLI.** The
offline `vta did-mgmt dids edit` holds no mediator session, so the only
transport it can initiate toward a DID host is REST. A host provisioned through
a VTA with a mediator, which is every Full Stack `dids` daemon, advertises only
`TSPTransport` and `DIDCommMessaging`, so the offline command stops with
`no transport in common … it advertises [tsp, didcomm] and this agent can
initiate [rest]`, after it has already committed the new entry locally. The
running VTA holds the mediator session and publishes over TSP or DIDComm; it
also re-publishes an unconfirmed local head before applying the next update,
which is how a stack left in that state by the earlier offline attempt heals.
`internal/vta` is the client; `publishRegistryReferral` in
`internal/setup/trust_registry_referral.go` is the step. It skips the grant and
the edit when the published document already carries the referral, and sends an
otherwise unchanged document when the VTA's head carries it but the host does
not, which makes the VTA reconcile and publish. New stacks avoid the edit by
passing `registry_did` to VTC setup. Do not report existing-stack activation as
verified until the referral publication succeeds.

A mediator-only host also blocks every other offline publish (`dids create`,
`dids register`). The durable fix on the host side is one more online edit, of
the daemon's own DID, adding `{"id":"<daemon did>#trust-tasks","type":"TrustTaskHTTPS","serviceEndpoint":"https://<dids host>/api"}`;
the daemon's readiness check logs that exact entry, and its control plane reloads
on publish. That is a separate change, not part of registry enablement.

**Downtime:** the VTC needs a restart for the new client/syncer. Provisioning
authorization needs two brief VTA maintenance windows on an existing stack, the
registry context grant and the referral grant, each `scale to 0 -> offline Job ->
restart`; a retry that finds the referral already published takes neither. Prefer compatible live
APIs where a suitable credential already exists, but do not promise zero VTA
downtime or bypass the fjall lock. DID publication should use the live hosting
interface. Always restart a temporarily stopped component on error. Preserve
shared mediator/DID-host service for agents attached to this stack.

The existing config handler operates on the selected component only and backs
up/restores its config (`internal/handler/setup_config.go`). Reuse that behavior
for the VTC; its readiness check is not by itself a registry-write acceptance test.

### Headless retries and legacy attempts

The new context is `trust-registry-{sid}-headless`. The earlier offline flow's
`trust-registry-{sid}` context did not carry admin-handoff; it is never silently
repurposed or deleted. Successful setup-key, ACL-grant, and setup Jobs have no
TTL and remain receipts. Retrying uses the same setup key and completion output;
it does not re-run a successful context creation or replace a credential file.

Upstream phase 2 refuses an existing credential file. If provisioning was
interrupted after creating the remote identity but before saving a complete
receipt, keep the original output and files for reconciliation; do not delete
the credential or context to make a retry pass. An expired/consumed one-hour grant
also requires reconciliation rather than a permanent `import-did` fallback.
An already-provisioned legacy registry needs a separate credential/context
migration before converting its Deployment to the headless file layout.

## 7. Proposed API, persistence, and retries

Add optional `trust_registry_enabled` and `trust_registry_image` to Full Stack
creation. Require an allowed immutable image only when enabled; old requests
retain their behavior. Reject registry enablement for `vta_only`.

Proposed add-on routes (under `/api/v1`):

| Method | Route | Behavior |
| --- | --- | --- |
| POST | `/setup/:id/trust-registry` | Enable or explicitly retry with an image; returns 202 and progress |
| GET | `/setup/:id/trust-registry` | Desired state, phase, identity, image, and sanitized error |
| POST/GET | `/admin/platform-stack/trust-registry` | Same operations, restricted to the platform stack |

Apply existing Full Stack entitlement checks on creation and session ownership
checks on existing-stack actions. Preserve current management access semantics;
do not expose general customer-stack mutation through a platform-admin route. Reject concurrent
maintenance, upgrade, recovery, teardown, and duplicate conflicting operations.
An already-active request with the same image is an idempotent success; image
changes use the existing upgrade machinery rather than re-provisioning.

Persist desired enablement (default false), image, context/path, registry DID,
add-on state (`disabled`, `enabling`, `active`, `failed`), resumable phase,
timestamps, and a sanitized error. No credentials/private keys in these columns.
For an existing stack, add-on failure must not mark the parent setup `failed`.
For new opted-in stacks, finish the add-on attempt before returning the baseline
stack to running. An add-on failure marks the new setup failed at the Trust
Registry step, while an in-place enablement failure stays visible and retryable
without failing the already-running four-component stack.

After persisting an add-on failure, remove its failed Jobs and their Pods,
ConfigMaps, and temporary input Secrets. Keep the phase and sanitized error in
the session for the UI. Successful setup-key, ACL-grant, and setup Jobs remain recovery receipts;
active Jobs are never removed by failure cleanup. Retrying reuses the persisted
context, credential and DID rather than relying on a failed Pod.

Use a durable per-session operation claim, compatible with existing maintenance
locking and startup resumption. A client timeout or API restart must not lose the
operation or allow another worker to execute it concurrently. Reconcile completed
steps before retrying: the temporary admin handoff is single-use, context creation
may already have succeeded, and DID-log updates must not mint another DID.

Before VTC wiring, failure leaves the original stack configuration intact.
After wiring, restore the exact saved runtime config if necessary and restart
the VTC. DID history is append-only: reverting a referral requires another signed
update, never restoring an old log over newer history. Retain partially created
registry resources and credentials for a visible retry; do not delete a registry
that a published referral still points to. Report incomplete restoration explicitly.

## 8. Existing members, health, and lifecycle

Check the persisted audit cursor, pending/failed jobs, current roster, and existing
registry mirror when enabling an old stack. Upstream can replay retained audit
history from a missing cursor, but that does not guarantee a complete historical
backfill after retention or previous partial synchronization. Reconcile against
the current roster during staging acceptance. The implementation retains and lets
the VTC process its existing audit history and queue; it does not directly edit
fjall or synthesize member events. Missing history after manual data repair needs
operator reconciliation through supported VTC operations. Do not replay stale
removals as fresh grants or reset cursors blindly.

Honor member `registryConsent`/`publishConsent` and publication policy. Enabling
the component cannot grant consent on a member's behalf. Report synchronization
progress separately from service readiness; a large backlog need not block all
community use. If the pinned VTC has no supported reconciliation operation, that
must be reconciled explicitly before claiming complete historical publication;
it is not a reason to rebuild the community.

HTTP `/health` can return 200 with `status: degraded`. Monitor body status and
write-path details, plus VTC sync queue/failure information. A successful read-only
probe cannot prove VTC write authorization. Verify signed put/query/delete in an
isolated test scope and exercise the VTC sync path with a consented test member
in staging. Do not publish synthetic members into a customer's live trust graph.

Include the component wherever deployed-component inventory is used: session
responses, progress/logs, version selection, self-service/admin upgrades, recovery,
resource profiles/capacity, backup/export, and full-session teardown. Capacity must
distinguish opted-out stacks from five-component stacks and admit an add-on using
its incremental cost. The working branch starts with a 10m CPU request, 64Mi
memory request, 256Mi memory limit, and 200Mi PVC. These are provisional factory
values, not measured production sizing. Validate them against expected records
and traffic in staging; memory defaults can be changed through the Farm UI.

Back up the Registry PVC (records and credential) and the Vault identity cache. Extend deletion to the
registry Jobs, Deployment, Service, PVC, and session Vault prefix. Audit policies
and bootstrap permissions must agree on the new prefix. A Farm rollback or a
down-migration must not silently orphan running registries or drop their data.

## 9. Implementation order and acceptance

1. Verify pinned image features/entrypoint, production component compatibility,
   headless provisioning and DID editing, and the existing-member reconciliation
   mechanism. Resolve these gates before touching a live stack.
2. Implement opt-in persistence, inventory, Vault cache policy and headless credential creation,
   Kubernetes resources, capacity, and the shared resumable enablement steps.
3. Wire the new-stack branch and existing-stack enable/retry routes, maintenance
   and upgrade exclusions, config restoration, and startup recovery.
4. Add owner/platform UI, OpenAPI, diagnostics, image upgrades, backup/export,
   teardown, and operator documentation across the relevant repositories.
5. Validate in development/staging; then separately schedule any production
   enablement. Writing this plan authorizes no live migration or image rollout.

Required acceptance coverage:

- Omitted/false options and migrated rows still create/run four-component stacks.
- Existing-stack enablement preserves original DIDs, keys, members, PVCs, URLs,
  and admin installation; only documented maintenance restarts occur.
- Referral and runtime configuration agree; unauthorized writes are refused and
  the existing VTC can publish, query, update, and remove a consented test member.
- Non-consenting members remain unpublished, including during initial backfill.
- Restarts preserve trust records; populated Vault cache supports the documented
  VTA-offline case without a plaintext identity-cache copy.
- Failures/restarts at credential handoff, DID publication, config update, and
  deployment resume without duplicate contexts/DIDs or a stuck stopped VTC/VTA.
- New-stack add-on failure stops completion and displays the Registry error;
  in-place failure remains visible without invalidating the existing parent
  stack. Config restoration and incomplete-referral restoration are accurately reported.
- Upgrades use a single writer; full teardown cleans only this session's registry
  resources/secrets. Capacity and concurrent-operation guards cover add-on work.

Run each changed repository's verification entry points. For this API, `make test`
runs PostgreSQL image parity, `go vet ./...`, and `go test ./...`. Container startup,
transport compatibility, and in-place enablement also need staging verification;
passing Go checks alone cannot establish them.

[image-package]: https://github.com/ic3software/trust-registry-image/pkgs/container/trust-registry
[image-run]: https://github.com/ic3software/trust-registry-image/actions/runs/39370122443
[image-dockerfile]: https://github.com/ic3software/trust-registry-image/blob/edb20bd3559ad12d1ebe7e36046f5eca9824a4b2/Dockerfile
[image-entrypoint]: https://github.com/ic3software/trust-registry-image/blob/edb20bd3559ad12d1ebe7e36046f5eca9824a4b2/entrypoint.sh
[vtc-server]: https://github.com/OpenVTC/verifiable-trust-infrastructure/blob/5b7757980945618f860e3a016eac8167ea475fc4/vtc-service/src/server.rs
[vtc-config]: https://github.com/OpenVTC/verifiable-trust-infrastructure/blob/5b7757980945618f860e3a016eac8167ea475fc4/vtc-service/src/config.rs
[vtc-guide]: https://github.com/OpenVTC/verifiable-trust-infrastructure/blob/5b7757980945618f860e3a016eac8167ea475fc4/docs/03-vtc/trust-registry.md
[vtc-setup]: https://github.com/OpenVTC/verifiable-trust-infrastructure/blob/5b7757980945618f860e3a016eac8167ea475fc4/vtc-service/src/setup/from_toml.rs
[tr-vta]: https://github.com/affinidi/affinidi-trust-registry-rs/blob/c534746480d55fc1e86b40528c5ce7d2fea48184/trust-registry/src/configs/vta.rs
[tr-secrets]: https://github.com/affinidi/affinidi-trust-registry-rs/blob/c534746480d55fc1e86b40528c5ce7d2fea48184/trust-registry/src/configs/secret_store.rs
[tr-loaders]: https://github.com/affinidi/affinidi-trust-registry-rs/blob/c534746480d55fc1e86b40528c5ce7d2fea48184/trust-registry/src/configs/loaders/mod.rs

[tr-headless]: https://github.com/affinidi/affinidi-trust-registry-rs/blob/be33b15/VTA_SETUP.md#headless-setup-automated
[vta-tr-template]: https://github.com/OpenVTC/verifiable-trust-infrastructure/pull/2252
