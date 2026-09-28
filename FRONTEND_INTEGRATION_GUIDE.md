# Course Marketplace — Frontend Integration Guide (React + Vite + TS)

This document is the complete contract between the existing React/Vite/TS app (which
already renders courses) and the `course-coupon` Go backend. It covers: base setup,
every endpoint with exact payloads, the claim→checkout→purchase redirect flow, the
state machine driving UI, and error-code → UI mapping. Treat this as the spec to
implement against — every field name and status code below matches the backend
exactly (`internal/httpapi`, `internal/course`, `internal/redemption`, `internal/wallet`).

---

## 1. High-level architecture

```
┌─────────────────────────────────────────────────────────────────┐
│ React (Vite, TS)                                                 │
│                                                                    │
│  CourseListPage ──▶ CourseDetailPage ──▶ ClaimModal              │
│                                              │                    │
│                                              ▼                    │
│                                     RedemptionStatusPage           │
│                                    (poll /redemptions/{id})        │
│                                              │                    │
│                              checkout ───────┼────── opened        │
│                                              ▼                    │
│                                  window.location = provider URL    │
│                                     (Pabbly checkout page)         │
│                                              │                    │
│                          user pays on Pabbly's hosted page         │
│                                              │                    │
│                                              ▼                    │
│                        Pabbly → webhook → our backend → worker     │
│                                              │                    │
│                                              ▼                    │
│                      RedemptionStatusPage polls until CONFIRMED    │
│                                              │                    │
│                                              ▼                    │
│                             CourseHistoryPage / WalletWidget       │
└─────────────────────────────────────────────────────────────────┘
```

Key principle: **the frontend never computes price, discount, or coin cost.** Every
number displayed comes from a backend response. The frontend's only jobs are:
1. Render what the API returns.
2. Drive the redemption state machine (claim → checkout → opened → poll).
3. Map error codes to UI messages/actions.

---

## 2. Base API client setup

### 2.1 Environment

```
# .env (Vite)
VITE_API_BASE_URL=http://localhost:8080/api/v1
```

### 2.2 Auth

The backend expects one of:
- `Authorization: Bearer <existing app JWT>` — production path, reuse whatever auth
  your app already has.
- In local dev only (`DEV_AUTH_ENABLED=true` on the backend), you may omit the
  Authorization header entirely and the backend treats you as its dev user. You can
  override which dev user with `X-Dev-User-Id: <uuid>` if you need to test multiple
  wallets locally.

```ts
// src/lib/apiClient.ts
const BASE_URL = import.meta.env.VITE_API_BASE_URL as string;

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details: Record<string, unknown> = {},
  ) {
    super(message);
  }
}

interface RequestOptions extends RequestInit {
  idempotencyKey?: string;
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const token = getAuthToken(); // your existing app's token getter

  const headers = new Headers(opts.headers);
  headers.set("Content-Type", "application/json");
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (opts.idempotencyKey) headers.set("Idempotency-Key", opts.idempotencyKey);

  const res = await fetch(`${BASE_URL}${path}`, { ...opts, headers });

  // 204/empty bodies aside, every response (success or error) is JSON.
  const body = res.status === 204 ? null : await res.json().catch(() => null);

  if (!res.ok) {
    const code = body?.code ?? "UNKNOWN_ERROR";
    const message = body?.message ?? "Something went wrong";
    throw new ApiError(res.status, code, message, body ?? {});
  }
  return body as T;
}

export const api = {
  get: <T>(path: string) => request<T>(path, { method: "GET" }),
  post: <T>(path: string, body?: unknown, idempotencyKey?: string) =>
    request<T>(path, {
      method: "POST",
      body: body ? JSON.stringify(body) : undefined,
      idempotencyKey,
    }),
};
```

### 2.3 Idempotency keys

Every mutating call that the spec marks as requiring one (`claim`) needs a client-
generated key that stays **stable across retries of the same logical action** (e.g.
double-click, network retry) but is **fresh for a genuinely new attempt**.

```ts
// src/lib/idempotency.ts
export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}
```

Pattern: generate the key once when the "Claim" button becomes actionable (e.g. on
modal open, stored in component state), reuse it for retries of that same click, and
throw it away (generate a new one) only when the user explicitly starts a new claim
attempt (e.g. closes and reopens the modal).

---

## 3. Data model / TypeScript types

Match these exactly to backend JSON — see `internal/course/http.go`,
`internal/redemption/http.go`, `internal/wallet/http.go`.

```ts
// src/types/marketplace.ts

export interface CoursePricing {
  mrp?: string;           // decimal rupees as string, e.g. "10000"
  selling_price: string;  // decimal rupees as string, e.g. "7000"
  currency: string;       // "INR"
}

export interface CourseOffer {
  discount_percent: string; // "20"
  coin_cost: number;
  expected_price: string;   // "5600"
}

export interface Course {
  id: string;
  title: string;
  plan_name: string;
  provider: string; // "PABBLY"
  class?: number;
  board?: string;
  subject?: string;
  language?: string;
  target_exam?: string;
  thumbnail_url?: string;
  duration?: string;
  description?: string;
  pricing: CoursePricing;
  offer?: CourseOffer; // absent = no active reward right now
}

export interface CourseListResponse {
  items: Course[];
  page: number;
  limit: number;
  total: number;
}

export type RedemptionStatus =
  | "COINS_RESERVED"
  | "COUPON_CREATING"
  | "COUPON_ISSUED"
  | "CHECKOUT_OPENED"
  | "PURCHASE_PENDING"
  | "PURCHASE_CONFIRMED"
  | "PAYMENT_FAILED"
  | "EXPIRED"
  | "PROVIDER_ERROR"
  | "REFUND_PENDING"
  | "REFUNDED";

export interface RedemptionCoupon {
  code: string;
  status: "CREATING" | "ACTIVE" | "USED" | "DISABLED" | "EXPIRED" | "FAILED";
  expires_at: string; // ISO timestamp
}

export interface RedemptionView {
  id: string;
  status: RedemptionStatus;
  course_id: string;
  title: string;
  plan_name: string;
  provider: string;
  pricing: {
    mrp?: string;
    provider_price: string;
    discount_percent: string;
    discount_amount: string;
    expected_checkout_price: string;
    currency: string;
  };
  coin_cost: number;
  coupon?: RedemptionCoupon;
  checkout_url?: string;
  coupon_prefilled?: boolean; // false => show code for manual entry
  quote_verified_at: string;
  expires_at: string;
  purchase_confirmed_at?: string;
  created_at: string;
}

export interface WalletLedgerEntry {
  type: "CREDIT" | "RESERVE" | "RELEASE" | "CONSUME" | "RESTORE";
  amount: number;
  redemption_id?: string;
  created_at: string;
}

export interface WalletView {
  available_coins: number;
  reserved_coins: number;
  ledger: WalletLedgerEntry[] | null;
}
```

---

## 4. Full endpoint reference

All paths are relative to `VITE_API_BASE_URL` (already includes `/api/v1`). All
require `Authorization` except the webhook route (backend-to-backend only, not
called by the frontend).

### 4.1 `GET /courses` — course catalog listing

**Query params** (all optional): `page`, `limit` (max 100, default 20), `class`,
`board`, `subject`, `language`, `target_exam`, `search`.

```ts
export async function listCourses(params: {
  page?: number; limit?: number; class?: number; board?: string;
  subject?: string; language?: string; target_exam?: string; search?: string;
}): Promise<CourseListResponse> {
  const qs = new URLSearchParams();
  Object.entries(params).forEach(([k, v]) => {
    if (v !== undefined && v !== "") qs.set(k, String(v));
  });
  return api.get(`/courses?${qs.toString()}`);
}
```

Response `200`:
```json
{
  "items": [
    {
      "id": "3fbb...",
      "title": "Class 12 Physics — Board + JEE Foundation",
      "plan_name": "Physics 2026",
      "provider": "PABBLY",
      "class": 12, "board": "CBSE", "subject": "Physics", "language": "Hinglish",
      "pricing": { "mrp": "10000", "selling_price": "7000", "currency": "INR" },
      "offer": { "discount_percent": "20", "coin_cost": 2000, "expected_price": "5600" }
    }
  ],
  "page": 1, "limit": 20, "total": 1
}
```

UI notes:
- If `offer` is absent, hide the "reward" badge and disable the Claim CTA
  (show "No reward available right now" instead of a broken price).
- `pricing.mrp` may be absent — do not fabricate a strikethrough price if missing.
- This is a plain read; no loading-state gotchas beyond a normal list fetch.

### 4.2 `GET /courses/{courseID}` — course detail

Same `Course` shape as one list item. 404 → `COURSE_NOT_FOUND`.

### 4.3 `POST /courses/{courseID}/claim` — claim the reward, reserve coins, issue coupon

**Headers:** `Idempotency-Key: <uuid>` (required — 400 without it).

**Body:** none.

```ts
export async function claimCourse(courseId: string, idempotencyKey: string): Promise<RedemptionView> {
  return api.post(`/courses/${courseId}/claim`, undefined, idempotencyKey);
}
```

Response `201 Created` → `RedemptionView` with `status: "COUPON_ISSUED"` and a
populated `coupon` object on success.

This call can take a couple of seconds (it does a live Pabbly price check + coupon
creation) — **show a blocking spinner/step indicator**, not a silent disabled button.

Possible failures (see §6 for the full error-code table): `COURSE_NOT_FOUND`,
`OFFER_NOT_AVAILABLE`, `COURSE_UNAVAILABLE`, `INSUFFICIENT_COINS`,
`ACTIVE_REDEMPTION_EXISTS`, `PROVIDER_UNAVAILABLE`, `COUPON_CREATION_FAILED`.

On `ACTIVE_REDEMPTION_EXISTS`, the response does not include the existing
redemption's ID — the UI should invite the user to check "My Rewards" /
course-history instead of retrying blindly.

### 4.4 `POST /redemptions/{id}/checkout` — get the checkout URL, revalidate price

**Body** (optional, only send when re-confirming a price change):
```ts
interface CheckoutRequest {
  accept_price?: string; // decimal rupees, e.g. "8000" — echoes back current_price from a 409
}
```

```ts
export async function requestCheckout(
  redemptionId: string,
  acceptPrice?: string,
): Promise<RedemptionView> {
  return api.post(`/redemptions/${redemptionId}/checkout`,
    acceptPrice ? { accept_price: acceptPrice } : undefined);
}
```

Response `200` → `RedemptionView` with `checkout_url` and `coupon_prefilled` set.

**Special response — price changed while waiting (`409 PRICE_CHANGED`):**
```json
{
  "code": "PRICE_CHANGED",
  "message": "the provider price changed; please confirm the new amount",
  "previous_price": "7000",
  "current_price": "8000",
  "expected_checkout_price": "6400",
  "currency": "INR"
}
```
UI must show a confirmation dialog ("Price updated from ₹7,000 to ₹8,000 — new
reward price ₹6,400. Continue?"). On confirm, re-call checkout with
`accept_price: "8000"` (the `current_price` value verbatim, as a decimal string).

### 4.5 `POST /redemptions/{id}/opened` — mark checkout as opened (fire-and-forget, idempotent)

Call this **immediately before** `window.location.href = checkout_url` (or opening a
new tab). No body.

```ts
export async function markCheckoutOpened(redemptionId: string): Promise<{ id: string; status: string; checkout_opened_at: string | null }> {
  return api.post(`/redemptions/${redemptionId}/opened`);
}
```

Safe to call multiple times (e.g. user navigates back and re-opens checkout).

### 4.6 `GET /redemptions/{id}` — poll for status

Poll this after the student returns from Pabbly (or in a background tab) until
`status` reaches a terminal state (`PURCHASE_CONFIRMED`, `PAYMENT_FAILED`,
`EXPIRED`, `PROVIDER_ERROR`, `REFUNDED`). See §5 for polling strategy.

```ts
export async function getRedemption(redemptionId: string): Promise<RedemptionView> {
  return api.get(`/redemptions/${redemptionId}`);
}
```

### 4.7 `POST /redemptions/{id}/refund` — request a refund (only from PURCHASE_CONFIRMED)

```ts
export async function requestRefund(redemptionId: string): Promise<{ id: string; status: string }> {
  return api.post(`/redemptions/${redemptionId}/refund`);
}
```

Returns `202 Accepted` with `status: "REFUND_PENDING"`. The refund only completes
(`REFUNDED`) after Pabbly's webhook is verified — keep polling `GET /redemptions/{id}`
the same way as purchase confirmation.

### 4.8 `GET /me/course-history` — purchase history

Query: `page`, `limit` (defaults 1 / 20, max 100).

```ts
export async function getCourseHistory(page = 1, limit = 20): Promise<{ items: RedemptionView[]; page: number; limit: number }> {
  return api.get(`/me/course-history?page=${page}&limit=${limit}`);
}
```

Note: historical entries show the **frozen price/coin cost at time of purchase**,
not today's price — never re-fetch the course to "correct" these numbers.

### 4.9 `GET /me/wallet` — coin balance + ledger

```ts
export async function getWallet(): Promise<WalletView> {
  return api.get(`/me/wallet`);
}
```

`ledger` is most-recent-first, capped at 50 entries; render as a simple transaction
list (icon per `type`, "+"/"-" per CREDIT/RESTORE vs RESERVE/CONSUME).

### 4.10 Admin/debug (optional, gate behind a dev-only screen — not for students)

```
POST /admin/providers/pabbly/sync
POST /admin/webhooks/process
POST /admin/redemptions/{id}/reconcile
```

Useful for a local "Force sync now" / "Force reconcile" debug panel while building —
do not ship these buttons to production students.

---

## 5. The redemption state machine (drives all UI)

```
        claim()
           │
           ▼
   COINS_RESERVED ──▶ COUPON_CREATING ──▶ COUPON_ISSUED
                                               │
                                   checkout()  │  opened()
                                               ▼
                                       CHECKOUT_OPENED
                                               │
                              (student pays on Pabbly's page)
                                               │
                                               ▼
                                      PURCHASE_PENDING  ◀── webhook received, unverified
                                               │
                        verified by worker against Pabbly API
                                 ┌─────────────┴─────────────┐
                                 ▼                           ▼
                     PURCHASE_CONFIRMED               PAYMENT_FAILED
                                 │
                         refund() requested
                                 ▼
                        REFUND_PENDING ──▶ REFUNDED

   Any COINS_RESERVED..PURCHASE_PENDING state can also become:
     EXPIRED          (TTL elapsed, worker released coins)
     PROVIDER_ERROR   (coupon creation failed conclusively, coins released)
```

### 5.1 Component-level mapping

| Status | Screen / UI treatment |
|---|---|
| `COINS_RESERVED`, `COUPON_CREATING` | Claim modal: spinner, "Reserving your reward…" — this is the in-flight state during the `claim()` call itself; the frontend rarely observes this at rest since claim() is synchronous, but poll briefly if you show an async claim flow. |
| `COUPON_ISSUED` | Show coupon code + "Continue to payment" CTA → calls checkout(). |
| `CHECKOUT_OPENED` | Show "Waiting for payment confirmation…" with a spinner and a manual "I've completed payment" refresh button; start polling `GET /redemptions/{id}` (see §5.2). |
| `PURCHASE_PENDING` | Same as above — webhook arrived but not yet verified. Keep polling. |
| `PURCHASE_CONFIRMED` | Success screen: "🎉 Purchase confirmed!" + link to course-history. Show a "Request refund" button if within policy. |
| `PAYMENT_FAILED` | "Payment did not go through. Your coins were not spent." + "Try again" button that starts a **new** claim (new idempotency key) since the old redemption is dead. |
| `EXPIRED` | "This reward expired. Your coins were released." + "Claim again" button. |
| `PROVIDER_ERROR` | "We couldn't generate your coupon. Your coins were not spent." + "Try again" / "Contact support". |
| `REFUND_PENDING` | "Refund in progress…" spinner, keep polling. |
| `REFUNDED` | "Refunded — coins restored." |

### 5.2 Polling strategy

```ts
// src/hooks/useRedemptionPolling.ts
import { useEffect, useRef, useState } from "react";
import { getRedemption } from "../lib/marketplaceApi";
import type { RedemptionView } from "../types/marketplace";

const TERMINAL: RedemptionView["status"][] = [
  "PURCHASE_CONFIRMED", "PAYMENT_FAILED", "EXPIRED", "PROVIDER_ERROR", "REFUNDED",
];

export function useRedemptionPolling(redemptionId: string | null) {
  const [redemption, setRedemption] = useState<RedemptionView | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const attempt = useRef(0);

  useEffect(() => {
    if (!redemptionId) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;

    async function tick() {
      try {
        const r = await getRedemption(redemptionId!);
        if (cancelled) return;
        setRedemption(r);
        attempt.current += 1;
        if (!TERMINAL.includes(r.status)) {
          // Backoff: 2s, 3s, 5s, 8s ... capped at 15s. Payment confirmation is not instant.
          const delay = Math.min(2000 + attempt.current * 1500, 15000);
          timer = setTimeout(tick, delay);
        }
      } catch (e) {
        if (!cancelled) setError(e as Error);
      }
    }
    tick();
    return () => { cancelled = true; clearTimeout(timer); };
  }, [redemptionId]);

  return { redemption, error };
}
```

Also poll (or at least re-check once) on `visibilitychange` / `window focus`, since
students commonly alt-tab back from the Pabbly tab:

```ts
useEffect(() => {
  const onFocus = () => { if (redemptionId) getRedemption(redemptionId).then(setRedemption); };
  window.addEventListener("focus", onFocus);
  return () => window.removeEventListener("focus", onFocus);
}, [redemptionId]);
```

### 5.3 The checkout redirect itself

```ts
async function goToCheckout(redemptionId: string) {
  const r = await requestCheckout(redemptionId);
  await markCheckoutOpened(redemptionId); // fire before navigating away
  if (r.coupon_prefilled) {
    window.location.href = r.checkout_url!;
  } else {
    // Show the code prominently so the student can paste it manually on Pabbly's page.
    showManualCouponDialog(r.coupon!.code, r.checkout_url!);
  }
}
```

`coupon_prefilled: false` is expected and normal for this MVP (Pabbly's coupon
query-param was not confirmed) — **never treat this as an error state**. Design the
manual-entry dialog as a first-class UI, not an afterthought:

```
┌─────────────────────────────────────────┐
│  Your reward code                        │
│                                           │
│   ┌─────────────────────────┐            │
│   │   STU-Q7MT9K4X    [Copy] │            │
│   └─────────────────────────┘            │
│                                           │
│  Paste this code at checkout to apply    │
│  your 20% reward.                        │
│                                           │
│         [ Continue to checkout → ]        │
└─────────────────────────────────────────┘
```

---

## 6. Error-code → UI mapping (complete table)

Every backend error returns `{ "code": string, "message": string, ...details }`.
**Key off `code`, never off `message`** (message text may change).

| `code` | HTTP | Typical trigger | UI treatment |
|---|---|---|---|
| `COURSE_NOT_FOUND` | 404 | Bad/stale course id | Redirect to course list with a toast "This course is no longer available." |
| `COURSE_UNAVAILABLE` | 409 | Provider deactivated the plan | Disable Claim CTA, show "Currently unavailable" badge on the card. |
| `OFFER_NOT_AVAILABLE` | 409 | No active offer for this course | Hide/disable Claim CTA; show "No reward active right now." |
| `INSUFFICIENT_COINS` | 409 | Wallet balance too low | Modal: "You need N more coins for this reward" + link to wallet/earn-coins flow. |
| `ACTIVE_REDEMPTION_EXISTS` | 409 | Student already claimed this course+offer | "You already have an active reward for this course." + CTA to course-history / "View my reward". |
| `PRICE_CHANGED` | 409 | Live price moved since quote | Confirmation dialog (see §4.4) — not a failure, a decision point. |
| `PROVIDER_UNAVAILABLE` | 503 | Pabbly unreachable / stale cache | Toast: "Pricing service is temporarily unavailable. Please try again in a moment." Retry button with backoff. |
| `COUPON_CREATION_FAILED` | 502 | Pabbly coupon API failed conclusively | "We couldn't issue your coupon. Your coins were not spent." + Retry (new claim). |
| `REDEMPTION_EXPIRED` | 409 | TTL passed before checkout | "This reward expired." + "Claim again" button. |
| `INVALID_REDEMPTION_STATE` | 409 | Wrong-state action (e.g. checkout on an already-confirmed redemption) | Refresh the redemption and re-render from its actual current status; don't show a raw error if avoidable. |
| `PURCHASE_NOT_CONFIRMED` | — | (reserved for future use) | Generic retry toast. |
| `INVALID_REQUEST` | 400 | Malformed body / missing Idempotency-Key | Developer-facing — should not surface in normal use; log to console, show generic "Something went wrong." |
| `IDEMPOTENCY_KEY_REUSED` | 409 | Same key sent with different payload | Generate a fresh key and retry once automatically; if it recurs, show generic error. |
| `UNAUTHORIZED` | 401 | Missing/expired auth | Redirect to your app's login flow. |
| `NOT_FOUND` | 404 | Redemption id not found / not owned | Redirect to course-history. |
| `INTERNAL_ERROR` | 500 | Unexpected | Generic "Something went wrong, please try again" toast + optionally a "report issue" link. |

### 6.1 Central error handler

```ts
// src/lib/errorHandling.ts
import { ApiError } from "./apiClient";
import { toast } from "your-toast-lib";

export function handleApiError(err: unknown, opts?: { onPriceChanged?: (d: any) => void }) {
  if (!(err instanceof ApiError)) {
    toast.error("Something went wrong. Please try again.");
    return;
  }

  switch (err.code) {
    case "PRICE_CHANGED":
      opts?.onPriceChanged?.(err.details);
      return; // handled by caller, not a toast
    case "INSUFFICIENT_COINS":
      toast.error("You don't have enough coins for this reward.");
      return;
    case "ACTIVE_REDEMPTION_EXISTS":
      toast.info("You already have an active reward for this course.");
      return;
    case "COURSE_UNAVAILABLE":
    case "OFFER_NOT_AVAILABLE":
      toast.error("This course isn't available for reward right now.");
      return;
    case "PROVIDER_UNAVAILABLE":
      toast.error("Pricing service is busy — please try again shortly.");
      return;
    case "COUPON_CREATION_FAILED":
      toast.error("Couldn't issue your reward coupon. Your coins were not spent.");
      return;
    case "REDEMPTION_EXPIRED":
      toast.error("This reward expired.");
      return;
    case "UNAUTHORIZED":
      redirectToLogin();
      return;
    default:
      toast.error(err.message || "Something went wrong.");
  }
}
```

Use it uniformly:
```ts
try {
  const redemption = await claimCourse(course.id, idempotencyKey);
  navigate(`/redemptions/${redemption.id}`);
} catch (err) {
  handleApiError(err);
}
```

---

## 7. Money formatting

All amounts arrive as **decimal-string rupees** (e.g. `"7000"`, `"5600.5"`), already
converted from paise by the backend. Never do paise math client-side.

```ts
// src/lib/money.ts
export function formatINR(amount: string, currency = "INR"): string {
  const n = Number(amount);
  return new Intl.NumberFormat("en-IN", {
    style: "currency", currency, maximumFractionDigits: 2,
  }).format(n);
}
```

---

## 8. Wiring into existing course components

Since the app already renders courses, the integration is additive:

1. **`CourseCard` / `CourseListPage`** — swap the existing data source for
   `listCourses()`; map `offer.expected_price` into the existing discount badge, and
   `offer` absence into a disabled/hidden reward badge.
2. **`CourseDetailPage`** — call `getCourse(id)`; the "Claim Reward" button opens
   `ClaimModal`.
3. **New: `ClaimModal`** — generates an idempotency key on open, calls `claimCourse`,
   handles the 4-9 error codes relevant to claim, and on success navigates to
   `/redemptions/:id`.
4. **New: `RedemptionStatusPage`** — uses `useRedemptionPolling`, renders the table
   in §5.1, and hosts the checkout/opened/refund actions.
5. **New: `CourseHistoryPage`** — paginated list from `getCourseHistory`, reusing
   the same `RedemptionView` rendering as the status page but read-only.
6. **New: `WalletWidget`** (header/sidebar) — `getWallet()`, shows
   `available_coins`, with a popover listing the ledger.

---

## 9. Things to explicitly NOT do in the frontend

- Do not compute discount/expected price locally — always render `pricing` /
  `offer` fields verbatim from the API.
- Do not cache a claimed coupon code across sessions as "the" price — always trust
  the latest `RedemptionView` from the backend.
- Do not retry `claim()` with a new idempotency key just because the first call is
  slow — wait for it (with a spinner); only generate a new key for a genuinely new
  user-initiated attempt.
- Do not build a "cancel redemption" button — cancellation isn't in the MVP scope;
  only expiry (automatic) and refund-after-purchase exist.
- Do not assume `coupon_prefilled` will become `true` later without checking the
  response each time — it's determined server-side per call.
