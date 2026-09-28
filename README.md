# Course Marketplace MVP

Two binaries share one PostgreSQL database:

- `cmd/api` — HTTP API (Chi). Applies migrations on startup.
- `cmd/worker` — catalog sync, expiry, coupon deactivation retry, webhook processing.
- `cmd/seed` — credits the dev wallet and attaches a default offer to synced courses.

## Run

```
cp .env.example .env
# fill in PABBLY_API_KEY, PABBLY_SECRET_KEY, PABBLY_WEBHOOK_SECRET, DATABASE_URL

set -a; source .env; set +a
go run ./cmd/api        # in one shell
go run ./cmd/worker      # in another, drives catalog sync + jobs
go run ./cmd/seed        # after the first catalog sync, seed offers + dev coins
```

`DEV_AUTH_ENABLED=true` lets requests skip a real JWT and act as `DEV_USER_ID`.
It is refused at startup when `APP_ENV=production`.

## Endpoints

```
GET  /api/v1/courses
GET  /api/v1/courses/{courseID}
POST /api/v1/courses/{courseID}/claim          (requires Idempotency-Key)
POST /api/v1/redemptions/{id}/checkout
POST /api/v1/redemptions/{id}/opened
POST /api/v1/redemptions/{id}/refund
GET  /api/v1/redemptions/{id}
GET  /api/v1/me/course-history
GET  /api/v1/me/wallet
POST /api/v1/webhooks/pabbly/{PABBLY_WEBHOOK_SECRET}

POST /api/v1/admin/providers/pabbly/sync
POST /api/v1/admin/webhooks/process
POST /api/v1/admin/redemptions/{id}/reconcile
```

Configure the Pabbly webhook to point at
`https://<host>/api/v1/webhooks/pabbly/<PABBLY_WEBHOOK_SECRET>` for Successful
Payment, Payment Failure and Payment Refund.
# coupon-course-onboard-workflow
# coupon-course-onboard-workflow
