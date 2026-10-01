# Mobile Connection App API

Local base URL: `http://localhost:8080`

## QR code payload

The Automatic Mobile Connection QR code displayed by VTA Farm contains this JSON:

```json
{
  "vta_did": "did:webvh:example:dev-dids.firstperson.dev:myvta-vta",
  "callback_url": "https://vtafarm-api.example.com/api/v1/mobile-connections/callback/6f7c7198-ab23-4599-94ba-092b3ff93bef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
}
```

VTA Farm generates the complete `callback_url`, including its signed callback credential. The app must use this URL exactly as received; it does not replace, construct, or modify any part of it. The value above is a non-working example.

The remaining examples use these placeholders:

- `<REQUEST_ID>`: the UUID returned by the callback
- `<PROGRESS_TOKEN>`: the `progress_token` returned by the callback
- `<ADMIN_DID>`: the phone's Ed25519 `did:key`

## 1. Submit the Admin DID

```http
POST {callback_url from the QR code}
Content-Type: application/json

{
  "admin_did": "<ADMIN_DID>"
}
```

```bash
CALLBACK_URL='<exact callback_url decoded from the QR code>'

curl -X POST \
  "$CALLBACK_URL" \
  -H 'Content-Type: application/json' \
  -d '{"admin_did":"<ADMIN_DID>"}'
```

Example `202 Accepted` response:

```json
{
  "connection": {
    "request_id": "6f7c7198-ab23-4599-94ba-092b3ff93bef",
    "status": "provisioning",
    "vta_did": "did:webvh:example:dev-dids.firstperson.dev:myvta-vta",
    "expires_at": "2026-09-30T22:05:00Z"
  },
  "server_time": "2026-09-30T22:00:10Z",
  "progress_token": "<PROGRESS_TOKEN>",
  "progress_url": "https://vtafarm-api.example.com/api/v1/mobile-connections/6f7c7198-ab23-4599-94ba-092b3ff93bef",
  "completion_url": "https://vtafarm-api.example.com/api/v1/mobile-connections/6f7c7198-ab23-4599-94ba-092b3ff93bef/complete",
  "progress_expires_at": "2026-09-30T23:00:10Z"
}
```

Save `request_id` and `progress_token`. Use the returned `progress_url` and `completion_url` outside local testing. The callback token cannot authenticate the next two endpoints.

Errors to handle:

| Status | Example response | App action |
| --- | --- | --- |
| `400` | `{"error":"admin_did must be a valid Ed25519 did:key","reason":"invalid_admin_did"}` | Correct the Admin DID; do not retry unchanged input |
| `400` | `{"error":"Invalid connection request body."}` | Send one JSON object containing only `admin_did` |
| `404` | `{"error":"Connection not found."}` | Stop and request a new QR code |
| `409` | `{"error":"connection already claimed or VTA not ready","reason":"connection_conflict"}` | Stop this attempt; another administrator may have claimed it, or the VTA is not ready |
| `410` | `{"error":"connection request expired or cancelled; scan a new QR code","reason":"connection_expired"}` | Stop and scan a new QR code |
| `429` | `{"error":"too many requests — try again later"}` | Retry with backoff |
| `503` | `{"error":"Automatic mobile connection is not available."}` | Stop and show service unavailable |
| `500` | `{"error":"Unable to update connection. Please retry.","reason":"connection_unavailable"}` | Retry with backoff; show an error if it persists |

## 2. Poll connection progress

```http
GET /api/v1/mobile-connections/<REQUEST_ID>
Authorization: Bearer <PROGRESS_TOKEN>
```

```bash
curl \
  'http://localhost:8080/api/v1/mobile-connections/<REQUEST_ID>' \
  -H 'Authorization: Bearer <PROGRESS_TOKEN>'
```

Example `200 OK` response when the VTA is ready:

```json
{
  "server_time": "2026-09-30T22:01:30Z",
  "connection": {
    "request_id": "6f7c7198-ab23-4599-94ba-092b3ff93bef",
    "status": "awaiting_mobile",
    "vta_did": "did:webvh:example:dev-dids.firstperson.dev:myvta-vta",
    "expires_at": "2026-09-30T22:05:00Z"
  }
}
```

Poll approximately every three seconds:

- `provisioning`: keep polling
- `awaiting_mobile`: connect to the VTA, then call the completion endpoint
- `connected`: finished
- `failed`: stop and display `connection.error`

Errors to handle:

| Status | Example response | App action |
| --- | --- | --- |
| `401` | `{"error":"Invalid mobile progress credential."}` | Stop; the credential is invalid for this request |
| `404` | `{"error":"Connection or VTA not found.","reason":"connection_not_found"}` | Stop; the connection was deleted |
| `410` | `{"error":"connection request expired or cancelled; scan a new QR code","reason":"connection_expired"}` | Stop; the progress credential expired or was revoked |
| `429` | `{"error":"too many requests — try again later"}` | Poll again with backoff |
| `500` | `{"error":"Unable to update connection. Please retry.","reason":"connection_unavailable"}` | Retry with backoff; show an error if it persists |

## 3. Report completion

Call this only after the app has successfully registered and connected to the VTA.

```http
POST /api/v1/mobile-connections/<REQUEST_ID>/complete
Authorization: Bearer <PROGRESS_TOKEN>
Content-Type: application/json

{
  "status": "connected"
}
```

```bash
curl -X POST \
  'http://localhost:8080/api/v1/mobile-connections/<REQUEST_ID>/complete' \
  -H 'Authorization: Bearer <PROGRESS_TOKEN>' \
  -H 'Content-Type: application/json' \
  -d '{"status":"connected"}'
```

Example `200 OK` response:

```json
{
  "server_time": "2026-09-30T22:02:00Z",
  "connection": {
    "request_id": "6f7c7198-ab23-4599-94ba-092b3ff93bef",
    "status": "connected",
    "vta_did": "did:webvh:example:dev-dids.firstperson.dev:myvta-vta",
    "expires_at": "2026-09-30T22:05:00Z"
  }
}
```

If the response is lost, the app can safely retry this request.

Errors to handle:

| Status | Example response | App action |
| --- | --- | --- |
| `400` | `{"error":"status must be connected"}` | Fix the request body; only `connected` is accepted |
| `401` | `{"error":"Invalid mobile progress credential."}` | Stop; the credential is invalid for this request |
| `404` | `{"error":"Connection or VTA not found.","reason":"connection_not_found"}` | Stop; the connection was deleted |
| `409` | `{"error":"connection already claimed or VTA not ready","reason":"connection_conflict"}` | Keep polling; call complete only after `awaiting_mobile` |
| `410` | `{"error":"connection request expired or cancelled; scan a new QR code","reason":"connection_expired"}` | Stop; the progress credential expired or was revoked |
| `429` | `{"error":"too many requests — try again later"}` | Retry with backoff |
| `500` | `{"error":"Unable to update connection. Please retry.","reason":"connection_unavailable"}` | Retry with backoff; show an error if it persists |
