# Keyring Wallet Sign-in Plan

- Status: Agreed product direction; implementation and Keyring compatibility pending
- Scope: `vtafarm-api`, the `vtafarm` login page, and the existing VTA pool
- Related designs: [Pooled VTA creation and claim](pooled-vta-claim-design.md),
  [public signup](public-signup-flow-design.md), and
  [VTA claim trigger links](vta-claim-implementation-design.md)

## 1. Product Decision

A person who has claimed a pooled VTA should be able to select it in Keyring,
approve a sign-in, and enter the existing Farm account associated with that VTA.
The Farm must verify both control of the signing identity and that identity's
authority to sign in to the account.

Use the VTI `auth/oob` sign-in flow. Its one-time challenge is part of the normal
login interaction, handled by the browser, Farm, and wallet. There is no separate
challenge page, activation link, manual account registration, or prerequisite VTC
membership. Keep passkey sign-in available.

The intended experience is:

```text
Sign in with Keyring -> select the VTA/identity in Keyring -> approve -> Farm Portal
```

Number matching and device verification are included when required by the
protocol. They do not constitute a separate enrollment journey.

## 2. Current Behavior and Missing Pieces

Pool creation already inserts a real `users` row and a `setup_sessions` row for
each VTA. Claiming does not create a new user: it associates the claimant's
administrator DID with the VTA through the confirmed provisioning flow. The
existing `setup_sessions.user_id` remains the resource owner.

The public claim capability permits reservation, status checks, confirmation,
and rejection for one claim. It does not establish a Farm account session.
Successful VTA handover also does not currently enroll a Portal login identity.

Today the Farm login page offers passkeys. The Trust Task handler serves
`provision/vta/claim` and `provision/vta/redeem`, but not `auth/oob`.

Existing components that can be reused include the Farm DID and reply signer,
Trust Task HTTPS endpoint, schema validation, replay protection, DID resolution,
QR rendering, and Portal cookie authentication. The current wallet verifier is
restricted to Ed25519 `did:key` authentication proofs; the nested identity and
grant proofs need additional verification with their respective DID relationships.

## 3. Account Ownership and Login Identity

### 3.1 Preserve the existing owner

The relationship is:

```text
verified login identity -> explicit Farm login binding -> existing user_id
                                                           |
                                             setup_sessions.user_id -> VTA
```

Do not create a second account on first wallet sign-in, move resources between
users, or change namespace and Vault ownership. A VTA DID or URL is a public
identifier, not a login credential.

### 3.2 Establish the initial binding from a completed claim

The initial trust anchor is the administrator DID verified during the original
VTA claim. Establish login eligibility only after the claim is confirmed and
provisioning/handover has completed successfully. Open, rejected, expired, or
failed claims cannot grant account access.

Record an explicit, revocable account login binding, including the account,
login DID, source setup session, and completed claim that authorized it. Retain
the claim and setup session as the authoritative ownership records; this binding
records authentication authority rather than a second resource owner. Creation
must be idempotent and must verify that the source session still belongs to the
same account.

Do not treat a mutable `admin_did` field or arbitrary live VTA ACL entry as an
automatic grant of Farm account ownership.

### 3.3 Distinguish the claimant key from a persona

These identifiers may differ:

- the VTA service DID;
- the claimant's administrator DID/key held by Keyring;
- a persona DID whose signing key is held by the VTA.

If Keyring supports the agreed `auth/oob` exchange as the original claimant DID,
the Farm can authenticate that DID against its claim-derived login binding.
This capability must be verified rather than assumed.

If Keyring instead signs in as a VTA persona, the Farm needs an authenticated
association between that persona and the original claimant. Require a fresh
authorization signed by the recognized claimant and proof of control of the
persona. Bind that authorization to the Farm, account, source VTA, persona, and
enrollment request. A VTA hosting or signing for a persona does not by itself
prove that the persona owns its Farm account.

Prefer establishing this association during the existing claim/handover
interaction, without another page. The exact wallet exchange is an implementation
dependency: agree a supported protocol before adding fields or tasks. Do not
invent a private extension to the existing claim payloads or silently accept an
unbound persona to make onboarding work.

Existing completed claims can establish the same binding on first sign-in using
fresh proof from the recorded claimant. No automatic migration may infer persona
ownership from a supplied VTA DID alone.

### 3.4 Keep account authority separate from VTA administration

Adding a VTA administrator or device does not automatically grant Farm login
access. Only the initial verified owner and explicitly authorized login
identities may enter the account. Removing or replacing a login identity must
revoke its binding and invalidate sessions derived from it.

Do not merge accounts because a wallet controls several VTAs or because emails
match. If an identity is authorized for more than one account, require an
explicit account selection bound to the sign-in before issuing a session; never
select an arbitrary first match.

The initial implementation targets pool-created accounts. Linking wallet login
to an existing passkey account requires an authenticated account session and proof
of the new identity, and must not overwrite an existing ownership relationship.

## 4. User Journey and OOB Exchange

OOB means **out-of-band**: approval happens in the wallet, outside the page that
requested the login. Both applications may run on the same phone.

1. **Start on the existing login page.** The user selects **Sign in with
   Keyring**. The browser creates a temporary, non-extractable signing key and
   sends `auth/oob/request`. The Farm accepts it only from the configured frontend
   origin and returns a short-lived request.
2. **Open or scan the trigger link.** Display the API-provided link as a QR and
   clickable link using `/vti/flow/sign-in/0.1`. A supported Keyring launch action
   enables same-device use. Confirm the wallet's sign-in deep-link support;
   support for VTA claim links does not establish support for sign-in links.
3. **Claim the login request.** Keyring verifies the Farm DID and advertised
   services, then sends `auth/oob/claim` using a temporary wallet key. The first
   valid claim locks the request. The Farm signs the service and origin details.
4. **Match the number and prove identity.** The original browser's signed
   `auth/oob/redeem` poll obtains the match number. Keyring submits
   `auth/oob/prove`, containing the identity's signed `auth/oob/identify` and the
   number. The Farm checks login eligibility, identity proof, and number. The
   sign-in number is distinct from the six-character VTA provisioning claim check.
5. **Approve in Keyring.** Show the Farm, chosen identity, and requester details.
   After device user verification, the identity signs `auth/oob/grant`; Keyring
   submits it in `auth/oob/respond`. The grant binds the browser key, wallet key,
   origin, request context, and session expiry.
6. **Complete in the original browser.** Only the original browser key may
   redeem the approved request. Recheck the account binding before issuing the
   Farm user cookie, consume the request atomically, and enter the existing
   Portal. Show the resolved account/identity before continuing when it differs
   from the browser's previous choice, following the VTI reference behavior.

Expose pending, declined, canceled, expired, unsupported-wallet, and unavailable
states on the same page. Allow cancellation through `auth/oob/cancel`. Hide the
code when the tab is hidden and follow the reference rules for renewal; never
renew a claimed request silently or poll indefinitely after expiry. Returning
from Keyring should resume the request if the browser still holds its key;
otherwise offer a fresh sign-in.

The QR request identifier is public. It must never act as a bearer credential or
be sufficient to retrieve the identity, match number, or session.

## 5. Protocol and Deployment Requirements

Follow the published Trust Task schemas and the VTI implementation, using the
generated types already available in the pinned Go dependency:

| Task | Reference version |
| --- | --- |
| `auth/oob/request` | `0.2` |
| `auth/oob/claim` | `0.2` |
| `auth/oob/identify` | `0.1` |
| `auth/oob/prove` | `0.3` |
| `auth/oob/grant` | `0.3` |
| `auth/oob/respond` | `0.3` |
| `auth/oob/redeem` | `0.2` |
| `auth/oob/cancel` | `0.1` |

These are the inspected reference versions, not a claim of compatibility with a
released Keyring build. Confirm the exact versions before implementation. Modern
payloads use RFC 3339 timestamps such as `sessionEndsAt`; do not copy obsolete
epoch-based payload examples. The trigger link's `_exp` remains an epoch value.

Serve the tasks on the existing `POST <TRUST_TASK_BASE>/trust-tasks` endpoint.
Keep login requests and provisioning claims in separate state machines and
storage; their similarly named `claim` and `redeem` tasks are different protocols.

The Farm DID must publish:

- `TrustTaskHTTPS` pointing to the configured API Trust Task base;
- `SignInPortal` pointing to the frontend login page, on its actual public origin;
- the assertion verification method used to sign Farm responses.

Extend the existing DID readiness check to cover sign-in. API and frontend may
have different origins: validate the configured frontend origin explicitly and
configure credentialed CORS/cookies accordingly. Never trust an arbitrary
request header to choose the portal origin. Use the supported trigger-link host,
following VTI's restriction against a same-domain universal-link host.

The observed development VTC reported missing `TrustTaskHTTPS` and `SignInPortal`
during this investigation. That deployment was not a successful end-to-end
reference; its configuration must not be copied without validation.

## 6. Security Requirements

- Verify outer wallet/browser proofs and nested identity proofs independently:
  `identify` uses `authentication`, `grant` uses `assertionMethod`, and Farm
  responses use the Farm's `assertionMethod`. Check the current verified DID
  document and the exact verification method, not just a matching public key.
- Use unpredictable, expiring requests, the reference claim/decision windows,
  and one-time grants. Verify all request, key, origin, context digest, recipient,
  and expiry bindings. Terminal requests cannot return to an active state.
- Persist login state in PostgreSQL with atomic transitions, idempotent delivery,
  and bounded polling across replicas and restarts. One request permits one
  active redeem poll and one session issuance. A duplicate delivery must not
  create another session or extend the approved lifetime.
- Deliver login cookies only to the authenticated starter browser. Preserve
  secure HttpOnly cookie handling and CSRF protections for authenticated
  operations. Do not return a session bearer token in a QR, URL, or wallet reply.
- Cap the session at the grant's `sessionEndsAt`. The existing fixed 24-hour JWT
  helper cannot be used unchanged when it would outlive that grant. Refresh, if
  supported, cannot extend the absolute deadline.
- Track the login binding used by each wallet session. Enforce revocation on
  authenticated requests; revoking the binding or deleting the account must
  prevent continued use of an otherwise unexpired cookie. A stateless JWT alone
  does not provide this behavior.
- Grant only a Farm user session. This flow never creates a Farm administrator
  session, and VTC membership grants no Farm account authority. Start with no
  optional delegated Trust Task signing scope; future delegation needs its own
  explicit authorization design.
- Rate-limit requests and proofs, reject unknown identities without arbitrary
  DID resolution, and avoid revealing account existence or private details.
  Use `Cache-Control: no-store`, no-referrer handling, and framing restrictions
  on login responses/pages. Exclude request material, signatures, cookies, and
  keys from logs; audit the account binding and security-relevant outcomes.
- Replacing a passkey or login binding and other sensitive account changes need
  fresh authorization. A new login method must not silently weaken recovery or
  permit an administrator added to a VTA to take over the Farm account.

## 7. Implementation Sequence

1. **Validate Keyring compatibility.** Test sign-in trigger links, task versions,
   the actual signing DID, number matching, and user-verification support with
   the intended app build. Determine whether claimant-key authentication works
   or persona enrollment needs a supported owner authorization exchange.
2. **Implement ownership binding.** Add durable, revocable login bindings and
   idempotent integration with completed pool handover. Support proof-based
   enrollment for existing claimants without moving accounts or infrastructure.
3. **Implement the API exchange.** Add typed task dispatch, proof verification,
   persistent OOB state, expiry/cancellation, readiness configuration, and
   browser-bound cookie issuance with bounded lifetime and revocation checks.
4. **Integrate the existing login page.** Add **Sign in with Keyring**, QR and
   supported same-device launch, number matching, status/error handling, and
   account confirmation. Keep the existing passkey option.
5. **Verify in development.** Publish and check the Farm DID services, run API
   and frontend verification, and complete the real Keyring acceptance scenarios.

Steps 2 and 3 must not assume that a VTA-hosted persona is the original claimant.
If compatibility is missing, retain passkey login and report the unsupported
wallet state; do not weaken proof or ownership checks as a fallback.

## 8. Acceptance Criteria

- A new pool claimant signs in to the VTA's existing account without separate
  registration, a challenge page, a second account, or VTC membership.
- An existing completed claimant can establish login using fresh ownership
  proof; historical public claim data alone cannot enroll an identity.
- Desktop QR and same-phone Keyring launch both complete with the intended app
  build, including number matching and user verification.
- Unknown, unbound, or merely VTA-hosted personas cannot obtain an account
  session. Adding a VTA administrator does not create a Farm login binding.
- Expired/canceled requests, wrong numbers, altered origins or context digests,
  wrong keys or DID relationships, and replayed grants fail without a session.
- Copying a QR or request ID cannot redeem another browser's session. Concurrent
  claims, duplicate responses, and retries across API replicas cannot issue
  extra sessions.
- Revoked bindings, deleted accounts, and expired grants cannot retain access;
  account selection cannot accidentally expose another account's resources.
- Passkey login continues working. Wallet login cannot issue an admin cookie or
  alter pool ownership, resource paths, or existing VTA claim behavior.

## 9. References

- VTI [member portal documentation](https://github.com/OpenVTC/verifiable-trust-infrastructure/blob/main/docs/03-vtc/website-and-admin.md),
  especially the wallet sign-in and member portal sections.
- VTI [browser OOB implementation](https://github.com/OpenVTC/verifiable-trust-infrastructure/blob/main/vtc-service/admin-ui/src/members/oob.ts)
  and [server task handlers](https://github.com/OpenVTC/verifiable-trust-infrastructure/blob/main/vtc-service/src/trust_tasks/oob_tasks.rs).
- VTI [wallet user-verification keys](https://github.com/OpenVTC/verifiable-trust-infrastructure/blob/main/docs/02-vta/wallet-sign-in-uv-keys.md).
- Published task schemas under `https://trusttasks.org/spec/auth/oob/` and the
  corresponding generated `specs/auth/oob` packages in the pinned Go module.
- [W3C DID authentication relationship](https://www.w3.org/TR/did-core/#authentication).
