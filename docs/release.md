# Release

How to publish a version of the API to GHCR.

A release publishes two artifacts and **deploys nothing**:

```text
ghcr.io/ic3software/vtafarm-api:<version>            container image
ghcr.io/ic3software/charts/vtafarm-api:<version>     Helm chart
```

Production is still updated separately with `make deploy`. A release puts a
version on the shelf; something else has to take it off.

## Versioning

`Chart.yaml` holds the only version number:

```yaml
version: 0.1.0
appVersion: "0.1.0"
```

The two are kept equal and one release bumps both. `make release` reads
`version` from this file — nothing else needs editing, and the image tag and the
chart version therefore cannot disagree.

This repo and `vtafarm` version independently. There is no reason for the two to
be on the same number.

While this is 0.x the values structure may change in any release. Anything that
would make an existing `values.yaml` behave differently belongs under
**Breaking** in the changelog. Two things make that especially easy to get wrong
here: `frontendHost` and `cluster.domain` are required with no default, and the
migrations run automatically on startup, so a rollback of the image does not
roll back the schema.

## Prerequisites

Docker Desktop running, and both registries logged in — Docker and Helm keep
separate credentials, so logging into one does not cover the other:

```bash
gh auth token | docker login ghcr.io -u <github-user> --password-stdin
gh auth token | helm registry login ghcr.io -u <github-user> --password-stdin
```

The `gh` token needs the `write:packages` scope:

```bash
gh auth refresh -h github.com -s write:packages -s read:packages
```

## Steps

Run them in this order. Steps 1 and 2 must not have any other commit between
them: the changelog, the version, the tag and the artifacts all have to describe
the same tree.

**1. Write the changelog and bump the version, in one commit.**

```bash
vim CHANGELOG.md                     # add the new version's entry
vim helm/vtafarm-api/Chart.yaml      # version and appVersion
git add CHANGELOG.md helm/vtafarm-api/Chart.yaml
git commit -s -m "chore: release 0.2.0"
```

**2. Tag that commit.**

```bash
git tag -s v0.2.0 -m "vtafarm-api 0.2.0"
```

Signed (`-s`), so GitHub shows the tag as verified rather than unverified. This
repo signs its commits already; `git config tag.gpgsign true` extends that to
tags so a plain `git tag -a` signs too.

The `v` prefix is used everywhere a person reads the version — the git tag, the
GitHub release, the changelog heading. `Chart.yaml` and the artifacts it names
stay bare, which is the Helm and OCI convention.

Tag before building. If the build fails, `git tag -d v0.2.0` and retry; an
untagged successful release is the worse failure, because nothing points at the
commit the artifacts came from.

**3. Build and publish.**

```bash
make release
```

This runs:

```bash
docker buildx build --platform linux/amd64 -t ghcr.io/ic3software/vtafarm-api:0.2.0 --push .
helm package helm/vtafarm-api -d .charts
helm push .charts/vtafarm-api-0.2.0.tgz oci://ghcr.io/ic3software/charts
```

`--platform linux/amd64` is not optional. The cluster nodes are x86; a release
built on an arm64 machine without it produces an image that fails to start with
`exec format error`.

Only `helm/vtafarm-api` is published. `helm/vtafarm-vault` and
`helm/vtafarm-transit` are not part of this release — stack 04 in `vtafarm-k8s`
owns Vault now.

**4. Push the commit and the tag.**

```bash
git push origin main
git push origin v0.2.0
```

`git push origin main` does not carry tags. They need their own push.

**5. Create the GitHub release.**

Pass only the new version's section, not the whole changelog:

```bash
awk '/^## \[v0\.2\.0\]/{f=1;next} f && /^## \[/{exit} f' CHANGELOG.md \
  | gh release create v0.2.0 --notes-file -
```

Only the version in the first pattern changes between releases. `next` drops the
heading — the release page already shows the version and the date — and the
`f &&` guard is what stops it exiting at the newest entry when the one being
released is further down.

Omitting `--title` makes the title the tag name, which is the convention.

## Verifying

```bash
helm show chart oci://ghcr.io/ic3software/charts/vtafarm-api --version 0.2.0
docker manifest inspect ghcr.io/ic3software/vtafarm-api:0.2.0 | grep architecture
```

The architecture must be `amd64`.

`docker manifest inspect` also lists an `unknown/unknown` entry. That is the
build provenance attestation buildx attaches by default, not a broken image —
the kubelet selects `linux/amd64` and ignores it.

## What a consumer still has to supply

The chart refuses to render without `frontendHost` and `cluster.domain`; there
are deliberately no defaults, so nobody inherits our domain by accident. Three
Secrets are expected to exist in the namespace already and are not created by
this chart:

| Secret | Created by |
| --- | --- |
| `vtafarm-api-secrets` | `k8s/secret.yaml.example`, applied by hand |
| `vtafarm-api-postgresql` | `kubectl create secret generic` before the first deploy |
| `vtafarm-api-vault` | `vault-bootstrap.sh farm` in `vtafarm-k8s` |

A missing `vtafarm-api-vault` leaves the pod in `CreateContainerConfigError`
until it appears, which is deliberate: its AppRole `secret_id` must never reach
OpenTofu state, so a human runs that script.

## When something fails partway

The steps are ordered so that a failure leaves nothing published under a version
number that later means something else. Registries treat a pushed tag as
immutable: never re-push a version that already exists, bump instead.

| Failed at | Recovery |
| --- | --- |
| Before `make release`, tag not pushed | `git tag -d v0.2.0`, fix, start again |
| Image pushed, chart failed | Fix and re-run `make release` — the image push is idempotent for the same content |
| Both pushed, then a bug is found | Do not overwrite. Release the fix as the next version |
| Tag already pushed | Leave it. Moving a published tag needs a force push, which this repo does not do |
