# Keyring Sign-in Integration

## Goal

A user who already obtained a VTA through VTA Farm must be able to sign in with
the same identity in Keyring.

This is a service sign-in flow, not a Community join flow. Keyring must not ask
the user to join a Community or create another identity.

## User Flow

1. The user selects **Sign in with Keyring** on the VTA Farm login page.
2. The user scans the QR code or opens it directly in Keyring.
3. Keyring opens the sign-in flow and lets the user select an existing identity.
4. The user selects the identity previously used to obtain the VTA.
5. VTA Farm displays a two-digit number in the browser.
6. Keyring asks the user to enter that number.
7. Keyring shows the VTA Farm name, login origin, and selected identity.
8. The user confirms the request.
9. Keyring shows **Approved**, and the original browser signs in automatically.

```text
Open sign-in QR
    -> Select existing identity
    -> Enter the two-digit number shown in the browser
    -> Confirm
    -> Approved
```

## Integration Flow

The QR code uses this VTI flow type:

```text
/vti/flow/sign-in/0.1
```

Keyring must route this flow type to its sign-in handler before applying any
Community membership checks.

Keyring resolves the Farm DID and verifies:

- the `TrustTaskHTTPS` endpoint used for the exchange;
- the `SignInPortal` URL representing the authorized login page;
- the Farm signatures against an authorized assertion key in the Farm DID.

Keyring then performs:

```text
auth/oob/claim/0.2
    -> auth/oob/identify/0.1
    -> auth/oob/prove/0.3
    -> auth/oob/grant/0.3
    -> auth/oob/respond/0.3
```

- `claim` opens the sign-in request.
- `identify` uses the identity selected by the user.
- `prove` sends the identity proof and the two-digit number entered by the user.
- `grant` records the user's approval after device user verification.
- `respond` returns the signed decision to VTA Farm.

The browser owns `auth/oob/request/0.2` and `auth/oob/redeem/0.2`. After Keyring
returns an approved decision, the browser receives the Farm session and finishes
the login.

## Required Behavior

- Use the same Keyring identity that was used to obtain the VTA.
- Never use the Farm DID or a Community DID as the user's identity.
- Do not create a new identity during sign-in.
- Do not require Community membership.
- Do not change the selected identity after the number has been entered.
- Stop the request when the number is incorrect, expired, or canceled.

If the selected identity is not linked to VTA Farm, show:

> This identity is not linked to a VTA Farm account. Select the identity that
> was used to obtain the VTA.

## Completion Criteria

The integration is complete when a user can:

1. obtain a VTA through the existing homepage flow;
2. skip Community enrollment;
3. select **Sign in with Keyring**;
4. enter the browser's two-digit number in Keyring;
5. confirm the request; and
6. enter the correct VTA Farm account in the original browser.
