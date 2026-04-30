# Zalo Official Account (OA) Setup Guide

End-to-end checklist for connecting a Zalo OA channel to GoClaw. Covers Zalo dev console prerequisites, GoClaw wizard fields, and webhook ingestion mode.

## 1. Prerequisites on the Zalo developer console

Replace `<app_id>` below with your Zalo app's numeric ID.

### 1.1 Verify your domain

Zalo only allows OAuth callbacks on verified domains. Until your domain is verified, the redirect URL will fail with `error_code=-14003`.

1. Open `https://developers.zalo.me/app/<app_id>/verify-domain`.
2. Add the public domain that hosts your callback page (e.g. `example.com`).
3. Follow Zalo's verification flow (HTML meta tag or DNS TXT record).
4. Wait for the domain to appear under **Danh sách domain xác thực**.

### 1.2 Set the Official Account Callback URL

After the domain is verified:

1. Open `https://developers.zalo.me/app/<app_id>/oa/settings`.
2. In **Thiết lập đường dẫn yêu cầu cấp quyền**, set **Official Account Callback Url** to the same URL you'll paste into GoClaw's "Redirect URI" field (e.g. `https://example.com/zalo-callback`).
3. Click **Cập nhật**.

The callback URL only needs to be a static page that displays the browser URL bar — operators copy the `code` query param manually after granting consent.

## 2. GoClaw create wizard fields

| Field | Source |
|-------|--------|
| App ID | Zalo dev console → app overview |
| Secret Key | Zalo dev console → app overview (OAuth v4 secret) |
| Redirect URI | Same URL set as **Official Account Callback Url** in step 1.2 |

OA ID is **not** an input — it is auto-discovered from the OAuth callback URL after the first successful Connect and stored encrypted server-side. The channel detail page surfaces it read-only after connect.

## 3. Ingestion mode: webhook (default) vs polling

GoClaw supports two transports. Webhook is the default because it is event-driven and lighter on the gateway.

### 3.1 Webhook mode (recommended)

Each channel routes by a per-instance **slug** (not by UUID query param). The slug auto-derives from the channel name on create — operators may override it via the **Webhook Path** field in the create wizard.

Zalo's setup is a chicken-and-egg flow: **the OA Secret Key is only revealed after the URL save succeeds.** GoClaw handles this with a *bootstrap mode* — a fresh webhook channel acks Zalo's URL-verification ping with HTTP 200 (drops events without dispatch) so the URL save succeeds, then turns Healthy once the operator pastes the secret back.

End-to-end:

1. **Create the channel in GoClaw with the Webhook Secret Key field empty.** The form accepts that. The channel reaches `Degraded` health with summary `awaiting webhook secret`. The bootstrap banner appears on the **Credentials** tab.
2. Copy the Webhook URL from the bootstrap banner (or the **Webhook setup** card on the General tab):
   ```
   https://<your-gateway-host>/channels/zalo/webhook/<your-slug>
   ```
3. On `developers.zalo.me/app/<app_id>/oa/webhook`, paste the URL → click **Thay đổi** → **Cập nhật**. Zalo POSTs a verification ping; GoClaw's bootstrap returns 200 within ~2s and Zalo persists the URL.
4. The **Khóa bí mật OA** field now appears on the Zalo console. Click the eye icon to reveal the secret. Copy the value.
5. Back in GoClaw → channel detail → **Credentials** → paste the value into **Webhook Secret Key** → **Update Credentials**. The channel reloads, transitions to `Healthy`, and signature verification activates. Subsequent events are dispatched.
6. Keep **Signature Mode** at `strict` for production. Use `log_only` only during migration cutover.

The same `/channels/zalo/webhook/` prefix serves all OA and Bot instances; the slug suffix disambiguates.

#### Bootstrap window — what's accepted, what's dropped

While the channel is `Degraded` with `awaiting webhook secret`:

- POSTs to the slug return HTTP 200 immediately. No signature check. Zalo's URL-save ping passes.
- Payloads are **dropped** — not decoded, not dispatched to the agent, not stored. Drop count shows in `slog.Warn("zalo_oa.webhook.bootstrap_drop", drop_count=N)`.
- Real Zalo events arriving in this window are also dropped (operator-paced; expected duration is seconds to a few minutes between URL-save and secret-paste). Zalo retries non-2xx, but bootstrap returns 200 — so retried events also get dropped until the secret is set.
- Per-instance rate limiting on the router still applies.

#### Choosing a slug

- Allowed: lowercase letters, digits, hyphens. Must start with `[a-z0-9]`. Length 2–63.
- Reserved (rejected): `zalo`, `webhook`, `_health`, `_metrics`.
- Defaults: derived from channel name (e.g. `My OA` → `my-oa`).
- Renaming the channel does **not** change the slug — the slug is the routing key the Zalo console points at. Edit the slug only when you are ready to re-paste the URL on the Zalo console.

### 3.2 Polling mode (fallback)

Pick polling when the gateway has no public HTTPS endpoint. GoClaw will call `listrecentchat` on a timer.

| Field | Default | Notes |
|-------|---------|-------|
| Poll Interval (seconds) | 15 | Min 5, max 120 |
| Poll Page Size | 50 | Min 10, max 200 |
| Burn-down Max Pages | 5 | Max 20; set to 1 to disable burst catch-up |

## 4. Common errors

| Symptom | Cause | Fix |
|---------|-------|-----|
| `error_code=-14003` on Connect | Redirect URI mismatch or unverified domain | Verify domain (1.1) and re-set OA callback URL (1.2) |
| Zalo console shows `Cập nhật` failed (URL save error) | Gateway not reachable from Zalo, or returned non-2xx within the 2s deadline | Confirm host is publicly reachable; channel must exist in GoClaw (slug registered) — bootstrap mode handles missing-secret case automatically |
| Channel stuck on `Degraded — awaiting webhook secret` | Operator never pasted the OA Secret Key back | Open **Credentials** tab, paste **Khóa bí mật OA** → **Update Credentials** |
| Webhook returns 401 | Signature secret mismatch (or typo when pasting) | Re-copy **Webhook Secret Key** from the Zalo console; re-paste in GoClaw Credentials tab |
| Webhook returns 404 | Slug not registered (channel Stop'd or path traversal) | Re-enable the channel; verify the URL slug matches the **Webhook Path** value on the channel detail |
| No inbound events after secret pasted | Signature mode reverted to `disabled`, or OA disabled the webhook for 12h non-200 retries | Set signature mode back to `strict`; on the Zalo console re-save the URL to clear the auto-disable |

## 5. Quoted replies

Outbound CS replies automatically quote the user's last inbound message via Zalo's `message.quote_message_id` field on `/v3.0/oa/message/cs`. This is on by default — operators don't need to configure anything.

- Only the **first chunk** of a multi-chunk reply quotes; continuation chunks ship plain.
- Image / file / GIF sends do not quote (Zalo API doesn't support quoted attachments).
- If the source message is older than Zalo's 48h interaction window or has been deleted, the gateway transparently retries without the quote field — the reply is still delivered, with a `zalo_oa.send.quote_dropped_payload_error` warning logged for diagnostics.

## 6. Reactions (status emoji on user messages)

Set `reaction_level` in the channel config to surface agent run progress as a Zalo reaction on the user's inbound message. The defaults are tuned for the B2C / customer-service surface that real OAs run — quiet by default, conservative when on:

- `off` (**default**) — no reactions sent. Existing tenants stay silent on upgrade.
- `minimal` (**recommended for production**) — terminal-only: `/-heart` on success, `:-((` on failure. Exactly 0–2 reactions per agent run; doesn't pollute the customer's chat with mid-flight noise.
- `full` — adds a single "received, working on it" `--b` (thumbs-up) on the first intermediate event, debounced to ≤1 call per 700 ms. Mid-run tool/coding/web statuses are intentionally NOT mapped on Zalo OA — chatty intermediate reactions on a customer support conversation feel unprofessional and eat into the 50-reaction-per-`message_id` cap. If you need the full Telegram-style transition set, extend `statusReactionVariants` in `internal/channels/zalo/oa/reactions.go`.

Zalo OA caps reactions at 50 per source `message_id`. The endpoint (`POST /v2.0/oa/message`) does NOT count against the OA monthly active-message quota. Reactions are best-effort: failures are logged at Debug and never flip channel health. `ClearReaction` sends the `/-remove` sentinel to retract a previously dropped reaction (Zalo has no separate clear endpoint).

## 7. Reference

- Backend webhook router: `internal/channels/zalo/common/webhook_router.go`
- Slug helpers: `internal/channels/zalo/common/slug.go`
- Webhook URL RPC: `channels.instances.zalo.webhook_url` (`internal/gateway/methods/zalo_webhook.go`)
- Config schema: `internal/config/config_channels.go` (`ZaloOAConfig`)
- Frontend wizard: `ui/web/src/pages/channels/zalo/zalo-oa-wizard-step.tsx`
- Frontend webhook-setup card: `ui/web/src/pages/channels/zalo/zalo-webhook-url-section.tsx`
- Routing key: the `webhook_path` config field — see `internal/channels/zalo/common/slug.go`.
