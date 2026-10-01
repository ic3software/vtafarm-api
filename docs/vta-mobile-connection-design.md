# VTA Mobile Connection Frontend Design

Status: initial and additional-device frontend and backend flows are implemented; automatic connection is labeled **Testing**. It becomes available whenever the cluster domain, signing key, and orchestrator are configured.

This design adds **Scan and Connect Manually** and **Automatic Mobile Connection** while retaining the local PNM flow. The same three methods appear when connecting the first administrator during VTA creation and when connecting another administrator to a running VTA. The manual option supports the current mobile app. The automatic option lets a compatible app scan a QR code, return an Admin DID, and complete VTA registration and connection without requiring the user to paste a DID into the browser.

This document defines the frontend flows, UI states, and implemented backend contract. Another team maintains the mobile app; its compatibility and end-to-end registration remain integration dependencies. Here, automatic registration means adding the VTA to the mobile app and completing the connection. VTA Farm continues to use the existing browser login session; this design introduces no new account login or single sign-on flow.

The partner mobile app is not required to link an identity or account to VTA Farm. The agreed callback uses the token from the QR code and the Admin DID the phone wants to connect. All target selection and one-time acceptance rules are enforced by the backend, without an additional browser approval step.

## Connection options and names

| Page option | Description | QR code contents | User actions | Mobile app dependency |
| --- | --- | --- | --- | --- |
| Local Connection | Use local PNM and paste the Admin DID it generates. | None | Run the local flow, paste the DID, and submit | None |
| Scan and Connect Manually | Scan with your phone, then paste the Admin DID from the app into this page. | VTA DID | Scan, copy the DID, paste, and submit | Assumes the current app can scan a VTA DID, as described in the requirements |
| Automatic Mobile Connection (Testing) | Scan and confirm on your phone to register and connect to the VTA automatically. | VTA DID and callback URL | Scan and complete the app's required confirmation | Requires callback support and the subsequent connection flow |

Scan and Connect Manually is a transitional option. It can be removed once the partner app supports the automatic flow and integration has been verified. Removal timing is a separate decision and does not affect Local Connection.

## Page placement and scope

Use one connection-card interaction in two contexts:

| Context | Entry point | Purpose | Completion |
| --- | --- | --- | --- |
| First administrator | `Create VTA → Admin DID` and the equivalent waiting session detail | Supply the first Admin DID required to continue provisioning | Advance to Deploy VTA, then Running |
| Additional administrator | `Agents → Select a running VTA → Connect another device` | Add another local PNM or mobile app to the VTA ACL | Restore the running VTA, then allow another device to be connected |

Both contexts show **Local Connection**, **Scan and Connect Manually**, and **Automatic Mobile Connection (Testing)**. Switching options replaces only the card contents, preserving the VTA name, deployment progress, DID, endpoints, ACL, and other details. Use a native radio group that supports keyboard selection and wraps on smaller screens.

The first-administrator context uses the existing points at which setup waits for an Admin DID:

| VTA type | State that allows connection setup | VTA DID source |
| --- | --- | --- |
| VTA Only | `vta_setup_complete` | `session.vta_did` |
| Full Stack | `awaiting_admin_did` | `session.collected.vta_did` |

If the VTA DID is missing or the session has not reached the required state, show “Preparing your VTA. You can connect when it is ready.” Do not generate a QR code. If external DID publication is pending, complete the existing publication step first.

For a running VTA, replace the separate “Link another PNM” form with the same connection card and retain its ACL list below the connection controls. Local and manual submissions continue to use the appropriate existing endpoint. Automatic requests do not identify an administrator type; the server derives the required work from the locked session lifecycle when it accepts the callback.

Keep Local Connection as the default. Label the automatic option **Automatic Mobile Connection (Testing)** at all times. Disable it only when the required server configuration is incomplete. Selecting Automatic Mobile Connection only reveals its controls. Create the bearer QR token only after the user selects **Generate QR code**; merely selecting the method or opening a page must not create one.

## Wireframe and UI copy

```text
Connect your first administrator / Connect another device

[ Local Connection ] [ Scan and Connect Manually ] [ Automatic Mobile Connection (Testing) ]

Scan and Connect Manually
  1. Scan the QR code below with your mobile app.
  2. Paste the Admin DID provided by the app into the field below.
  3. Select “Connect to VTA”, then follow the app's instructions.

  [ VTA DID QR code ]
  VTA DID: {complete DID, wrapping as needed}          [Copy]
  Admin DID: [ did:key:…                                  ]
                                           [Connect to VTA]

Automatic Mobile Connection
  [ QR code containing VTA DID and callback URL ]
  QR code expires in 04:59
  Waiting for confirmation from your phone…
  VTA DID: {complete DID, wrapping as needed}          [Copy]
                                    [Generate new QR code]
```

Both QR views show the selected VTA name and DID to help users verify the target. A QR size of at least 240 × 240 CSS pixels is recommended, with clear margins on all sides. Never truncate the encoded DID; verify scanning with the longest expected payload. Generate QR codes locally in the frontend rather than sending callback URLs to an external QR generation service.

The connection method selector must support keyboard navigation. Provide readable error and expiry messages for assistive technology instead of relying only on color. Update the visual countdown every second, but announce only significant state changes. This version assumes the browser and scanning phone are separate devices. A deep link for opening the app from a browser on the same phone can be designed after the app protocol is agreed.

## Scan and Connect Manually

1. When the user selects this option, encode the current VTA DID string directly into a QR code and provide a separate button to copy the DID.
2. The user scans with the existing app, which provides an Admin DID through its current flow.
3. The user pastes the Admin DID into the browser and selects “Connect to VTA”. Reuse the existing `did:key` format validation, trim leading and trailing whitespace, and show format errors below the field.
4. In the first-administrator context, call `POST /api/v1/setup/:id/admin`. In the additional-administrator context, call `POST /api/v1/setup/:id/admins`. Both bodies contain `{ "admin_did": "<DID generated by the mobile app>" }`. Disable duplicate submissions while the request is in progress.
5. Once the backend accepts the request, show context-specific progress: “Setting up your VTA” for the first administrator or “Adding administrator and restarting your VTA” for an additional administrator. An accepted API response does not mean the connection is complete.
6. When the VTA reaches or returns to `running`, show “Your VTA is ready. Follow the instructions in your mobile app to finish connecting.” The manual flow cannot confirm that the phone has actually connected, so do not show “Phone connected”.

This QR code contains only the DID, has no callback, and is not subject to the five-minute expiry rule. The browser cannot detect a scan and must not show a “Scanned” state. Keep the input after a failed submission so the user can correct it or retry. If the outcome is uncertain, read the session state before submitting again.

**Compatibility assumption:** avoiding app changes depends on the existing app being able to scan a DID and provide an Admin DID compatible with the current API. The app is outside this repository and has not been verified. Before integration, confirm its QR format and output on a real device; if it requires an existing wrapper format, adjust the encoding based on that result.

## Automatic Mobile Connection

### Normal flow

1. An authenticated user selects Automatic Mobile Connection. The browser asks the backend to create a connection request for the current user and VTA.
2. The backend returns the VTA DID, a one-time callback URL, a request ID, server time, and expiry time. The request is valid for five minutes from its creation on the server.
3. The browser generates the QR code and displays the countdown. The payload has only two application parameters: VTA DID and callback URL. The URL contains the one-time request information.
4. The app scans the code, identifies the VTA, obtains any required user confirmation, generates or selects an Admin DID, and submits it to the callback. Until the backend confirms receipt, the browser continues to show “Waiting for confirmation from your phone”.
5. In one database transaction, the backend verifies that the request is valid and unused, fixes the accepted Admin DID, consumes the one-time request, and derives the required operation from the locked session state. A session waiting at the setup gate continues VTA provisioning; a running session uses the serialized ACL grant flow. Only after commit may a worker start the derived operation. The browser removes the QR code and shows context-specific progress.
6. Once the VTA reaches or returns to `running`, the app uses the agreed progress mechanism to continue registration and connection locally. The user does not need to re-enter the VTA DID or callback, or return to the browser to submit data.
7. The app reports completion after connecting. Once the backend confirms success, the browser shows “Phone connected” and offers another connection. ACL refresh remains an explicit user action because it temporarily stops and restarts the VTA.

Administrators accepted through Automatic Mobile Connection are imported into the VTA ACL with the label `mobile integration`. Local and manual connection methods retain their existing PNM-oriented labels.

The user only scans and completes the necessary confirmation on the phone. They should not need to register the VTA again or complete a new VTA Farm browser login. App unlocking and identity confirmation continue to follow the app's own rules.

### Draft QR payload

The following JSON is a proposed integration format, not an agreement with the app team. All example values are placeholders.

```json
{
  "vta_did": "<VTA DID>",
  "callback_url": "https://api.example.com/api/v1/mobile-connections/callback/<one-time-token>"
}
```

The QR code contains no Admin DID, private key, or VTA Farm login credentials. The phone generates or selects the Admin DID. The callback targets a backend endpoint; a redirect to a browser page is not a substitute. The client cannot select or override the server-side operation.

The QR still includes `vta_did` so the app can identify the VTA. The callback URL contains only the opaque token as its request identifier; it does not contain a VTA DID. The backend stores an immutable mapping from that token to one VTA record and its DID, together with the initiating owner and expiry. The phone cannot change that mapping.

The proposed callback is `POST {callback_url}` with this body:

```json
{
  "admin_did": "<DID generated or selected by the mobile app>"
}
```

The URL supplies the token; the body supplies the Admin DID. Do not accept a client-selected `vta_did`, VTA ID, or owner ID as an override. Reject target override fields, and always resolve the target from the stored token mapping. The backend validates the Admin DID's supported format and length before accepting it.

### Five-minute expiry and automatic replacement

- The five-minute limit applies to the backend first accepting the phone's confirmation. Scanning before expiry is insufficient if acceptance occurs afterward. Check server time at the atomic acceptance step and reject a new acceptance when `now >= expires_at`. A retry of an already accepted callback follows the duplicate handling rules below and cannot authorize anything again.
- Calculate the countdown from `expires_at` and `server_time`. Resynchronize when the user returns to the tab rather than relying only on a browser counter that decrements every second.
- At expiry, immediately cover the old QR code and show that it has expired. The old code must no longer be usable.
- Do not generate a replacement automatically. Show a replacement action and wait for the user to select it. The new five-minute period starts at the replacement request's server creation time.
- If replacement fails, keep the expired state and offer “Retry QR generation”. Never make the old code appear valid again. Pause network retries while offline; when connectivity returns, synchronize state before replacing the code.
- Apply the same rules when the user selects “Generate new QR code”. The backend atomically invalidates the old request and creates its replacement. Allow at most one pending request per VTA, and synchronize older pages with the latest state.
- The backend resolves races between callbacks and replacement. If the callback is accepted first, the replacement operation returns the in-progress state. If the old request is invalidated first, reject its callback and ask the user to scan again in the app.
- Once a callback has been accepted within the validity window, do not replace it while server-side work is still running. After a request reaches `awaiting_mobile`, the owner may explicitly regenerate the QR code or switch connection methods. The backend then marks that mobile attempt cancelled, invalidating both its callback and progress credentials; completed provisioning or an administrator already written to the VTA ACL remains in place.

### Navigation and recovery

When switching to another VTA, immediately clear the old QR code, input, and timers to prevent mixing data between VTAs. Match every asynchronous result against the VTA and request ID, ignoring delayed responses from older requests.

Switching between automatic, local, and manual controls does not cancel a pending request. Returning to automatic mode must show the same unexpired QR without creating or replacing its token. If the user actually submits an Admin DID through a local or manual method, that accepted submission cancels the pending mobile request atomically. While an accepted request is provisioning, all options share the same progress and must not allow a second submission. A request may be abandoned after it reaches `awaiting_mobile`; switching methods must cancel that attempt before enabling the selected method.

After a refresh or return to the detail page, retrieve the current connection state from the backend. If a pending request is still valid, render its existing callback URL as the same QR code without creating or replacing a request. Follow the replacement flow only after it expires, restore provisioning progress if it has been accepted, or show the result if it has completed. Closing the tab must not depend on a successful unload cancellation: pending requests expire naturally within five minutes, and accepted provisioning can continue.

## Automatic connection UI states

The following connection request states are separate from the existing deployment `SetupSession.status`.

| State | Display | Available actions and next steps |
| --- | --- | --- |
| Creating `creating` | Generating QR code | Prevent duplicate creation |
| Pending confirmation `pending` | QR code, countdown, waiting for phone confirmation | Generate a new code or switch methods |
| Expired `expired` | Previous code expired; generating a replacement | Replace automatically; allow retry on failure |
| Cancelled `cancelled` | This connection request was cancelled | Return to the selected alternative method |
| Provisioning `provisioning` | Phone confirmation received; setting up VTA | Hide QR and follow backend progress |
| Waiting for phone `awaiting_mobile` | VTA is ready; waiting for the phone to finish connecting | Update automatically; prompt the user to resume in the app if it loses connectivity |
| Connected `connected` | Phone connected to this VTA | View VTA details |
| Failed `failed` | Error for the relevant stage and an actionable next step | Retry only as permitted by the backend; do not blindly restart provisioning |

If status cannot be retrieved, show “Unable to confirm connection status. Retrying” and retain the last confirmed progress. Do not infer success or failure. Synchronize once when the page opens, then poll every three seconds only for `pending`, `provisioning`, or `awaiting_mobile`. Stop polling when no request exists or it reaches a terminal state. Polling may pause while the page is hidden, but must synchronize immediately when visible again.

`running` means only that the VTA is ready. `connected` requires confirmation that the phone has completed registration and connection. If the partner app can only return an Admin DID, the browser can report no more than “VTA is ready”; the complete automatic connection flow has not passed acceptance.

## Frontend backend and mobile integration contract

The manual flow reuses the existing setup and running-VTA APIs. Automatic requests use one mobile protocol and one current-request stream in both contexts. The backend selects the operation from session state when it accepts the callback; a running VTA uses the serialized ACL maintenance worker.

| Caller | Interface | Behavior |
| --- | --- | --- |
| Browser | `POST /api/v1/setup/:id/mobile-connections` | Verify authentication, VTA ownership, and session eligibility; create a request or return the existing valid request on repeated calls |
| Browser | `GET /api/v1/setup/:id/mobile-connections/current` | Restore or poll the current request; return an empty state if none exists |
| Browser | `POST /api/v1/setup/:id/mobile-connections/:request_id/refresh` | Regenerate the QR by replacing a pending request atomically, or abandon a request at `awaiting_mobile`; an older request ID must not invalidate a newer request. This does not refresh the ACL |
| Browser | `DELETE /api/v1/setup/:id/mobile-connections/:request_id` | Cancel a pending request, or abandon a request at `awaiting_mobile`, before changing methods |
| Mobile app | `POST {callback_url}` | Supply the token in the URL and `admin_did` in the body; atomically accept the request and let the server select setup provisioning or an ACL grant from session state |
| Mobile app | `GET /api/v1/mobile-connections/:request_id` and `POST /api/v1/mobile-connections/:request_id/complete` | Use the separate progress bearer token to retrieve VTA readiness and report completed mobile registration |

The browser's request data must include at least `request_id`, `status`, `vta_did`, `server_time`, and `expires_at`. Return `callback_url` only while `pending`. After acceptance, return displayable provisioning progress; on failure, return an identifiable error code and retry capabilities. The frontend must not generate authorization tokens or decide their validity period.

The callback must not depend on the phone having a VTA Farm account binding or browser cookie. The agreed authorization uses a high-entropy, one-time token bound on the server to the initiating user and one VTA. No separate DID signature or proof-of-control exchange is required by this callback contract. Scanning or making an HTTP GET request must not directly grant administrator access.

This makes possession of a valid QR token the authority to nominate an Admin DID for the mapped VTA. The immutable mapping prevents changing the target VTA, but does not establish that the scanning phone belongs to the owner. Someone who obtains an unused token could submit their own DID first; the five-minute window and one-time use limit that exposure without eliminating it. This is the trust boundary of the agreed flow.

### Atomic acceptance in the database

Checking that a token is unused and marking it used in separate operations is insufficient. Two concurrent requests can both pass the check before either updates the token. A disabled browser button cannot prevent mobile retries, and a process-local mutex cannot coordinate separate API replicas.

Use a database transaction with row locking or conditional updates to enforce the following steps as one acceptance operation:

1. Resolve the token to its stored request and VTA. Validate the supplied Admin DID, then inspect any previously accepted result for duplicate handling.
2. For a new acceptance, verify that the token is pending, unexpired, and not revoked, and inspect the locked VTA lifecycle state.
3. Persist the accepted Admin DID and derive `provision_vta` when the session is waiting at its setup gate, or `grant_acl` when it is already running. These are backend work operations, not administrator types, and the client cannot select them.
4. Consume the token and invalidate other pending requests for the same VTA.
5. Commit the accepted request together with the setup-session transition or ACL work marker. If any step fails, roll back the entire acceptance; do not start external work.

Only the transaction that successfully locks and claims the VTA may create the logical operation. A competing transaction must reread the committed state and return the existing result or a conflict. The database allows only one pending request and one unfinished accepted request per session so correctness holds across API replicas and restarts.

Callbacks, QR replacement, cancellation, and existing local/manual submission endpoints must use the same VTA-level acceptance rules. Token-level protection alone is insufficient because manual submissions and replacement tokens are different entry points to the same initial setup. Replacement or cancellation can invalidate only a pending request; they cannot undo an accepted operation or alter its Admin DID.

### Duplicate handling

| Request | Backend behavior |
| --- | --- |
| First valid token and Admin DID | Atomically accept and create one durable provisioning operation |
| Same accepted token and same validated Admin DID | Return the existing operation reference and status; do not create new work or grant access again |
| Same accepted token and a different Admin DID | Return a conflict; preserve the original DID and operation |
| Expired or revoked token that was never accepted | Reject and require a new scan |
| A different request or manual submission after initial connection was claimed | Return the existing setup state or a conflict; never replace the accepted DID |

Retain accepted request metadata for a defined retry period so that a lost callback response does not strand a successful acceptance. Expiry of the original five-minute window prevents new acceptance; it does not reverse acceptance or restart the window for a duplicate. A duplicate response acknowledges the existing operation only and does not renew mobile progress credentials. Accepted callback retries are supported for 24 hours after acceptance. The original progress credential expires one hour after acceptance and is never renewed by a retry.

### Restart recovery and provisioning retries

Persisting the accepted request and its pending work in the same transaction closes the gap between consuming a token and starting provisioning. If the API crashes before commit, no acceptance is recorded and a valid request can retry. If it crashes after commit, a worker discovers the saved work and continues the same operation without another scan.

Workers must coordinate through durable claims and resume safely after a crash. A database transaction cannot make external Kubernetes or VTA actions execute exactly once: a worker can lose its response after an action succeeded. Use stable operation and resource identifiers, inspect existing results, and make provisioning steps safe to retry. Retries must preserve the original Admin DID and must not create duplicate grants. An uncertain outcome must be reconciled before repeating the action.

Both `provisionAdmin` and the mobile callback lock the same `setup_sessions` row. The setup session's Admin DID and lifecycle status are the durable record for setup provisioning; no separate first-administrator table exists. An accepted mobile row stores the server-derived operation needed for callback recovery. Process-local worker tracking is only an optimization; a PostgreSQL advisory lock on a dedicated connection excludes other setup provisioning workers for the same session.

Mobile progress queries and completion reports require a separate credential scoped to that connection. Do not extend the consumed callback token into indefinite authorization. The progress credential lasts one hour from acceptance, allowing setup beyond the five-minute QR window. The backend authenticates completion with this request-scoped credential and requires a running VTA. Completion is a report from the app, not independent verification of its transport connection; the browser cannot declare success independently.

The callback URL contains temporary authorization information. Do not put it in analytics events, general application logs, or localStorage; backend and proxy logs must also redact the token. The app must accept only agreed HTTPS callback origins. These requirements are part of automatically granting administrator access and must be implemented during integration.

## Frontend implementation scope

Use one shared connection card in the Create VTA Admin DID step, the equivalent waiting session detail, and the running-session additional-device section. `VtaConnectionCard` provides the three options, local QR rendering, server-based countdown, expiry replacement, and connection states. Automatic flow testing uses partner test builds or controlled callbacks; production enablement requires partner acceptance.

| Existing file | Design integration |
| --- | --- |
| [CreateVTAView.tsx](../../vtafarm/src/pages/portal/CreateVTAView.tsx) | Show the shared connection card at the VTA-only Admin DID step instead of a local-only form |
| [FullStackCreateProgress.tsx](../../vtafarm/src/pages/portal/FullStackCreateProgress.tsx) | Show the shared connection card at the full-stack Admin DID step |
| [SessionDetailView.tsx](../../vtafarm/src/pages/portal/SessionDetailView.tsx) | Show the shared card for both a waiting first administrator and a running VTA |
| [portalUtils.tsx](../../vtafarm/src/pages/portal/portalUtils.tsx) | Reuse `isValidAdminDid` and existing display conventions |
| [api.ts](../../vtafarm/src/lib/api.ts) | Route local/manual submissions to their existing endpoints and expose one automatic owner request stream |
| [SessionPnmCard.tsx](../../vtafarm/src/pages/portal/SessionPnmCard.tsx) | Retain the ACL display, but move additional-device connection controls into the shared card |

The existing backend distinguishes setup provisioning in `provisionAdmin` in [setup.go](../internal/handler/setup.go) from adding an administrator in [setup_admins.go](../internal/handler/setup_admins.go). These remain separate backend operations even though every Admin DID has the same meaning and the UI/mobile transport are shared. The callback derives which operation is necessary from the locked session state.

## Acceptance scenarios

| Scenario | Expected result |
| --- | --- |
| Reach the Admin DID step during creation | All three connection methods are available without leaving the creation flow |
| Open a running VTA and connect another device | The same three methods are available above the existing ACL list |
| Switch among all three methods | Content changes within the same card; QR always belongs to the current VTA |
| Use the existing local flow | Existing submission and provisioning complete normally |
| Scan the manual QR with the existing app | App recognizes the VTA and provides a usable Admin DID without an app update |
| Submit a local or manual DID for a running VTA | Use the additional-admin endpoint, restart the VTA safely, refresh its ACL, and leave prior administrators intact |
| Manual DID format is invalid or submission fails | Show an actionable error, retain input, and prevent duplicate submissions |
| Complete the automatic connection flow | No DID pasting after scan and confirmation; app registers automatically when VTA is ready, and the browser eventually shows connected |
| Complete an automatic additional-device flow | Record durable ACL work, return the VTA to running, let the app finish connecting, and permit a later request for another device |
| Callback accepted but VTA not ready | Show provisioning, not connected |
| Five minutes pass without confirmation | Mark the old code expired, automatically show a replacement with a new five-minute countdown, and prompt a new scan |
| First callback for an old code arrives after expiry | Backend rejects it; app requests another scan; it cannot be applied to the replacement request |
| Callback accepted before expiry, but provisioning exceeds five minutes | Continue the same flow without replacing the code or requiring another scan |
| Expiry and callback occur together | Backend atomic processing decides the result; at most one logical provisioning operation is accepted |
| Duplicate callback or actions from two tabs | No duplicate authorization or DID overwrite; pages synchronize the valid request or current progress |
| Callback supplies a VTA DID, VTA ID, or owner override | Reject the override; the stored token mapping remains authoritative |
| Same accepted token is retried with the same Admin DID within the retry retention period | Return the existing operation and status, including after the original QR expiry; no new authorization or work |
| Same accepted token is retried with a different Admin DID | Return a conflict and retain the original Admin DID |
| Competing callbacks reach different API replicas | Database acceptance allows one initial connection operation, independent of process-local locks |
| Callback races with a local or manual submission | Both use the same VTA-level claim; only one Admin DID is accepted |
| API crashes before the acceptance transaction commits | No partial token consumption or work record; retry is possible if the token remains valid |
| API crashes after commit but before provisioning starts | Recover the saved operation and continue without another scan |
| Worker crashes after an external action succeeds | Reconcile and resume the same operation without duplicate grants or DID replacement |
| Phone confirms while the user switches to manual mode | Backend decides which operation is valid; the page must not submit another DID |
| QR replacement fails, connection goes offline, or tab sleeps | Never present an expired code as valid; synchronize before resuming |
| Refresh, return to the page, or switch VTAs | Restore the correct backend state and ignore delayed responses from older views |
| Phone never reports completion or app closes | Show at most VTA ready or waiting for phone; prompt the user to resume in the app |
| Automatic capability is unavailable | Keep **Automatic Mobile Connection (Testing)** visible but disabled; never present simulated success as a real result |

## Questions to resolve before integration

1. The existing app's supported QR format, Admin DID format, and whether the user must select a continue action after authorization. These determine the precise copy for the manual flow.
2. The QR payload encoding and callback transport/error details accepted by the app team. The application inputs are fixed: token in the callback URL and `admin_did` in the body, with no VTA Farm account binding or client-selected target.
3. Partner adoption of the progress and completion protocol below, including recovery after the app closes.
4. Partner acceptance of the one-hour progress lifetime and 24-hour callback retry window. Their expiry never revokes VTA provisioning or authorizes another initial Admin DID.

The complete Automatic Mobile Connection feature requires acceptance with the actual partner app. Local tests cannot verify that the app registers a VTA or supports the QR payloads.


## Implemented API and operation

The complete request and response definitions are in [OpenAPI](../internal/apidocs/openapi.yaml). Owner endpoints use the existing Farm user cookie and verify the selected VTA's ownership. Mobile endpoints do not require a Farm account or cookie.

### Mobile protocol

1. Decode the automatic QR as JSON with exactly `vta_did` and `callback_url`. The app must verify the configured HTTPS callback origin before making a request; it should show the VTA identity when asking for confirmation.
2. POST `{ "admin_did": "<Ed25519 did:key>" }` to the callback URL. The backend trims surrounding whitespace, validates the Ed25519 key encoding, limits the DID to 128 characters, and rejects unknown body fields. Callback and completion bodies have a 4 KiB limit. GET never consumes a callback.
3. A successful callback returns HTTP 202 with `connection`, `server_time`, `progress_token`, `progress_url`, `completion_url`, and `progress_expires_at`. Save the accepted request and scoped credential securely in the app. If the response was lost, retry with the same token and Admin DID. A duplicate returns the same operation and credential, with the original expiration; a different DID returns 409.
4. GET `progress_url` with `Authorization: Bearer <progress_token>`. Poll approximately every three seconds. `provisioning` means setup is unfinished; `awaiting_mobile` means the VTA is ready for the app's existing registration flow. Resolve and connect to the returned VTA DID using the app's supported VTA protocol.
5. Only after registration and connection succeed, POST `{ "status": "connected" }` to `completion_url` using the same bearer credential. Completion before the VTA reaches `running` returns 409. Repeated valid completion reports are idempotent; the browser then shows `connected`.

Owner status responses contain `{ enabled, server_time, connection }`, with `connection: null` when no request exists. Mobile progress responses contain `{ server_time, connection }`. Connection views contain `request_id`, `status`, `vta_did`, `expires_at`, and an optional safe `error`. Only an owner view of a valid pending request contains `callback_url`. Browser responses never include the internal operation or mobile progress credentials.

| Limit or response | Implemented behavior |
| --- | --- |
| QR acceptance | Five minutes from database creation time; database clock checked after acquiring the VTA lock |
| Accepted callback retry | 24 hours from acceptance; HTTP 202 acknowledges existing work |
| Mobile progress and completion | One hour from acceptance; expired credentials return 410 |
| Progress timeout while VTA runs | Show mobile confirmation expired; preserve the configured VTA and finish manually in the app |
| Callback retry after progress expiry | Return the accepted state without issuing a progress token or renewing its lifetime |
| Invalid DID or body | 400; unknown target override fields are rejected |
| Unknown or invalid callback token | 404 |
| Invalid progress token or wrong request scope | 401 |
| Competing DID, stale refresh, or VTA not ready | 409 |
| Expired or cancelled unused callback | 410; scan the current QR again |
| QR replacement cooldown | Five seconds per VTA, enforced in the database; 429 with `Retry-After: 5` |
| Endpoint rate limit | 120 requests per minute per IP per route group, per API replica; this is abuse throttling, not the one-time authorization guarantee |

The API stores a random request UUID and immutable VTA mapping. Callback and progress credentials use separate HMAC-SHA256 purposes with a dedicated server secret, so a callback credential cannot authenticate a progress request. Credentials are regenerated from the stored request without persisting raw tokens. Deleting a session removes its request and work records through foreign-key cascades.

### Deployment configuration

Apply migration `000039_mobile_connections` before running this API version. It creates one mobile connection schema for both setup-time and running-VTA connections. Setup provisioning remains durable in `setup_sessions`; there is no separate first-administrator table.

| Configuration | Purpose |
| --- | --- |
| `MOBILE_CONNECTION_SIGNING_KEY` | Dedicated cryptographically random secret with at least 32 bytes; supply through the existing application Secret, never Helm values or a committed environment file |
| `ORCHESTRATOR_RESUME` | Enable in the API deployment for restart recovery and the durable queue scan; keep disabled for local development against the shared database |

The public callback origin is derived from the existing cluster configuration as `https://vtafarm-api.<CLUSTER_DOMAIN>`; it is not configured separately. All API replicas must share the same signing key. Changing the key invalidates outstanding QR and progress credentials; it does not undo accepted provisioning. A valid signing key and available Kubernetes orchestrator enable the capability directly; there is no separate feature flag. Invalid or incomplete configuration leaves it unavailable. The existing application Secret supplies the signing key. Example files contain placeholders only.

The API sets `Cache-Control: no-store`, omits mobile credential paths from access logs, and uses sanitized panic handling on the public mobile routes. The reverse proxy and any external tracing or analytics must also suppress callback tokens and authorization headers before enablement. Do not log response bodies carrying these credentials. HTTPS origin validation is not an app identity binding: possession of an unused callback token still authorizes nomination of the initial Admin DID, as agreed above.

### Durable provisioning and deletion

Acceptance commits the Admin DID, consumed request, and server-derived operation in one transaction. The queues check unfinished work every ten seconds and also start immediately after acceptance. Setup provisioning uses a dedicated PostgreSQL advisory lock and heartbeat. ACL grants use the existing cross-replica VTA maintenance lock, idempotently probe the ACL before importing, synchronize the ACL snapshot, and wait for the VTA to restart before reporting `awaiting_mobile`. Kubernetes Jobs use stable session-based names; authorization and post-gate setup Jobs retain their completion receipts until session teardown rather than expiring after an hour.

Deleting a VTA locks the same session, marks its lifecycle as deleting, and cancels unfinished QR requests. This prevents a delayed callback or manual submission from starting work during teardown and stops a setup worker on another replica through its heartbeat. Deletion waits for an active setup provisioning worker to release its database lock before removing resources. If that wait or the database update fails, deletion reports an error and can be retried.

The existing provisioning pipeline still owns Kubernetes and VTA behavior. This implementation does not claim exactly-once external execution under arbitrary cluster failure or manual deletion of retained Jobs. Lost external state requires reconciliation; a failed setup does not automatically authorize another DID.

### Verification

Run `make test` in the API repository and `pnpm lint` / `pnpm build` in the frontend repository. For database transaction and recovery tests, set `VTAFARM_TEST_DATABASE_URL` to an isolated PostgreSQL URL; the test helper creates a temporary schema and applies the real migrations. Without that variable, PostgreSQL integration tests explicitly skip. Do not use the shared development or production database for these tests.

The integration suite covers competing Admin DIDs, callback versus manual acceptance, replacement and expiry, rollback, ownership, credential scope, callback retry windows, authenticated completion, crash recovery, replica exclusion, and teardown. Kubernetes fake-client tests verify retained authorization Jobs survive repeated creation without changing the accepted command. Browser checks cover manual submission, disabled automatic capability, expiry and replacement, cancellation, retry behavior, readiness versus mobile completion, VTA switching, and narrow layout using mocked partner responses. Actual partner-app scanning and live cluster provisioning remain deployment acceptance checks.
