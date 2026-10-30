# Keyring Sign-in Integration

This document tells a Keyring App developer how to sign a person in to VTA
Farm. Obtaining a VTA is a separate flow documented in the
[Keyring VTA acquisition contract](mobile-connection-app-api.md).

## What Keyring must implement

Keyring must:

1. recognize the `/vti/flow/sign-in/0.1` trigger link;
2. resolve and verify the VTA Farm DID;
3. let the person select an existing VTA identity;
4. complete the `auth/oob` Trust Task exchange; and
5. show whether the request was approved, rejected, canceled, or expired.

Keyring must not create a new identity, claim another VTA, or require Community
membership during sign-in.

## User experience

```text
Open or scan the sign-in link
    -> Select an existing VTA in Keyring
    -> Enter the two-digit number shown by VTA Farm
    -> Review and approve the request
    -> Return to the signed-in browser
```

Each VTA obtained by Keyring has its own administrator identity. If Keyring
contains multiple VTAs, the person chooses the VTA in Keyring. The VTA Farm
browser must not be the normal VTA picker.

The two-digit sign-in number is different from the six-character check used
when obtaining a VTA.

### Existing and legacy VTAs

A VTA previously obtained through the Keyring acquisition flow can sign in even
if its Farm login binding was not created at claim time; VTA Farm derives that
binding from the verified, successfully released claim on first sign-in.

A legacy VTA that was never obtained or explicitly linked through Keyring has no
Keyring identity that VTA Farm can verify and cannot use Keyring sign-in. The
person must continue using an existing sign-in method, such as a passkey, until
VTA Farm provides an authenticated **Link Keyring** flow. Do not require the
person to replace an existing VTA solely to enable sign-in.

## 1. Parse the trigger link

The link has this shape:

```text
https://link.trustoverip.org/t#_from=<farm-did>&_id=<request-id>&_exp=<unix-time>&_type=/vti/flow/sign-in/0.1
```

Keyring must:

- require `_type=/vti/flow/sign-in/0.1`;
- reject an expired `_exp`;
- use `_from` as the Farm DID; and
- copy `_id` exactly as the sign-in request ID.

Route this flow directly to the sign-in handler. Do not apply Community join or
membership checks.

## 2. Discover and verify VTA Farm

Resolve the Farm DID from `_from`. Its DID document must provide:

- `TrustTaskHTTPS`, whose `serviceEndpoint` identifies the Trust Task base;
- `SignInPortal`, whose `serviceEndpoint` identifies the authorized login page;
- an assertion method that verifies signed Farm responses.

Send tasks to:

```text
POST <TrustTaskHTTPS.serviceEndpoint>/trust-tasks
Content-Type: application/json
Accept: application/json
```

Verify every Farm response against the current Farm DID document. Do not take
the endpoint, portal origin, or Farm verification key from another request
field.

## 3. Complete the Trust Task exchange

Keyring sends three HTTP tasks. `identify` and `grant` are signed documents
carried inside `prove` and `respond`; they are not separate HTTP requests.

```text
Keyring                 VTA Farm                 Browser
   |                        |                       |
   |-- auth/oob/claim ----->|                       |
   |<-- service + origin ---|                       |
   |                        |<-- redeem polling ----|
   |   user selects VTA     |                       |
   |   and enters number    |                       |
   |-- auth/oob/prove ----->|                       |
   |<-- requester context --|                       |
   |   user approves        |                       |
   |-- auth/oob/respond --->|                       |
   |<-- approved/declined --|                       |
   |                        |-- browser session --->|
```

Use these versions and signers:

| Document | Version | Signer and proof purpose | Keyring action |
| --- | --- | --- | --- |
| `auth/oob/claim` | `0.2` | temporary interaction key, `authentication` | Lock the request ID from `_id` |
| `auth/oob/identify` | `0.1` | selected VTA identity, `authentication` | Bind the selected identity and entered number to the request |
| `auth/oob/prove` | `0.3` | temporary interaction key, `authentication` | Carry the signed `identify` document |
| `auth/oob/grant` | `0.3` | selected VTA identity, `assertionMethod` | Approve or decline the browser session and expiry |
| `auth/oob/respond` | `0.3` | temporary interaction key, `authentication` | Carry the signed `grant` document |

The temporary interaction key and the selected VTA identity are different
roles. The temporary key owns this sign-in interaction; the selected VTA
identity proves which existing Farm account the person may enter.

Before sending `respond`, Keyring must show:

- the VTA Farm service name;
- the verified `SignInPortal` origin;
- the selected VTA identity;
- the requester details returned by the Farm; and
- the requested session expiry.

Require device user verification before signing an approving `grant`.

The browser separately performs `auth/oob/request/0.2` and
`auth/oob/redeem/0.2`. Keyring does not receive or create the Farm browser
session. It may send `auth/oob/cancel/0.1` to cancel an active request.

## 4. Validate the exchange

For every request and response, validate the published schema and verify:

- document type and supported version;
- issuer, recipient, and verification method;
- proof purpose and signature;
- request ID, thread, and correlation fields;
- issued and expiry times;
- Farm service and portal origin; and
- session key, context digest, and session expiry where present.

Do not change the selected VTA identity after sending `identify`. Do not log
private keys, signed documents, proofs, or session material.

## 5. Handle outcomes

- **Approved:** show success and return control to the original browser.
- **Declined or canceled:** stop and show that no sign-in occurred.
- **Expired:** discard the interaction and require a new trigger link.
- **Wrong number or context mismatch:** stop; do not silently retry with changed
  signed content.
- **Identity not linked:** ask the person to select the VTA identity originally
  used to obtain the VTA.
- **Retryable unavailable response:** retry with backoff and a new document ID.
- **No HTTP response:** resend the exact same signed bytes and document ID.

Never continue polling or retrying after a terminal result.

## Integration checklist

The integration is ready when:

- a Keyring with one VTA can sign in to its existing Farm account;
- a Keyring with two VTAs can select either VTA in Keyring and reach the matching
  account;
- a legacy VTA that was never claimed through Keyring cannot use Keyring
  sign-in because it has no verified Keyring ownership claim;
- QR scanning and same-device link opening both work;
- wrong identities, wrong numbers, expired requests, altered context, and
  invalid Farm signatures fail; and
- sign-in never creates a new identity, joins a Community, or claims a VTA.

## References

- [Keyring VTA acquisition contract](mobile-connection-app-api.md)
- Published schemas under `https://trusttasks.org/spec/auth/oob/`
