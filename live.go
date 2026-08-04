package goalapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// WebSocket support.
//
// LiveClient is the client, and for almost everything it is what you want:
//
//	live := client.Live()
//	if err := live.Connect(ctx); err != nil { return err }
//	defer live.Close()
//	live.Subscribe(fixtureID)
//	for msg := range live.Messages() { ... }
//
// The pieces below are what LiveClient is built from, kept exported because the protocol
// is the contract. Reach for them when you want the frames without the connection
// management: driving the socket from an existing event loop, or through a WebSocket
// library you already depend on.
//
// The connection authenticates twice, because two services are involved. The gateway
// authorises the HTTP upgrade from the Authorization header, and websocket-service then
// requires an auth frame as the very first message, closing with 4001 if anything else
// arrives first. AuthMessage() builds that frame.
//
//	conn, _, err := websocket.Dial(ctx, client.WebSocketURL(), &websocket.DialOptions{
//	    HTTPHeader: client.WebSocketHeader(),
//	})
//	if err != nil { return err }
//	defer conn.CloseNow()
//
//	// Must be the first frame on the wire.
//	auth, _ := json.Marshal(client.AuthMessage())
//	if err := conn.Write(ctx, websocket.MessageText, auth); err != nil { return err }
//
//	for {
//	    _, data, err := conn.Read(ctx)
//	    if err != nil { return err }
//	    var msg goalapi.LiveMessage
//	    if err := json.Unmarshal(data, &msg); err != nil { continue }
//	    switch msg.Type {
//	    case goalapi.LiveAuthSuccess:
//	        sub, _ := json.Marshal(goalapi.SubscribeMessage(fixtureID))
//	        _ = conn.Write(ctx, websocket.MessageText, sub)
//	    case goalapi.LiveMatchUpdate:
//	        // msg.Data holds the match payload
//	    }
//	}
//
// The server caps client messages at 60/minute and concurrent subscriptions by plan.

// Server to client message types. Replies to client requests are named
// "<request>_response", so subscribe is answered with subscribe_response.
const (
	LiveAuthSuccess              = "auth_success"
	LiveMatchUpdate              = "match_update"
	LivePong                     = "pong"
	LiveStatus                   = "status"
	LiveServerShutdown           = "server_shutdown"
	LiveError                    = "error"
	LiveSubscribeResponse        = "subscribe_response"
	LiveUnsubscribeResponse      = "unsubscribe_response"
	LiveGetSubscriptionsResponse = "get_subscriptions_response"
)

// LiveMessage is a frame from the live WebSocket.
type LiveMessage struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`
	Timestamp int64           `json:"timestamp,omitempty"`
	Success   *bool           `json:"success,omitempty"`

	// Set on an "error" frame.
	Message string `json:"message,omitempty"`
	Code    string `json:"code,omitempty"`
}

// Into decodes the Data payload into dst.
func (m *LiveMessage) Into(dst any) error {
	if len(m.Data) == 0 {
		return nil
	}
	return json.Unmarshal(m.Data, dst)
}

// WebSocketURL returns the live endpoint, derived from the client's base URL so a staging
// override carries over.
//
// Note the path is /ws on the host root, not /v1/ws. nginx routes the socket with
// "location ^~ /ws", the only location carrying the Upgrade headers; /v1/ws falls into the
// REST location and silently answers 200 rather than upgrading.
func (c *Client) WebSocketURL() string {
	parsed, err := url.Parse(c.baseURL)
	if err != nil {
		return c.baseURL + "/ws"
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	}
	parsed.Path = "/ws"
	parsed.RawQuery = ""
	return parsed.String()
}

// AuthMessage builds the frame that must be sent first on a new connection. Anything else
// first and the server closes with 4001.
func (c *Client) AuthMessage() map[string]any {
	return map[string]any{"type": "auth", "apiKey": c.apiKey}
}

// AuthMessageWithToken is the browser-side variant, using a token from MintConnectToken
// rather than the raw API key.
func AuthMessageWithToken(token string) map[string]any {
	return map[string]any{"type": "auth", "token": token}
}

// WebSocketHeader returns the headers to send on the handshake. Pass it to your WebSocket
// library's dial options.
func (c *Client) WebSocketHeader() http.Header {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.apiKey)
	header.Set("User-Agent", c.userAgent)
	for key, value := range c.headers {
		header.Set(key, value)
	}
	return header
}

// ConnectToken is a short-lived, single-use WebSocket handshake token.
type ConnectToken struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expiresIn"`
}

// MintConnectToken mints a token for a browser client.
//
// Go servers can set the Authorization header, so they don't need this. Use it when your
// backend hands a token to a frontend, so the browser can connect to
// wss://.../ws?wsToken=<token> without seeing your API key. Single-use, consumed on
// first connect.
func (c *Client) MintConnectToken(ctx context.Context) (*ConnectToken, error) {
	var response struct {
		Success bool         `json:"success"`
		Data    ConnectToken `json:"data"`
	}
	if err := c.Post(ctx, "/ws/token", map[string]any{}, &response); err != nil {
		return nil, err
	}
	return &response.Data, nil
}

// SubscribeMessage builds the frame that subscribes to a match.
func SubscribeMessage(matchID string) map[string]any {
	return map[string]any{"type": "subscribe", "resource": "match", "matchId": matchID}
}

// UnsubscribeMessage builds the frame that unsubscribes from a match.
func UnsubscribeMessage(matchID string) map[string]any {
	return map[string]any{"type": "unsubscribe", "resource": "match", "matchId": matchID}
}

// PingMessage builds a keepalive frame. The server replies with a "pong".
func PingMessage() map[string]any {
	return map[string]any{"type": "ping"}
}

// StatusMessage asks for the connection's plan, subscriptions and feature flags.
func StatusMessage() map[string]any {
	return map[string]any{"type": "status"}
}

// ListSubscriptionsMessage asks the server which matches this connection is subscribed to.
func ListSubscriptionsMessage() map[string]any {
	return map[string]any{"type": "get_subscriptions"}
}
