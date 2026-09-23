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
version: 0.7.0
appVersion: "0.7.0"
```

The two are kept equal and one release bumps both. `make release` reads
`version` from this file, so the image tag and the chart version cannot
disagree. Add the release notes to `CHANGELOG.md` at the same time.

This repo and `vtafarm` have separate version numbers. Check the frontend
changelog for its required API version before releasing them together.

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

Run them in this order. `main` requires the `✍️ Sign-off` status check, which
runs on pull requests, and this repository uses squash merges. Prepare each
release on a branch, merge it, and tag the resulting commit on `main`. The tag
and the artifacts must describe that same tree.

**1. Create a release branch, then write the changelog and bump the version in
one commit.**

```bash
git switch main
git pull --ff-only
git switch -c chore/release-0.7.0
vim CHANGELOG.md                     # add the new version's entry
vim helm/vtafarm-api/Chart.yaml      # version and appVersion
git add CHANGELOG.md helm/vtafarm-api/Chart.yaml docs/release.md
git commit -s -m "chore: release 0.7.0"
```

**2. Push the branch, open a pull request and wait for its checks.**

```bash
git push -u origin chore/release-0.7.0
```

Open the pushed branch on GitHub, create a pull request targeting `main`, and
wait for the `✍️ Sign-off` check to pass. Merge it on GitHub with **Squash and
merge**. Do not create the version tag on the release branch: squash merging
creates a different commit on `main`.

**3. Update `main` and tag the merged commit.**

```bash
git switch main
git pull --ff-only
git tag -s v0.7.0 -m "vtafarm-api 0.7.0"
```

The tag is signed (`-s`), so GitHub shows it as verified rather than
unverified. This repo signs its commits already; `git config tag.gpgsign true`
extends that to tags so a plain `git tag -a` signs too.

The `v` prefix is used everywhere a person reads the version — the git tag, the
GitHub release, the changelog heading. `Chart.yaml` and the artifacts it names
stay bare, which is the Helm and OCI convention.

Tag before building. If the build fails, `git tag -d v0.7.0` and retry; an
untagged successful release is the worse failure, because nothing points at the
commit the artifacts came from.

**4. Build and publish.**

```bash
make release
```

This runs:

```bash
docker buildx build --platform linux/amd64 -t ghcr.io/ic3software/vtafarm-api:0.7.0 --push .
helm package helm/vtafarm-api -d .charts
helm push .charts/vtafarm-api-0.7.0.tgz oci://ghcr.io/ic3software/charts
```

`--platform linux/amd64` is not optional. The cluster nodes are x86; a release
built on an arm64 machine without it produces an image that fails to start with
`exec format error`.

**5. Push the tag.**

```bash
git push origin v0.7.0
```

The pull request already updated `main`. Git does not include tags with that
update, so the version tag needs its own push.

**6. Create the GitHub release.**

Pass only the new version's section, not the whole changelog:

```bash
awk '/^## \[v0\.7\.0\]/{f=1;next} f && /^## \[/{exit} f' CHANGELOG.md \
  | gh release create v0.7.0 --notes-file -
```

Only the version in the first pattern changes between releases. `next` drops the
heading — the release page already shows the version and the date — and the
`f &&` guard is what stops it exiting at the newest entry when the one being
released is further down.

Omitting `--title` makes the title the tag name, which is the convention.

## Verifying

```bash
helm show chart oci://ghcr.io/ic3software/charts/vtafarm-api --version 0.7.0
docker manifest inspect ghcr.io/ic3software/vtafarm-api:0.7.0 | grep architecture
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
| Before the pull request is merged | Fix the release branch, commit with `-s`, push and wait for the checks again |
| After merge, before `make release`, tag not pushed | `git tag -d v0.7.0`; fix through another pull request, then tag the new `main` commit |
| Image pushed, chart failed | Fix and re-run `make release` — the image push is idempotent for the same content |
| Both pushed, then a bug is found | Do not overwrite. Release the fix as the next version |
| Tag already pushed | Leave it. Moving a published tag needs a force push, which this repo does not do |
