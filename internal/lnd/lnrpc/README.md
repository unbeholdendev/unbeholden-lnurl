The protobuf definitions come from LND v0.20.1-beta's `lnrpc/lightning.proto`
and `lnrpc/routerrpc/router.proto`, under the adjacent MIT licence. Only the
six RPCs this client uses and their message definitions are retained. Field
numbers, protobuf packages and service names are unchanged. Both packages
share this licence.

Regenerate from the repository root with protoc 36.2, protoc-gen-go v1.36.11
and protoc-gen-go-grpc v1.6.2:

```
protoc -I internal/lnd/lnrpc -I internal/lnd/routerrpc \
  --go_out=. --go_opt=module=unbeholden-lnurl \
  --go-grpc_out=. --go-grpc_opt=module=unbeholden-lnurl \
  internal/lnd/lnrpc/lightning.proto internal/lnd/routerrpc/router.proto
```
