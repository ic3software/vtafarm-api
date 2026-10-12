# VTA Farm Mobile Integration

This guide describes the API flow for linking a mobile Keyring to VTA Farm.
The app scans a QR code, sends a signed claim, displays a six-character check,
and polls until the Farm returns the VTA.

This replaces the removed callback/progress-token and SIOPv2 flows.

All example DIDs, UUIDs, timestamps, and signatures below are placeholders.

## Integration flow

| Step | Mobile action | Farm result |
| --- | --- | --- |
| 1 | Scan and parse the VTI trigger link | Farm DID, handle, and expiry |
| 2 | Resolve the Farm DID | Trust Task HTTPS endpoint and Farm verification keys |
| 3 | Generate a fresh Ed25519 `did:key` | This key becomes the VTA administrator |
| 4 | POST a signed `provision/vta/claim/0.1` | Signed `trust-task-next-step` |
| 5 | Display the locally calculated claim check | User compares it with the Farm page |
| 6 | POST signed `provision/vta/redeem/0.1` requests | Pending, rejected, expired, or terminal success |
| 7 | Receive a verified redeem `#response` | Store the VTA DID and administrator key |

There is one public HTTP endpoint for both Trust Tasks:

```text
POST <TrustTaskHTTPS.serviceEndpoint>/trust-tasks
Content-Type: application/json
Accept: application/json
```

The endpoint does not use a Farm login cookie or bearer token. Each request is
authenticated by its Data Integrity proof.

## 1. Parse the QR trigger link

The QR contains a link with this shape:

```text
https://link.trustoverip.org/t#_from=<farm-did>&_id=<handle>&_exp=<unix-time>&_type=/vti/flow/vta-claim/0.1
```

Read these fragment values:

| Field | Meaning |
| --- | --- |
| `_from` | Farm DID; use it as the Trust Task `recipient` |
| `_id` | Reservation handle; copy it exactly |
| `_exp` | QR expiry as UTC Unix seconds |
| `_type` | Must equal `/vti/flow/vta-claim/0.1` |

Reject the link locally if it is expired, the type is unsupported, or the
handle does not match `[A-Za-z0-9_-]{22,43}`.

## 2. Discover the Trust Task endpoint

Resolve the DID from `_from` and find its `TrustTaskHTTPS` service. Append
`/trust-tasks` to that service's `serviceEndpoint`.

Example:

```text
serviceEndpoint: https://vtafarm-api.example.com/trust-task
POST endpoint:   https://vtafarm-api.example.com/trust-task/trust-tasks
```

Use the Farm DID document's `assertionMethod` keys to verify Farm replies. Do
not obtain the API endpoint or Farm key from any other QR field or response.

## 3. Create the administrator key

Generate a fresh Ed25519 key pair for this QR and encode its public key as:

```text
walletDid = did:key:z6Mk...
verificationMethod = did:key:z6Mk...#z6Mk...
```

Persist the private key before sending the claim. Use the same key for the
claim and every redeem request. Keep it after terminal success because it is
the VTA administrator key. Delete it after a terminal rejection or expiry.

## 4. Send the claim

Task type:

```text
https://trusttasks.org/spec/provision/vta/claim/0.1
```

Example request:

```json
{
  "id": "urn:uuid:06db7ae7-b87d-4de9-8668-2d5d95fae1fb",
  "threadId": "urn:uuid:a8e6cbd9-16d7-4668-93bb-b5300a65b6ac",
  "parentThreadId": "q3Vt7mXo9LwA2bR5cJ8kNg",
  "type": "https://trusttasks.org/spec/provision/vta/claim/0.1",
  "issuer": "did:key:z6MkExampleClaimantKey",
  "recipient": "did:webvh:example-farm-did",
  "issuedAt": "2026-10-12T20:00:00Z",
  "expiresAt": "2026-10-12T20:02:00Z",
  "payload": {
    "handle": "q3Vt7mXo9LwA2bR5cJ8kNg"
  },
  "proof": {
    "type": "DataIntegrityProof",
    "cryptosuite": "eddsa-jcs-2022",
    "created": "2026-10-12T20:00:00Z",
    "verificationMethod": "did:key:z6MkExampleClaimantKey#z6MkExampleClaimantKey",
    "proofPurpose": "authentication",
    "proofValue": "zExampleBase58btcSignature"
  }
}
```

Important fields:

- `id` must be unique for this document.
- `parentThreadId` and `payload.handle` must both equal `_id` from the QR.
- `issuer` is the fresh mobile `did:key`.
- `recipient` is `_from` from the QR.
- `issuedAt` is required and uses RFC 3339 UTC.
- `expiresAt` is recommended; two minutes is sufficient.
- The proof uses `eddsa-jcs-2022` with `proofPurpose: authentication`.
- The payload contains only `handle`; the Farm derives the administrator and
  operation from the signed document and reservation.

The Farm accepts only reasonably fresh requests. Keep the device clock
synchronized; requests more than approximately five minutes old or more than
60 seconds in the future can be rejected.

## 5. Handle the claim response

A successful claim returns HTTP 200 and a signed next-step:

```json
{
  "id": "urn:uuid:38bbf6db-868a-4de7-89ba-0f80a099e21e",
  "threadId": "urn:uuid:a8e6cbd9-16d7-4668-93bb-b5300a65b6ac",
  "parentThreadId": "q3Vt7mXo9LwA2bR5cJ8kNg",
  "type": "https://trusttasks.org/spec/trust-task-next-step/0.1",
  "issuer": "did:webvh:example-farm-did",
  "recipient": "did:key:z6MkExampleClaimantKey",
  "issuedAt": "2026-10-12T20:00:01Z",
  "payload": {
    "expects": [
      {
        "typeUri": "https://trusttasks.org/spec/provision/vta/redeem/0.1",
        "hint": {
          "handle": "q3Vt7mXo9LwA2bR5cJ8kNg"
        }
      }
    ],
    "continuation": "proceed",
    "inResponseTo": {
      "typeUri": "https://trusttasks.org/spec/provision/vta/claim/0.1",
      "id": "urn:uuid:06db7ae7-b87d-4de9-8668-2d5d95fae1fb"
    }
  },
  "proof": {
    "type": "DataIntegrityProof",
    "cryptosuite": "eddsa-jcs-2022",
    "created": "2026-10-12T20:00:01Z",
    "verificationMethod": "did:webvh:example-farm-did#z6MkExampleFarmKey",
    "proofPurpose": "assertionMethod",
    "proofValue": "zExampleBase58btcSignature"
  }
}
```

Verify the Farm signature and confirm that the parties, thread, handle, and
`inResponseTo` match the claim.

This response means only that the handle is reserved for the mobile key. It is
not a completed link. Only the redeem `#response` in section 8 is terminal
success.

## 6. Calculate the claim check

Calculate the six-character check from the complete mobile `did:key` string:

```text
digest      = SHA-256(UTF-8(walletDid))
claim_check = BASE32_RFC4648_NO_PADDING(digest).uppercase().substring(0, 6)
```

The result uses `A-Z2-7`. Display it after receiving the valid next-step. The
Farm independently displays the same value in the signed-in browser. The user
compares the two values and selects **Codes match** or **Not mine** on the Farm
page. The mobile app does not send the claim check back to the Farm.

## 7. Poll with redeem

Task type:

```text
https://trusttasks.org/spec/provision/vta/redeem/0.1
```

Send the first redeem request after the next-step. Use the same mobile key and
handle, but create a new `id`, timestamps, and proof for every new poll.

```json
{
  "id": "urn:uuid:e832ab4d-e027-4fb4-864c-c57b33e22f86",
  "threadId": "urn:uuid:a8e6cbd9-16d7-4668-93bb-b5300a65b6ac",
  "parentThreadId": "q3Vt7mXo9LwA2bR5cJ8kNg",
  "type": "https://trusttasks.org/spec/provision/vta/redeem/0.1",
  "issuer": "did:key:z6MkExampleClaimantKey",
  "recipient": "did:webvh:example-farm-did",
  "issuedAt": "2026-10-12T20:00:02Z",
  "expiresAt": "2026-10-12T20:02:02Z",
  "payload": {
    "handle": "q3Vt7mXo9LwA2bR5cJ8kNg"
  },
  "proof": {
    "type": "DataIntegrityProof",
    "cryptosuite": "eddsa-jcs-2022",
    "created": "2026-10-12T20:00:02Z",
    "verificationMethod": "did:key:z6MkExampleClaimantKey#z6MkExampleClaimantKey",
    "proofPurpose": "authentication",
    "proofValue": "zExampleBase58btcSignature"
  }
}
```

The Farm may hold this request for up to 25 seconds. Use an HTTP timeout of at
least 35 seconds and keep only one redeem request open for a handle.

## 8. Handle redeem responses

### Pending

While the browser decision or provisioning is unfinished, the Farm returns
HTTP 422 with a signed Trust Task error:

```json
{
  "type": "https://trusttasks.org/spec/trust-task-error/0.5",
  "issuer": "did:webvh:example-farm-did",
  "recipient": "did:key:z6MkExampleClaimantKey",
  "payload": {
    "code": "provision/vta/redeem:pending",
    "inResponseTo": {
      "typeUri": "https://trusttasks.org/spec/provision/vta/redeem/0.1",
      "id": "urn:uuid:e832ab4d-e027-4fb4-864c-c57b33e22f86"
    },
    "message": "claim is not ready for hand-over",
    "retryable": true,
    "details": {
      "state": "claimed",
      "confirmBy": "2026-10-12T20:05:01Z"
    }
  },
  "proof": {
    "type": "DataIntegrityProof",
    "cryptosuite": "eddsa-jcs-2022",
    "created": "2026-10-12T20:00:27Z",
    "verificationMethod": "did:webvh:example-farm-did#z6MkExampleFarmKey",
    "proofPurpose": "assertionMethod",
    "proofValue": "zExampleBase58btcSignature"
  }
}
```

`details.state` can be:

| State | Meaning |
| --- | --- |
| `claimed` | Waiting for the user to compare the codes |
| `confirmed` | Codes matched; Farm is starting the operation |
| `provisioning` | Farm is installing the administrator or waiting for the VTA |

For `retryable: true`, wait briefly and send a new signed redeem document.

### Terminal success

The link is complete only when the Farm returns HTTP 200 with:

```text
https://trusttasks.org/spec/provision/vta/redeem/0.1#response
```

Example:

```json
{
  "id": "urn:uuid:d1bf849d-44e3-4b78-b60d-0c1e3f1d8633",
  "threadId": "urn:uuid:a8e6cbd9-16d7-4668-93bb-b5300a65b6ac",
  "parentThreadId": "q3Vt7mXo9LwA2bR5cJ8kNg",
  "type": "https://trusttasks.org/spec/provision/vta/redeem/0.1#response",
  "issuer": "did:webvh:example-farm-did",
  "recipient": "did:key:z6MkExampleClaimantKey",
  "issuedAt": "2026-10-12T20:00:10Z",
  "payload": {
    "handle": "q3Vt7mXo9LwA2bR5cJ8kNg",
    "vtaDid": "did:webvh:example-vta-did",
    "operation": "provisionVta",
    "mediatorDid": "did:example:mediator"
  },
  "proof": {
    "type": "DataIntegrityProof",
    "cryptosuite": "eddsa-jcs-2022",
    "created": "2026-10-12T20:00:10Z",
    "verificationMethod": "did:webvh:example-farm-did#z6MkExampleFarmKey",
    "proofPurpose": "assertionMethod",
    "proofValue": "zExampleBase58btcSignature"
  }
}
```

Required payload fields:

| Field | Meaning |
| --- | --- |
| `handle` | Must equal the scanned handle |
| `vtaDid` | The VTA now administered by the mobile key |
| `operation` | `provisionVta` for a new VTA, or `grantAcl` for an existing VTA |

`mediatorDid` and `didLogUrl` are optional. After verifying the Farm signature
and correlation fields, store the VTA association and retain the administrator
key.

### Terminal rejection or expiry

These are signed `trust-task-error` responses with HTTP 422:

| Code | Meaning | Mobile action |
| --- | --- | --- |
| `provision/vta/redeem:rejected` | User selected **Not mine** | Delete the pending key and require a new QR |
| `provision/vta:handleExpired` | QR or confirmation window expired | Delete the pending key and require a new QR |
| `provision/vta:notClaimant` | Redeem used a different key | Stop; the original key or a new QR is required |

The old handle cannot be claimed again after rejection or expiry.

## 9. Other errors

| Code or HTTP status | Meaning | Action |
| --- | --- | --- |
| `provision/vta:keyUnsupported` | Claim issuer is not a supported Ed25519 `did:key` | Fix key generation and scan a new QR |
| `provision/vta:handleNotFound` | Handle does not exist | Request a new QR |
| `provision/vta/claim:alreadyClaimed` | Another key claimed the handle first | Compare the browser code; use **Not mine** and generate a new QR when it does not match |
| `malformedRequest` / HTTP 400 | Envelope or payload is invalid | Fix the request; do not retry unchanged |
| `proofRequired` or `proofInvalid` | Proof is missing or invalid | Fix signing; do not treat it as a network retry |
| `wrongRecipient` | `recipient` is not the Farm DID | Re-read `_from` and rebuild the request |
| `expired` | Trust Task `expiresAt` elapsed | Create a new document if the handle is still valid |
| `idConflict` / HTTP 409 | Same document ID was used with changed content | Use a new ID for a new document |
| `unavailable` / HTTP 503 | Temporary failure or another redeem poll is open | Retry with backoff when `retryable` is true |
| HTTP 415 | Content type is not `application/json` | Correct the request header |
| HTTP 429 | Client rate limit | Back off before retrying |
| HTTP 500 | Unexpected Farm failure | Retry conservatively |

For signed `trust-task-error` documents, use `payload.retryable` to decide
whether to retry. HTTP 422 is expected for `redeem:pending` and is not by itself
a terminal failure.

## 10. Retry rules

- If no response was received, resend the **exact same serialized signed
  document** with the same `id`.
- If a response says to retry, create a new document with a new `id`, current
  timestamps, and a new proof.
- Reusing an ID with changed JSON or a changed proof returns `idConflict`.
- HTTP 202 with an empty body means the identical request is still executing;
  wait and resend the same bytes.
- HTTP 204 with an empty body means a duplicate was absorbed without a retained
  response; for redeem, wait and begin a new poll.
- Never run two redeem polls for the same handle at the same time.

## 11. Minimum integration tests

1. **Success:** scan, claim, compare matching codes, redeem through pending,
   and receive the signed `#response`.
2. **Not mine:** claim, select **Not mine**, and receive
   `provision/vta/redeem:rejected` without storing the VTA.
3. **Expiry:** let the QR or confirmation window expire and receive
   `provision/vta:handleExpired`.
4. **Second scanner:** claim the same QR with two keys and confirm that only one
   succeeds while the other receives `provision/vta/claim:alreadyClaimed`.
5. **Wrong key:** claim with one key and redeem with another; expect
   `provision/vta:notClaimant`.
6. **Lost response:** resend the exact same signed bytes and confirm the Farm
   returns the retained result without executing the claim twice.

## References

- [`provision/vta/claim/0.1`](https://trusttasks.org/spec/provision/vta/claim/0.1)
- [`provision/vta/redeem/0.1`](https://trusttasks.org/spec/provision/vta/redeem/0.1)
- [Trust Tasks](https://trusttasks.org/)
