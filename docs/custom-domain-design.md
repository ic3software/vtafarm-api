# Custom Domain Attachment — Design

Lets a **`full_stack_with_vtc`** session run under a domain the user owns
(`vta.aaa.com`, `vtc.aaa.com`, `mediator.aaa.com`, `dids.aaa.com`) instead of a
generated name in the farm's `firstperson.dev` zone.

Two things ride along in the same work:

- The dev-environment marker moves from an infix (`vta-local-<name>`) to a
  **prefix** (`dev-vta-<name>`) — §2.
- A **platform stack** at `vta.firstperson.dev` / `vtc.` / `mediator.` /
  `dids.`, which is the farm's own flagship stack and the mediator + DID host
  that `vta_only` sessions point at — §3.3.

> **Status: specification.** The architecture decisions are settled (§16.1);
> §16.3 lists what is deliberately parked. Nothing is implemented yet.

Companion: [`vtafarm/docs/custom-domain-frontend.md`](../../vtafarm/docs/custom-domain-frontend.md).

---

## 1. Scope

| In scope | Out of scope |
| --- | --- |
| `full_stack_with_vtc` sessions | `vta_only` sessions — §18 |
| A standalone **Domains** page/resource, verified before any session exists | Verification inside the session-create flow (an earlier draft; §5 explains why it moved) |
| CNAME + TXT verification, records created **manually** by the user | Automatic DNS provisioning via the user's registrar API |
| Per-host TLS via cert-manager + Let's Encrypt HTTP-01 | Cloudflare for SaaS — evaluated and deferred, §8.5 |
| The `dev-` rename, and the platform stack | Renaming already-provisioned dev sessions |

> **Clean slate.** Every existing production session will be deleted before
> this ships, so unique indexes can be redefined freely and no live session
> depends on the old `-local-` labels.

---

## 2. Environment prefix rename: `-local-` → `dev-`

```text
production   vta-vincent.firstperson.dev
development  vta-local-vincent.firstperson.dev      ← today
development  dev-vta-vincent.firstperson.dev        ← after this change
```

The marker moves to the front so every record a locally-run API created sorts
and greps together in the Cloudflare dashboard — the whole reason it exists.

```go
// EnvPrefix returns the label prefix marking records created by a locally run
// API: "dev-" in development, "" everywhere else. Prefix (not infix) so every
// dev record sorts together in the DNS zone.
func EnvPrefix(env string) string {
	if env == "development" {
		return "dev-"
	}
	return ""
}

// componentHost builds a component's DNS label. name == "" is the fixed-label
// form used by custom and platform domains (§4).
func componentHost(env, component, name string) string {
	if name == "" {
		return EnvPrefix(env) + component
	}
	return EnvPrefix(env) + component + "-" + name
}
```

`VtaHost`, `FullStackHosts` and `FullStackWithVtcHosts` keep their signatures.

**Compatibility.** No migration needed: the rendered labels live in
`setup_sessions.subdomain` / `mediator_subdomain` / `dids_subdomain` /
`vtc_subdomain`, and every FQDN accessor reads those columns. `maxNameLength`
stays 48 (the longest prefix shrinks from `mediator-local-` to `dev-mediator-`,
so 48 becomes conservative rather than exact — update the comment).
`subdomain_test.go` expectations change, plus new fixed-label cases.

`.env` values still pointing at the old dev session
(`MEDIATOR_DID`, `DID_HOSTING_CONTROL_URL`, `DID_HOSTING_SERVER_URL` →
`dids-local-vincent.firstperson.dev`) keep working until that session is
replaced — and §3.3 removes that fragility for good.

---

## 3. Three domain kinds

Two independent axes were conflated in an earlier draft. Separating them is
what makes the platform stack nearly free:

| | **hostname layout** | **who owns the zone** |
| --- | --- | --- |
| | `vta-<name>.<zone>` (named) · `vta.<zone>` (fixed) | ours (`firstperson.dev`) · theirs (`aaa.com`) |

| | zone = ours | zone = theirs |
| --- | --- | --- |
| **named** | ✅ `managed` — today's per-user sessions | ✗ meaningless |
| **fixed** | ✅ `platform` — the flagship stack | ✅ `custom` |

### 3.1 `managed` — unchanged

`vta-<vta_name>.firstperson.dev` and friends. vtafarm-api creates four
**proxied** Cloudflare A records at session-create time. TLS comes from the
cluster-wide `*.firstperson.dev` wildcard that ingress-nginx serves as its
`default-ssl-certificate`. No Domains record involved, nothing for the user to
do.

### 3.2 `custom` — new

The user owns the zone. vtafarm-api **never touches their DNS**: it prints the
records to create, then verifies them (§6). Four fixed hostnames:

```text
vta.aaa.com        →  VTA REST + DIDComm
vtc.aaa.com        →  VTC REST + admin SPA + public website
mediator.aaa.com   →  DIDComm Mediator
dids.aaa.com       →  WebVH DID Hosting daemon
```

Because the labels are fixed, **one domain backs at most one session**, and the
user-chosen name carries no hostname meaning — it degrades to a display
**label** that only appears in `did:webvh` paths (§4.3).

TLS: one Let's Encrypt certificate covering all four names, via cert-manager
HTTP-01 (§8).

### 3.3 `platform` — new, admin-only

The same fixed-label layout applied to **our own zone**:

```text
vta.firstperson.dev · vtc.firstperson.dev · mediator.firstperson.dev · dids.firstperson.dev
```

This is the farm's flagship stack — the mediator and DID-hosting daemon that
`vta_only` sessions point at. Because the zone is ours:

- **No verification.** We create the four proxied A records ourselves via the
  Cloudflare API, exactly like `managed`.
- **No certificate work at all.** The `*.firstperson.dev` wildcard already
  covers these names. No cert-manager Certificate, no ACME order, and **no
  consumption of any Let's Encrypt quota** (§9).
- **Created by an admin**, never through the normal user flow.

Two consequences worth stating plainly:

1. It makes the shared infrastructure **stable by construction**. Today
   `vta_only` depends on `dids-local-vincent.firstperson.dev` — an ordinary,
   deletable dev session. After this, `DID_HOSTING_SERVER_URL` becomes
   `https://dids.firstperson.dev`, a name guaranteed by the architecture.
2. It exercises the entire fixed-label code path with **none** of the
   verification or TLS machinery, which is why it ships first (§17).

Deleting a platform stack must require an explicit admin confirmation — every
`vta_only` session depends on it.

### 3.4 Why a domain is immutable once a session runs on it

Structural, not policy. The DID-hosting hostname is embedded in every
`did:webvh` the session mints:

```text
did:webvh:<scid>:dids.aaa.com:<label>-vta
did:webvh:<scid>:dids.aaa.com:<label>-mediator
did:webvh:<scid>:dids.aaa.com:<label>-vtc
```

Those identifiers are handed to counterparties and resolved by third parties.
Changing the domain invalidates all of them. The UI must say so before the
session is created.

---

## 4. Hostname derivation

| Kind | Env | VTA | Mediator | DIDs | VTC |
| --- | --- | --- | --- | --- | --- |
| managed | prod | `vta-<name>.firstperson.dev` | `mediator-<name>.…` | `dids-<name>.…` | `vtc-<vtc_name>.…` |
| managed | dev | `dev-vta-<name>.firstperson.dev` | `dev-mediator-<name>.…` | `dev-dids-<name>.…` | `dev-vtc-<vtc_name>.…` |
| custom | prod | `vta.aaa.com` | `mediator.aaa.com` | `dids.aaa.com` | `vtc.aaa.com` |
| custom | dev | `dev-vta.aaa.com` | `dev-mediator.aaa.com` | `dev-dids.aaa.com` | `dev-vtc.aaa.com` |
| platform | prod | `vta.firstperson.dev` | `mediator.firstperson.dev` | `dids.firstperson.dev` | `vtc.firstperson.dev` |
| platform | dev | `dev-vta.firstperson.dev` | `dev-mediator.firstperson.dev` | `dev-dids.firstperson.dev` | `dev-vtc.firstperson.dev` |

```go
// FixedHosts derives the four fixed labels shared by custom and platform
// domains. No user-chosen name — one domain backs one session (§5.2).
func FixedHosts(env string) (vtaSub, mediatorSub, didsSub, vtcSub string) {
	return componentHost(env, "vta", ""),
		componentHost(env, "mediator", ""),
		componentHost(env, "dids", ""),
		componentHost(env, "vtc", "")
}
```

### 4.1 The CNAME target

| Env | Target | Cloud | Record |
| --- | --- | --- | --- |
| production | `lb.firstperson.dev` | **grey (DNS only)** | `A → CLUSTER_INGRESS_IP` |
| development | `dev-lb.firstperson.dev` | **grey (DNS only)** | `A → CLUSTER_INGRESS_IP` |

Grey cloud is required: an orange record resolves to Cloudflare's anycast IPs,
and Cloudflare answers `Host: vta.aaa.com` with **Error 1014 (CNAME Cross-User
Banned)** because that hostname isn't in any zone on our account. Serving it
through Cloudflare would require Cloudflare for SaaS (§8.5).

Derived by default as `EnvPrefix(env) + "lb." + CLUSTER_DOMAIN`, overridable
via `CUSTOM_DOMAIN_CNAME_TARGET` (§12).

**Why a CNAME to a hostname rather than an A record to the IP:** the user's
records are effectively permanent (§3.4), so pointing them at a name we control
keeps the freedom to change the cluster IP — or to move to Cloudflare for SaaS
later by flipping `lb` to orange — **without any user touching their DNS
again**. This is the single most important reversibility property in the
design; an A record to a raw IP forfeits it permanently.

### 4.2 Development and production may attach the same domain

Attaching `aaa.com` on the test instance must not collide with the same domain
attached in production, and both must be able to run at once. That holds, on
three independent levels:

| Level | Development | Production | Isolated by |
| --- | --- | --- | --- |
| Hostnames | `dev-vta.aaa.com` … | `vta.aaa.com` … | `EnvPrefix` (§2) |
| CNAME target | `dev-lb.firstperson.dev` | `lb.firstperson.dev` | §4.1 |
| Database | local Postgres | in-cluster Postgres | separate `domains` tables — `domains_domain_unique` never crosses environments |
| Kubernetes | namespace `fpp-user-<uid>` | namespace `vtafarm-user-<uid>` | `K8S_NAMESPACE_PREFIX` |

The TXT record is the one place both environments meet: they share the name
`_vtafarm-challenge.aaa.com` and each mints its **own token**. That is fine
precisely because §6.3 requires matching *any* TXT value rather than the only
one — the two records coexist, and each environment verifies against its own.

> **Invariant: `K8S_NAMESPACE_PREFIX` must differ between environments.**
> It is `fpp-user` in development and `vtafarm-user` in production today, and
> that difference is the *only* thing keeping cluster resources apart — both
> instances talk to the **same cluster**, and session IDs come from separate
> databases, so dev session 1 and prod session 1 would otherwise both try to
> create `fs-1-vta`, `fs-1-tls` and friends in one namespace. Nothing in the
> code enforces this; if the prefixes are ever aligned, resources collide
> silently. Worth a startup assertion.

Certificates are also isolated: `dev-vta.aaa.com` and `vta.aaa.com` are
different name sets, so development churn never consumes production's
"5 per identical set" budget (§9.3). The per-registered-domain allowance **is**
shared between them, which is one more reason development must use the staging
issuer (§9.4).

### 4.3 Names become a label on fixed-label domains

On the managed zone `vta_name` / `vtc_name` *are* hostnames, so they must be
globally unique. On custom and platform domains they appear in no hostname, and
keep one job: `did:webvh` path components, already made distinct by their
`-vta` / `-mediator` / `-vtc` suffixes and by the hostname itself.

- **One `label` field replaces both name fields.** The handler sets
  `vta_name = vtc_name = label`.
- **Duplicates across users are allowed.**
- Format validation unchanged (`setup.ValidateName`) — it still lands in DID
  paths and URLs.
- The two unique indexes become **partial, managed-only** (§5.4).

Wire fields stay `vta_name` / `vtc_name`; only the UI relabels.

---

## 5. The Domains resource

Domain verification is **decoupled from session creation**. A user verifies a
domain on its own page; only then can a session be created against it.

This is worth the extra resource because it removes an entire state from the
session state machine. An earlier draft parked sessions in `awaiting_dns` while
the user edited DNS — meaning a half-built session held a name reservation,
needed a capacity re-check when it finally started, and had to be garbage
collected if abandoned. With a separate resource, **a session is only ever
created against DNS that is already live**, so it starts provisioning
immediately and its state machine is unchanged from today apart from one TLS
step.

### 5.1 Table `domains`

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `bigserial` PK | |
| `user_id` | `bigint NOT NULL` | owner; for `platform` rows, the admin who created it |
| `domain` | `text NOT NULL` | `aaa.com` — the registrable domain or any subdomain of it |
| `kind` | `varchar(16) NOT NULL` | `custom` \| `platform` |
| `verify_token` | `text NOT NULL DEFAULT ''` | the TXT value; `custom` only |
| `verified_at` | `timestamptz NULL` | `NULL` until the check passes |
| `created_at` / `updated_at` | `timestamptz` | |

### 5.2 Constraints

```sql
CREATE TABLE domains ( … );

ALTER TABLE domains ADD CONSTRAINT domains_kind_check
  CHECK (kind IN ('custom', 'platform'));

-- A domain exists once, globally. Blocks two accounts holding the same name.
CREATE UNIQUE INDEX domains_domain_unique ON domains (domain);

-- One custom domain per account (decision §16.1). Platform rows are exempt —
-- an admin's own account may hold both.
CREATE UNIQUE INDEX domains_one_custom_per_user
  ON domains (user_id) WHERE kind = 'custom';

-- A domain backs at most one live session: the four labels are fixed.
CREATE UNIQUE INDEX setup_sessions_domain_unique
  ON setup_sessions (domain_id) WHERE domain_id IS NOT NULL;
```

`setup_sessions` gains `domain_id bigint NULL REFERENCES domains(id)` — `NULL`
means `managed`. The existing `domain` / `subdomain` / `mediator_subdomain` /
`dids_subdomain` / `vtc_subdomain` columns keep holding the **rendered** values,
so every FQDN accessor works untouched.

`domain_type varchar(16) NOT NULL DEFAULT 'managed'` is also kept on the
session — denormalised from the linked domain's `kind`, because the partial
indexes in §5.4 need it and because it keeps mode dispatch a single column read.

### 5.3 Lifecycle

```text
POST /domains {domain}
   │  row created, verified_at = NULL, verify_token minted
   ▼
pending ─────────────────────────────────────────────┐
   │  user creates 1 TXT + 4 CNAME records            │ check fails →
   │  POST /domains/:id/verify                        │ stays pending,
   ▼                                                  │ per-record detail returned
verified   (verified_at set)  ───────────────────────┘
   │
   │  POST /setup {domain_id}   ← the domain now appears in the create form
   ▼
in_use     (a session references it)
   │
   │  session deleted
   ▼
verified   (selectable again)
```

A `pending` domain holds **no** cluster resources and reserves nothing except
its row. Deleting it is always allowed; deleting a domain that a session
references is a 409.

### 5.4 Migration `000020_domains`

```sql
-- up
CREATE TABLE domains ( … as §5.1 … );
-- indexes and check constraint as §5.2

ALTER TABLE setup_sessions
  ADD COLUMN domain_id   bigint REFERENCES domains(id),
  ADD COLUMN domain_type varchar(16) NOT NULL DEFAULT 'managed';

ALTER TABLE setup_sessions ADD CONSTRAINT setup_sessions_domain_type_check
  CHECK (domain_type IN ('managed', 'custom', 'platform'));

CREATE UNIQUE INDEX setup_sessions_domain_unique
  ON setup_sessions (domain_id) WHERE domain_id IS NOT NULL;

-- vta_name / vtc_name are hostnames only on the managed zone (§4.3).
DROP INDEX IF EXISTS setup_sessions_vta_name_unique;
DROP INDEX IF EXISTS setup_sessions_vtc_name_unique;
CREATE UNIQUE INDEX setup_sessions_vta_name_unique
  ON setup_sessions (vta_name) WHERE domain_type = 'managed';
CREATE UNIQUE INDEX setup_sessions_vtc_name_unique
  ON setup_sessions (vtc_name)
  WHERE domain_type = 'managed' AND mode = 'full_stack_with_vtc';
```

> Verify the two existing index names against the live schema first —
> `internal/handler/setup.go` matches on the strings
> `setup_sessions_vta_name_unique` / `setup_sessions_vtc_name_unique` to turn a
> constraint violation into a 409, and those checks must keep matching.

Every existing row becomes `managed` with `domain_id = NULL`.

---

## 6. Verification

Two records, proving two different things. **Both are required.**

| Record | Proves | Example |
| --- | --- | --- |
| `_vtafarm-challenge.aaa.com TXT` | you **control** the zone | `vtafarm-verify=a3f9c2e8…` |
| 4 × `CNAME → lb.firstperson.dev` | traffic **routes** to us | `vta.aaa.com CNAME lb.firstperson.dev` |

### 6.1 Why the TXT is not redundant

The CNAMEs alone verify *"this hostname points at us"* — **not** *"the person
asking controls this hostname"*. Those come apart in one specific, realistic
case:

> User A verifies `aaa.com`, runs a session, deletes it, and never removes
> their CNAMEs. The domain frees up. User B attaches `aaa.com`, and the check
> passes on A's leftover records. B now serves content — and holds a valid TLS
> certificate — on a hostname they have never had any control over.

That is a textbook subdomain takeover. A **freshly minted, per-attempt** TXT
token closes it: B cannot write into `aaa.com`, and A's stale token doesn't
match the new one.

The alternative considered and rejected was binding `domain → user_id`
permanently in a claims table. It blocks the takeover too, but it records
*"who was first"* as if it were *"who owns it"* — so it goes stale the moment
someone leaves a company, a domain changes hands, or a different colleague
manages the account. Every drift becomes a support ticket whose only real
resolution is "can you edit the DNS?" — which is exactly what the TXT asks
directly. **Control is the ownership signal; verify it live rather than cache
it.**

### 6.2 Token rules

- Minted when the domain row is created; random, bound to `(user_id, domain)`.
- **Does not rotate** while the domain sits `pending` — re-minting on every
  check press would be unusable.
- A **new** token on every fresh attach. Stale tokens never verify, which is
  what makes both the takeover case and the "a colleague now manages this
  domain" case resolve correctly.
- **May be removed after verification.** Checked only at verification time; no
  periodic re-verification, which would break running sessions the first time a
  user tidies their DNS. Say so in the UI.

### 6.3 Accept TXT at either location

Check `_vtafarm-challenge.<domain>` **and** the apex `<domain>`. A handful of
DNS panels can't create underscore-prefixed labels, and accepting both costs
one extra lookup while removing a whole class of support request.

**A name may carry several TXT values.** Match if *any* value equals the
expected token — never require it to be the only one. A domain used for both a
dev and a prod attach will legitimately hold two `_vtafarm-challenge` values,
and apex TXT records routinely hold SPF/DMARC/other vendors' tokens.

### 6.4 The resolver

New package `internal/dnscheck`.

```go
type Result struct {
	FQDN   string
	CNAME  string   // final CNAME target, if the name is an alias
	IPs    []string
	OK     bool
	Detail string   // human-readable reason when !OK
}
```

`net.Resolver{PreferGo: true, Dial: → 1.1.1.1:53}`, falling back to
`8.8.8.8:53`; a host passes if either agrees. Going straight to a public
resolver avoids CoreDNS negative-caching a correction for minutes. 5s timeout,
five lookups (1 TXT + 4 CNAME) in parallel.

Pass criteria for `custom`: each of the four names resolves to **exactly**
`CLUSTER_INGRESS_IP` (following the CNAME chain), and the TXT matches.

> **Managed and platform sessions use different criteria.** Their records are
> **proxied** (`Proxied: true` in `cloudflare.CreateARecord`), so they resolve
> to Cloudflare edge IPs and can never equal `CLUSTER_INGRESS_IP`. For those,
> `dns_wait` only checks that the name resolves at all. A shared "must equal
> the ingress IP" rule would break every managed session — this is the easiest
> thing in the whole design to get wrong.

### 6.5 If the user's domain is also on Cloudflare

The four CNAMEs must be **DNS only (grey cloud)**. A proxied record is served
by *their* zone, so resolution never reaches us and the check fails. Polish
worth adding: recognise Cloudflare's published IPv4 ranges and return
`"points at Cloudflare's proxy — switch the record to DNS only (grey cloud)"`
instead of a generic mismatch.

Everywhere else — Route 53, GoDaddy, Namecheap, Gandi, self-hosted BIND — a
CNAME is a CNAME and there is nothing provider-specific to get wrong.

### 6.6 Negative caching

A user who presses *Verify* before creating the records will have the NXDOMAIN
cached by the public resolvers for the zone's SOA minimum (often 5–60 min). The
UI must set that expectation. Querying the domain's authoritative nameservers
directly would avoid it but needs `miekg/dns` — deferred (§16.2 O3).

---

## 7. Session lifecycle changes

### 7.1 Statuses

Only one status is added. `awaiting_dns` from the earlier draft is **gone** —
§5 explains why.

| Status | Applies to | Meaning |
| --- | --- | --- |
| `dns_wait` | all | Orchestrator's first step: confirm the hostnames resolve before spending cluster resources. |
| `tls_provision` | `custom` only | Waiting for cert-manager to issue the session's certificate. Skipped for `managed` and `platform` — the wildcard already covers them. |

### 7.2 Orchestrator

```go
// dns_wait — before env_provision
o.fsSetStatus(sessionID, "dns_wait")
if fail("DNS not resolving", o.fsWaitDNS(ctx, s)) { return }

// … env_provision, k8s_provision unchanged …

// tls_provision — after k8s_provision, before step_vta_setup
if s.DomainType == model.DomainCustom {
	o.fsSetStatus(sessionID, "tls_provision")
	if fail("TLS certificate issuance failed", o.fsWaitCert(ctx, ns, s)) { return }
}
```

`tls_provision` sits after `k8s_provision` because the Ingresses must exist
first, and before `step_vta_setup` because from `step_vta_register_dids` onward
the components resolve each other's `did:webvh` identifiers over HTTPS — an
untrusted certificate there fails the pipeline somewhere much harder to
diagnose.

---

## 8. TLS

**Decision: traffic goes directly to the origin, and we issue the certificates
ourselves with cert-manager + Let's Encrypt HTTP-01.**

### 8.1 One certificate, four names

**The default path and why we don't take it.** cert-manager's *ingress-shim*
watches Ingresses: put `cert-manager.io/cluster-issuer` in the annotations and
a `tls:` block in the spec, and it creates a `Certificate` for you. With four
Ingresses per session that means four Certificates, four ACME orders and four
Secrets — for four hostnames in one zone that will always be requested
together.

Instead vtafarm-api creates **one `Certificate` listing all four names**, and
the four Ingresses reference its single Secret.

**Step 1 — the Certificate.** The desired object:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: fs-{sid}-tls
  namespace: vtafarm-user-{uid}
spec:
  secretName: fs-{sid}-tls
  issuerRef:
    name: letsencrypt-http01        # ACME_CLUSTER_ISSUER
    kind: ClusterIssuer
  dnsNames:
    - vta.aaa.com
    - vtc.aaa.com
    - mediator.aaa.com
    - dids.aaa.com
```

Created through client-go's **dynamic** client, so we never import the
cert-manager Go module. `k8s.Client` already carries one (`c.dyn`, added for
the Longhorn CRDs behind the admin dashboard), so there is no new plumbing —
just a new GVR:

```go
// internal/k8s/certificates.go
var certificateGVR = schema.GroupVersionResource{
	Group: "cert-manager.io", Version: "v1", Resource: "certificates",
}

// CreateSessionCert creates one Certificate covering every hostname the
// session serves. Idempotent — AlreadyExists is ignored, matching the other
// Create* helpers.
func (c *Client) CreateSessionCert(ctx context.Context, ns, name, issuer string, hosts []string) error

// CertReady reports status.conditions[type=Ready]; the message is surfaced in
// the session's error when issuance times out.
func (c *Client) CertReady(ctx context.Context, ns, name string) (ready bool, message string, err error)

// DeleteSessionCert removes the Certificate. The Secret is deliberately left
// behind — see §9.3 and §11.
func (c *Client) DeleteSessionCert(ctx context.Context, ns, name string) error
```

**Step 2 — the Ingresses.** Each of the four gets a `tls:` block naming the
**same** shared Secret, and **no cert-manager annotation**:

```yaml
spec:
  tls:
  - hosts: ["vta.aaa.com"]        # just this Ingress's own host
    secretName: fs-{sid}-tls      # the shared Secret, identical on all four
```

> **The annotation must be absent.** If `cert-manager.io/cluster-issuer` is
> left on an Ingress *and* we create our own Certificate, ingress-shim creates
> a second Certificate pointing at the same `secretName`. Two Certificates
> owning one Secret will overwrite each other and re-issue in a loop — which
> burns ACME quota continuously rather than once. This is the single easiest
> way to get this wrong.

**Step 3 — waiting.** `tls_provision` polls `CertReady` on that one object
(§8.4) instead of four.

**What it actually buys.** Being precise, because it is easy to oversell:

| | 4 Certificates | 1 Certificate, 4 SANs |
| --- | --- | --- |
| ACME orders per session | 4 | **1** |
| Charged to "50 certs / registered domain / week" | 4 | **1** |
| → session recreations before *that* limit | ~12 / week | **~50 / week** |
| HTTP-01 challenges | 4 | 4 — unchanged, one per name |
| "5 per identical name set / week" | 5 per name | 5 for the set |
| → recreations before *that* limit | 5 | **5 — unchanged** |
| K8s Secrets per session | 4 | 1 |
| Readiness conditions to poll | 4 | 1 |

So it does **not** relax the limit that actually binds (§9.3 — five
recreations per week either way). What it does is take the per-registered-domain
allowance from "comfortable" to "irrelevant", cut the moving parts by four, and
make failure diagnosis a single object lookup.

### 8.2 ClusterIssuer — `k8s/tls/clusterissuer-http01.yaml`

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-http01
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: admin@firstperson.dev          # ours, not the customer's — §9.5
    privateKeySecretRef:
      name: letsencrypt-http01
    solvers:
    - http01:
        ingress:
          class: nginx
```

Ship a `letsencrypt-http01-staging` twin pointed at
`https://acme-staging-v02.api.letsencrypt.org/directory`, selected by
`ACME_CLUSTER_ISSUER`. §9.4 makes using it in development mandatory, not
optional.

HTTP-01 works precisely because the user already pointed the hostname at our
ingress — that record *is* the proof of control, and `ssl-redirect: "true"`
doesn't interfere because cert-manager's solver Ingress claims the more
specific `/.well-known/acme-challenge/` path.

### 8.3 Ingress changes

`k8s.CreateComponentIngress` takes seven positional arguments today; replace
them with a struct and add the TLS fields:

```go
type ComponentIngressSpec struct {
	Namespace, Name, ServiceName, Host string
	Port                               int32
	TLSSecret                          string // "" → cluster wildcard (managed/platform)
}
```

`CreateVtaIngress` (the `vta_only` path) is untouched.

### 8.4 Waiting for issuance

Poll the `cert-manager.io/v1 Certificate` for
`status.conditions[type=Ready].status == "True"` via client-go's dynamic client
(already vendored). Budget 5 minutes, 10s interval. Reading the Secret instead
would need cluster-wide `secrets: get` — a far larger grant for no benefit.

ClusterRole additions (keep `CLAUDE.md` and
`helm/vtafarm-api/templates/vtafarm-api/clusterrole.yaml` in sync):

```yaml
- apiGroups: ["cert-manager.io"]
  resources: ["certificates"]
  verbs: ["get", "list", "watch", "create", "delete"]
```

On timeout, fail with something actionable — the cause is almost always the
user's DNS having changed, or a CAA record on their zone forbidding Let's
Encrypt:

```text
TLS certificate issuance timed out for vta.aaa.com. Check that the record still
points at 157.180.68.139 (DNS only, not proxied) and that any CAA record on
aaa.com allows letsencrypt.org.
```

### 8.5 What was considered and rejected

| Option | Verdict |
| --- | --- |
| **Cloudflare for SaaS** — users CNAME to a proxied `lb`, Cloudflare issues and renews the edge certificate | **Deferred.** Free at this scale (100 custom hostnames included on every plan, then $0.10/mo each) and it would hide the origin IP. But origin-side TLS is unresolved: with a fallback origin, Cloudflare sends SNI = the custom hostname, our nginx answers with the wildcard, and Full (strict) rejects it. Fixing that means either downgrading the whole zone to Full, or running cert-manager **as well** — i.e. this option plus all of §8. Parked in §16.3; §4.1 keeps the migration path free. |
| **LE DNS-01 with `_acme-challenge` delegation** | Rejected — 4 more records for the user, and its benefits (no port-80 dependency, wildcard support) are ones we don't need. |
| **LE DNS-01 with the user's DNS API credentials** | Rejected — a large trust ask, and provider-specific. |
| **User-supplied certificates** | Rejected — manual renewal every 90 days. |
| **Caddy on-demand TLS** | Rejected — the slickest answer for arbitrary hostnames, but it means replacing RKE2's bundled ingress-nginx. |

### 8.6 On the origin IP

Worth recording plainly, because it shaped this decision. The origin IP is
**already public**, and Cloudflare is **already bypassable**. Measured directly:

```text
dig vtafarm.firstperson.dev            → 172.67.139.142, 104.21.94.190   (Cloudflare)
openssl s_client 157.180.68.139:443    → CN=*.firstperson.dev
curl --resolve dids-…:443:157.180.68.139  → HTTP 200   (identical to via-Cloudflare)
```

ingress-nginx serves the wildcard certificate to *any* direct connection on
:443, so internet-wide scanners (Censys, Shodan) already index the
IP ↔ domain link. Choosing direct-to-origin therefore gives up nothing that is
currently held. The real control would be firewalling the origin to
Cloudflare's ranges — a separate decision, and one that is incompatible with
this architecture, and parked (§16.3 O1).

---

## 9. Let's Encrypt limits

### 9.1 The limits, and who each one is charged to

| Limit | Value | **Charged to** | Overridable |
| --- | --- | --- | --- |
| New orders per account | 300 / 3 h | **our ACME account** | yes, by request |
| **New certificates per registered domain** | **50 / 7 days** | **the customer's domain** | yes, by request |
| Certificates per exact set of identifiers | 5 / 7 days | that hostname set | no |
| Authorization failures per identifier per account | 5 / hour | our account × that hostname | no |
| ARI-driven renewals | **exempt from all limits** | — | — |

### 9.2 What the 50/week limit actually means

**It is not a cap on our cluster or our account.** `50 certificates per
registered domain per 7 days` is charged to **the customer's registered
domain** — `aaa.com` — and every customer domain carries its own independent
allowance. A thousand customers is a thousand separate 50-per-week budgets;
the number **does not aggregate as the user base grows**, and there is no
ceiling anywhere that says "this cluster may issue 50 certificates".

Two details that follow from how the limit is scoped:

- **"Registered domain" means the eTLD+1.** All four of our hostnames
  (`vta.` / `vtc.` / `mediator.` / `dids.aaa.com`) count against the single
  `aaa.com` bucket — which is another reason to request them as one
  certificate rather than four (§8.1).
- **The counter is global across every ACME account, not just ours.** If the
  customer already issues Let's Encrypt certificates for `aaa.com` elsewhere —
  their marketing site, a mail host, a staging environment — those consume the
  same 50. Rarely a problem at 1 certificate per session, but it is the reason
  a failure here may have nothing to do with us, and the error message should
  not assert that it does.

With one certificate per session (§8.1), a single customer could create and
destroy ~50 sessions a week on their domain before this one binds. It is not
the constraint that matters in practice.

Our only shared limit is **300 new orders per 3 hours**, which at one order per
session means 300 new custom-domain sessions per 3 hours. It is also the one
Let's Encrypt will raise on request (there is a form for hosting providers;
approval takes weeks). Neither is close to binding.

And because **ARI-driven renewals are exempt from every limit**, steady-state
operation consumes no budget at all. Enable ARI in cert-manager.

### 9.3 The limit that does bind: 5 per identical name set

This is the one to design around, and it is a direct consequence of the fixed
labels. Deleting a session and recreating it on the same domain requests a
certificate for the **exact same four names** every time — which is precisely
what the "5 certificates per exact set of identifiers per 7 days" limit counts.

> **A user can delete and recreate a session on the same domain about five
> times per week before issuance starts failing.** Refill is one per 34 hours.
> This limit cannot be raised.

Mitigations, in order of preference:

1. **Don't delete the TLS Secret when the session is torn down.** If the user's
   namespace survives (they still have other sessions), a recreation finds a
   valid certificate for exactly those names and cert-manager reuses it — zero
   ACME traffic. This alone covers most of the churn.
2. **Staging issuer for all development and testing** (§9.4).
3. If it ever becomes a real limit, issue the certificate **at the domain
   level** rather than per session: one long-lived Certificate tied to the
   `domains` row, reflected into the user namespace, surviving every session
   recreation. More machinery; only worth it if measured.

### 9.4 Two operational rules

**All development and testing uses the staging issuer.** Staging's limits are
orders of magnitude higher and it never touches the production budget. This is
the one rule that causes real damage if ignored — reattaching a misconfigured
domain a few times in an hour will burn through the 5-failures-per-hour
allowance on a production hostname.

**`tls_provision` must not retry aggressively.** Once the 5-failures-per-hour
limit is hit the hostname is locked out for a while, and retrying makes it
worse. Fail the session with an actionable message and let the user fix DNS.

The `domains` gate helps here structurally: because DNS is verified before a
session can exist, we almost never request a certificate for a hostname that
isn't already resolving correctly — which keeps us far away from the failure
limit in normal operation.

### 9.5 We do not need the customer's email address

- **Issuing certificates for domains we don't own is the standard hosting
  model.** ACME validates *control of the name*, not *ownership*: the customer
  pointing DNS at our ingress **is** the authorization, and nobody without that
  record can obtain the certificate. Netlify, Vercel, Heroku, Fly.io, GitHub
  Pages and Shopify all work exactly this way; Let's Encrypt's rate-limit
  override form for hosting providers exists because this is expected usage.
  The Subscriber Agreement is between **us** and Let's Encrypt — we are the
  subscriber, the customer is not.
- **The ACME account email is ours**, one per platform. Putting a customer's
  address there would misrepresent who operates the certificate.
- **It would achieve nothing anyway.** Let's Encrypt stopped sending expiry
  notification emails on **4 June 2025**, citing the privacy cost of retaining
  millions of addresses tied to issuance records.
- Expiry monitoring belongs in **our** infrastructure: surface Certificate
  readiness and `notAfter` through the existing `/api/v1/monitor/*` endpoints
  that UptimeRobot already polls.

### 9.6 Platform and managed domains cost nothing

Neither consumes any Let's Encrypt quota: both are covered by the existing
`*.firstperson.dev` wildcard, which is issued once via DNS-01 and renewed
automatically. Only `custom` domains reach ACME at all.

---

## 10. API surface

### 10.1 Domains

| Method | Path | Role | Description |
| --- | --- | --- | --- |
| `GET` | `/api/v1/domains` | user | The caller's domains (at most one `custom`), each with `verified`, `in_use`, and the records to create |
| `POST` | `/api/v1/domains` | user | Attach a domain → row + `verify_token` + the 5 records. 409 if the caller already has one |
| `GET` | `/api/v1/domains/:id` | user | Detail + last known per-record state |
| `POST` | `/api/v1/domains/:id/verify` | user | Run the check; on success set `verified_at`. Rate-limited |
| `DELETE` | `/api/v1/domains/:id` | user | 409 while a session references it |
| `POST` | `/api/v1/admin/domains` | admin | Create a `platform` domain — verified immediately, no checks (§3.3) |

`POST /domains/:id/verify` response:

```jsonc
{
  "domain": "aaa.com",
  "verified": false,
  "verified_at": null,
  "target": "lb.firstperson.dev",
  "txt": {
    "name": "_vtafarm-challenge.aaa.com",
    "expected": "vtafarm-verify=a3f9c2e8…",
    "found": ["vtafarm-verify=00000000…"],
    "ok": false,
    "detail": "no TXT value matches — an older token is still present"
  },
  "records": [
    { "component": "vta", "fqdn": "vta.aaa.com", "expected_type": "CNAME",
      "expected_value": "lb.firstperson.dev", "resolved": ["157.180.68.139"],
      "ok": true },
    { "component": "vtc", "fqdn": "vtc.aaa.com", "resolved": [], "ok": false,
      "detail": "no record found" }
    // mediator, dids …
  ]
}
```

A failing check returns **200**, not 4xx — it is an expected, retryable state
the UI renders, not a client error.

### 10.2 Setup

- `POST /api/v1/setup` gains `domain_id` (optional; omitted → `managed`) and
  `label` (fixed-label domains only, replacing `vta_name`/`vtc_name`, §4.3).
  Rejected if the domain isn't verified, isn't the caller's, or already backs a
  session.
- `GET /api/v1/setup` and `/setup/:id` gain `domain_type` and `domain`.
- `GET /api/v1/setup/domain-info` — `{managed_domain, env_prefix, target_ip,
  target_host}`, so the portal stops hardcoding `vta-<name>.firstperson.dev` in
  its hints (wrong in four of the six §4 combinations).
- `GET /admin/setup-sessions` gains `domain_type`.

**Every new route must be documented in `internal/apidocs/openapi.yaml`** under
the `User` (or `Admin`) tag — repo rule.

---

## 11. Teardown

```go
switch session.DomainType {
case model.DomainManaged, model.DomainPlatform:
	// existing: delete the four Cloudflare records by stored id
case model.DomainCustom:
	// nothing — we never created the user's records. The UI tells them to
	// remove the four CNAMEs themselves; a record left pointing at us is a
	// dangling-DNS liability (§6.1).
}
```

Deleting a session **frees its domain** (`setup_sessions_domain_unique` is
released), so the same user can immediately create a new session on it — see
§9.3 for the certificate-churn consequence.

**Do not delete the TLS Secret** with the session (§9.3 mitigation 1).
Namespace deletion cleans it up when the user's last session goes.

Deleting a `platform` stack requires explicit admin confirmation — every
`vta_only` session depends on its mediator and DID host.

---

## 12. Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `CUSTOM_DOMAIN_ENABLED` | `false` | Kill switch; ships dark until the phase-0 prerequisites are in place |
| `CUSTOM_DOMAIN_CNAME_TARGET` | derived: `{envPrefix}lb.{CLUSTER_DOMAIN}` | Explicit override for the CNAME target |
| `ACME_CLUSTER_ISSUER` | `letsencrypt-http01` | **Set to `letsencrypt-http01-staging` in development** (§9.4) |

Existing `CLUSTER_INGRESS_IP`, `CLUSTER_DOMAIN` and `APP_ENV` keep their
meanings. `.env.example` and the `CLAUDE.md` env table need the new rows.

**Cluster / DNS prerequisites (one-off, manual):**

- `lb.firstperson.dev` → `A` → `CLUSTER_INGRESS_IP`, **grey cloud**
- `dev-lb.firstperson.dev` → `A` → `CLUSTER_INGRESS_IP`, **grey cloud**
- Apply both ClusterIssuers
- Enable ARI in cert-manager (§9.2)

---

## 13. Validation and errors

Normalise first: lowercase, strip any `https://` scheme and path, strip a
trailing dot, IDN → punycode via `golang.org/x/net/idna` (currently indirect;
promoting it is the only go.mod change).

| Condition | HTTP | Message |
| --- | --- | --- |
| Fewer than two labels, or an invalid DNS label | 400 | `invalid domain` |
| A `firstperson.dev` name, from a non-admin | 400 | `firstperson.dev is managed by VTA Farm — choose the managed option` |
| IP literal, `localhost`, or a `.local`/`.internal`/`.test`/`.invalid`/`.example` name | 400 | `this domain can't be issued a public TLS certificate` |
| Any of the four FQDNs would exceed 253 chars | 400 | `domain too long` |
| The caller already has a custom domain | 409 | `only one custom domain per account` |
| The domain exists on another account | 409 | `this domain is already attached to another account` |
| `domain_id` given but not verified | 409 | `verify this domain before creating a session` |
| `domain_id` already backs a session | 409 | `this domain is already in use by another session` |
| `label` sent with a managed session, or `vta_name` with a fixed-label one | 400 | mutually exclusive |

Public-suffix inputs (`co.uk`, `github.io`) are not specially rejected — doing
it properly needs the PSL, and verification is a sufficient backstop since
nobody can write a TXT record into a public suffix they don't control.

---

## 14. Security

- **Proof of control is the TXT record**, checked live at attach time (§6.1).
  No stored ownership claim, deliberately.
- **Squatting is impossible.** An unverified domain reserves nothing beyond its
  row, and `domains_domain_unique` only becomes meaningful once someone proves
  control. A malicious attach of `victim.com` never verifies, so it never
  blocks the real owner from a session — though the row does hold the name, so
  an admin needs a way to release it.
- **Dangling-DNS takeover is closed** by the per-attempt token (§6.2).
- **Domain currently in use by another account's live session** → 409 with a
  clear message; an admin can release it. Narrow by construction: it requires
  the other session to still be running, not merely to have existed.
- **SSRF.** The checker resolves names but never fetches them. If an HTTP
  reachability probe is ever added, it must refuse private and loopback ranges.
- **Hostname trust.** All four hostnames are derived server-side from the
  verified domain; no user-supplied label reaches an Ingress `host` field.

---

## 15. Files touched

| File | Change |
| --- | --- |
| `internal/setup/subdomain.go` (+ test) | `EnvPrefix`, rewritten `componentHost`, `FixedHosts` |
| `internal/model/domain.go` | new — `Domain` model |
| `internal/model/setup_session.go` | `DomainID`, `DomainType`, constants, helpers |
| `migrations/000020_domains.{up,down}.sql` | new |
| `internal/dnscheck/checker.go` (+ test) | new — TXT + CNAME resolution (§6.4) |
| `internal/handler/domain.go` | new — the six routes in §10.1 |
| `internal/handler/setup.go` | `domain_id` / `label` binding and validation |
| `internal/handler/setup_fullstack_vtc.go` | fixed-label branch of create |
| `internal/handler/setup_fullstack.go` | teardown branch; new response fields |
| `internal/setup/orchestrator_fullstack.go` | `dns_wait`, `tls_provision` |
| `internal/k8s/component_resources.go` | `ComponentIngressSpec` + `tls:` block |
| `internal/k8s/certificates.go` | new — create/poll/delete the session Certificate |
| `internal/k8s/fullstack_names.go` | `FSTLSSecret` |
| `internal/config/config.go` | three new vars (§12) |
| `internal/router/router.go` | new routes |
| `internal/apidocs/openapi.yaml` | document all of them |
| `helm/vtafarm-api/templates/.../clusterrole.yaml` | `cert-manager.io/certificates` |
| `k8s/tls/clusterissuer-http01.yaml` | new (+ staging twin) |
| `CLAUDE.md`, `.env.example` | env table, routes, structure |

---

## 16. Decisions

### 16.1 Settled

| Decision | |
| --- | --- |
| **Traffic path** | Direct to origin. Users CNAME to a **grey-cloud** `lb.firstperson.dev` (`dev-lb.firstperson.dev` in development). |
| **Certificates** | cert-manager + Let's Encrypt HTTP-01, one certificate covering all four names. |
| **Verification** | TXT (control) + 4 CNAME (routing). Both required. Fresh token per attach; removable after verification. No ownership claims table. |
| **Domains are a separate resource** | Verified on their own page before any session exists. `awaiting_dns` is gone from the session state machine. |
| **One custom domain per account** | And one live session per domain. |
| **`firstperson.dev` is a `platform` domain** | Admin-only, no verification, no ACME, no quota. |
| **Names** | Fixed-label domains take a single `label`; duplicates across users allowed; unique indexes become managed-only. |
| **Legacy sessions** | All deleted before rollout — no compatibility work owed. |

### 16.2 Open

| # | Item | Notes |
| --- | --- | --- |
| **O3** | Query authoritative nameservers instead of public resolvers? | Removes the negative-caching false failures in §6.6, at the cost of a `miekg/dns` dependency. Revisit if users actually hit it. |

### 16.3 Explicitly deferred — do not revisit during implementation

| # | Item | Why it's parked |
| --- | --- | --- |
| **O1** | **Firewalling the origin to Cloudflare's ranges** | The only real defence against the bypass measured in §8.6, and **incompatible with this architecture** — custom-domain traffic arrives from arbitrary clients. **Decision: not now.** The origin stays open. Revisit only if there is real traffic and a real threat; §4.1's CNAME target means moving to Cloudflare for SaaS later needs no user to touch their DNS. |
| **O2** | Cloudflare for SaaS, and whether Full (strict) survives a fallback origin | Only relevant if O1 is ever revisited. Parked with it. |

---

## 17. Implementation order

| Phase | Contents | Ships independently |
| --- | --- | --- |
| **0** | `lb` + `dev-lb` grey-cloud records; both ClusterIssuers; ARI enabled | yes |
| **1** | `dev-` rename + tests + `GET /setup/domain-info` + frontend hint fix | yes — small, no schema change |
| **2** | `domains` table, `FixedHosts`, single-`label` handling, **`platform` domain end to end** | **yes — and this is the milestone that matters** |
| **3** | `internal/dnscheck`, the Domains routes, verification | yes |
| **4** | Certificate creation, `tls_provision`, RBAC, teardown branch | completes the backend |
| **5** | Frontend (companion doc) | needs 1–4 |
| **6** | Polish: Cloudflare-proxy detection, admin domain release | optional |

**Phase 2 is the one to aim for first.** Standing up the platform stack at
`vta.firstperson.dev` exercises the entire fixed-label code path — new
hostname derivation, the `label` field, the `domains` table, session linkage —
with **no verification and no certificate work at all**, because it lives in
our own zone under the existing wildcard. It also replaces the farm's
dependency on a disposable dev session with a stable hostname (§3.3). By the
time phases 3 and 4 add verification and ACME, the risky half of the feature is
already running in production.

---

## 18. Out of scope / future

- **`vta_only` custom domains** — the same machinery with a single host. The
  mode points at a shared mediator and DID host, so a user's domain would cover
  only part of their footprint. Reuses §6 and §8 unchanged if wanted later.
- **Apex domains** (`aaa.com` itself) — the four components need four distinct
  hostnames.
- **More than one custom domain per account**, and more than one live session
  per domain.
- **Automatic DNS provisioning** for users who delegate a scoped API token.
- **Domain migration for a running session** — deliberately impossible (§3.4).
- **Wildcard certificates on custom domains** — needs DNS-01, hence the user's
  DNS credentials.
