# Changelog

## [v0.9.4] - 2026-09-29

### Changed

- DID Hosting management uses signed Trust Tasks and verifies signed replies;
  the removed REST management and bearer-token flows are no longer supported.
- DID Hosting enrollment requires the claim code alongside the single-use URL.

## [v0.9.3] - 2026-09-29

### Changed

- New VTA, mediator and DID-hosting workloads request 64 MiB of memory with
  256 MiB limits; VTC workloads request 128 MiB with a 512 MiB limit.
- New VTA, DID-hosting and VTC workloads apply explicit Fjall cache, write
  buffer and journal budgets, including VTA-only sessions.
- Capacity estimates use the same resource profiles as workload provisioning.

## [v0.9.2] - 2026-09-29

### Added

- DID Hosting enrollment responses include the required claim code together
  with the single-use enrollment URL.

## [v0.9.1] - 2026-09-29

### Added

- Failed component image upgrades automatically restore the previous image,
  including recovery of interrupted rollbacks after an API restart.

### Fixed

- Full-stack setup grants the ephemeral VTC setup DID one-time handoff access,
  allowing VTC to replace it with its permanent admin DID.

## [v0.9.0] - 2026-09-27

### Added

- Admin resource endpoints expose desired and live memory settings and apply
  validated single-session or batch changes with sequential readiness checks
  and rollback on failure.

### Changed

- New VTA, VTC and DID-hosting workloads use safer memory defaults for their
  embedded Fjall stores, and capacity estimates account for the higher limits.

### Fixed

- Failed setup sessions retain the stage where they stopped, including
  compatibility inference for sessions created before this release.

## [v0.8.0] - 2026-09-27

### Breaking

- The share-code connection API has been removed, including
  `PUT /api/v1/setup/{id}/sharing`, its admin equivalent and
  `POST /api/v1/setup/connection/validate`. `MAX_STACK_CONNECTIONS` has also
  been removed and no longer has any effect. Clients must inspect and submit
  DID-hosting and mediator DIDs instead.

### Added

- `POST /api/v1/setup/connection/inspect` validates a DID-hosting DID and a
  separately selected mediator DID, then classifies the host as platform,
  in-farm or external. VTA-only creation accepts the same DID pair.
- External DID hosting retains the generated VTA `did.jsonl` for download and
  pauses at `awaiting_did_publication`. Owner-only download and validation
  endpoints resume setup after the published DID history resolves correctly.

### Changed

- In-farm custom connections identify their DID host directly instead of using
  an owner-generated share code.

## [v0.7.1] - 2026-09-24

### Fixed

- Configuration updates can create and remove the temporary Kubernetes Secrets
  used by write and rollback Jobs, so applying a change no longer fails before
  the Job starts.

## [v0.7.0] - 2026-09-23

### Added

- Session owners can read, validate and update the TOML configuration of each
  component in a running agent; admins can do the same for the platform stack.
  VTA-only agents expose only their VTA configuration.
- Applying a change restarts only the selected component and checks its
  readiness. If writing or starting it fails, the API attempts to restore the
  previous configuration and restart the component.

## [v0.6.1] - 2026-09-21

### Changed

- User and admin ACL endpoints now return only unrestricted Super Admin
  entries. Complete synchronized ACL snapshots continue to be retained in the
  database.

## [v0.6.0] - 2026-09-21

### Added

- Session owners can add another PNM administrator to a running VTA, read the
  most recently synchronized ACL and explicitly refresh it from the live VTA.
- Platform admins can inspect and refresh the platform stack's live VTA ACL.
  ACL maintenance is serialized per session, updates a persisted snapshot and
  restarts the VTA after the operation completes.

## [v0.5.0] - 2026-09-18

### Added

- Existing user and admin accounts can link VTA Wallet persona DIDs and use
  SIOPv2 as a second login method. The flow uses durable one-time challenges
  and issues the existing role-specific session cookie after verification.
- SIOP token verification validates Ed25519 signatures and claims against
  authenticated `did:key` or `did:webvh` keys, including `did:webvh` history
  and key rotation.

### Changed

- Accounts with a linked VTA Wallet identity must retain at least one passkey,
  preserving a passkey recovery path.

## [v0.4.0] - 2026-08-31

### Added

- Admin load-test APIs create batches of 1–50 VTA-only sessions, list and
  inspect runs, check every member's live Kubernetes readiness, and tear the
  complete run down. Runs use the existing platform account, and only one run
  may own active resources at a time.
- Load-test runs survive API restarts in an inspectable and retryable state, and
  the database migration links every member session to its run.

### Changed

- Provisioning load tests generate an ephemeral admin DID per run instead of
  requiring an operator-supplied DID.
- VTA-only sessions are marked `running` only after the VTA `/health` readiness
  probe reports a Ready replica.
- Full-stack DID hosting, mediator and VTC deployments now use their HTTP
  readiness endpoints before the provisioning pipeline advances.
- DID-hosting access tokens are cached and authentication is coordinated per
  server, preventing concurrent DID publications from revoking one another. A
  stale-token 401 is re-authenticated and retried once.

## [v0.3.1] - 2026-08-30

### Changed

- Full-stack setups configure mediators with `cors = "any"`, allowing browser
  clients from any origin to connect to them.

## [v0.3.0] - 2026-08-20

### Added

- `GET /setup/{id}/export/configs` and `/setup/{id}/export/logs`, plus the admin
  twins under `/admin/setup-sessions/{id}/`. Each answers a zip holding one
  member per component: its rendered `config.toml`, or its running pod's log
  (last 10000 lines). Read from the pods themselves rather than from anything
  this API stored, so they only answer for what is actually running — a
  component that could not be read is named in an `errors.txt` member, and only
  an empty archive is an error.
- Note that the configs archive carries whatever credentials setup generated
  into those files, the mediator's admin and JWT material in particular. It is
  the same disclosure the portal's admin keys card already makes, in file form.

### Changed

- CORS exposes `Content-Disposition`, so a frontend on another origin can read
  the filename the export routes choose.

## [v0.2.0] - 2026-08-19

### Breaking

- `imagePullSecrets` removed. The images are public, so nothing needs a pull
  secret; a values file still setting it is ignored from this version on.

### Added

- `postgresql.storageClass`. Name a class whose `reclaimPolicy` is `Retain`, so
  a deleted claim does not take the database volume with it. Empty keeps the
  cluster default.

### Changed

- `CORS_ALLOWED_ORIGINS` comes from the environment instead of being pinned in
  the binary, which fixes an install on any domain but ours. The Vite dev ports
  stay allowed unconditionally.
- The wait-for-db initContainer takes `postgresql.image`, pinning it to the same
  exact patch as the database it waits for.

### Removed

- `helm/vtafarm-vault` and `helm/vtafarm-transit`. `vtafarm-k8s` stack 04
  installs both Vaults now, and its `scripts/vault-bootstrap.sh` replaces the
  charts' own bootstrap scripts.
- `scripts/deploy.sh`, whose only caller was the deleted workflow.

## [v0.1.0] - 2026-08-19

First published release. Image and chart on GHCR.
