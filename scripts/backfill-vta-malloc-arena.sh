#!/bin/bash
set -euo pipefail

# Set MALLOC_ARENA_MAX=2 on every VTA Deployment that does not have it yet.
#
#   KUBE_CONTEXT=<context> ./scripts/backfill-vta-malloc-arena.sh           # dry run — prints the plan
#   KUBE_CONTEXT=<context> ./scripts/backfill-vta-malloc-arena.sh --apply   # do it
#
#   KUBE_CONTEXT      kubectl context to use (required — this restarts VTAs,
#                     so it never falls back to the current context)
#   NS_PREFIX         user namespace prefix          (default: vtafarm-user)
#   ROLLOUT_TIMEOUT   per-VTA rollout wait           (default: 180s)
#
# ── Why this has to exist ────────────────────────────────────────────────────
#
# vtafarm-api only ever *creates* a VTA Deployment from its template —
# AlreadyExists is ignored. VtaTuningEnv() (internal/k8s/vta_resources.go) now
# carries MALLOC_ARENA_MAX=2, which stops glibc keeping freed memory in one
# arena per thread (a VTA sat at 50Mi after a light session without it, 18Mi
# with it). That reaches new VTAs only. Existing ones pick the variable up at
# their next image upgrade, because SetDeploymentImage adds missing tuning
# variables; this script is for getting there without waiting for a release.
#
# What it does, one VTA at a time:
#
#   1. finds Deployments labelled app=vta or app=fs-vta in the user namespaces
#      whose first container has no MALLOC_ARENA_MAX,
#   2. sets it, which restarts the pod (strategy Recreate, ~30–40 s of downtime
#      for that VTA),
#   3. waits for the rollout and stops at the first one that fails.
#
# A value that is already set is left alone, whatever it is. Parked pool VTAs
# (0 replicas) get the variable without a rollout wait.
#
# Run it while no upgrade batch or placement move is in progress: a placement
# rollback restores the pod template it saved moments earlier and would drop
# the variable again for that one VTA.
#
# Safe to re-run.

if [ -z "${KUBE_CONTEXT:-}" ]; then
  echo "KUBE_CONTEXT is required (for example KUBE_CONTEXT=rke2-vtafarm-staging)" >&2
  exit 2
fi
CTX="$KUBE_CONTEXT"
NS_PREFIX="${NS_PREFIX:-vtafarm-user}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-180s}"
APPLY=false
[ "${1:-}" = "--apply" ] && APPLY=true

VAR="MALLOC_ARENA_MAX"
VALUE="2"

k() { kubectl --context "$CTX" "$@"; }

# namespace, name, replicas — one line per VTA Deployment missing the variable.
missing() {
  k get deployments --all-namespaces -l 'app in (vta, fs-vta)' \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.replicas}{"\t"}{range .spec.template.spec.containers[0].env[*]}{.name}{","}{end}{"\n"}{end}' |
    awk -F'\t' -v prefix="$NS_PREFIX-" -v var="$VAR" \
      'index($1, prefix) == 1 && index("," $4, "," var ",") == 0 { print $1 "\t" $2 "\t" $3 }' |
    sort -t "$(printf '\t')" -k1,1 -k2,2V
}

TARGETS="$(missing)"
if [ -z "$TARGETS" ]; then
  echo "[$CTX] every VTA Deployment already has $VAR — nothing to do"
  exit 0
fi
COUNT="$(printf '%s\n' "$TARGETS" | wc -l)"

if ! $APPLY; then
  echo "[$CTX] dry run — $COUNT VTA Deployment(s) would get $VAR=$VALUE and restart:"
  printf '%s\n' "$TARGETS" | awk -F'\t' '{ printf "  %s/%s (replicas %s)\n", $1, $2, $3 }'
  echo "Re-run with --apply to do it."
  exit 0
fi

echo "[$CTX] setting $VAR=$VALUE on $COUNT VTA Deployment(s), one at a time"
DONE=0
while IFS="$(printf '\t')" read -r NS NAME REPLICAS; do
  DONE=$((DONE + 1))
  echo "[$DONE/$COUNT] $NS/$NAME"
  k -n "$NS" set env "deployment/$NAME" "$VAR=$VALUE" >/dev/null
  if [ "${REPLICAS:-0}" = "0" ]; then
    echo "  parked (0 replicas) — variable set, no rollout to wait for"
    continue
  fi
  if ! k -n "$NS" rollout status "deployment/$NAME" --timeout="$ROLLOUT_TIMEOUT" >/dev/null; then
    echo "  rollout of $NS/$NAME did not finish within $ROLLOUT_TIMEOUT — stopping." >&2
    echo "  Inspect it with: kubectl --context $CTX -n $NS describe deployment/$NAME" >&2
    exit 1
  fi
done <<<"$TARGETS"

LEFT="$(missing | wc -l)"
echo "[$CTX] done — $DONE updated, $LEFT VTA Deployment(s) still without $VAR"
