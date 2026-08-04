package goalapi

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A WebSocket server, good enough to prove the client speaks the protocol.
//
// The client is hand-rolled, so testing it against a mock that merely returns canned bytes
// would only prove the mock agrees with itself. This does the real handshake and real
// framing, and asserts the things RFC 6455 requires of a client: that it masks, that it
// answers pings, that it reassembles fragments.

type testServer struct {
	*httptest.Server

	mu        sync.Mutex
	conns     []*testConn
	handshake func(r *http.Request) (int, string) // non-101 status to reject with
}

type testConn struct {
	conn net.Conn
	br   *bufio.Reader

	mu     sync.Mutex
	closed bool
}

func newTestServer(t *testing.T, onConn func(c *testConn)) *testServer {
	t.Helper()

	ts := &testServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ts.handshake != nil {
			if status, body := ts.handshake(r); status != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
		}

		key := r.Header.Get("Sec-WebSocket-Key")
		if key == "" {
			http.Error(w, "missing key", http.StatusBadRequest)
			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			return
		}

		response := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
		if _, err := io.WriteString(conn, response); err != nil {
			_ = conn.Close()
			return
		}

		tc := &testConn{conn: conn, br: buf.Reader}
		ts.mu.Lock()
		ts.conns = append(ts.conns, tc)
		ts.mu.Unlock()

		onConn(tc)
	}))

	t.Cleanup(func() {
		ts.Server.Close()
		ts.mu.Lock()
		for _, c := range ts.conns {
			c.close()
		}
		ts.mu.Unlock()
	})

	return ts
}

// wsURL turns the httptest http:// address into a ws:// one.
func (ts *testServer) wsURL() string {
	return "ws" + strings.TrimPrefix(ts.Server.URL, "http")
}

// readFrame reads one client frame. Client frames must always be masked.
//
// Nothing here calls t.Fatalf: these run on the server's own goroutine, where FailNow is
// not allowed, and a read error during teardown is expected rather than a failure.
func (c *testConn) readFrame() (opcode byte, payload []byte, err error) {
	var header [2]byte
	if _, err := io.ReadFull(c.br, header[:]); err != nil {
		return 0, nil, err
	}
	opcode = header[0] & 0x0F
	masked := header[1]&0x80 != 0
	if !masked {
		return 0, nil, errors.New("client frame was not masked, which RFC 6455 forbids")
	}

	length := int64(header[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}

	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return 0, nil, err
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return opcode, payload, nil
}

// readJSON reads the next text frame and decodes it. The bool is false once the
// connection is gone, which is how the handler goroutines know to stop.
func (c *testConn) readJSON() (map[string]any, bool) {
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			return nil, false
		}
		if opcode != opText {
			continue
		}
		var out map[string]any
		if err := json.Unmarshal(payload, &out); err != nil {
			return nil, false
		}
		return out, true
	}
}

// writeFrame writes an unmasked server frame, which is what servers must send.
func (c *testConn) writeFrame(fin bool, opcode byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}

	var header []byte
	first := opcode
	if fin {
		first |= 0x80
	}
	header = append(header, first)

	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(length))
	case length <= 0xFFFF:
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(length))
	default:
		header = append(header, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(length))
	}

	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

func (c *testConn) writeJSON(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(true, opText, payload)
}

func (c *testConn) writeClose(code int, reason string) error {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)
	return c.writeFrame(true, opClose, payload)
}

func (c *testConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		_ = c.conn.Close()
	}
}

// testClient builds a client pointed at the fake server.
func testClient(t *testing.T, ts *testServer) *Client {
	t.Helper()
	c, err := New("test-key", WithBaseURL(ts.Server.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// expectAuth reads the auth frame the client must send first and answers auth_success.
// It reports whether the exchange completed; the caller returns if it did not.
func expectAuth(c *testConn) bool {
	msg, ok := c.readJSON()
	if !ok || msg["type"] != "auth" {
		// The real server closes with 4001 when the first frame is not auth.
		return false
	}
	return c.writeJSON(map[string]any{"type": "auth_success"}) == nil
}

func TestLiveConnectSendsAuthFirst(t *testing.T) {
	ready := make(chan map[string]any, 1)
	ts := newTestServer(t, func(c *testConn) {
		msg, ok := c.readJSON()
		if !ok {
			return
		}
		ready <- msg
		_ = c.writeJSON(map[string]any{"type": "auth_success"})
		select {} // hold the connection open for the duration of the test
	})

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := live.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer live.Close()

	select {
	case msg := <-ready:
		if msg["type"] != "auth" {
			t.Errorf("first frame type = %q, want auth", msg["type"])
		}
		if msg["apiKey"] != "test-key" {
			t.Errorf("auth frame apiKey = %v, want test-key", msg["apiKey"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the auth frame")
	}

	if !live.Connected() {
		t.Error("Connected() = false after a successful auth")
	}
}

func TestLiveReceivesMatchUpdate(t *testing.T) {
	ts := newTestServer(t, func(c *testConn) {
		if !expectAuth(c) {
			return
		}
		_ = c.writeJSON(map[string]any{
			"type": "match_update",
			"data": map[string]any{"id": "match-1", "homeScore": 2},
		})
		select {}
	})

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()))

	got := make(chan LiveMessage, 1)
	live.On(LiveMatchUpdate, func(m LiveMessage) { got <- m })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := live.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer live.Close()

	select {
	case msg := <-got:
		var payload struct {
			ID        string `json:"id"`
			HomeScore int    `json:"homeScore"`
		}
		if err := msg.Into(&payload); err != nil {
			t.Fatalf("Into: %v", err)
		}
		if payload.ID != "match-1" || payload.HomeScore != 2 {
			t.Errorf("payload = %+v, want {match-1 2}", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no match_update arrived")
	}
}

func TestLiveSubscribeSendsFrame(t *testing.T) {
	frames := make(chan map[string]any, 4)
	ts := newTestServer(t, func(c *testConn) {
		if !expectAuth(c) {
			return
		}
		for {
			opcode, payload, err := c.readFrame()
			if err != nil {
				return
			}
			if opcode != opText {
				continue
			}
			var msg map[string]any
			if json.Unmarshal(payload, &msg) == nil {
				frames <- msg
			}
		}
	})

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := live.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer live.Close()

	if err := live.Subscribe("match-42"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	select {
	case msg := <-frames:
		if msg["type"] != "subscribe" || msg["matchId"] != "match-42" || msg["resource"] != "match" {
			t.Errorf("subscribe frame = %v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no subscribe frame arrived")
	}

	if subs := live.Subscriptions(); len(subs) != 1 || subs[0] != "match-42" {
		t.Errorf("Subscriptions() = %v, want [match-42]", subs)
	}
}

func TestLiveAnswersPingWithPong(t *testing.T) {
	pong := make(chan []byte, 1)
	ts := newTestServer(t, func(c *testConn) {
		if !expectAuth(c) {
			return
		}
		if err := c.writeFrame(true, opPing, []byte("keepalive")); err != nil {
			return
		}
		for {
			opcode, payload, err := c.readFrame()
			if err != nil {
				return
			}
			if opcode == opPong {
				pong <- payload
				return
			}
		}
	})

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := live.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer live.Close()

	select {
	case payload := <-pong:
		// RFC 6455 section 5.5.3: the pong carries the ping's payload verbatim.
		if string(payload) != "keepalive" {
			t.Errorf("pong payload = %q, want keepalive", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client never answered the ping")
	}
}

func TestLiveReassemblesFragmentedMessage(t *testing.T) {
	ts := newTestServer(t, func(c *testConn) {
		if !expectAuth(c) {
			return
		}
		// One JSON object split across three frames, with a ping interleaved: control
		// frames are allowed to arrive in the middle of a fragmented message.
		full := `{"type":"match_update","data":{"id":"frag-1"}}`
		_ = c.writeFrame(false, opText, []byte(full[:15]))
		_ = c.writeFrame(true, opPing, []byte("mid"))
		_ = c.writeFrame(false, opContinuation, []byte(full[15:30]))
		_ = c.writeFrame(true, opContinuation, []byte(full[30:]))
		select {}
	})

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()))

	got := make(chan LiveMessage, 1)
	live.On(LiveMatchUpdate, func(m LiveMessage) { got <- m })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := live.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer live.Close()

	select {
	case msg := <-got:
		var payload struct {
			ID string `json:"id"`
		}
		if err := msg.Into(&payload); err != nil {
			t.Fatalf("Into: %v", err)
		}
		if payload.ID != "frag-1" {
			t.Errorf("id = %q, want frag-1", payload.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fragmented message was never reassembled")
	}
}

func TestLiveLargeMessageUsesExtendedLength(t *testing.T) {
	// Payloads over 125 bytes switch to a 16-bit length, and over 64KiB to a 64-bit one.
	// Both header shapes have to decode.
	big := strings.Repeat("x", 70_000)
	ts := newTestServer(t, func(c *testConn) {
		if !expectAuth(c) {
			return
		}
		_ = c.writeJSON(map[string]any{"type": "match_update", "data": map[string]any{"id": big}})
		select {}
	})

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()))

	got := make(chan LiveMessage, 1)
	live.On(LiveMatchUpdate, func(m LiveMessage) { got <- m })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := live.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer live.Close()

	select {
	case msg := <-got:
		var payload struct {
			ID string `json:"id"`
		}
		if err := msg.Into(&payload); err != nil {
			t.Fatalf("Into: %v", err)
		}
		if len(payload.ID) != len(big) {
			t.Errorf("payload length = %d, want %d", len(payload.ID), len(big))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("large message never arrived")
	}
}

func TestLiveAuthRejectionSurfacesAsAuthError(t *testing.T) {
	ts := newTestServer(t, func(c *testConn) {
		if _, ok := c.readJSON(); !ok {
			return
		}
		// 4001 is what websocket-service sends when the auth frame is not first, and it
		// also uses the range for a rejected key.
		_ = c.writeClose(4001, "invalid api key")
		time.Sleep(100 * time.Millisecond)
		c.close()
	})

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()), WithLiveAutoReconnect(false))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := live.Connect(ctx)
	if err == nil {
		t.Fatal("Connect succeeded against a server that rejected the auth")
	}
	if !errors.Is(err, ErrAuthentication) {
		t.Errorf("error = %v, want it to wrap ErrAuthentication", err)
	}
}

func TestLiveHandshakeRejectionSurfacesAPIError(t *testing.T) {
	ts := newTestServer(t, func(c *testConn) { c.close() })
	ts.handshake = func(r *http.Request) (int, string) {
		return http.StatusUnauthorized, `{"success":false,"error":{"message":"bad key","code":"AUTHENTICATION_ERROR"}}`
	}

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()), WithLiveAutoReconnect(false))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := live.Connect(ctx)
	if err == nil {
		t.Fatal("Connect succeeded against a 401 handshake")
	}
	// A non-101 carries a normal API error body and should read like any other API failure.
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v (%T), want *goalapi.Error", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
}

func TestLiveSendsAuthorizationHeaderOnHandshake(t *testing.T) {
	header := make(chan string, 1)
	ts := newTestServer(t, func(c *testConn) {
		if !expectAuth(c) {
			return
		}
		select {}
	})
	ts.handshake = func(r *http.Request) (int, string) {
		select {
		case header <- r.Header.Get("Authorization"):
		default:
		}
		return 0, ""
	}

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := live.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer live.Close()

	select {
	case got := <-header:
		// The gateway authorises the upgrade from this header, before the auth frame is
		// ever read.
		if got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake never ran")
	}
}

// withFixedReconnectDelay collapses the backoff so a reconnect test finishes in
// milliseconds instead of seconds. Test-only, which is why it is not a LiveOption in the
// public set.
func withFixedReconnectDelay(d time.Duration) LiveOption {
	return func(l *LiveClient) {
		l.reconnectDelay = func(int) time.Duration { return d }
	}
}

var attemptsMu sync.Mutex

func TestLiveReconnectsAndReplaysSubscriptions(t *testing.T) {
	var attempts int
	resubscribed := make(chan string, 4)

	ts := newTestServer(t, func(c *testConn) {
		attemptsMu.Lock()
		attempts++
		n := attempts
		attemptsMu.Unlock()

		if !expectAuth(c) {
			return
		}

		if n == 1 {
			// Read the first subscribe, then drop the connection to force a reconnect.
			_, _ = c.readJSON()
			c.close()
			return
		}

		for {
			msg, ok := c.readJSON()
			if !ok {
				return
			}
			if msg["type"] == "subscribe" {
				if id, ok := msg["matchId"].(string); ok {
					resubscribed <- id
				}
			}
		}
	})

	client := testClient(t, ts)
	live := client.Live(
		WithLiveURL(ts.wsURL()),
		withFixedReconnectDelay(20*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := live.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer live.Close()

	if err := live.Subscribe("match-7"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	select {
	case id := <-resubscribed:
		// The server forgets a dropped connection's subscriptions, so without replay a
		// reconnect would deliver nothing.
		if id != "match-7" {
			t.Errorf("resubscribed to %q, want match-7", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client never replayed its subscription after reconnecting")
	}
}

func TestLiveCloseStopsReconnecting(t *testing.T) {
	ts := newTestServer(t, func(c *testConn) {
		if !expectAuth(c) {
			return
		}
		select {}
	})

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := live.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if err := live.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if live.Connected() {
		t.Error("Connected() = true after Close")
	}
	// Close twice must be safe: deferred Close after an explicit one is the common shape.
	if err := live.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	// Messages is closed on shutdown, so ranging over it terminates.
	select {
	case _, open := <-live.Messages():
		for open {
			_, open = <-live.Messages()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Messages was never closed")
	}
}

func TestLiveRunReturnsOnContextCancel(t *testing.T) {
	ts := newTestServer(t, func(c *testConn) {
		if !expectAuth(c) {
			return
		}
		select {}
	})

	client := testClient(t, ts)
	live := client.Live(WithLiveURL(ts.wsURL()))

	if err := live.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- live.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestAcceptKeyMatchesRFCExample(t *testing.T) {
	// The worked example from RFC 6455 section 1.3.
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("acceptKey = %q, want s3pPLMBiTxaQ9kYGzzhZRbK+xOo=", got)
	}
}

func TestHeaderContainsToken(t *testing.T) {
	cases := []struct {
		value string
		token string
		want  bool
	}{
		{"Upgrade", "upgrade", true},
		{"keep-alive, Upgrade", "upgrade", true},
		{"UPGRADE", "upgrade", true},
		{"keep-alive", "upgrade", false},
		{"", "upgrade", false},
	}
	for _, tc := range cases {
		if got := headerContainsToken(tc.value, tc.token); got != tc.want {
			t.Errorf("headerContainsToken(%q, %q) = %v, want %v", tc.value, tc.token, got, tc.want)
		}
	}
}
