// Separate module on purpose: the SDK itself has zero dependencies, and this example
// needs a WebSocket library. Keeping it here means `go get` on the SDK pulls nothing.
module goalapi-live-example

go 1.21

require (
	github.com/Devara-sarl/goal-api-go v0.0.0
	github.com/coder/websocket v1.8.12
)

replace github.com/Devara-sarl/goal-api-go => ../..
