[![Buy Me a Beer](https://img.shields.io/badge/buymeacoffee-_?style=for-the-badge&logo=buymeacoffee&logoColor=white&color=F16061)](https://buymeacoffee.com/rickerd)

I make software in my spare time. Please buy me a coffee, if you want, for my work :)

# Cloudreve Payment Bridge for Mollie

A small, self-hosted service that lets [Cloudreve](https://cloudreve.org) accept payments through [Mollie](https://www.mollie.com). It implements Cloudreve's [custom payment provider](https://docs.cloudreve.org/en/payment/custom) API and translates it to the Mollie Payments API.

Customers pay on Mollie's hosted checkout, so every payment method enabled on your Mollie account (iDEAL, cards, and so on) is available without extra work.

> **Unofficial project.** This bridge is not affiliated with or endorsed by Cloudreve or Mollie.

## Features

- Creates a Mollie payment for every Cloudreve order and returns the checkout URL
- Verifies every request from Cloudreve with its HMAC-SHA256 signature
- Handles Mollie webhooks and notifies Cloudreve when a payment is paid
- Answers Cloudreve's order status queries with live data from Mollie
- Built-in thank-you page, so customers see the payment result after checkout
- Idempotent: a repeated order returns the existing checkout instead of creating a second payment
- Single small container, SQLite storage, no external database
- Runs as a non-root user and includes a health check

## How it works

1. A customer starts a purchase in Cloudreve. Cloudreve sends `POST /order` to the bridge.
2. The bridge verifies the signature, creates a payment at Mollie and returns the checkout URL.
3. The customer pays on Mollie and is redirected to the bridge's `/return` page.
4. Mollie calls `POST /webhook/mollie`. The bridge fetches the payment from Mollie and, if it is paid, calls the `notify_url` that Cloudreve provided.
5. Cloudreve also polls `GET /order?order_no=...` to confirm the status.

The webhook body is never trusted. The bridge only reads the payment ID from it and asks the Mollie API for the actual status.

## Requirements

- A Cloudreve installation with custom payment provider support (tested with Cloudreve Pro)
- A Mollie account and API key
- Docker and Docker Compose
- A **public HTTPS URL** for the bridge, for example `https://payments.example.com`, because Mollie must be able to reach the webhook

## Quick start

Create a `.env` file:

```env
# Only needed if you run Cloudreve Pro from the same compose file.
# This is used by the Cloudreve container, not by the bridge.
CR_LICENSE_KEY=your-cloudreve-license-key

# Bridge settings
CLOUDREVE_COMMUNICATION_KEY=change-me-to-a-long-random-string
PUBLIC_URL=https://payments.example.com
MOLLIE_API_KEY=test_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
MAX_SIGNATURE_LIFETIME_SECONDS=172800

# Optional. This is already the default, so you can leave it out.
REDIRECT_URL_TEMPLATE=${PUBLIC_URL}/return?order_no={order_no}
```

Generate a good communication key with `openssl rand -hex 32`.

`REDIRECT_URL_TEMPLATE` is optional and refers to `PUBLIC_URL`, so the domain only appears once. Referring to another variable inside a `.env` file needs a recent version of Docker Compose. If the redirect URL turns out empty or contains a literal `${PUBLIC_URL}` (the bridge logs it at startup as `redirect template: ...`), remove the line and the default is used.

Create a `docker-compose.yaml`:

```yaml
services:
  mollie-bridge:
    image: rickerd/cloudreve-payment-bridge-mollie:latest
    container_name: cloudreve-payment-bridge-mollie
    restart: unless-stopped
    environment:
      MOLLIE_API_KEY: ${MOLLIE_API_KEY}
      CLOUDREVE_COMMUNICATION_KEY: ${CLOUDREVE_COMMUNICATION_KEY}
      PUBLIC_URL: ${PUBLIC_URL}
      MAX_SIGNATURE_LIFETIME_SECONDS: ${MAX_SIGNATURE_LIFETIME_SECONDS:-172800}
      REDIRECT_URL_TEMPLATE: ${REDIRECT_URL_TEMPLATE:-}
      MOLLIE_LOCALE: ${MOLLIE_LOCALE:-nl_NL}
    volumes:
      - ./data:/data
    ports:
      - "8080:8080"
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:8080/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

Start it and check that it runs:

```bash
docker compose up -d
curl https://payments.example.com/healthz   # should print "ok"
```

### Important!
The container runs as a non-root user, so the `./data` directory must be writable for that user.

### Configure Cloudreve

In the Cloudreve admin panel, add a custom payment provider:

- **Endpoint:** `https://payments.example.com/order`
- **Communication key:** exactly the same value as `CLOUDREVE_COMMUNICATION_KEY`

Then make a test order. See the [Cloudreve documentation](https://docs.cloudreve.org/en/payment/custom) for the exact settings screen.

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `MOLLIE_API_KEY` | yes | | Mollie API key. `test_...` for testing, `live_...` for production. |
| `MOLLIE_LOCALE` | yes | nl_NL | Locale for Mollie. |
| `CLOUDREVE_COMMUNICATION_KEY` | yes | | Shared secret. Must match the key configured in Cloudreve exactly. |
| `PUBLIC_URL` | yes | | Public base URL of the bridge. Used for Mollie's webhook and the default return page. |
| `REDIRECT_URL_TEMPLATE` | no | `${PUBLIC_URL}/return?order_no={order_no}` | Where customers go after Mollie's checkout. Supports `{order_no}` and `{cloudreve_site_url}`. |
| `DB_PATH` | no | `/data/bridge.db` | Location of the SQLite database inside the container. |
| `MAX_SIGNATURE_LIFETIME_SECONDS` | no | `900` | Maximum time a Cloudreve signature is allowed in the future. Cloudreve signs with an expiry of about a day ahead, so set this to `172800` (see Troubleshooting). |
| `MOLLIE_LOCALE` | no | `nl_NL` | Language of Mollie's checkout page. |

Use either a `test_` or a `live_` key. A payment created with one key cannot be read with the other.

An empty value (for example `REDIRECT_URL_TEMPLATE=` or `REDIRECT_URL_TEMPLATE: ${REDIRECT_URL_TEMPLATE:-}` with nothing set) counts as not set, so the default is used.

`CR_LICENSE_KEY` is **not** a bridge setting. It is the license key of the Cloudreve Pro container. It only appears in the `.env` example because Cloudreve and the bridge often share one compose project.

## Endpoints

| Method | Path | Called by | Purpose |
|---|---|---|---|
| `POST` | `/order` | Cloudreve | Create a payment and return the Mollie checkout URL |
| `GET` | `/order?order_no=...` | Cloudreve | Return the payment status (`PAID`, `OPEN`, `EXPIRED`, ...) |
| `POST` | `/webhook/mollie` | Mollie | Receive payment updates and notify Cloudreve |
| `GET` | `/return?order_no=...` | Customer | Thank-you page showing the payment result |
| `GET` | `/healthz` | Docker / monitoring | Returns `ok` when the database is reachable |

If you use a reverse proxy or tunnel, it must pass request paths through **unchanged**. The signature Cloudreve sends covers the request path.

## Building from source

```bash
git clone https://github.com/lemoninternet/cloudreve-payment-bridge-mollie.git
cd cloudreve-payment-bridge-mollie
docker build -t cloudreve-payment-bridge-mollie .
```

To build with Compose instead of pulling the image, replace `image:` in the example above with:

```yaml
    build:
      context: .
      dockerfile: Dockerfile
```

The service is a single Go file (`main.go`). It uses SQLite through `go-sqlite3`, so the build needs CGO (the Dockerfile takes care of that).

## License

MIT, see [LICENSE](LICENSE).
