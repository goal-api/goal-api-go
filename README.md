# goal-go

Go client for the [GOAL API](https://goal-api.com): football fixtures, live scores,
standings, player stats, odds and real-time match updates over WebSocket.

Standard library only, no dependencies — including the WebSocket client. Go 1.21+.

```bash
go get github.com/goal-api/goal-api-go
```

## Quick start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    goalapi "github.com/goal-api/goal-api-go"
)

type Fixture struct {
    HomeTeam  struct{ Name string } `json:"homeTeam"`
    AwayTeam  struct{ Name string } `json:"awayTeam"`
    HomeScore *int                  `json:"homeScore"`
    AwayScore *int                  `json:"awayScore"`
}

func main() {
    client, err := goalapi.New(os.Getenv("GOAL_API_KEY"))
    if err != nil {
        log.Fatal(err)
    }

    page, err := client.Fixtures.Live(context.Background(), nil)
    if err != nil {
        log.Fatal(err)
    }

    var live []Fixture
    if err := page.Into(&live); err != nil {
        log.Fatal(err)
    }
    for _, match := range live {
        fmt.Println(match.HomeTeam.Name, "vs", match.AwayTeam.Name)
    }
}
```

Get a key at [goal-api.com/signup](https://goal-api.com/signup).

A `*Client` is safe for concurrent use. Share one so connections pool and the rate-limit
snapshot stays meaningful.

## Client options

```go
client, err := goalapi.New(apiKey,
    goalapi.WithTimeout(15*time.Second),   // per attempt; default 30s
    goalapi.WithMaxRetries(3),             // 429 + 5xx + network errors; default 2
    goalapi.WithBaseURL("https://api.goal-api.com/v1"),
    goalapi.WithHTTPClient(myInstrumentedClient),
    goalapi.WithHeader("X-My-App", "scoreboard"),
)
```

Retries use exponential backoff with full jitter and always honour a server-sent
`Retry-After`. A cancelled context is never retried.

## Responses

`Page` and `Item` keep `Data` as `json.RawMessage`, since rows are provider-shaped and
change. Decode into your own types:

```go
page, err := client.Leagues.Standings(ctx, leagueID, nil)

type Row struct {
    Position int    `json:"position"`
    Points   int    `json:"points"`
    Team     struct{ Name string } `json:"team"`
}
var table []Row
if err := page.Into(&table); err != nil { ... }

page.Pagination.HasMore   // *Pagination, nil on single-resource endpoints
page.Source               // "cache" | "database"
```

For scripts, `page.Rows()` and `item.Row()` give you `map[string]any` without a struct.

## Endpoints

Grouped by resource. `Params` is a `map[string]any`; nil and empty values are dropped, so
build it unconditionally. Full reference in [`ENDPOINTS.md`](ENDPOINTS.md).

```go
client.Status.Get(ctx)                                  // no API key needed
client.Countries.List(ctx, goalapi.Params{"search": "spa"})
client.Leagues.List(ctx, goalapi.Params{"isActive": true, "limit": 100})
client.Leagues.Standings(ctx, leagueID, nil)
client.Leagues.TopScorers(ctx, leagueID, goalapi.Params{"limit": 10})
client.Teams.Get(ctx, teamID, goalapi.Params{"includePlayers": true})
client.Teams.Statistics(ctx, teamID, goalapi.Params{"season": "2025-2026"})
client.Fixtures.List(ctx, goalapi.Params{"from": "2026-08-01", "to": "2026-08-07", "status": goalapi.StatusScheduled})
client.Fixtures.ByDate(ctx, "2026-08-15", goalapi.Params{"leagueId": leagueID})
client.Fixtures.Lineups(ctx, fixtureID)
client.Fixtures.Statistics(ctx, fixtureID, goalapi.Params{"half": goalapi.HalfFirst})
client.Standings.Form(ctx, leagueID, nil)
client.Players.Search(ctx, "haaland", goalapi.Params{"limit": 5})
client.Players.Compare(ctx, playerA, playerB)
client.Players.Top(ctx, goalapi.StatGoals, goalapi.Params{"limit": 20})
client.Coaches.ByTeam(ctx, teamID)
client.H2H.Stats(ctx, teamA, teamB)
client.Results.Today(ctx)
client.Videos.Recent(ctx, goalapi.Params{"leagueId": leagueID, "limit": 10})
client.Odds.List(ctx, goalapi.Params{"bookmaker": "bet365"})
client.Predictions.List(ctx, goalapi.Params{"matchId": matchID})
```

Enum values are constants: `Status*`, `PlayerType*`, `Stat*`, `Half*`.

### One exception: `client.Status.*`

The five `/public/*` endpoints don't use the `{success, data}` envelope. They return bare
objects, so these methods hand back `Raw` rather than `Page` or `Item`:

```go
body, err := client.Status.Get(ctx)

var status struct {
    Status     string `json:"status"`
    Components []struct{ Name, Status string } `json:"components"`
}
if err := body.Into(&status); err != nil { ... }
```

They also paginate with `page`/`limit` instead of `limit`/`offset`, so `Paginate` does not
apply to `CoverageLeagues`.

## Pagination

```go
fetch := func(ctx context.Context, p goalapi.Params) (*goalapi.Page, error) {
    return client.Leagues.Teams(ctx, leagueID, p)
}

// Collect everything into a slice:
var teams []Team
if err := client.CollectInto(ctx, fetch, nil, &teams); err != nil { ... }

// Or stream, to avoid holding it all in memory:
pager := client.Paginate(fetch, &goalapi.PaginateOptions{PageSize: 500, MaxItems: 5000})
for pager.Next(ctx) {
    var team Team
    if err := pager.Into(&team); err != nil { ... }
    fmt.Println(team.Name)
}
if err := pager.Err(); err != nil { ... }
```

Default `PageSize` is 100, the limit ceiling on most endpoints. `/results` and
`/countries` take 500.

## Errors

One `*Error` type plus sentinels, which is the Go-idiomatic shape. Branch with
`errors.Is`, read the detail with `errors.As`:

```go
page, err := client.Fixtures.Get(ctx, fixtureID)
switch {
case err == nil:
    // ...
case errors.Is(err, goalapi.ErrNotFound):
    return nil, nil
case errors.Is(err, goalapi.ErrRateLimited):
    var apiErr *goalapi.Error
    errors.As(err, &apiErr)
    time.Sleep(time.Duration(apiErr.RetryAfter) * time.Second)
case errors.Is(err, goalapi.ErrValidation):
    var apiErr *goalapi.Error
    errors.As(err, &apiErr)
    log.Printf("server rejected: %s %s", apiErr.Message, apiErr.Details)
default:
    return nil, err
}
```

Sentinels: `ErrValidation`, `ErrAuthentication`, `ErrPermission`,
`ErrPlanUpgradeRequired`, `ErrNotFound`, `ErrConflict`, `ErrRateLimited`,
`ErrServiceUnavailable`, `ErrServer`, `ErrTimeout`, `ErrNetwork`.

### Two error shapes

The API answers with one of two bodies, and the SDK normalises both:

| | Gateway (auth, routing, rate limits) | Football service (most endpoints) |
|---|---|---|
| text | `message` | `error` |
| `code` | yes | yes |
| `category` | yes | no |
| `correlationId` | yes | **no** |
| `details` | object | array, on validation errors |

So `apiErr.Message` and `apiErr.Code` are always populated, and `apiErr.CorrelationID` is
only set on gateway errors. Quote it in a support ticket when you have it.

### Rate limits

```go
quota := client.RateLimit()
// quota.Limit, quota.Remaining, quota.Reset (unix seconds), quota.Type ("DAILY"|"MONTHLY")
```

## Live WebSocket updates

> The socket is at `wss://api.goal-api.com/ws`, **not** `/v1/ws`. Only nginx's
> `location ^~ /ws` carries the `Upgrade` headers; `/v1/ws` is proxied as ordinary HTTP and
> answers 200 instead of upgrading. The SDK derives the right URL for you.
>
> Two services authenticate: the gateway authorises the upgrade from the header or
> `?wsToken=`, then websocket-service needs an `{"type": "auth", ...}` frame as the very
> first message. The SDK sends it, and treats `auth_success` as the point the connection is
> usable.
>
> **`subscribe` is capped per plan and the cap can be 0.** `auth_success` reports
> `maxSubscriptions`; if it is 0 the socket works but no `match_update` will ever arrive.
> See the known server issue in [`ENDPOINTS.md`](ENDPOINTS.md).

`client.Live()` is the client. It owns the socket, the auth handshake, keepalives and
reconnection, and it still pulls in no dependencies: the RFC 6455 client is implemented on
the standard library in [`ws.go`](ws.go).

```go
live := client.Live()

live.On(goalapi.LiveMatchUpdate, func(msg goalapi.LiveMessage) {
    var update MatchUpdate
    _ = msg.Into(&update)
})

if err := live.Connect(ctx); err != nil {   // returns once auth_success arrives
    return err
}
defer live.Close()

live.Subscribe(fixtureID)
live.Run(ctx)                               // blocks until ctx ends or it gives up
```

Or skip handlers and range over the channel, which is usually the more Go-shaped way:

```go
for msg := range live.Messages() {
    if msg.Type == goalapi.LiveMatchUpdate {
        var update MatchUpdate
        _ = msg.Into(&update)
    }
}
```

Handlers run on the reader goroutine, one message at a time, so a handler that blocks
stops the feed; hand slow work to a goroutine of your own. `Messages()` is a buffered
channel (1000 by default) that drops the oldest message when a consumer falls behind,
so a slow reader costs history rather than the connection.

Subscriptions are replayed after a reconnect, because the server does not remember a
dropped connection's. Options: `WithLiveAutoReconnect`, `WithLiveMaxReconnectAttempts`,
`WithLivePingInterval`, `WithLiveReadTimeout`, `WithLiveQueueSize`, `WithLiveAuthTimeout`,
`WithLiveTLSConfig`, `WithLiveConnectToken`, `WithLiveURL`.

Events: the server types below, plus `LiveEventOpen` and `LiveEventClose` for the
transport itself, and `LiveEventAny` for everything.

### Driving the socket yourself

The frame builders stay exported, for when you want the protocol without the connection
management — an existing event loop, or a WebSocket library you already depend on:
`SubscribeMessage`, `UnsubscribeMessage`, `PingMessage`, `StatusMessage`,
`ListSubscriptionsMessage`, alongside `WebSocketURL()`, `WebSocketHeader()` and
`AuthMessage()`. Server message types: `LiveMatchUpdate`, `LiveAuthSuccess`, `LiveStatus`,
`LivePong`, `LiveServerShutdown`, `LiveError`, `LiveSubscribeResponse`,
`LiveUnsubscribeResponse`, `LiveGetSubscriptionsResponse`.

The server caps client messages at 60/minute and concurrent subscriptions by plan. Only
`resource: "match"` is supported.

### Handing a token to a browser

A Go server can set the `Authorization` header, so it needs no token. Mint one when your
backend hands live access to a frontend, so the browser never sees your API key:

```go
token, err := client.MintConnectToken(ctx)
// browser: new WebSocket(`wss://api.goal-api.com/v1/ws?wsToken=${token.Token}`)
```

Single-use, consumed on first connect.

## Webhooks

Verify against the **raw** body. A decoded-and-re-encoded struct has different bytes and
will never match.

```go
func handler(w http.ResponseWriter, r *http.Request) {
    body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
    if err != nil {
        http.Error(w, "read failed", http.StatusBadRequest)
        return
    }

    raw, err := goalapi.VerifyWebhook(body, r.Header.Get(goalapi.SignatureHeader), secret, 0)
    if err != nil {
        http.Error(w, "bad signature", http.StatusBadRequest)
        return
    }

    switch r.Header.Get(goalapi.EventHeader) {
    case goalapi.EventGoalScored:
        var event GoalScored
        _ = json.Unmarshal(raw, &event)
    case goalapi.EventMatchFinished:
        // ...
    }
    w.WriteHeader(http.StatusOK)   // ack fast; retries are ~1m, 5m, 25m, 2h, 10h
}
```

`tolerance` of 0 uses `DefaultWebhookTolerance` (5 minutes). Deliveries older than that
are rejected as replays.

## Escape hatch

For an endpoint this SDK doesn't wrap yet:

```go
var page goalapi.Page
err := client.Get(ctx, "/some/new/endpoint", goalapi.Params{"limit": 10}, &page)
```

## Examples

| Directory | Shows |
|---|---|
| [`examples/basic`](examples/basic) | Status, live fixtures, standings, pagination |
| [`examples/live`](examples/live) | The live socket: subscribing and handling frames |
| [`examples/export`](examples/export) | Walking every page of a collection to CSV |

```bash
GOAL_API_KEY=... go run ./examples/basic
GOAL_API_KEY=... go run ./examples/live
GOAL_API_KEY=... go run ./examples/export > countries.csv
```

## Testing

```bash
go test -race ./...                       # unit tests, no network
GOAL_API_KEY=... go test -race ./...      # also runs the live tests
go vet ./... && gofmt -l .
go run ./examples/basic
```

The live tests skip themselves without a key. Endpoint-by-endpoint coverage of the API
lives in `tools/sweep.py` in the SDK workspace.

## Licence

MIT. See [LICENSE](LICENSE).

No dependencies, direct or transitive: standard library only, which is why there is no
`go.sum`. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

Security issues: [SECURITY.md](SECURITY.md).
