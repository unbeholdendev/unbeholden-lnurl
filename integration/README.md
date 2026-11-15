# Regtest integration tests

Run from the repository root with Go, a C compiler, Bitcoin Core, Core Lightning
with CLNRest, and LND on `PATH`:

```sh
LNURL_REGTEST=1 go test -race -count=1 -timeout=10m -v ./integration
```

On x86_64 Linux, the pinned Nix environment includes CLNRest:

```sh
nix-shell integration/shell.nix --run \
  'LNURL_REGTEST=1 go test -race -count=1 -timeout=10m -v ./integration'
```

The pinned versions are Bitcoin Core 32.1, Core Lightning 26.06.10 and LND
0.21.6-beta. Nixpkgs omits CLNRest, so the shell extracts it from the upstream
26.06.10 release archive, checks its hash and patches its library paths. With
another installation, set `LNURL_CLNREST_PLUGIN` to the plugin executable if
CLNRest is not already bundled with `lightningd`.

Forgejo CI runs the suite on pull requests and pushes to main.

The suite starts a private regtest chain, a CLN payer and one receiver of each
backend. It mines funds and opens channels itself. Nodes and service processes
use temporary directories and local ports, and stop when the tests finish.
No Docker daemon, existing node, real funds or public relay is needed.
An enabled run fails if a dependency is missing. Ordinary `go test ./...` skips it.
The service executable is built with the race detector too.

Both backends run the same tests for address metadata, invoice description
hashes, verification after configuration edits, payment notifications after
downtime and delivery failures, notifications-only mode, proxy trust, rate
limits, voucher claims after disconnects and restarts, corrupt state, and zaps
without ntfy.

The zap test pays while the service is stopped, then checks verification and
receipt generation after restart. It supplies a loopback relay and checks that
the production publisher refuses it. The WebSocket tests in `internal/nostr`
cover relay delivery; this suite does not test a public relay or wallet UI.
