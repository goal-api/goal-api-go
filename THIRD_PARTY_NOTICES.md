# Third-party notices

This module is MIT licensed and has **no dependencies**, direct or transitive. `go.mod`
declares only the module path and the language version, and there is no `go.sum` because
there is nothing to verify.

Everything it needs comes from the standard library: `net/http`, `encoding/json`,
`crypto/hmac`, `crypto/sha256`, `context`, `sync/atomic`.

That is also why there is no WebSocket client here. Adding one would mean taking a
dependency on every consumer's behalf, so the package exposes `WebSocketURL()`,
`WebSocketHeader()` and the message builders instead, and you bring your own socket
library. See the README.

## Checking this yourself

```bash
go list -m all      # just this module
```
