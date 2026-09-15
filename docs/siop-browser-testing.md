# VTA Wallet SIOP browser test

VTA Wallet login is disabled until `SIOP_RP_DID` is set. Production must use
the dedicated VTA Farm RP `did:webvh` whose history resolves over public HTTPS.
Do not reuse `DID_HOSTING_DID`.

For a local browser-contract test, a public `did:key` can be used as the
audience because the RP does not sign with it. This exercises wallet discovery,
challenge persistence, VTA token minting, persona verification, account
linking, and the role-matched cookie. It is not a substitute for the production
`did:webvh` consent and resolution check.

## Start locally

With the database tunnel running, start the API with a dev-only RP DID:

```bash
SIOP_RP_DID=did:key:z6MkkVc5EPGcCa3ZWB5i2YGX7BnLBm8vgf1qUwqTb9i87wLj
make dev
```

Start the frontend in its repository:

```bash
pnpm dev
```

Open `http://localhost:5173/login`. The API metadata can be checked without a
session:

```bash
curl http://localhost:8080/api/v1/auth/siop/metadata
```

It should return `enabled: true`, and Chrome should show **Continue with VTA
Wallet** when the extension exposes both `walletProfile` and `proxyLogin`.

## User flow

1. Sign in with the existing passkey.
2. Open **Settings → VTA Wallet identities**.
3. Select **Link**, choose a wallet persona, and approve the one-time assertion.
4. Confirm the identity appears in the list, then log out.
5. On `/login`, select **Continue with VTA Wallet** and approve the assertion.
6. Confirm the browser reaches `/portal` using the `vtafarm_user` httpOnly
   cookie. No SIOP token should appear in local or session storage.
7. Replay and wrong-persona attempts must fail. Unlink the identity and confirm
   wallet login then reports that it is not linked.

## Admin flow

Repeat the same sequence at **Admin → Security** and `/admin/login`. The admin
flow uses only admin routes and sets only `vtafarm_admin`; linking a user
identity does not authorize the admin route.

An account with a linked VTA Wallet identity cannot delete its final passkey.
Unlink every wallet identity first if the passkey must be removed.
