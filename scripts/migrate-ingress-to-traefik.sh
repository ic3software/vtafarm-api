#!/bin/bash
set -euo pipefail

# Move every Ingress this API created off ingress-nginx and onto Traefik.
#
#   ./scripts/migrate-ingress-to-traefik.sh           # dry run — prints the plan
#   ./scripts/migrate-ingress-to-traefik.sh --apply   # do it
#
#   KUBE_CONTEXT   kubectl context to use   (default: the current context)
#
# ── Why this has to exist ────────────────────────────────────────────────────
#
# vtafarm-api only ever *creates* Ingresses — AlreadyExists is ignored and
# nothing updates them afterwards. So every session provisioned before the
# switch keeps `ingressClassName: nginx` forever, Traefik never adopts it, and
# the hostname 404s. Changing the Go constant fixes new sessions only; this
# fixes the ones already out there.
#
# It does three things per user namespace (label managed-by=vtafarm):
#
#   1. repoints every Ingress at the traefik class,
#   2. drops the ingress-nginx annotations, now that the HTTP→HTTPS redirect is
#      an entrypoint setting,
#   3. for namespaces running a dids daemon: creates the strip-forwarded-host
#      Middleware and references it from that daemon's Ingress.
#
# What it deliberately leaves alone: vtafarm-api's own Ingress (Helm-managed —
# `helm upgrade` carries it), and anything not labelled as ours.
#
# Safe to re-run.

CTX="${KUBE_CONTEXT:-$(kubectl config current-context)}"
APPLY=false
[ "${1:-}" = "--apply" ] && APPLY=true

# Keep in step with internal/k8s/traefik.go — the Go side creates this same
# object for every new session, under this same name.
MIDDLEWARE="strip-forwarded-host"
CLASS="traefik"

k() { kubectl --context "$CTX" "$@"; }

# run prints the command, and only executes it with --apply.
run() {
  if $APPLY; then
    printf '  → %s\n' "$*"
    "$@"
  else
    printf '  would run: %s\n' "$*"
  fi
}

echo "context: $CTX"
$APPLY || echo "DRY RUN — nothing will be changed. Re-run with --apply."
echo

# ── Preflight ────────────────────────────────────────────────────────────────
# Both of these are cluster-side prerequisites (k8s/tls/rke2-traefik-config.yaml).
# Failing here is much cheaper than half-migrating and finding out at step 3.
fail=0
if ! k get crd middlewares.traefik.io >/dev/null 2>&1; then
  echo "ERROR: CRD middlewares.traefik.io not found — is Traefik installed with providers.kubernetesCRD enabled?" >&2
  fail=1
fi
if ! k get ingressclass "$CLASS" >/dev/null 2>&1; then
  echo "ERROR: IngressClass '$CLASS' not found — check ingressClass.enabled in the Traefik values." >&2
  fail=1
fi
[ "$fail" -eq 0 ] || exit 1

namespaces=$(k get ns -l managed-by=vtafarm -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
if [ -z "$namespaces" ]; then
  echo "No namespaces labelled managed-by=vtafarm. Nothing to do."
  exit 0
fi

total=0
for ns in $namespaces; do
  ingresses=$(k get ingress -n "$ns" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  [ -n "$ingresses" ] || continue

  echo "namespace $ns"

  # The Middleware first: Traefik fails a router that references one which
  # isn't there yet, so creating it after the annotation would break the daemon
  # for as long as the gap lasts.
  if echo "$ingresses" | grep -q -- '-dids$'; then
    if $APPLY; then
      echo "  → create Middleware $MIDDLEWARE"
      k apply -f - >/dev/null <<EOF
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: $MIDDLEWARE
  namespace: $ns
spec:
  headers:
    customRequestHeaders:
      X-Forwarded-Host: ""
EOF
    else
      echo "  would create Middleware $MIDDLEWARE"
    fi
  fi

  for ing in $ingresses; do
    echo "  ingress $ing"
    run k patch ingress "$ing" -n "$ns" --type=merge \
      -p "{\"spec\":{\"ingressClassName\":\"$CLASS\"}}"

    # Trailing '-' removes; both are no-ops when the annotation is absent, so
    # this stays safe on a re-run and on Ingresses created after the switch.
    run k annotate ingress "$ing" -n "$ns" \
      nginx.ingress.kubernetes.io/ssl-redirect-
    run k annotate ingress "$ing" -n "$ns" \
      kubernetes.io/ingress.class-

    case "$ing" in
      *-dids)
        run k annotate ingress "$ing" -n "$ns" --overwrite \
          "traefik.ingress.kubernetes.io/router.middlewares=${ns}-${MIDDLEWARE}@kubernetescrd"
        ;;
    esac
    total=$((total + 1))
  done
  echo
done

echo "$total ingress(es) across $(echo "$namespaces" | wc -w | tr -d ' ') namespace(s)."

# Anything still on the old class that this script does not own — the portal
# frontend and vtafarm-api's own Ingress both live in `default` and come from
# Helm charts, so they are somebody's `helm upgrade`, not ours. Report them
# rather than patching: silently editing charts' output is how the next deploy
# quietly reverts it. Left behind, they are simply down.
leftovers=$(k get ingress -A \
  -o jsonpath='{range .items[?(@.spec.ingressClassName!="'"$CLASS"'")]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}')
if [ -n "$leftovers" ]; then
  header_shown=false
  echo "$leftovers" | while read -r lns lname; do
    [ -n "$lname" ] || continue
    # Ours are handled above — in a dry run they are still on the old class, so
    # without this every one of them would be listed twice.
    echo "$namespaces" | grep -qx "$lns" && continue
    if ! $header_shown; then
      echo
      echo "Still not on the $CLASS class (not managed by this script):"
      header_shown=true
    fi
    echo "  $lns/$lname"
    echo "      fix in its chart, or right now with:"
    echo "      kubectl --context $CTX patch ingress $lname -n $lns --type=merge -p '{\"spec\":{\"ingressClassName\":\"$CLASS\"}}'"
  done
fi
$APPLY && cat <<'EOF'

Done. Worth checking now — against the node IP, not the hostname: managed
records are proxied through Cloudflare, so curling the hostname reports
Cloudflare's certificate and status, not the cluster's.

  kubectl get ingress -A                     # CLASS column, all traefik. ADDRESS stays empty here — expected
  curl -skI --resolve <dids host>:443:<node IP> https://<dids host>
  kubectl logs deploy/<fs-N-dids> -n <ns>    # the forwarded-host warnings should have stopped
EOF
exit 0
