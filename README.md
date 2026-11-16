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
- Notifications for every invoice settled on the node, via ntfy, including ones settled
  while the service was down
- LNURL-withdraw vouchers (LUD-03) listed in the config
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
`withdraw` for vouchers, `notifications` for payment notifications, `nostr` for zaps. With
only `notifications`, no HTTP listener is started.

Payment notifications and zap receipts track settlements independently, so ntfy failures
retry without blocking later zap receipts. Keep `settle_index` and `zap_settle_index`
in `state_dir` across restarts. On upgrade, the zap cursor starts from the existing
`settle_index` before the HTTP listener starts.

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

### Zaps

`nostr.private_key_file` holds the key zap receipts are signed with, as hex or `nsec`.
Use a key of its own, not your profile's: it is published as `nostrPubkey`, and the server
holds it in the clear. Generate one with `openssl rand -hex 32`.

Receipts go to the relays the zap request names, over `wss://` and to public addresses
only. A relay that refuses one is not retried.

Set `outbound_proxy: "socks5://127.0.0.1:9050"` to send ntfy requests and relay
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

On shutdown the server waits up to 2m10s for requests in flight, so a withdraw payment can
finish. Give the container at least that long to stop; the Compose file does.

## Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/.well-known/lnurlp/{name}` | Returns the LNURL-pay `payRequest` document |
| `GET` | *(callback from above)* | Returns a BOLT11 invoice for the requested amount |
| `GET` | *(`verify` from above)* | Reports whether that invoice is paid, with its preimage once it is |

## Security notes

Give the server a macaroon with only the permissions its enabled features need, never
`admin.macaroon`:

| Feature | Permissions |
| --- | --- |
| `addresses` | `invoices:read`, `invoices:write` |
| `notifications`, `nostr` | `invoices:read` |
| `withdraw` | `offchain:write`, `info:read` |

LND's `invoice.macaroon` covers addresses and notifications. Bake others with
`lncli bakemacaroon`.

On Core Lightning, give a rune limited to the methods the enabled features call:

| Feature | Methods |
| --- | --- |
| `addresses` | `invoice`, `listinvoices` |
| `notifications`, `nostr` | `listinvoices`, `waitanyinvoice` |
| `withdraw` | `pay`, `listpays`, `getinfo` |

For example, for addresses and notifications:

```bash
lightning-cli createrune restrictions='[["method=invoice","method=listinvoices","method=waitanyinvoice"]]'
```

Core Lightning stores no memo on an invoice, so the payer's comment is kept in the
invoice's label.

Without `withdraw`, the server cannot move funds: it asks your node for an invoice and hands
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
