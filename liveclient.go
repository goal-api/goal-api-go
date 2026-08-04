package goalapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
)

// LiveClient is a connection to the live match feed.
//
// It owns the socket, the auth handshake, keepalives and reconnection. Create one with
// Client.Live, register handlers, then Connect:
//
//	live := client.Live()
//	live.On(goalapi.LiveMatchUpdate, func(m goalapi.LiveMessage) {
//	    var match Match
//	    _ = m.Into(&match)
//	})
//	if err := live.Connect(ctx); err != nil {
//	    return err
//	}
//	defer live.Close()
//	live.Subscribe(fixtureID)
//	live.Run(ctx)
//
// Or skip handlers entirely and range over Messages, which is usually the more Go-shaped
// way to write it:
//
//	for msg := range live.Messages() {
//	    if msg.Type == goalapi.LiveMatchUpdate {
//	        // ...
//	    }
//	}
//
// Handlers run on the reader goroutine, one message at a time and in registration order.
// A handler that blocks stops the feed, so hand slow work to a goroutine of your own.
type LiveClient struct {
	client *Client

	url            string
	autoReconnect  bool
	maxAttempts    int
	pingInterval   time.Duration
	authTimeout    time.Duration
	readTimeout    time.Duration
	dialTimeout    time.Duration
	queueSize      int
	tlsConfig      *tls.Config
	connectToken   string
	reconnectDelay func(attempt int) time.Duration

	mu            sync.Mutex
	conn          *wsConn
	handlers      map[string][]handlerEntry
	nextHandlerID uint64
	subscriptions map[string]struct{}
	authenticated bool
	userClosed    bool
	done          chan struct{}
	runErr        error
	closeOnce     sync.Once
	wg            sync.WaitGroup

	// messages is closed on shutdown so callers can range over it, which means a send
	// racing that close would panic. sendMu serialises the two: dispatch holds it for
	// reading, finish takes it for writing before closing. It cannot be mu, because
	// finish is reached from the reader goroutine while mu is free but a dispatch may
	// still be in flight.
	sendMu         sync.RWMutex
	messages       chan LiveMessage
	messagesClosed bool
}

type handlerEntry struct {
	id uint64
	fn func(LiveMessage)
}

// LiveEventOpen and LiveEventClose are emitted by the SDK rather than the server, so a
// caller can react to the transport itself. They are delivered to On handlers and to
// Messages like any other event.
const (
	LiveEventOpen  = "open"
	LiveEventClose = "close"
	// LiveEventAny receives every message, whatever its type.
	LiveEventAny = "*"
)

// LiveOption configures a LiveClient.
type LiveOption func(*LiveClient)

// WithLiveURL overrides the derived WebSocket endpoint.
func WithLiveURL(rawURL string) LiveOption {
	return func(l *LiveClient) { l.url = rawURL }
}

// WithLiveAutoReconnect enables or disables reconnection. On by default.
func WithLiveAutoReconnect(enabled bool) LiveOption {
	return func(l *LiveClient) { l.autoReconnect = enabled }
}

// WithLiveMaxReconnectAttempts caps consecutive reconnects. Zero or less means unlimited,
// which is the default.
func WithLiveMaxReconnectAttempts(n int) LiveOption {
	return func(l *LiveClient) { l.maxAttempts = n }
}

// WithLivePingInterval sets the keepalive period. Defaults to 30s.
func WithLivePingInterval(d time.Duration) LiveOption {
	return func(l *LiveClient) { l.pingInterval = d }
}

// WithLiveAuthTimeout bounds the wait for auth_success. Defaults to 10s.
func WithLiveAuthTimeout(d time.Duration) LiveOption {
	return func(l *LiveClient) { l.authTimeout = d }
}

// WithLiveReadTimeout sets how long a silent connection is tolerated before it is treated
// as dead. Defaults to three ping intervals.
func WithLiveReadTimeout(d time.Duration) LiveOption {
	return func(l *LiveClient) { l.readTimeout = d }
}

// WithLiveQueueSize sets the Messages buffer. Defaults to 1000. When the buffer is full
// the oldest message is dropped, so a slow consumer degrades instead of stalling the feed.
func WithLiveQueueSize(n int) LiveOption {
	return func(l *LiveClient) { l.queueSize = n }
}

// WithLiveTLSConfig overrides the TLS settings used for wss connections.
func WithLiveTLSConfig(cfg *tls.Config) LiveOption {
	return func(l *LiveClient) { l.tlsConfig = cfg }
}

// WithLiveConnectToken authenticates with a token from MintConnectToken instead of the
// client's API key. Single-use, and consumed on first connect, so it cannot be replayed by
// the reconnect loop.
func WithLiveConnectToken(token string) LiveOption {
	return func(l *LiveClient) { l.connectToken = token }
}

// Live creates a live feed client. Nothing connects until Connect is called.
func (c *Client) Live(opts ...LiveOption) *LiveClient {
	l := &LiveClient{
		client:        c,
		url:           c.WebSocketURL(),
		autoReconnect: true,
		pingInterval:  30 * time.Second,
		authTimeout:   10 * time.Second,
		dialTimeout:   30 * time.Second,
		queueSize:     1000,
		handlers:      map[string][]handlerEntry{},
		subscriptions: map[string]struct{}{},
		done:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(l)
	}
	if l.readTimeout <= 0 {
		l.readTimeout = 3 * l.pingInterval
	}
	if l.queueSize < 1 {
		l.queueSize = 1
	}
	if l.reconnectDelay == nil {
		l.reconnectDelay = defaultReconnectDelay
	}
	l.messages = make(chan LiveMessage, l.queueSize)
	return l
}

// defaultReconnectDelay is exponential backoff from 1s, capped at 30s. Attempt is 1-based.
func defaultReconnectDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
	if backoff > 30*time.Second || backoff <= 0 {
		backoff = 30 * time.Second
	}
	return backoff
}

// On registers a handler and returns a function that removes it.
//
// Event is a server message type such as LiveMatchUpdate, one of LiveEventOpen or
// LiveEventClose, or LiveEventAny for everything.
func (l *LiveClient) On(event string, handler func(LiveMessage)) func() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.nextHandlerID++
	id := l.nextHandlerID
	l.handlers[event] = append(l.handlers[event], handlerEntry{id: id, fn: handler})

	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		entries := l.handlers[event]
		for i, entry := range entries {
			if entry.id == id {
				l.handlers[event] = append(entries[:i:i], entries[i+1:]...)
				return
			}
		}
	}
}

// Messages returns the message stream. It is closed when the client stops for good, so it
// is safe to range over.
func (l *LiveClient) Messages() <-chan LiveMessage { return l.messages }

// Connected reports whether the socket is up and authenticated.
func (l *LiveClient) Connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil && l.authenticated
}

// Subscriptions lists the matches this connection is subscribed to, sorted. They are
// replayed automatically after a reconnect.
func (l *LiveClient) Subscriptions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.subscriptions))
	for id := range l.subscriptions {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Connect dials, authenticates and starts the reader.
//
// It returns once the server has accepted the auth frame, so a caller can Subscribe
// immediately afterwards. The ctx bounds the connect only; use Close or Run's ctx to stop
// the feed later.
func (l *LiveClient) Connect(ctx context.Context) error {
	l.mu.Lock()
	if l.userClosed {
		l.mu.Unlock()
		return errors.New("goalapi: live client is closed")
	}
	if l.conn != nil {
		l.mu.Unlock()
		return errors.New("goalapi: live client is already connected")
	}
	l.mu.Unlock()

	if err := l.dialAndAuth(ctx); err != nil {
		return err
	}

	l.wg.Add(1)
	go l.readLoop()

	l.wg.Add(1)
	go l.pingLoop()

	return nil
}

// dialAndAuth opens one connection and completes the auth handshake.
//
// The server requires the auth frame to be the very first message and closes with 4001
// otherwise, so this reads inline until auth_success rather than starting the reader
// first.
func (l *LiveClient) dialAndAuth(ctx context.Context) error {
	header := l.client.WebSocketHeader()
	if l.connectToken != "" {
		// A token connection is browser-shaped: no Authorization header, the token goes in
		// the auth frame.
		header.Del("Authorization")
	}

	conn, _, err := wsDial(ctx, l.url, header, l.tlsConfig, l.dialTimeout)
	if err != nil {
		return err
	}

	auth := l.client.AuthMessage()
	if l.connectToken != "" {
		auth = AuthMessageWithToken(l.connectToken)
	}
	payload, err := json.Marshal(auth)
	if err != nil {
		conn.close()
		return fmt.Errorf("goalapi: could not encode the auth frame: %w", err)
	}
	if err := conn.writeText(payload); err != nil {
		conn.close()
		return fmt.Errorf("goalapi: could not send the auth frame: %w", err)
	}

	deadline := time.Now().Add(l.authTimeout)
	if err := conn.setReadDeadline(deadline); err != nil {
		conn.close()
		return err
	}

	for {
		opcode, data, err := conn.readMessage()
		if err != nil {
			conn.close()
			var closeErr *wsCloseError
			if errors.As(err, &closeErr) && closeErr.Code == 4001 {
				return &Error{
					StatusCode: http.StatusUnauthorized,
					Message:    "live authentication rejected: " + closeErr.Reason,
					Category:   "authentication",
					wrapped:    ErrAuthentication,
				}
			}
			return fmt.Errorf("goalapi: live authentication failed: %w", err)
		}
		if opcode != opText {
			continue
		}

		var msg LiveMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case LiveAuthSuccess:
			if err := conn.setReadDeadline(time.Now().Add(l.readTimeout)); err != nil {
				conn.close()
				return err
			}
			l.mu.Lock()
			l.conn = conn
			l.authenticated = true
			l.mu.Unlock()

			l.dispatch(LiveMessage{Type: LiveEventOpen, Timestamp: time.Now().UnixMilli()})
			l.dispatch(msg)
			return nil

		case LiveError:
			conn.close()
			return &Error{
				StatusCode: http.StatusUnauthorized,
				Message:    "live authentication rejected: " + msg.Message,
				Code:       msg.Code,
				Category:   "authentication",
				wrapped:    ErrAuthentication,
			}
		}
		// Anything else before auth_success is noise; keep reading until the deadline.
	}
}

// readLoop pumps messages until the connection dies, then reconnects if configured.
func (l *LiveClient) readLoop() {
	defer l.wg.Done()

	for {
		l.mu.Lock()
		conn := l.conn
		l.mu.Unlock()

		if conn == nil {
			return
		}

		opcode, data, err := conn.readMessage()
		if err != nil {
			if l.handleDisconnect(err) {
				continue
			}
			return
		}
		if opcode != opText {
			continue
		}

		// A frame arrived, so the peer is alive; push the silence deadline out.
		_ = conn.setReadDeadline(time.Now().Add(l.readTimeout))

		var msg LiveMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		l.dispatch(msg)
	}
}

// handleDisconnect tears down the dead connection and decides whether to reconnect. It
// reports whether the read loop should carry on with a fresh connection.
func (l *LiveClient) handleDisconnect(cause error) bool {
	l.mu.Lock()
	conn := l.conn
	l.conn = nil
	l.authenticated = false
	userClosed := l.userClosed
	auto := l.autoReconnect
	maxAttempts := l.maxAttempts
	l.mu.Unlock()

	if conn != nil {
		conn.close()
	}

	l.dispatch(LiveMessage{
		Type:      LiveEventClose,
		Message:   causeMessage(cause),
		Timestamp: time.Now().UnixMilli(),
	})

	if userClosed || !auto {
		l.finish(cause)
		return false
	}

	for attempt := 1; maxAttempts <= 0 || attempt <= maxAttempts; attempt++ {
		select {
		case <-l.done:
			return false
		case <-time.After(l.reconnectDelay(attempt)):
		}

		l.mu.Lock()
		stopped := l.userClosed
		l.mu.Unlock()
		if stopped {
			return false
		}

		ctx, cancel := context.WithTimeout(context.Background(), l.dialTimeout+l.authTimeout)
		err := l.dialAndAuth(ctx)
		cancel()
		if err != nil {
			continue
		}

		l.replaySubscriptions()
		return true
	}

	l.finish(fmt.Errorf("goalapi: live reconnect gave up after %d attempts: %w", maxAttempts, cause))
	return false
}

// causeMessage renders a disconnect reason for the close event.
func causeMessage(err error) string {
	if err == nil {
		return ""
	}
	var closeErr *wsCloseError
	if errors.As(err, &closeErr) {
		return closeErr.Error()
	}
	return err.Error()
}

// replaySubscriptions re-subscribes after a reconnect. The server does not remember a
// dropped connection's subscriptions, so without this a reconnect would silently deliver
// nothing.
func (l *LiveClient) replaySubscriptions() {
	for _, id := range l.Subscriptions() {
		_ = l.send(SubscribeMessage(id))
	}
}

// pingLoop keeps the connection warm with the feed's application-level ping.
func (l *LiveClient) pingLoop() {
	defer l.wg.Done()

	ticker := time.NewTicker(l.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			if l.Connected() {
				_ = l.send(PingMessage())
			}
		}
	}
}

// dispatch delivers a message to handlers and to the Messages channel.
//
// Handlers are copied under the lock and called outside it, so a handler may call On or
// Subscribe without deadlocking.
func (l *LiveClient) dispatch(msg LiveMessage) {
	l.mu.Lock()
	specific := append([]handlerEntry(nil), l.handlers[msg.Type]...)
	wildcard := append([]handlerEntry(nil), l.handlers[LiveEventAny]...)
	l.mu.Unlock()

	for _, entry := range specific {
		entry.fn(msg)
	}
	for _, entry := range wildcard {
		entry.fn(msg)
	}

	l.sendMu.RLock()
	defer l.sendMu.RUnlock()
	if l.messagesClosed {
		return
	}

	select {
	case l.messages <- msg:
	default:
		// The consumer is behind. Drop the oldest rather than block the reader, so a slow
		// range over Messages costs history instead of the connection.
		select {
		case <-l.messages:
		default:
		}
		select {
		case l.messages <- msg:
		default:
		}
	}
}

// send marshals and writes a client frame.
func (l *LiveClient) send(message map[string]any) error {
	l.mu.Lock()
	conn := l.conn
	l.mu.Unlock()

	if conn == nil {
		return errors.New("goalapi: live client is not connected")
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return conn.writeText(payload)
}

// Subscribe asks for updates on a match.
//
// The id is recorded before the frame goes out, so it survives a reconnect even if the
// write fails. The server caps concurrent subscriptions by plan and client messages at
// 60/minute.
func (l *LiveClient) Subscribe(matchID string) error {
	l.mu.Lock()
	l.subscriptions[matchID] = struct{}{}
	l.mu.Unlock()
	return l.send(SubscribeMessage(matchID))
}

// Unsubscribe stops updates for a match.
func (l *LiveClient) Unsubscribe(matchID string) error {
	l.mu.Lock()
	delete(l.subscriptions, matchID)
	l.mu.Unlock()
	return l.send(UnsubscribeMessage(matchID))
}

// ListSubscriptions asks the server what this connection is subscribed to. The answer
// arrives as a LiveGetSubscriptionsResponse message.
func (l *LiveClient) ListSubscriptions() error { return l.send(ListSubscriptionsMessage()) }

// RequestStatus asks for the connection's plan, subscriptions and feature flags. The
// answer arrives as a LiveStatus message.
func (l *LiveClient) RequestStatus() error { return l.send(StatusMessage()) }

// Ping sends a keepalive. The server answers with LivePong. The ping loop already does
// this on a timer.
func (l *LiveClient) Ping() error { return l.send(PingMessage()) }

// Run blocks until the client stops, ctx is cancelled, or reconnection gives up.
//
// It returns nil for a clean shutdown, ctx.Err() on cancellation, and the underlying
// failure otherwise.
func (l *LiveClient) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		_ = l.Close()
		return ctx.Err()
	case <-l.done:
		l.mu.Lock()
		err := l.runErr
		l.mu.Unlock()
		return err
	}
}

// finish records why the client stopped and releases everyone waiting on it.
func (l *LiveClient) finish(err error) {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		if l.runErr == nil && !l.userClosed {
			l.runErr = err
		}
		l.mu.Unlock()
		close(l.done)

		l.sendMu.Lock()
		l.messagesClosed = true
		close(l.messages)
		l.sendMu.Unlock()
	})
}

// Close shuts the connection down and stops reconnecting. Safe to call more than once.
func (l *LiveClient) Close() error {
	l.mu.Lock()
	if l.userClosed {
		l.mu.Unlock()
		return nil
	}
	l.userClosed = true
	conn := l.conn
	l.conn = nil
	l.authenticated = false
	l.mu.Unlock()

	var err error
	if conn != nil {
		err = conn.gracefulClose(closeGoingAway, "client closing")
	}

	l.finish(nil)
	return err
}
