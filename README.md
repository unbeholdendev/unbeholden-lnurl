# unbeholden-lnurl

A small, self-hosted Lightning Address server written in Go. It lets you receive Lightning
payments at `you@yourdomain.com` using your own node, instead of relying on a custodial
provider.

Most Lightning Addresses are issued by a third party. That means someone else's server
creates the invoices you get paid with, and sees who is paying you. This serves them from
your own LND or Core Lightning node instead.

## How it works

A Lightning Address is a human-readable wrapper around LNURL-pay:

1. A wallet resolves `you@yourdomain.com` to `GET /.well-known/lnurlp/you`
2. The server replies with a `payRequest` document: callback URL, min/max sendable amounts,
   and metadata
3. The wallet calls the callback with the amount it wants to send
4. The server asks your node to create an invoice and returns the BOLT11 string
5. The wallet pays the invoice and the funds settle in your node

## Features

- LNURL-pay / Lightning Address support (LUD-06, LUD-16), with comments (LUD-12), a
  message after payment (LUD-09), a long description (LUD-20), an avatar, and payment
  verification (LUD-21)
- Nostr zaps (NIP-57): zap receipts are published to the payer's relays once paid
- Notifications for every invoice settled on the node, via ntfy or signed webhooks, including ones settled
  while the service was down
- LNURL-withdraw vouchers (LUD-03) listed in the config
- A payments API that lets your own applications pay invoices and Lightning Addresses,
  each with its own token and spending limits
- LND backend over gRPC with macaroon authentication
- Core Lightning backend over CLNRest with rune authentication
- YAML configuration
- Per-client request rate limiting, configurable with `rate_limit`
- Docker image and Compose file for deployment

## Requirements

- One of:
  - an LND node reachable over gRPC, plus its TLS certificate and a macaroon
  - a Core Lightning node with CLNRest enabled (`clnrest-port`), plus a rune and the
    node's `ca.pem`
- A domain with a reverse proxy in front of the server (Caddy, nginx, or similar).
  Onion domains use HTTP; other domains need TLS.
- Go 1.26+ if building from source

## Quick start

From a checkout of the repository:

```bash
cp config.example.yaml config.yaml
# edit config.yaml: your address names and node connection details
# copy tls.cert and invoice.macaroon beside docker-compose.yaml

docker compose up -d
```

The container runs as a non-root user. Compose stores writable state in a named
volume, initialised with the image's data-directory ownership. Create the config,
certificate and macaroon files before starting Compose, and make them readable
by the container's UID 65532. Mount any other files your config uses,
such as a withdraw secret, separately.

Keep the named volume across upgrades. `docker compose down` keeps it;
`docker compose down -v` deletes it, including voucher claims and settlement cursors.

Then verify it resolves:

```bash
curl https://yourdomain.com/.well-known/lnurlp/you
```

## Configuration

See [`config.example.yaml`](./config.example.yaml) for all available options.

Configure one node, under `lnd` or `cln`.

Each feature runs only when its section is present: `addresses` for Lightning Addresses,
`withdraw` for vouchers, `payments` for the payments API, `notifications` for payment
notifications, `nostr` for zaps. With
only `notifications`, no HTTP listener is started.

Payment notifications and zap receipts track settlements independently, so ntfy failures
retry without blocking later zap receipts. Keep `settle_index` and `zap_settle_index`
in `state_dir` across restarts. On upgrade, the zap cursor starts from the existing
`settle_index` before the HTTP listener starts.

### Webhooks

Under `notifications.webhooks`, give each receiver a name, an HTTPS URL, a list
of events and its own signing secret. Set either `secret` or `secret_file`.
Generate a secret for each receiver and give it a copy over a trusted channel.

```sh
printf 'whsec_%s\n' "$(openssl rand -base64 32)"
```

| Event | When it fires | Fields in `data` |
| --- | --- | --- |
| `invoice.created` | This service creates an address invoice | `amount_msat`, `payment_hash`, `address`, `comment` |
| `payment.received` | Any invoice on the node settles | `amount_msat`, `payment_hash` |
| `voucher.claimed` | The voucher payout succeeds | `amount_msat`, `voucher_id` |
| `voucher.payout_failed` | The node confirms the payout failed | `amount_msat`, `voucher_id` |
| `voucher.payout_unknown` | The payout result is uncertain and the voucher stays claimed | `amount_msat`, `voucher_id` |
| `payment.sent` | A payments API payment succeeds | `client`, `id`, `amount_msat`, `fee_msat`, `payment_hash`, `destination` |
| `payment.failed` | The node confirms a payments API payment failed | `client`, `id`, `amount_msat`, `fee_msat`, `payment_hash`, `destination` |

Each JSON POST contains `id`, `type`, `occurred_at` (UTC) and `data`. Verify
the signature before acting. Use the exact request body and the
`webhook-id`, `webhook-timestamp` and `webhook-signature` headers with a
[Standard Webhooks verifier](https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md#symmetric-signatures).
Reject stale delivery timestamps. Record processed IDs alongside the action
they trigger so a retry cannot repeat it. Return a 2xx status after accepting
the event.

New endpoints start with new events. Pending deliveries survive restarts under
`state_dir/webhooks/`. Each endpoint retries temporary failures for up to 72
hours. Events can arrive out of order. Failed deliveries remain on disk for
inspection. Removing an endpoint or changing its URL retires its old queue.

### Diagnostics

Check the deployment without starting a listener or creating invoices:

```sh
unbeholden-lnurl -config config.yaml -diagnose
```

With Compose, run the checks under the container's user with its mounted files:

```sh
docker compose run --rm --no-deps unbeholden-lnurl -config /app/config.yaml -diagnose
```

The command loads the config and its feature files, checks verification and zap
keys, probes state-directory writes and renames, and makes read-only node calls.
It prints `OK`, `FAIL`, `NOTE` and `SKIP` lines and exits nonzero if a check fails.
A missing state directory is created; temporary probe files are removed. Existing
settlement cursors and voucher claims are left alone.

While the service is running, add a public metadata URL to check HTTPS, LNURL-pay
metadata, the callback host, browser CORS access and the configured zap public key:

```sh
unbeholden-lnurl -config config.yaml -diagnose \
  -public-url https://yourdomain.com/.well-known/lnurlp/you
```

The public check fetches metadata only. It does not follow redirects or request an
invoice. HTTP is accepted for onion addresses, and `outbound_proxy` also applies
to this check. Node and public requests each have a 10-second limit, within a
30-second limit for the command. Invoice creation, payment and subscription
permissions are not exercised, and no notifications or zap receipts are sent.

### Payment verification

Set `verify_key_file` to keep LUD-21 verify URLs valid when the node's macaroon or rune
changes. For a new deployment, create a private file with `openssl rand -out verify.key 32`.
Keep it across restarts. To preserve existing verify URLs, copy the current macaroon or
rune file byte for byte before changing the node credential, then use that copy.

Without `verify_key_file`, the server still derives the key from the node credential.
Replacing that credential invalidates previously issued verify URLs.

### Withdraw vouchers

Keep the withdraw secret outside the config with `withdraw_secret_file`. The file holds
at least 32 characters; generate it with `openssl rand -hex 32`. Use either
`withdraw_secret` or `withdraw_secret_file`, not both. Moving the same secret to a file
preserves voucher URLs. Voucher definitions may stay in a world-readable config.

Startup logs show only the unclaimed voucher count. To print their URLs and LNURLs as
JSON, run `unbeholden-lnurl -config config.yaml -list-vouchers`. It reads the claim state
and exits without connecting to the node. Treat its output as bearer secrets.

### Payments API

Under `payments.clients`, give each application a name and a bearer token of at least
32 characters, as `token` or `token_file`. Generate one with `openssl rand -hex 32`.
Each client may also set these limits. Amounts are in millisatoshis.

| Setting | Effect |
| --- | --- |
| `allowed_ips` | Accepts requests only from these addresses or CIDR ranges |
| `max_payment` | Caps the amount of one payment |
| `daily_budget` | Caps amounts plus fees over any 24 hours. A pending payment counts its maximum fee |
| `max_fee` | Caps the fee of one payment. The default is 1% of the amount, at least 10 sat |

A limit left out does not apply. The API runs on the same listener as LNURL, and its
responses carry no CORS header, so web pages cannot call it. Route only the LNURL paths
through the public reverse proxy, and reach `/payments` from your applications' network.

Pay an invoice, or a Lightning Address with an amount and an optional comment:

```sh
curl -H "Authorization: Bearer $TOKEN" https://pay.internal/payments \
  -d '{"id": "order-1", "invoice": "lnbc..."}'
curl -H "Authorization: Bearer $TOKEN" https://pay.internal/payments \
  -d '{"id": "tip-1", "address": "alice@example.com", "amount_msat": 21000, "comment": "Thanks"}'
```

The client chooses `id`: 1 to 64 letters, digits or `._:-`, unique among its own payments.
Sending the same request again returns the existing payment and pays nothing. Reusing an
`id` for a different request returns 409. Invoices must carry an amount. The server
fetches a Lightning Address over HTTPS, or through `outbound_proxy` for an onion address,
and rejects an invoice whose amount or description hash does not match.

The request waits up to 2 minutes and returns the payment:

```json
{
  "id": "order-1",
  "status": "succeeded",
  "amount_msat": 21000,
  "fee_msat": 3,
  "payment_hash": "...",
  "preimage": "...",
  "destination": "lnbc...",
  "created_at": "2026-11-26T12:00:00Z",
  "completed_at": "2026-11-26T12:00:02Z"
}
```

`status` is `pending`, `succeeded` or `failed`. While a payment is pending, `GET
/payments/{id}` asks the node for its outcome; the server also checks pending payments at
startup. `GET /payments?since=2026-11-01T00:00:00Z` lists the client's payments created
since that time, oldest first, under `payments`.

Errors return `{"error": "..."}` with status 400 for an invalid request, 401 for a wrong
token, 403 for a disallowed address or an exceeded limit, 404 for an unknown `id`, 409
for a reused `id` or an invoice already paid, and 502 when a Lightning Address cannot be
resolved.

The server keeps payments in `state_dir/payments.json` for 90 days after they complete.

### Zaps

`nostr.private_key_file` holds the key zap receipts are signed with, as hex or `nsec`.
Use a key of its own, not your profile's: it is published as `nostrPubkey`, and the server
holds it in the clear. Generate one with `openssl rand -hex 32`.

Receipts go to the relays the zap request names, over `wss://` and to public addresses
only. A relay that refuses one is not retried.

Set `outbound_proxy: "socks5://127.0.0.1:9050"` to send ntfy requests, webhook deliveries and relay
publishing through a SOCKS5 proxy. The node connection is unchanged. The proxy resolves
relay names and must enforce any address restrictions; the server's public-address
check applies only to direct relay connections.

### Behind a reverse proxy

List the proxy's address in `trusted_proxies`. Only requests from a listed address may set
the client address with `X-Forwarded-For`, used for rate limiting, and the domain with
`X-Forwarded-Host`. Other requests use the connection's address and `Host`. With Docker's
port publishing, the proxy's address is usually the gateway of the container's network.

Earlier versions trusted these headers from anyone. Behind a proxy, without
`trusted_proxies`, all clients now share the proxy's rate limit, and a proxy that does not
pass the original `Host` serves the wrong domain.

On shutdown the server waits up to 2m10s for requests in flight, so a withdraw or API
payment can finish. Give the container at least that long to stop; the Compose file does.

## Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/.well-known/lnurlp/{name}` | Returns the LNURL-pay `payRequest` document |
| `GET` | *(callback from above)* | Returns a BOLT11 invoice for the requested amount |
| `GET` | *(`verify` from above)* | Reports whether that invoice is paid, with its preimage once it is |
| `POST` | `/payments` | Pays an invoice or Lightning Address for a payments client |
| `GET` | `/payments/{id}` | Returns a client's payment |
| `GET` | `/payments` | Lists a client's payments, optionally `since` a time |

## Security notes

Give the server a macaroon with only the permissions its enabled features need, never
`admin.macaroon`:

| Feature | Permissions |
| --- | --- |
| `addresses` | `invoices:read`, `invoices:write` |
| `notifications`, `nostr` | `invoices:read` |
| `withdraw` | `offchain:write`, `info:read` |
| `payments` | `offchain:read`, `offchain:write`, `info:read` |

LND's `invoice.macaroon` covers addresses and notifications. Bake others with
`lncli bakemacaroon`.

On Core Lightning, give a rune limited to the methods the enabled features call:

| Feature | Methods |
| --- | --- |
| `addresses` | `invoice`, `listinvoices` |
| `notifications`, `nostr` | `listinvoices`, `waitanyinvoice` |
| `withdraw` | `pay`, `listpays`, `getinfo` |
| `payments` | `pay`, `listpays`, `getinfo` |

For example, for addresses and notifications:

```bash
lightning-cli createrune restrictions='[["method=invoice","method=listinvoices","method=waitanyinvoice"]]'
```

Core Lightning stores no memo on an invoice, so the payer's comment is kept in the
invoice's label.

Without `withdraw` or `payments`, the server cannot move funds: it asks your node for an invoice and hands
it to the payer.

## Development

```bash
go run ./cmd/unbeholden-lnurl
go test ./...
```

The [regtest integration suite](integration/README.md) starts disposable LND and Core
Lightning nodes to test payments, configuration and restart recovery.

Layout follows the standard Go convention: entrypoint in `cmd/unbeholden-lnurl`, everything
else under `internal/`.

## References

- [LNURL specifications (LUDs)](https://github.com/lnurl/luds)
- [LUD-16: Lightning Address](https://github.com/lnurl/luds/blob/luds/16.md)
- [LND API reference](https://lightning.engineering/api-docs/api/lnd/)
- [Core Lightning RPC reference](https://docs.corelightning.org/reference)

## License

MIT
