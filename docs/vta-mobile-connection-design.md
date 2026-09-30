# VTA Mobile Connection Frontend Design

Status: design draft, not implemented.

This design adds **Scan and Connect Manually** and **Automatic Mobile Connection** to the existing VTA detail page while retaining the local connection flow. The manual option supports the current mobile app. The automatic option lets a compatible app scan a QR code, return an Admin DID, and complete VTA registration and connection without requiring the user to paste a DID into the browser.

This phase defines frontend flows, UI states, and a draft integration contract. Another team maintains the mobile app; new backend and app capabilities are dependencies, not existing features. Here, automatic registration means adding the VTA to the mobile app and completing the connection. VTA Farm continues to use the existing browser login session; this design introduces no new account login or single sign-on flow.

The partner mobile app is not required to link an identity or account to VTA Farm. The agreed callback uses the token from the QR code and the Admin DID the phone wants to connect. All target selection and one-time acceptance rules are enforced by the backend, without an additional browser approval step.

## Connection options and names

| Page option | Description | QR code contents | User actions | Mobile app dependency |
| --- | --- | --- | --- | --- |
| Local Connection | Use local PNM and paste the Admin DID it generates. | Existing behavior | Run the local flow, paste the DID, and submit | None |
| Scan and Connect Manually | Scan with your phone, then paste the Admin DID from the app into this page. | VTA DID | Scan, copy the DID, paste, and submit | Assumes the current app can scan a VTA DID, as described in the requirements |
| Automatic Mobile Connection | Scan and confirm on your phone to register and connect to the VTA automatically. | VTA DID and callback URL | Scan and complete the app's required confirmation | Requires callback support and the subsequent connection flow |

Scan and Connect Manually is a transitional option. It can be removed once the partner app supports the automatic flow and integration has been verified. Removal timing is a separate decision and does not affect Local Connection.

## Page placement and scope

Keep the existing entry point: `Agents → Select a VTA → Detail page`. Add the three options inside the current connection card. Switching options replaces only the card contents, preserving the VTA name, deployment progress, DID, endpoints, and other details. Use tabs on desktop; a full-width selector is an option for smaller screens.

This design initially covers the first connection, using the existing points at which setup waits for an Admin DID:

| VTA type | State that allows connection setup | VTA DID source |
| --- | --- | --- |
| VTA Only | `vta_setup_complete` | `session.vta_did` |
| Full Stack | `awaiting_admin_did` | `session.collected.vta_did` |

If the VTA DID is missing or the session has not reached the required state, show “Preparing your VTA. You can connect when it is ready.” Do not generate a QR code. If external DID publication is pending, complete the existing publication step first.

Running VTAs already have a separate “Link another PNM” feature. Preserve it in this phase. Extending the mobile QR flow to add another administrator is future scope and must not directly reuse the initial provisioning endpoint.

Initially, keep Local Connection as the default. Until the backend and app support the automatic flow, label that option “Coming soon” and disable it; design previews may simulate the full flow. Once support is available, Automatic Mobile Connection is the recommended default.

## Wireframe and UI copy

```text
Connect to your VTA
Selected VTA: {VTA name}

[ Local Connection ] [ Scan and Connect Manually ] [ Automatic Mobile Connection ]

Scan and Connect Manually
  1. Scan the QR code below with your mobile app.
  2. Paste the Admin DID provided by the app into the field below.
  3. Select “Connect to VTA”, then follow the app's instructions.

  [ VTA DID QR code ]
  VTA DID: {complete DID, wrapping as needed}          [Copy]
  Admin DID: [ did:key:…                                  ]
                                           [Connect to VTA]

Automatic Mobile Connection
  Scan with a compatible mobile app and confirm on your phone.
  You do not need to return here to paste an Admin DID.

  [ QR code containing VTA DID and callback URL ]
  QR code expires in 04:59
  Waiting for confirmation from your phone…
  VTA DID: {complete DID, wrapping as needed}          [Copy]
                                    [Generate new QR code]
```

Both QR views show the selected VTA name and DID to help users verify the target. A QR size of at least 240 × 240 CSS pixels is recommended, with clear margins on all sides. Never truncate the encoded DID; verify scanning with the longest expected payload. Generate QR codes locally in the frontend rather than sending callback URLs to an external QR generation service.

Tabs must support keyboard navigation. Provide readable error and expiry messages for assistive technology instead of relying only on color. Update the visual countdown every second, but announce only significant state changes. This version assumes the browser and scanning phone are separate devices. A deep link for opening the app from a browser on the same phone can be designed after the app protocol is agreed.

## Scan and Connect Manually

1. When the user selects this option, encode the current VTA DID string directly into a QR code and provide a separate button to copy the DID.
2. The user scans with the existing app, which provides an Admin DID through its current flow.
3. The user pastes the Admin DID into the browser and selects “Connect to VTA”. Reuse the existing `did:key` format validation, trim leading and trailing whitespace, and show format errors below the field.
4. Reuse `POST /api/v1/setup/:id/admin` with `{ "admin_did": "<DID generated by the mobile app>" }`. Disable duplicate submissions while the request is in progress.
5. Once the backend accepts the request, show “Setting up your VTA” and follow the existing session updates until the VTA is ready. An accepted API response does not mean the connection is complete.
6. When the VTA reaches `running`, show “Your VTA is ready. Follow the instructions in your mobile app to finish connecting.” The current flow cannot confirm that the phone has actually connected, so do not show “Phone connected”.

This QR code contains only the DID, has no callback, and is not subject to the five-minute expiry rule. The browser cannot detect a scan and must not show a “Scanned” state. Keep the input after a failed submission so the user can correct it or retry. If the outcome is uncertain, read the session state before submitting again.

**Compatibility assumption:** avoiding app changes depends on the existing app being able to scan a DID and provide an Admin DID compatible with the current API. The app is outside this repository and has not been verified. Before integration, confirm its QR format and output on a real device; if it requires an existing wrapper format, adjust the encoding based on that result.

## Automatic Mobile Connection

### Normal flow

1. An authenticated user selects Automatic Mobile Connection. The browser asks the backend to create a connection request for the current user and VTA.
2. The backend returns the VTA DID, a one-time callback URL, a request ID, server time, and expiry time. The request is valid for five minutes from its creation on the server.
3. The browser generates the QR code and displays the countdown. The payload has only two application parameters: VTA DID and callback URL. The URL contains the one-time request information.
4. The app scans the code, identifies the VTA, obtains any required user confirmation, generates or selects an Admin DID, and submits it to the callback. Until the backend confirms receipt, the browser continues to show “Waiting for confirmation from your phone”.
5. In one database transaction, the backend verifies that the request is valid and unused, claims the VTA's initial connection, fixes the accepted Admin DID, consumes the one-time request, and records durable provisioning work. Only after commit may a worker start the provisioning flow. The browser shows “Phone confirmation received. Setting up your VTA”, removes the QR code, and stops the countdown, including while the durable work is waiting to start.
6. Once the VTA is ready, the app uses the agreed progress mechanism to continue registration and connection locally. The user does not need to re-enter the VTA DID or callback, or return to the browser to submit data.
7. The app reports completion after connecting. Once the backend confirms success, the browser shows “Phone connected” and offers “View VTA details”.

The user only scans and completes the necessary confirmation on the phone. They should not need to register the VTA again or complete a new VTA Farm browser login. App unlocking and identity confirmation continue to follow the app's own rules.

### Draft QR payload

The following JSON is a proposed integration format, not an agreement with the app team. All example values are placeholders.

```json
{
  "vta_did": "<VTA DID>",
  "callback_url": "https://api.example.com/api/v1/mobile-connections/callback/<one-time-token>"
}
```

The QR code contains no Admin DID, private key, or VTA Farm login credentials. The phone generates or selects the Admin DID. The callback targets a backend endpoint; a redirect to a browser page is not a substitute.

The QR still includes `vta_did` so the app can identify the VTA. The callback URL contains only the opaque token as its request identifier; it does not contain a VTA DID. The backend stores an immutable mapping from that token to one VTA record and its DID, together with the initiating owner, purpose, and expiry. The phone cannot change that mapping.

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
- At expiry, immediately cover the old QR code and show “This QR code has expired. Generating a new QR code.” The old code must no longer be usable.
- First check whether the request has already been accepted. If it has not, automatically request a replacement from the backend. After showing the new code, retain the message “The previous QR code has expired. Please scan the new QR code.” The new five-minute period starts at the replacement request's server creation time.
- If replacement fails, keep the expired state and offer “Retry QR generation”. Never make the old code appear valid again. Pause network retries while offline; when connectivity returns, synchronize state before replacing the code.
- Apply the same rules when the user selects “Generate new QR code”. The backend atomically invalidates the old request and creates its replacement. Allow at most one pending request per VTA, and synchronize older pages with the latest state.
- The backend resolves races between callbacks and replacement. If the callback is accepted first, the replacement operation returns the in-progress state. If the old request is invalidated first, reject its callback and ask the user to scan again in the app.
- Once a callback has been accepted within the validity window, do not replace the code, cancel setup, or require another scan even if provisioning takes longer than five minutes.

### Navigation and recovery

When switching to another VTA, immediately clear the old QR code, input, and timers to prevent mixing data between VTAs. Match every asynchronous result against the VTA and request ID, ignoring delayed responses from older requests.

When switching from automatic mode to local or manual mode while confirmation is pending, cancel the pending request before allowing another submission. If cancellation fails, synchronize state and offer a retry. Once the callback has been accepted, all options share the same setup progress and must not allow a second submission.

After a refresh or return to the detail page, retrieve the current connection state from the backend. Show the current QR if it is still valid, follow the replacement flow if it has expired, restore provisioning progress if it has been accepted, or show the result if it has completed. Closing the tab must not depend on a successful unload cancellation: pending requests expire naturally within five minutes, and accepted provisioning can continue.

## Automatic connection UI states

The following proposed connection request states are separate from the existing deployment `SetupSession.status`.

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

If status cannot be retrieved, show “Unable to confirm connection status. Retrying” and retain the last confirmed progress. Do not infer success or failure. Polling may pause while the page is hidden, but must synchronize immediately when visible again. Reusing the current three-second polling interval is recommended for the first version.

`running` means only that the VTA is ready. `connected` requires confirmation that the phone has completed registration and connection. If the partner app can only return an Admin DID, the browser can report no more than “VTA is ready”; the complete automatic connection flow has not passed acceptance.

## Frontend backend and mobile integration contract

The manual flow can reuse the existing API. The automatic flow requires the capabilities below. All paths, fields, and states are proposals and must not be assumed to exist in the backend.

| Caller | Proposed interface | Behavior |
| --- | --- | --- |
| Browser | `POST /api/v1/setup/:id/mobile-connections` | Verify authentication, VTA ownership, and initial connection state; create a request or return the existing valid request on repeated calls |
| Browser | `GET /api/v1/setup/:id/mobile-connections/current` | Restore or poll the current request; return an empty state if none exists |
| Browser | `POST /api/v1/setup/:id/mobile-connections/:request_id/refresh` | Replace the request atomically; an older request ID must not invalidate a newer request |
| Browser | `DELETE /api/v1/setup/:id/mobile-connections/:request_id` | Cancel a request that has not been accepted; return current progress if already accepted |
| Mobile app | `POST {callback_url}` | Supply the token in the URL and `admin_did` in the body; atomically accept one initial connection and record provisioning work |
| Mobile app | Progress and completion interfaces provided after callback acceptance | Retrieve VTA readiness, finish mobile registration and connection, and report completion; exact protocol remains to be agreed |

The browser's request data must include at least `request_id`, `status`, `vta_did`, `server_time`, and `expires_at`. Return `callback_url` only while `pending`. After acceptance, return displayable provisioning progress; on failure, return an identifiable error code and retry capabilities. The frontend must not generate authorization tokens or decide their validity period.

The callback must not depend on the phone having a VTA Farm account binding or browser cookie. The agreed authorization uses a high-entropy, one-time token bound on the server to the initiating user, one VTA, and the initial connection purpose. No separate DID signature or proof-of-control exchange is required by this callback contract. Scanning or making an HTTP GET request must not directly grant administrator access.

This makes possession of a valid QR token the authority to nominate the initial Admin DID. The immutable mapping prevents changing the target VTA, but does not establish that the scanning phone belongs to the owner. Someone who obtains an unused token could submit their own DID first; the five-minute window and one-time use limit that exposure without eliminating it. This is the trust boundary of the agreed flow.

### Atomic acceptance in the database

Checking that a token is unused and marking it used in separate operations is insufficient. Two concurrent requests can both pass the check before either updates the token. A disabled browser button cannot prevent mobile retries, and a process-local mutex cannot coordinate separate API replicas.

Use a database transaction with row locking or conditional updates to enforce the following steps as one acceptance operation:

1. Resolve the token to its stored request and VTA. Validate the supplied Admin DID, then inspect any previously accepted result for duplicate handling.
2. For a new acceptance, verify that the token is pending, unexpired, and not revoked, and that the VTA is still eligible for its initial connection.
3. Claim the VTA's initial connection and persist the accepted Admin DID. This value is fixed for that operation and cannot be overwritten by a competing submission.
4. Consume the token and invalidate any other pending initial connection requests for that VTA.
5. Record durable provisioning work tied to the accepted operation, then commit. If any step fails, roll back the entire acceptance; do not start provisioning.

Only the transaction that successfully claims the VTA may create the logical provisioning operation. A competing transaction must reread the committed state and return the existing result or a conflict. Enforce uniqueness for the initial connection operation at the database level so correctness holds across API replicas and restarts.

Callbacks, QR replacement, cancellation, and existing local/manual submission endpoints must use the same VTA-level acceptance rules. Token-level protection alone is insufficient because manual submissions and replacement tokens are different entry points to the same initial setup. Replacement or cancellation can invalidate only a pending request; they cannot undo an accepted operation or alter its Admin DID.

### Duplicate handling

| Request | Backend behavior |
| --- | --- |
| First valid token and Admin DID | Atomically accept and create one durable provisioning operation |
| Same accepted token and same validated Admin DID | Return the existing operation reference and status; do not create new work or grant access again |
| Same accepted token and a different Admin DID | Return a conflict; preserve the original DID and operation |
| Expired or revoked token that was never accepted | Reject and require a new scan |
| A different request or manual submission after initial connection was claimed | Return the existing setup state or a conflict; never replace the accepted DID |

Retain accepted request metadata for a defined retry period so that a lost callback response does not strand a successful acceptance. Expiry of the original five-minute window prevents new acceptance; it does not reverse acceptance or restart the window for a duplicate. A duplicate response acknowledges the existing operation only and does not renew mobile progress credentials. The retention period and exact response codes remain integration details.

### Restart recovery and provisioning retries

Persisting the accepted request and its pending work in the same transaction closes the gap between consuming a token and starting provisioning. If the API crashes before commit, no acceptance is recorded and a valid request can retry. If it crashes after commit, a worker discovers the saved work and continues the same operation without another scan.

Workers must coordinate through durable claims and resume safely after a crash. A database transaction cannot make external Kubernetes or VTA actions execute exactly once: a worker can lose its response after an action succeeded. Use stable operation and resource identifiers, inspect existing results, and make provisioning steps safe to retry. Retries must preserve the original Admin DID and must not create duplicate grants. An uncertain outcome must be reconciled before repeating the action.

The existing `provisionAdmin` handler checks session state before launching asynchronous work, and `Orchestrator.launch` uses process-local cancellation. These mechanisms do not implement the database acceptance guarantee above. Route existing manual submission and the new callback through the shared acceptance operation before integrating the automatic flow.

Mobile progress queries and completion reports require a separate credential scoped to that connection. Do not extend the consumed callback token into indefinite authorization. The credential lifetime and recovery mechanism remain to be agreed, and must allow setup to continue beyond five minutes. The backend must verify the mobile completion signal and bind it to the same request; the browser cannot declare success independently.

The callback URL contains temporary authorization information. Do not put it in analytics events, general application logs, or localStorage; backend and proxy logs must also redact the token. The app must accept only agreed HTTPS callback origins. These requirements are part of automatically granting administrator access and must be implemented during integration.

## Frontend implementation scope

The later implementation should focus on replacing the existing connection card while retaining the detail page structure. Build the three options, two QR views, countdown, and state presentation first. Use replaceable mock responses to verify the automatic flow, and enable it in production only when the required capabilities are available.

| Existing file | Design integration |
| --- | --- |
| [SessionDetailView.tsx](../../vtafarm/src/pages/portal/SessionDetailView.tsx) | Connection card entry point, VTA DID source, Admin DID waiting conditions, and session updates |
| [portalUtils.tsx](../../vtafarm/src/pages/portal/portalUtils.tsx) | Reuse `isValidAdminDid` and existing display conventions |
| [api.ts](../../vtafarm/src/lib/api.ts) | Reuse `provisionAdmin`; later add automatic connection types and request methods |
| [SessionPnmCard.tsx](../../vtafarm/src/pages/portal/SessionPnmCard.tsx) | Preserve the existing additional PNM feature for running VTAs |

The existing backend distinguishes initial provisioning in `provisionAdmin` in [setup.go](../internal/handler/setup.go) from adding an administrator in [setup_admins.go](../internal/handler/setup_admins.go). Keep these purposes separate.

## Acceptance scenarios

| Scenario | Expected result |
| --- | --- |
| Select a VTA and switch among all three methods | Content changes within the same detail page; QR always belongs to the current VTA |
| Use the existing local flow | Existing submission and provisioning complete normally |
| Scan the manual QR with the existing app | App recognizes the VTA and provides a usable Admin DID without an app update |
| Manual DID format is invalid or submission fails | Show an actionable error, retain input, and prevent duplicate submissions |
| Complete the automatic connection flow | No DID pasting after scan and confirmation; app registers automatically when VTA is ready, and the browser eventually shows connected |
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
| Automatic capability is unavailable | Disable the option in production; never present simulated success as a real result |

## Questions to resolve before integration

1. The existing app's supported QR format, Admin DID format, and whether the user must select a continue action after authorization. These determine the precise copy for the manual flow.
2. The QR payload encoding and callback transport/error details accepted by the app team. The application inputs are fixed: token in the callback URL and `admin_did` in the body, with no VTA Farm account binding or client-selected target.
3. The protocol for retrieving progress, registering the VTA, and reporting connection success, including recovery after the app closes.
4. The mobile progress credential lifetime and the timeout for awaiting mobile completion. These are separate from the five-minute scan request; their expiry must not revoke completed VTA provisioning.
5. The accepted callback retry retention period and exact status/error responses. These must preserve the atomic acceptance, fixed Admin DID, and recovery rules above.

These open protocol details do not block frontend UI and simulated flow design. The complete Automatic Mobile Connection feature requires integration acceptance with both the backend and the app.
