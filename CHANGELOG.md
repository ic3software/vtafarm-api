# Changelog

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
