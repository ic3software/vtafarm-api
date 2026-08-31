# Changelog

## [v0.4.0] - 2026-08-31

### Added

- Admin load-test APIs create batches of 1–50 VTA-only sessions, list and
  inspect runs, check every member's live Kubernetes readiness, and tear the
  complete run down. Runs use the existing platform account, and only one run
  may own active resources at a time.
- Load-test runs survive API restarts in an inspectable and retryable state, and
  the database migration links every member session to its run.

### Changed

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
