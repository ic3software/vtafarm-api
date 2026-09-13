# VTA-proxied SIOPv2 login — Design

## Status and recommendation

**Proposed; not implemented.** Put this cross-cutting design in `vtafarm-api/docs`.
The relying-party protocol, challenge lifecycle, DID verification, account mapping,
and final browser cookie all belong to the API. The frontend only discovers the
wallet, asks it to mint the assertion, and renders the result.

The first release adds **VTA-proxied SIOPv2 as a second login method** for both
the user portal and the admin console. Passkey registration remains mandatory
before a persona can be linked, and passkeys remain the primary recovery
method. A successful SIOP login only issues the existing cookie for the account
type selected by its route (`vtafarm_user` or `vtafarm_admin`); a user persona
never becomes an admin login merely because its DID verifies.

This plan follows the working flow in the sibling `siop-login` project, but
does not copy its in-memory sessions or return its bearer tokens to the browser.
VTA Farm already has durable user accounts and an established cookie/session
boundary; it should keep both.

## 1. Goal and scope

### In scope

- A **Continue with VTA Wallet** path on `/login` and `/admin/login` for
  existing accounts that have first completed passkey registration.
- A signed, one-time SIOP challenge bound to the selected persona DID.
- Verification of a VTA-minted `id_token`: issuer/subject, audience, nonce,
  expiry/issued-at, Ed25519 signature, DID document, and `authentication` key
  authorization.
- Durable mappings from users and admins to explicitly linked persona DIDs.
  The user and admin mappings are separate: linking a DID to an admin account
  requires that admin's own authenticated passkey session and never reuses a
  user mapping as authority.
- Issuing the existing role-matched httpOnly JWT cookie after a successful
  SIOP login, so all protected routes continue to use their current middleware
  unchanged.
- Authenticated user Settings and admin Security flows to add or remove a
  linked VTA persona.

### Explicitly out of scope for the first release

- Automatic account creation for any previously unseen DID. It would silently
  create anonymous accounts and bypass the current account/onboarding policy.
- Replacing passkeys, changing the JWT format or lifetime, or accepting the
  VTA's `id_token` as a bearer credential on ordinary API routes.
- Using SIOP as an account-recovery method or permitting an account with a
  linked persona to lose its last passkey.
- DID method support beyond the methods proven by the verifier integration
  (`did:webvh`, `did:key`, and `did:peer` are expected from `siop-login`).
- Refresh-token sessions, wallet key storage, or calling a user's VTA from the
  API after authentication.

## 2. Why this belongs to the API documentation

The browser cannot establish trust in an `id_token`; it can only obtain it.
The API must bind the token to server-side state and verify it before deciding
which local account is logging in. It also owns the local JWT cookie consumed by
`middleware.AuthRequired`.

The frontend work belongs in `vtafarm` and is deliberately small: wallet
feature detection, the three protocol requests, and login/settings UI. Keep a
short implementation note next to that work if needed, but this document is the
source of truth for the protocol and security invariants.

## 3. Target architecture

```text
VTA Farm page                 vtafarm-api                    Wallet / holder VTA
    |                               |                                  |
    | profile -> selected DID       |                                  |
    |-- POST challenge ------------>| persist one-time challenge        |
    |<-- nonce + session ID --------|                                  |
    |-- proxyLogin(nonce, RP DID) ------------------------------------->|
    |<-- id_token ------------------------------------------------------|
    |-- POST authenticate ---------->| consume, resolve DID, verify     |
    |<-- role-matched cookie --------| lookup linked local account       |
```

### 3.1 Relying-party identity

VTA Farm needs its **own stable, public RP DID**. Do not use the farm's
`DID_HOSTING_DID`: that key is an infrastructure-control credential and is not
the website identity the wallet user is consenting to.

Provision a small, dedicated `vtafarm-auth` VTA with persistent state and a
public HTTPS-hosted `did:webvh` document under the VTA Farm domain. Configure
the API with its DID as `SIOP_RP_DID`. The frontend obtains the same value from
an unauthenticated API metadata endpoint; it must not be baked into the image.

Before enabling the feature, verify the DID resolves from an extension context
over public HTTPS. A development `localhost` DID may be useful for tests, but
some `did:webvh` resolvers always use HTTPS and wallet consent will show it as
unresolved. It is not an acceptable production identity.

### 3.2 Go token verifier

Implement the verifier inside `vtafarm-api` as `internal/siop`; do **not** add
a Rust service. The sibling `affinidi-webvh-service` remains the behavioural
reference: its `did-hosting-common::verify_siop_id_token`, control auth route,
and `vta_provisioned_auth` regression test define the checks our Go tests must
match.

The compact-JWS verifier will use only the Go standard library; do **not** add
`go-jose/v4`. Its supported token shape is deliberately narrow: exactly three
base64url segments, a protected header with `alg=EdDSA`, an Ed25519 public key
obtained from the exact DID `kid`, and a small fixed claim set. The verifier
uses `encoding/base64`, `encoding/json`, and `crypto/ed25519.Verify`, preserving
the exact signed input as `base64url(header) + "." + base64url(payload)`.

This means VTA Farm owns strict compact-JWS parsing. It must reject an
incorrect segment count, non-canonical/invalid base64url or JSON, duplicate or
unknown security-relevant header fields, an absent `kid`, any algorithm other
than `EdDSA`, invalid Ed25519 key/signature lengths, and missing or invalid
SIOP claims. It must never implement EdDSA mathematics itself: the Go standard
library performs the cryptographic verification. Parsing an untrusted payload
is not authentication; only a successful signature and claim verification is.

Implement a narrow `did:webvh` resolver in `internal/siop` against the
[v1 specification](https://identity.foundation/didwebvh/v1.0/), rather than
adding a runtime resolver package. Phase 0 must validate it with
VTA-generated `did:webvh` v1 logs, including key rotation and
`authentication` verification-method lookup. Do not fall back to Rust or
accept an unverified DID document.

The Go verifier takes no HTTP request of its own:

```text
Verify(ctx, idToken, expectedDID, audience, nonce)
    -> { subject, kid }
```

The handler owns challenge consumption and account lookup. The verifier returns
only after all token and DID/key checks succeed; it never issues an application
session.

It must match the token and session checks demonstrated by
`did_hosting_common::server::didcomm_unpack::verify_siop_id_token` and the
control route, with one additional VTA Farm safeguard:

- Read an unverified token only to reject `iss != expected_did` **before** DID
  resolution. The Rust reference binds the signer to the challenge in its
  canonical handler after signature verification; VTA Farm can make this
  earlier rejection because its challenge is already bound to a linked local
  DID. It prevents anonymous callers from turning the resolver into an
  arbitrary DID-fetch proxy.
- Require `iss == sub`, exact `aud == SIOP_RP_DID`, the original nonce, valid
  `iat`/`exp` within a small configured clock skew, and EdDSA verification.
- Bind resolution to the JWS header `kid`; require that key to belong to `iss`,
  be in the DID document's `authentication` relationship, and be Ed25519. For
  `did:key`, also pin the resolved key to the key encoded in the DID.
- Use normal HTTPS certificate verification; constrain redirects, DNS targets,
  and response sizes in the DID resolver. Do not add an insecure TLS switch.

`siop-login` remains a useful browser-contract reference: its `walletProfile`
→ challenge → `proxyLogin` → authenticate sequence is the same one VTA Farm's
React application should use. Matching only a happy-path JWT is insufficient.

### 3.3 Go direction — mirror the Rust trust pipeline

The Rust implementation is the source of truth for the backend security flow.
`siop-login` is only the browser-side interaction reference. The Go code must
preserve the Rust stages and their boundaries; it must not reduce SIOP to
“decode a JWT and look up its `sub`”.

| Rust responsibility | VTA Farm Go responsibility | Intended location |
| --- | --- | --- |
| `routes/auth.rs::challenge` validates input, rate-limits, and creates a pending challenge | Validate DID length/shape; apply IP and per-DID limits; confirm a same-role linked identity exists without exposing which one; persist random `session_id`, nonce digest, expected DID, role, and expiry | `handler/siop.go`, application persistence/model code |
| `verify_siop_id_token` parses compact JWS, binds `kid` to `iss`, resolves the authentication key, and verifies EdDSA | Parse only a three-part compact JWS; allow only `EdDSA`; resolve the exact `kid` authentication key; verify it with the Go standard library's `ed25519.Verify` | `internal/siop/token.go`, `resolver.go` |
| Route checks `aud`, `iat`, and `exp` | Require exact `SIOP_RP_DID`, a valid issued-at/expiry window and bounded clock skew | `internal/siop/token.go` |
| `handle_authenticate` binds signer DID and nonce to a live one-time session, then applies ACL/role | Bind verified DID and nonce to the stored challenge; atomically consume it; re-check the same-role local DID mapping; issue only the matching local cookie | `handler/siop.go`, PostgreSQL transaction |
| Existing server auth response creates its own session/token | Reuse VTA Farm's current JWT/cookie helpers; return the same user/admin summary that the passkey endpoints return | `handler/auth.go`, `middleware/auth.go` |

The Go code should have this narrow, testable shape:

```go
type AuthenticationKeyResolver interface {
    ResolveAuthenticationKey(ctx context.Context, did, kid string) (ed25519.PublicKey, error)
}

type VerifiedIDToken struct {
    Subject string // verified iss == sub
    Kid     string
}

func VerifyIDToken(
    ctx context.Context,
    compact string,
    expectedDID string,
    audience string,
    nonce string,
    resolver AuthenticationKeyResolver,
) (VerifiedIDToken, error)
```

`VerifyIDToken` has no database or Gin dependency. It owns parsing, algorithm
restriction, DID/key binding, signature verification, and token claims.
`handler.SIOPHandler` owns HTTP limits, challenge and identity queries, and
cookie issuance. This separation is the Go equivalent of Rust's split between
`verify_siop_id_token` and `handle_authenticate`.

#### Implement in VTA Farm first, extract later

The first implementation lives in `vtafarm-api`; do not create or publish an
OpenVTC Go module before the VTA Farm integration is proven. Organise the code
as though the stateless verifier will later become `rp-sdk-go`, using this
initial boundary:

```text
vtafarm-api/
  internal/siop/
    claims.go          SIOPv2 claim types and validation
    token.go           strict compact-JWS parsing
    verifier.go        complete id_token verification pipeline
    resolver.go        AuthenticationKeyResolver interface
    didkey.go          did:key authentication-key resolution
    errors.go          typed, caller-safe verification failures
    webvh/
      resolver.go      did:webvh history validation and key selection

  internal/handler/
    siop.go             HTTP limits, challenge flow and cookie issuance

  internal/model/
    siop_identity.go    linked identities and durable challenges
```

The extractable verifier code must not import Gin, GORM, VTA Farm models,
cookie helpers, environment configuration, or application logging. All policy
inputs such as audience, nonce, clock, clock skew, resolver and HTTP client are
passed explicitly. It returns a verified identity or a typed verification
error; it never creates a VTA Farm account, selects a role, stores a challenge,
or issues an application session.

The application-specific layer retains passkey step-up, challenge persistence
and one-time consumption, DID-to-account mappings, user/admin separation,
rate limits, audit events, and VTA Farm JWT/cookie creation. These behaviours
must not move into the future generic SDK.

Port the security behaviour and interoperability fixtures from `rp-sdk-js` and
the Rust reference instead of translating their source line by line. After the
VTA Wallet end-to-end flow is stable, the reusable code can move without
behaviour changes to an OpenVTC-owned Go module, provisionally split as:

```text
github.com/OpenVTC/rp-sdk-go/siop   compact JWS and SIOPv2 verification
github.com/OpenVTC/rp-sdk-go/webvh did:webvh validation and resolution
```

Extraction is ready only when the public interfaces have survived the VTA Farm
integration, the same fixtures pass before and after the move, and a second
consumer can use the verifier without importing VTA Farm types. Until then,
`internal/` intentionally prevents other repositories from depending on an
unstable API.

#### Authentication request sequence in Go

1. The browser posts `id_token` and `session_id` to either the user or admin
   SIOP authenticate route.
2. The handler enforces a small body limit and validates the envelope shape.
3. It reads the unconsumed pending challenge to obtain `expected_did`, role,
   expiry, and nonce digest. It returns a generic failure if it is absent or
   expired.
4. It reads the untrusted `iss` only to reject a mismatch with `expected_did`.
   This is a resolver-abuse guard, not authentication.
5. `VerifyIDToken` verifies the compact JWS using the exact DID
   `authentication` key, then checks `iss == sub`, `kid`, `aud`, nonce, `iat`,
   and `exp`.
6. In one database statement/transaction, delete the challenge only when its
   ID, nonce digest, expected DID, and expiry still match. A competing request
   can win once; every other replay fails.
7. Re-read the linked identity in the selected role table, update its audit
   fields, and issue `vtafarm_user` or `vtafarm_admin`. There is no path that
   derives a role from the DID or crosses between the two identity tables.

For a `did:key`, `ResolveAuthenticationKey` decodes the multibase Ed25519 key
and verifies it agrees with the exact `kid`. For a `did:webvh`, it must fetch
and validate the `did.jsonl` history before selecting the key from the resolved
DID document's `authentication` relationship. Caching is permitted only after
successful validation and must respect a bounded TTL; a cache miss must never
fall back to an unverified document.

## 4. Account linking model

SIOP proves control of a persona DID, not ownership of an arbitrary VTA Farm
account. Therefore an already passkey-authenticated account explicitly links a
DID before it can log in. SIOP never creates an account and cannot become its
only recovery path.

Migration `000029_siop_identities`:

```sql
CREATE TABLE user_siop_identities (
    id                  BIGSERIAL PRIMARY KEY,
    user_id             BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    did                 TEXT NOT NULL,
    label               TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_authenticated_at TIMESTAMPTZ NULL,
    last_kid            TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX user_siop_identities_did_unique
    ON user_siop_identities (did);
CREATE INDEX user_siop_identities_user_id_idx
    ON user_siop_identities (user_id);

CREATE TABLE admin_siop_identities (
    id                  BIGSERIAL PRIMARY KEY,
    admin_id            BIGINT NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    did                 TEXT NOT NULL,
    label               TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_authenticated_at TIMESTAMPTZ NULL,
    last_kid            TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX admin_siop_identities_did_unique
    ON admin_siop_identities (did);
CREATE INDEX admin_siop_identities_admin_id_idx
    ON admin_siop_identities (admin_id);

CREATE TABLE siop_challenges (
    id                  TEXT PRIMARY KEY,
    purpose             TEXT NOT NULL CHECK (purpose IN ('login', 'link')),
    account_role        TEXT NOT NULL CHECK (account_role IN ('user', 'admin')),
    expected_did        TEXT NOT NULL,
    user_id             BIGINT NULL REFERENCES users(id) ON DELETE CASCADE,
    admin_id            BIGINT NULL REFERENCES admins(id) ON DELETE CASCADE,
    nonce_sha256        BYTEA NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT siop_challenge_account_matches_purpose CHECK (
      (purpose = 'login' AND user_id IS NULL AND admin_id IS NULL)
      OR (purpose = 'link' AND (
        (account_role = 'user' AND user_id IS NOT NULL AND admin_id IS NULL)
        OR (account_role = 'admin' AND admin_id IS NOT NULL AND user_id IS NULL)
      ))
    )
);
CREATE INDEX siop_challenges_expires_at_idx ON siop_challenges (expires_at);
```

`did` is the stable account key within its account type. A DID may be linked to
a user account and an admin account only through two separate, authenticated
passkey sessions; it is never promoted between types automatically. `label` is
display-only and cannot grant access. `last_kid` and `last_authenticated_at`
are audit/support facts, not a pin: persona keys may rotate legitimately. A
periodic cleanup removes expired challenge rows; authentication deletes its row
in the same transaction, so a challenge can be used once even when API replicas
race.

### Link and unlink

1. An already authenticated user opens **Settings → VTA Wallet identities**;
   an already authenticated admin uses the equivalent Security setting.
2. The API first confirms the current account has at least one registered
   passkey, then creates a `purpose=link` challenge tied to that exact account;
   the browser asks the wallet to sign it for the RP DID.
3. After full verification, insert the identity. The unique index returns 409
   if that DID is already linked to another account of the same type.
4. Unlink requires the role-matched cookie and removes only an identity owned
   by that account. Deleting a passkey must be refused when it would leave a
   SIOP-linked account with zero passkeys. This keeps a passkey recovery path
   even if the persona DID or wallet is lost.

Never let a login request choose an account ID, infer a local account from
email/DID-host/display data, or use a user identity to find an admin account.

## 5. Public API contract

`GET /api/v1/auth/siop/metadata` is shared and returns `{enabled, rp_did}` so
both pages can feature-gate the option. It contains no DID or account data.

Every remaining route has an identical **user** or **admin** variant. The
selected namespace fixes the role before any identity lookup; the success path
sets only its matching existing cookie and never returns a verifier token.

| Method | Route suffix | Public request / response | Behaviour |
| --- | --- | --- | --- |
| `POST` | `/auth/{user,admin}/siop/challenge` | `{did}` → `{challenge, session_id, expires_at}` | Validates DID shape, confirms it is linked for that role, then creates a 120-second `login` challenge. Return the same generic response for unknown and disabled identities to avoid account enumeration. |
| `POST` | `/auth/{user,admin}/siop/authenticate` | `{id_token, session_id}` → account summary + cookie | Atomically consumes the challenge, verifies it, looks up the DID in the selected role's table, then issues the normal matching JWT/cookie. |
| `POST` | `/{user,admin}/siop/link/challenge` | `{did}` → challenge response | Requires the matching current cookie and a registered passkey; creates a `link` challenge bound to that account. |
| `POST` | `/{user,admin}/siop/link/authenticate` | `{id_token, session_id, label?}` → identity | Requires the matching cookie; consumes, verifies, and inserts the mapping. |
| `GET` | `/{user,admin}/siop/identities` | → identity list | Requires the matching cookie; returns only the caller's identities. |
| `DELETE` | `/{user,admin}/siop/identities/:id` | → `204` | Requires the matching cookie; deletes only the caller's identity. |

Use one request shape for challenge and one for authentication; do not copy the
Trust Task envelope from the demo unless the VTA Wallet API requires it. The
current browser side can send the minimal API contract above, while the verifier
handles the standard SIOP token.

Update `internal/apidocs/openapi.yaml` to describe both methods as
alternatives: passkey remains supported and required to link SIOP; SIOP is
available only to a previously linked identity of the selected account type.
Mark all error responses `Cache-Control: no-store`.

## 6. Frontend implementation plan (`vtafarm`)

Add a small `src/auth/siop/` module, based on the well-scoped code in
`siop-login/src/auth/`:

- `wallet.ts`: a local TypeScript declaration for the injected
  `window.vtaWallet`, capability detection for `walletProfile` and `proxyLogin`,
  and no wallet object persisted in storage.
- `proxyMinter.ts`: invokes `proxyLogin({entryId, nonce, target:{kind:'did',
  did:rpDid}})` and extracts the `Authorization: Bearer` value from the
  returned SessionBlob.
- `login.ts`: accepts `user` or `admin` as an explicit route parameter, obtains
  RP metadata, chooses the site's wallet profile, requests the matching API
  challenge, invokes the wallet, and sends `id_token` plus `session_id` to the
  matching authenticate route.

On `UserLogin.tsx` and `AdminLogin.tsx`, show **Continue with VTA Wallet**
alongside the passkey button only when metadata says the feature is enabled and
the provider supports the required methods. The admin page must call only the
admin routes and update only `AdminAuthContext`; it cannot infer its role from
the DID. If the extension is absent, explain how to install/enable it for this
origin; do not render a broken button. Treat user cancellation as silent, show
actionable errors for a missing identity and verifier rejection, and only call
the existing session setter after the API has set its cookie.

Extend user Settings and the admin Security page with list/link/unlink controls.
The link UI must state that a registered passkey is required and cannot be
removed while SIOP is linked. The frontend must not decode an `id_token` for
authorization; optional decoding is display-only. Store neither the id token
nor any VTA/session secret in `localStorage` or logs.

## 7. Backend implementation plan (`vtafarm-api`)

1. Provision the dedicated RP VTA/DID and add `SIOP_RP_DID`, challenge TTL,
   clock-skew, DID-resolution timeout, and body-size configuration. Fail closed:
   when the RP DID or Go verifier configuration is absent, metadata reports
   `enabled: false` and SIOP routes return 404/503 without affecting passkeys.
2. Add migration, `model.UserSIOPIdentity`, `model.AdminSIOPIdentity`, and
   repository helpers with
   transaction-safe challenge consume (`DELETE ... WHERE id = ? AND expires_at
   > now() RETURNING ...`). Do not use process memory because API replicas and
   restarts would break one-time semantics.
3. Add the in-repository `internal/siop` layout defined in section 3.3: narrow
   `did:key` and `did:webvh` resolvers plus a strict standard-library
   compact-JWS verifier. Keep it free of framework, persistence and VTA Farm
   account/session types so it can be extracted after integration is stable.
4. Add a focused `handler.SIOPHandler`. Keep parsing, input-size limits, rate
   limits, account lookup, passkey-presence checks, and cookie issuance in Go.
   Reuse private helpers for role-matched `GenerateToken` plus the Strict/
   httpOnly cookie so passkey and SIOP cannot drift.
5. Register public user and admin login routes before their auth groups;
   register user link/list/delete under `userAuth` and admin counterparts under
   `adminAuth`. Guard passkey deletion so a SIOP-linked account retains at
   least one passkey.
6. Add OpenAPI, Helm values/config maps, and deployment documentation. Update
   the top-level auth wording from “passkey-only” to “passkey plus optional
   linked VTA Wallet SIOP for user and admin accounts.”

## 8. Security and operational requirements

- Generate at least 32 random nonce bytes and opaque session IDs. Store only a
  nonce digest. TTL is 120 seconds; successful or failed authentication burns
  the challenge.
- Rate-limit challenge and authenticate by IP and expected DID. Bound pending
  challenges per DID and return generic errors before login can reveal whether
  a DID is linked.
- Apply a small request-body limit to `id_token`, never log it, and redact
  authorization headers, nonce, session ID, and DID-resolution URLs if they
  may reveal private host details.
- Verify the challenge DID before resolver work; enforce `iss == sub`, exact
  audience, nonce, time claims, algorithm, key ownership, and authentication
  relationship. Reject `alg=none`, key confusion, absent `kid`, and unknown
  DID methods.
- The browser sees a short-lived assertion only. It must never receive an RP
  private key or a token that can invoke VTA Farm APIs without the normal
  cookie/session boundary.
- Keep cookie `Secure`, `HttpOnly`, and `SameSite=Strict` behaviour unchanged.
  Existing CORS remains credentialed only for the configured frontend origin;
  no wildcard origin is needed for SIOP.
- Audit security events with a hashed DID (or a protected audit store):
  challenge issued, verification success/failure reason, link/unlink, and
  account selected. Retain enough correlation to investigate replay without
  writing the `id_token`.

## 9. Delivery sequence and acceptance tests

### Phase 0 — interoperability/security spike

- Implement and test the in-repository `did:webvh` resolver against public
  VTA-generated v1 logs, including key rotation and `authentication` key
  lookup. Keep it behind an interface so it remains directly testable.
- Test the strict standard-library compact-JWS verifier against the Rust SIOP
  fixtures. It must accept valid tokens and reject malformed,
  algorithm-confused, wrong-key, wrong-nonce, and altered-signature tokens.
- Run the successful `affinidi-webvh-service` SIOP regression fixture through
  the Go verifier.
- Prove rejection for altered signature, wrong audience, wrong nonce, expired
  token, `iss != sub`, `kid` from another DID, and a key outside
  `authentication`.
- Exercise resolver timeout, malformed/oversized documents, redirect policy,
  DNS/network failure, and a `localhost` identity. Do not proceed until the
  public HTTPS deployment works in the real wallet extension.

### Phase 1 — API and persistence

- Unit-test challenge creation/expiry, atomic single consumption, same-role
  identity uniqueness, link/unlink ownership, and the rule that a linked
  account must retain at least one passkey.
- Handler-test unknown DID, replay, resolver timeout/error, valid linked user
  and admin flows, and proof that each output cookie accesses only its own
  unchanged route group.
- Run migration upgrade and downgrade against a disposable PostgreSQL database.

### Phase 2 — frontend and end-to-end

- Test extension unavailable, cancellation, normal linked login, wrong
  persona, identity-not-linked, and verifier rejection.
- Test user Settings and admin Security linking, then SIOP login after a clean
  browser session; validate existing passkey login, passkey deletion guard,
  and logout still work.
- Add one browser-level happy path against a real VTA Wallet test environment,
  plus a manual production-domain consent check before rollout.

### Phase 3 — controlled rollout

- Ship configuration disabled by default; deploy the RP DID and Go verifier
  configuration first.
- Enable only in a non-production environment, then for a small set of linked
  users. Monitor verifier failures, DID-resolution latency, challenge replay
  attempts, and login completion rate.
- Keep passkey login as the documented recovery path. Rollback means disable
  `SIOP_RP_DID`/feature metadata and remove public routing; linked identities
  stay in the database for a later re-enable and no existing cookie is revoked.

### Phase 4 — optional OpenVTC package extraction

- Freeze the verifier and resolver interfaces after the VTA Farm end-to-end
  flow and negative-test suite are stable.
- Move only the framework-independent `siop` and `webvh` code plus shared
  fixtures into an OpenVTC-owned Go module. Keep account linking, challenges,
  passkey policy, role selection and cookies in `vtafarm-api`.
- Run the same Rust, `rp-sdk-js` and VTA Wallet fixtures against the extracted
  module, then replace the internal code with the module dependency without
  changing authentication behaviour.
- Publish a version only after another Go consumer can integrate through the
  public interfaces without depending on VTA Farm implementation details.

## 10. Recorded product decisions

1. SIOP cannot create or register an account. A user or admin must first
   complete the existing passkey onboarding, then authenticate with that
   passkey before linking a persona.
2. SIOP cannot be an account's only recovery path. A SIOP-linked account must
   retain at least one passkey; the API enforces this at link time and when a
   passkey is deleted.
3. Both user and admin accounts support SIOP as a second login option. Their
   identity tables, routes, cookies, and role checks stay separate; each link
   requires an authenticated passkey session of the target account type.
4. The implementation remains Go-only and uses the standard library for
   compact-JWS parsing and Ed25519 verification. `affinidi-webvh-service` is a
   protocol and test reference, not a runtime dependency. The in-repository
   `did:webvh` resolver must pass Phase 0 compatibility and negative tests
   before SIOP is enabled.
5. Implement the reusable verifier and resolver inside `vtafarm-api/internal`
   first. Preserve package boundaries that allow later extraction into an
   OpenVTC-owned `rp-sdk-go`, but do not publish or depend on that module until
   the VTA Farm integration and public interfaces are proven.
