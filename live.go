package goalapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// WebSocket support.
//
// There is no WebSocket client here. This package has no dependencies and the standard
// library has no WebSocket support, so instead of picking one for you it exposes the two
// GOAL-specific pieces (the URL and the auth) and you bring your own socket library:
// github.com/coder/websocket, github.com/gorilla/websocket, nhooyr.io/websocket.
//
// A Go client is server-side, so it can set the Authorization header on the handshake and
// does not need a minted token:
//
//	import "github.com/coder/websocket"
//
//	conn, _, err := websocket.Dial(ctx, client.WebSocketURL(), &websocket.DialOptions{
//	    HTTPHeader: client.WebSocketHeader(),
//	})
//	if err != nil { return err }
//	defer conn.CloseNow()
//
//	sub, _ := json.Marshal(goalapi.SubscribeMessage(fixtureID))
//	if err := conn.Write(ctx, websocket.MessageText, sub); err != nil { return err }
//
//	for {
//	    _, data, err := conn.Read(ctx)
//	    if err != nil { return err }
//	    var msg goalapi.LiveMessage
//	    if err := json.Unmarshal(data, &msg); err != nil { continue }
//	    if msg.Type == goalapi.LiveMatchUpdate {
//	        // msg.Data holds the match payload
//	    }
//	}
//
// The server caps client messages at 60/minute and concurrent subscriptions by plan.

// Server → client message types.
const (
	LiveAuthSuccess    = "auth_success"
	LiveMatchUpdate    = "match_update"
	LivePong           = "pong"
	LiveStatus         = "status"
	LiveServerShutdown = "server_shutdown"
	LiveError          = "error"
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
func (c *Client) WebSocketURL() string {
	if after, ok := strings.CutPrefix(c.baseURL, "https://"); ok {
		return "wss://" + after + "/ws"
	}
	if after, ok := strings.CutPrefix(c.baseURL, "http://"); ok {
		return "ws://" + after + "/ws"
	}
	return c.baseURL + "/ws"
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
