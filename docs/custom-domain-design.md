# Custom Domain Attachment — Design

Lets a **`full_stack`** session run under a domain the user owns
(`vta.aaa.com`, `vtc.aaa.com`, `mediator.aaa.com`, `dids.aaa.com`) instead of a
generated name in the farm's `firstperson.dev` zone.

Two things ride along in the same work:

- The dev-environment marker moves from an infix (`vta-local-<name>`) to a
  **prefix** (`dev-vta-<name>`) — §2.
- A **platform stack** at `vta.firstperson.dev` / `vtc.` / `mediator.` /
  `dids.`, which is the farm's own flagship stack and the mediator + DID host
  that `vta_only` sessions point at — §3.3.

> **Status: phases 1–3 shipped; phase 4 (TLS) is specification.** The
> architecture decisions are settled (§16.1); §16.3 lists what is deliberately
> parked. §17 tracks what is built.

Companion: [`vtafarm/docs/custom-domain-frontend.md`](../../vtafarm/docs/custom-domain-frontend.md).

---

## 1. Scope

| In scope | Out of scope |
| --- | --- |
| `full_stack` sessions | `vta_only` sessions — §18 |
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

`VtaHost` and `FullStackHosts` keep their signatures, so neither call site
(`handler/setup.go`, `handler/setup_fullstack.go`) changes.

**Compatibility.** No migration needed: the rendered labels live in
`setup_sessions.subdomain` / `mediator_subdomain` / `dids_subdomain` /
`vtc_subdomain`, and every FQDN accessor reads those columns. `maxNameLength`
stays 48 (the longest prefix shrinks from `mediator-local-` to `dev-mediator-`,
so 48 becomes conservative rather than exact — update the comment).
`subdomain_test.go` expectations change, plus new fixed-label cases.

> **Shipped 2026-07-26** (`faff4af`), together with `GET /setup/domain-info`
> (§10.2). Existing dev sessions keep their old `-local-` labels, exactly as
> the compatibility note predicts.

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
cluster-wide `*.firstperson.dev` wildcard that Traefik serves as its default
certificate (the `default` TLSStore). No Domains record involved, nothing for
the user to do.

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
- **Created by an admin on a dedicated page**, never through the user flow.

Two consequences worth stating plainly:

1. It makes the shared infrastructure **stable by construction**. Today
   `vta_only` depends on `dids-local-vincent.firstperson.dev` — an ordinary,
   deletable dev session. After this, `DID_HOSTING_SERVER_URL` becomes
   `https://dids.firstperson.dev`, a name guaranteed by the architecture.
2. It exercises the entire fixed-label code path with **none** of the
   verification or TLS machinery, which is why it ships first (§17).

#### 3.3.1 The reservation is absolute

`CLUSTER_DOMAIN` **and every subdomain of it** are rejected by the user-facing
`POST /api/v1/domains` for **every caller, including admins** (§13). There is
no role check to get wrong and no "admins may also attach it as custom" path:
the *only* way a `firstperson.dev` row comes into existence is
`POST /api/v1/admin/platform-stack`, which always writes `kind = 'platform'`.

Enforcing it at the route rather than by role matters because the two paths
produce different objects — a `custom` row would drag in TXT verification, an
ACME certificate, and per-user ownership, none of which apply to a zone we
already control.

#### 3.3.2 One action creates the whole stack

The admin page creates the domain **and** the session together. Splitting them
("first go make a domain, then go make an agent") would expose a two-step
ceremony for something that only ever happens once per environment:

```text
POST /api/v1/admin/platform-stack { label, vta_image, mediator_image, dids_image, vtc_image }
   ├─ get-or-create the system account (§3.3.6) → its user id owns everything below
   ├─ upsert the firstperson.dev domain row (kind=platform, verified_at=now)
   ├─ create the four proxied Cloudflare A records
   └─ create the full_stack session against it → orchestrator starts
```

- `label` defaults to `firstperson`; it never appears in a hostname and only
  reaches the DID paths — `did:webvh:<scid>:dids.firstperson.dev:firstperson-vta`
  and friends (§4.3).
- **`beta_access` does not apply** — that gate exists for users, and the caller
  here is an admin.
- **Cluster capacity still applies.** The platform stack consumes the same
  resources as any other full stack; if the cluster genuinely cannot fit it,
  that is information the admin needs, not something to override silently.
- Exactly one platform stack per environment, enforced by the same
  `setup_sessions_domain_unique` index every other domain uses (§5.2).

#### 3.3.3 Development and production each get their own

Both environments can run a platform stack simultaneously, and they do not
collide — for the same reasons as §4.2, applied to `firstperson.dev` itself:

```text
production   vta.firstperson.dev      mediator.firstperson.dev      …
development  dev-vta.firstperson.dev  dev-mediator.firstperson.dev  …
```

The `domains` rows both say `firstperson.dev`, but they live in **separate
databases**, so `domains_domain_unique` never sees them together. The DNS
records differ by the `dev-` prefix, and the Kubernetes resources by
`K8S_NAMESPACE_PREFIX`.

#### 3.3.4 Handing its values back to configuration

`MEDIATOR_DID` can only be known *after* the platform stack finishes setup — it
is minted by the pipeline. So the admin page must surface the collected values
(`mediator_did`, `did_hosting_did`, and the `https://dids.firstperson.dev`
URLs) as copyable fields once the session reaches `running`, for pasting into
configuration.

A later improvement, deliberately **not** in this design: read those values
straight from the platform session row instead of from env, which would remove
the copy step entirely. Worth doing once the platform stack has proven itself;
not worth coupling the two before then.

#### 3.3.5 Deletion

Deleting a platform stack takes every `vta_only` session's mediator and DID
host with it. It requires the strongest confirmation in the product — see
§11.1.

#### 3.3.6 Who owns it: a dedicated system account

An earlier draft said the `platform` rows belong to "the admin who created it".
That cannot be implemented as written:

```sql
-- migrations/000003
user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE
```

Admins live in the **`admins`** table, users in **`users`**. Storing an admin's
id in `setup_sessions.user_id` would point at whichever *user* happens to hold
that id — and since the namespace is derived as
`{K8S_NAMESPACE_PREFIX}-{user_id}`, the platform stack would land inside a real
customer's namespace.

**Decision (2026-07-26): the platform stack is owned by a dedicated system
account.** `POST /api/v1/admin/platform-stack` gets-or-creates one row in
`users` — `unique_id = 'platform'`, `email` NULL — and both the `domains` row
and the `setup_sessions` row hang off that id.

- Nothing about the FK, the namespace derivation, teardown, or capacity
  accounting needs a `platform` special case: it is an ordinary session in an
  ordinary per-user namespace.
- It is not tied to a person. Binding the farm's flagship stack to a real
  admin's user account would mean deleting that account cascade-deletes the
  platform stack.
- `beta_access` on that row is meaningless and never checked (§3.3.2).
- `GET /api/v1/admin/users` should mark or filter the row so it doesn't read as
  a real signup.

The alternative — letting an admin nominate an existing user account — was
rejected for the cascade-delete reason above.

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

Always derived, as `EnvPrefix(env) + "lb." + CLUSTER_DOMAIN` — that's
`setup.CNAMETarget(env, clusterDomain)`, shipped in phase 1. An earlier draft
made it overridable via a `CUSTOM_DOMAIN_CNAME_TARGET` env var; that was
dropped, because the value is a pure function of config we already set and a
knob nobody sets is still a knob somebody can set wrong. If the load balancer
is ever renamed, change the derivation.

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
| `user_id` | `bigint NOT NULL` | owner; for `platform` rows, the system account (§3.3.6) |
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

### 5.4 Migration `000021_domains`

`000020_vtc_name_index_full_stack` is the highest migration on disk, so this
one is **000021**. (An earlier draft said 000020 — written before that
migration existed.)

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
  WHERE domain_type = 'managed' AND vtc_name <> '';
```

The `vtc_name` predicate keys on **the value, not the mode string** — that is
what `000020` changed it to, and the reasoning carries over verbatim: "a
session that has a `vtc_name` must own it exclusively" is the actual invariant,
and it survives the next mode rename. A draft of this section still said
`mode = 'full_stack_with_vtc'`, which by now matches no rows at all and would
have silently disabled the index.

Live definitions this migration replaces, for reference:

```sql
-- 000013
CREATE UNIQUE INDEX setup_sessions_vta_name_unique ON setup_sessions (vta_name);
-- 000020
CREATE UNIQUE INDEX setup_sessions_vtc_name_unique ON setup_sessions (vtc_name)
    WHERE vtc_name <> '';
```

> Both names are load-bearing beyond the database: `internal/handler/setup.go`
> matches on the strings `setup_sessions_vta_name_unique` /
> `setup_sessions_vtc_name_unique` to turn a constraint violation into a 409,
> so they must be recreated under exactly these names.

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
| HTTP-01 challenges | 4 | 4 — unchanged, one per name |
| Recreations before the binding limit (§9.3) | 5 / week | **5 / week — unchanged** |
| K8s Secrets per session | 4 | 1 |
| Readiness conditions to poll | 4 | 1 |

So it does **not** relax the limit that actually binds (§9.3 — five recreations
per week either way). What it buys is fewer moving parts: one Secret instead of
four, one readiness condition instead of four, and failure diagnosis that is a
single object lookup.

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
          ingressClassName: traefik
```

**One issuer, every environment — there is deliberately no staging twin.**

An earlier revision shipped a `letsencrypt-http01-dev` twin on Let's Encrypt's
staging endpoint and made it mandatory outside production, to keep §9.4's
allowances safe. It protected the quota and broke the feature. A staging
certificate chains to a root no public trust store carries, and this pipeline
does not merely *serve* TLS: from `step_vta_register_dids` onward the components
resolve each other's `did:webvh` identifiers over HTTPS, and those clients reject
an untrusted chain outright. So the certificate issued, `tls_provision` passed —
cert-manager only asks whether it was issued — and the mediator then crash-looped
several steps later on an error that named a network failure rather than a
certificate one. A custom-domain session could not complete outside production
at all.

The cost is that every environment now spends the same unraisable allowances
(§9.4), and the five-per-identical-name-set-per-week limit is the binding one:
the four names never vary, so it caps rebuilds **per domain, counted across
every environment sharing this issuer**. Two things follow — keep development
iteration on domains we own rather than a customer's, and prefer reusing an
already-issued Secret over requesting again (§8.4).

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
| **Cloudflare for SaaS** — users CNAME to a proxied `lb`, Cloudflare issues and renews the edge certificate | **Deferred.** Free at this scale (100 custom hostnames included on every plan, then $0.10/mo each) and it would hide the origin IP. But origin-side TLS is unresolved: with a fallback origin, Cloudflare sends SNI = the custom hostname, our ingress answers with the wildcard, and Full (strict) rejects it. Fixing that means either downgrading the whole zone to Full, or running cert-manager **as well** — i.e. this option plus all of §8. Parked in §16.3; §4.1 keeps the migration path free. |
| **LE DNS-01 with `_acme-challenge` delegation** | Rejected — 4 more records for the user, and its benefits (no port-80 dependency, wildcard support) are ones we don't need. |
| **LE DNS-01 with the user's DNS API credentials** | Rejected — a large trust ask, and provider-specific. |
| **User-supplied certificates** | Rejected — manual renewal every 90 days. |
| **Caddy on-demand TLS** | Rejected — the slickest answer for arbitrary hostnames, but it means replacing the bundled ingress controller. (Written when that was ingress-nginx; the cluster moved to Traefik afterwards, for unrelated reasons, and the verdict is unchanged — Caddy would still replace it.) |

### 8.6 On the origin IP

Worth recording plainly, because it shaped this decision. The origin IP is
**already public**, and Cloudflare is **already bypassable**. Measured directly:

```text
dig vtafarm.firstperson.dev            → 172.67.139.142, 104.21.94.190   (Cloudflare)
openssl s_client 157.180.68.139:443    → CN=*.firstperson.dev
curl --resolve dids-…:443:157.180.68.139  → HTTP 200   (identical to via-Cloudflare)
```

The ingress serves the wildcard certificate to *any* direct connection on
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

### 9.2 Nothing here needs tracking on our side

**No quota accounting, counters or pre-flight budget checks are part of this
design**, and none should be built.

The per-registered-domain limit is charged to the *customer's* domain, so it is
theirs, not ours — there is no cluster-wide ceiling to watch, and it does not
aggregate as the user base grows. The one limit charged to our account (300 new
orders / 3 h) sits three orders of magnitude away from real usage at one
certificate per session, and Let's Encrypt raises it on request if that ever
changes. **ARI-driven renewals are exempt from every limit**, so steady-state
operation consumes nothing at all — enable ARI in cert-manager.

The table above is reference material for diagnosing a failure, not a budget to
manage. Only §9.3 has any bearing on the design, and it is handled by a
teardown rule rather than by counting anything.

One consequence worth remembering when writing error messages: the
per-registered-domain counter is global across *every* ACME account, not just
ours. A customer already issuing Let's Encrypt certificates for `aaa.com`
elsewhere consumes the same allowance, so a failure here may have nothing to do
with us — don't word the message as though it must.

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
| `POST` | `/api/v1/admin/platform-stack` | admin | Create the platform domain **and** its session in one action (§3.3.2). The only route that can mint a `firstperson.dev` row |
| `GET` | `/api/v1/admin/platform-stack` | admin | Current state: not created / provisioning / running, plus the collected values to copy into config (§3.3.4) |
| `DELETE` | `/api/v1/admin/setup-sessions/:id` | admin | Delete any session; requires `{"confirm": "<label>"}` for the platform stack (§11.1) |

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

### 11.1 Admin-initiated deletion

Today only the owning user can delete a session — `DELETE /api/v1/setup/:id`
looks the row up by `unique_id AND user_id`, and there is no admin equivalent.
The admin sessions view is read-only (`GET /api/v1/admin/setup-sessions`).

Add:

| Method | Path | Role | Description |
| --- | --- | --- | --- |
| `DELETE` | `/api/v1/admin/setup-sessions/:id` | admin | Delete **any** session, whoever owns it |

It reuses the existing teardown path verbatim — orchestrator cancel, DNS
records, K8s resources, Vault seed, namespace collection, DB row — differing
only in the lookup: `unique_id` alone, with no `user_id` filter. Keeping one
teardown implementation matters more than the small amount of duplication a
separate admin path would save; a divergent second teardown is how orphaned
namespaces and stranded Vault entries happen.

**Three tiers of confirmation**, by blast radius. The API enforces the third;
the first two are UI (frontend doc §6):

| Target | Confirmation |
| --- | --- |
| A user's own session (existing user-facing flow) | unchanged |
| Another user's session, from the admin view | modal naming the session **and its owner**; a plain confirm button is enough |
| The **platform stack** | modal that spells out that every `vta_only` session loses its mediator and DID host, plus **type-to-confirm** of the label |

For the platform stack the API must not rely on the UI alone: require an
explicit body field (e.g. `{"confirm": "<label>"}`) and answer 400 without it.
A destructive action reachable by a single mis-click in an admin table is worth
one deliberate extra step — and this is the one deletion in the product that
degrades every other user's service.

The response should also report what it tore down (session id, mode, owner) so
the action is legible in logs after the fact.

---

## 12. Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| ~~`CUSTOM_DOMAIN_ENABLED`~~ | — | **Dropped 2026-07-26.** The kill switch was specified to ship the feature dark until phase 0 landed. In practice it only hid the routes from the people building against them, and it hid the wrong thing: without phase 0 a verification fails anyway, and a failing check *with a reason* tells an operator more than a route that pretends not to exist. Removed rather than defaulted on, so there is no half-state to reason about |
| `ACME_CLUSTER_ISSUER` | `letsencrypt-http01` | The same issuer in every environment — §8.2 explains why the staging twin was withdrawn. Every environment therefore shares §9.4's unraisable allowances, so keep iteration on domains we own. Overridable where a cluster names its issuer something else |

Two variables, not three. The CNAME target is **derived**, never configured —
see §4.1.

Existing `CLUSTER_INGRESS_IP`, `CLUSTER_DOMAIN` and `APP_ENV` keep their
meanings. `.env.example` and the `CLAUDE.md` env table need the new rows.

**Cluster / DNS prerequisites (one-off, manual):**

- `lb.firstperson.dev` → `A` → `CLUSTER_INGRESS_IP`, **grey cloud**
- `dev-lb.firstperson.dev` → `A` → `CLUSTER_INGRESS_IP`, **grey cloud**

  Both must be created by hand in the dashboard. `cloudflare.CreateARecord`
  hardcodes `Proxied: true`, so the API cannot create them even though it holds
  a token for the zone — and a proxied `lb` is precisely the error-1014 failure
  §4.1 describes.

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
| `CLUSTER_DOMAIN` itself, **or any subdomain of it, from any caller including an admin** (§3.3.1) | 400 | `firstperson.dev is managed by VTA Farm and can't be attached — choose the managed option` |
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
✅ marks what phase 1 already landed.

| File | Change |
| --- | --- |
| ✅ `internal/setup/subdomain.go` (+ test) | `EnvPrefix`, rewritten `componentHost`, `CNAMETarget`, `FixedHosts` |
| ✅ `internal/setup/domain.go` (+ test) | new — normalisation, validation, token minting, `CustomHosts` (§13) |
| ✅ `internal/handler/setup.go` | `DomainInfo`; `domain_id` / `label` binding and validation |
| ✅ `internal/router/router.go` | all of the routes |
| ✅ `internal/apidocs/openapi.yaml` | document all of them |
| ✅ `internal/model/domain.go` | new — `Domain` model |
| ✅ `internal/model/setup_session.go` | `DomainID`, `DomainType`, constants, helpers |
| ✅ `migrations/000021_domains.{up,down}.sql` | new |
| ✅ `internal/dnscheck/checker.go` (+ test) | new — TXT + CNAME resolution (§6.4) |
| ✅ `internal/handler/domain.go` | new — the five user routes in §10.1 |
| ✅ `internal/handler/admin_platform_stack.go` | new — §10.1's two admin routes, incl. the system account (§3.3.6) |
| ✅ `internal/handler/setup_fullstack.go` | fixed-label branch of create; teardown branch; new response fields |
| ✅ `internal/config/config.go` | two new vars (§12) |
| ✅ `CLAUDE.md`, `.env.example` | env table, routes, structure |
| ✅ `internal/setup/orchestrator_fullstack.go` | `dns_wait`, `tls_provision` |
| ✅ `internal/k8s/component_resources.go` | `ComponentIngressSpec` + `tls:` block |
| ✅ `internal/k8s/certificates.go` | new — create/poll/delete the session Certificate |
| ✅ `internal/k8s/fullstack_names.go` | `FSTLSSecret` |
| ✅ `helm/vtafarm-api/templates/.../clusterrole.yaml` | `cert-manager.io/certificates` |
| ✅ `k8s/tls/clusterissuer-http01.yaml` | new — one issuer for every environment, no staging twin (§8.2) |

`setup_vtc.go` holds only `ReissueVtcInstall` / `AckVtcInstall`; the
create path is `createFullStack` in `setup_fullstack.go`, which is where the
fixed-label branch goes.

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
| **The platform stack is owned by a system account** | A dedicated `users` row, not an admin id — which the FK on `setup_sessions.user_id` makes impossible (§3.3.6). |
| **Names** | Fixed-label domains take a single `label`; duplicates across users allowed; unique indexes become managed-only. |
| **The CNAME target is derived, not configured** | No `CUSTOM_DOMAIN_CNAME_TARGET` env var; `setup.CNAMETarget` computes it (§4.1). |
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
| **0** | `lb` + `dev-lb` grey-cloud records; both ClusterIssuers; ARI enabled | yes — **still not done**. Nothing is gated on it now that the kill switch is gone; until it lands, every verification simply fails |
| ✅ **1** | `dev-` rename + tests + `GET /setup/domain-info` + frontend hint fix | done 2026-07-26 (`faff4af`) |
| ✅ **2** | `domains` table, `FixedHosts`, single-`label` handling, **`platform` domain end to end** | done 2026-07-26 (`ee8d0b6`) — the milestone that mattered |
| ✅ **3** | `internal/dnscheck`, the Domains routes, verification | done 2026-07-26 |
| ✅ **4** | Certificate creation, `dns_wait` + `tls_provision`, RBAC, teardown branch | done 2026-07-26 — backend complete |
| **5** | Frontend (companion doc) | needs 1–4 |
| **6** | Polish: admin domain release | optional |

> **Phase 3 notes.**
>
> - **No feature flag.** It shipped behind `CUSTOM_DOMAIN_ENABLED` and that was
>   removed the same day (§12). The switch hid the routes from the people
>   building against them while hiding the wrong thing: phase 0 missing makes
>   verification *fail*, and a failing check with a reason is more useful than
>   a 404.
> - **`GET /domains/:id` resolves live and promotes to verified**, so the
>   portal's 30s poll shows a ✓ appearing on its own. `POST .../verify` is the
>   same operation behind the button. A verified domain then performs no lookups
>   at all — §6.2's "no periodic re-verification" falls straight out of that.
> - **Cloudflare-proxy detection landed early** (was phase 6). It is fifteen
>   lines against a static IP range list, and without it the most common failure
>   in the whole feature reads as a bare IP mismatch.
> - **Two admin-cookie twins were added** that the design didn't anticipate:
>   `GET /admin/setup/domain-info` and `GET /admin/setup-sessions/:id/logs`. The
>   platform stack is owned by a passkey-less system account, so the admin panel
>   could otherwise neither name its hostnames before creating it nor watch it
>   provision.
> - **`vta_name`/`vtc_name` uniqueness checks are now scoped to
>   `domain_type = 'managed'`**, matching the partial indexes migration 000021
>   created. Without that the handler would answer 409 for a name the database
>   would have accepted.
>
> **Correction to phase 2 (2026-07-26).** Phase 2 made `admin_did` required on
> `POST /admin/platform-stack`, reasoning that the pipeline would otherwise park
> at `awaiting_admin_did` with no way to resume it — the only route that does is
> `POST /setup/:id/admin`, which filters by `user_id`, and the platform stack's
> owner has no passkey. **The problem was real; the fix was impossible to
> satisfy.** `pnm setup` mints the admin DID locally *from the VTA DID*, which
> does not exist until `step_vta_setup` has run — so it can never be known at
> create time. Replaced by `POST /admin/setup-sessions/:id/admin`, the
> admin-cookie twin that looks a session up by `unique_id` alone. The platform
> stack now follows exactly the sequence a user's session does; the only
> difference is that any admin may complete it.
>
> **Phase 4 notes.**
>
> - **`dns_wait` applies to every session, and its pass criteria differ by
>   kind** (§6.4). Managed and platform records are proxied, so they resolve to
>   Cloudflare's edge and can never equal `CLUSTER_INGRESS_IP` — for those,
>   resolving at all is the whole test. Budget 2 minutes: a managed session's
>   records were created seconds earlier, and a custom domain's were verified
>   before the session could exist, so this is a sanity check rather than a wait
>   for anyone to go and edit DNS.
> - **Deviation: only a custom domain *fails* on `dns_wait`.** §7.1 says the
>   status applies to all, and it does — but on our own zone the records were
>   just created through the Cloudflare API and their ids are in hand, so there
>   is no user error left to catch. What can still go wrong there is a public
>   resolver holding a negative answer for a name queried before it existed
>   (§6.6), and Cloudflare's SOA minimum outlives any budget worth waiting.
>   Failing would turn a caching artifact into a dead session on a path that has
>   always worked, so managed and platform log the timeout and continue.
> - **`tls_provision` does not retry.** Once Let's Encrypt's five failures per
>   hostname per hour is hit, retrying makes the lockout worse. It fails with a
>   message naming the two actual causes — a DNS change since verification, or a
>   CAA record forbidding Let's Encrypt.
> - **Teardown deletes the Certificate and keeps the Secret** (§9.3
>   mitigation 1). Namespace deletion collects it when the user's last session
>   goes.
> - The `helm` ClusterRole and `CLAUDE.md`'s copy of it are both updated. They
>   have to stay in sync by hand.

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
