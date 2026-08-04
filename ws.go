package goalapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A minimal RFC 6455 client, enough for the GOAL live feed and nothing more.
//
// The standard library has no WebSocket, and this package promises no dependencies, so the
// wire format lives here: the HTTP upgrade, frame masking, fragmentation reassembly and
// the close handshake. It is deliberately client-only. There is no server, no extension
// negotiation and no permessage-deflate, because the feed uses none of them.
//
// Everything below is unexported. LiveClient is the supported surface.

// magicGUID is appended to Sec-WebSocket-Key before hashing, per RFC 6455 section 1.3.
const magicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Frame opcodes, RFC 6455 section 5.2.
const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// Close codes we send. The server's own 4001 (auth frame not first) arrives as a close
// code on the read side and is surfaced verbatim.
const (
	closeNormal        = 1000
	closeGoingAway     = 1001
	closeProtocolError = 1002
	closeTooLarge      = 1009
)

// maxControlPayload is fixed by RFC 6455 section 5.5: control frames carry at most 125
// bytes and are never fragmented.
const maxControlPayload = 125

// defaultMaxPayload caps a reassembled message. Match updates are a few KB; anything past
// this is a bug or a hostile peer, and without a cap a single frame header claiming 2^63
// bytes would drive an unbounded allocation.
const defaultMaxPayload int64 = 8 << 20

// wsCloseError reports that the peer closed the connection with a code and reason.
type wsCloseError struct {
	Code   int
	Reason string
}

func (e *wsCloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("goalapi: websocket closed with code %d", e.Code)
	}
	return fmt.Sprintf("goalapi: websocket closed with code %d: %s", e.Code, e.Reason)
}

// wsConn is a client connection. Writes are serialised by wmu; a single reader is assumed,
// which is how liveClient drives it.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}

	maxPayload int64
}

// wsDial performs the HTTP upgrade and returns a live connection.
//
// The handshake response is read off the same bufio.Reader that then reads frames, because
// the server is allowed to put the first frames in the same TCP segment as the 101 and a
// second reader would lose whatever the first one buffered.
func wsDial(ctx context.Context, rawURL string, header http.Header, tlsConfig *tls.Config, timeout time.Duration) (*wsConn, *http.Response, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("goalapi: invalid websocket url %q: %w", rawURL, err)
	}

	var useTLS bool
	switch parsed.Scheme {
	case "wss", "https":
		useTLS = true
	case "ws", "http":
		useTLS = false
	default:
		return nil, nil, fmt.Errorf("goalapi: unsupported websocket scheme %q", parsed.Scheme)
	}

	host := parsed.Host
	if parsed.Port() == "" {
		if useTLS {
			host = net.JoinHostPort(parsed.Hostname(), "443")
		} else {
			host = net.JoinHostPort(parsed.Hostname(), "80")
		}
	}

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, nil, &Error{
			StatusCode: 0,
			Message:    "websocket dial failed: " + err.Error(),
			Category:   "network",
			Network:    true,
			wrapped:    ErrNetwork,
		}
	}

	// From here on any early return must not leak the socket.
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	if useTLS {
		cfg := tlsConfig
		if cfg == nil {
			cfg = &tls.Config{}
		} else {
			cfg = cfg.Clone()
		}
		if cfg.ServerName == "" {
			cfg.ServerName = parsed.Hostname()
		}
		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, nil, &Error{
				StatusCode: 0,
				Message:    "websocket TLS handshake failed: " + err.Error(),
				Category:   "network",
				Network:    true,
				wrapped:    ErrNetwork,
			}
		}
		conn = tlsConn
	}

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("goalapi: could not generate websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce)

	requestURI := parsed.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}

	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", requestURI)
	fmt.Fprintf(&req, "Host: %s\r\n", parsed.Host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&req, "Sec-WebSocket-Key: %s\r\n", key)
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	for name, values := range header {
		// The handshake headers above are ours to set; a caller override would break it.
		switch http.CanonicalHeaderKey(name) {
		case "Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version", "Host":
			continue
		}
		for _, value := range values {
			fmt.Fprintf(&req, "%s: %s\r\n", name, value)
		}
	}
	req.WriteString("\r\n")

	if _, err := io.WriteString(conn, req.String()); err != nil {
		return nil, nil, fmt.Errorf("goalapi: websocket handshake write failed: %w", err)
	}

	br := bufio.NewReader(conn)
	httpReq, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	resp, err := http.ReadResponse(br, httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("goalapi: websocket handshake read failed: %w", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		// A non-101 carries a normal API error body, so report it the way REST failures are
		// reported rather than as a bare protocol complaint.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, resp, errorFromResponse(resp.StatusCode, body, resp.Header)
	}

	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!headerContainsToken(resp.Header.Get("Connection"), "upgrade") {
		return nil, resp, errors.New("goalapi: server did not upgrade the connection")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		return nil, resp, errors.New("goalapi: websocket accept key mismatch")
	}

	// The handshake deadline must not become a deadline on the feed itself.
	_ = conn.SetDeadline(time.Time{})

	success = true
	return &wsConn{
		conn:       conn,
		br:         br,
		closed:     make(chan struct{}),
		maxPayload: defaultMaxPayload,
	}, resp, nil
}

// acceptKey computes the Sec-WebSocket-Accept value the server must echo.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + magicGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// headerContainsToken reports whether a comma-separated header lists a token,
// case-insensitively. "Connection: keep-alive, Upgrade" is legal.
func headerContainsToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// writeFrame writes one masked frame. Clients must mask every frame they send
// (RFC 6455 section 5.3); an unmasked client frame is a protocol error the server will
// close on.
func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()

	select {
	case <-w.closed:
		return net.ErrClosed
	default:
	}

	var header [14]byte
	header[0] = 0x80 | opcode // FIN set: this SDK never fragments what it sends.

	length := len(payload)
	var headerLen int
	switch {
	case length < 126:
		header[1] = 0x80 | byte(length)
		headerLen = 2
	case length <= 0xFFFF:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:4], uint16(length))
		headerLen = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:10], uint64(length))
		headerLen = 10
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("goalapi: could not generate websocket mask: %w", err)
	}
	copy(header[headerLen:headerLen+4], mask[:])
	headerLen += 4

	masked := make([]byte, length)
	for i := 0; i < length; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}

	if _, err := w.conn.Write(header[:headerLen]); err != nil {
		return err
	}
	if length > 0 {
		if _, err := w.conn.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

// writeText sends a text message.
func (w *wsConn) writeText(payload []byte) error { return w.writeFrame(opText, payload) }

// writePing sends a ping. The peer is expected to answer with a pong carrying the same
// payload, though the GOAL feed's own application-level ping is a JSON text frame, not
// this.
func (w *wsConn) writePing(payload []byte) error {
	if len(payload) > maxControlPayload {
		payload = payload[:maxControlPayload]
	}
	return w.writeFrame(opPing, payload)
}

// writeClose sends a close frame. Errors are advisory: the connection is going away and a
// failed write here changes nothing.
func (w *wsConn) writeClose(code int, reason string) error {
	if len(reason) > maxControlPayload-2 {
		reason = reason[:maxControlPayload-2]
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], uint16(code))
	copy(payload[2:], reason)
	return w.writeFrame(opClose, payload)
}

// readMessage returns the next complete application message, reassembling fragments and
// answering control frames as they arrive.
//
// Control frames may be interleaved between the fragments of a message, so ping and close
// are handled inline rather than after the loop.
func (w *wsConn) readMessage() (byte, []byte, error) {
	var (
		fragments   []byte
		messageType byte
		assembling  bool
	)

	for {
		fin, opcode, payload, err := w.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch opcode {
		case opPing:
			// Answer with the same payload, per RFC 6455 section 5.5.3.
			if err := w.writeFrame(opPong, payload); err != nil {
				return 0, nil, err
			}
			continue

		case opPong:
			continue

		case opClose:
			code := closeNormal
			reason := ""
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload[:2]))
				reason = string(payload[2:])
			}
			// Echo the close, then stop. Ignore the write error: the peer may already be
			// gone, and we are returning an error either way.
			_ = w.writeClose(closeNormal, "")
			w.close()
			return 0, nil, &wsCloseError{Code: code, Reason: reason}

		case opText, opBinary:
			if assembling {
				w.protocolClose("data frame during fragmented message")
				return 0, nil, errors.New("goalapi: websocket data frame interrupted a fragmented message")
			}
			if fin {
				return opcode, payload, nil
			}
			assembling = true
			messageType = opcode
			fragments = append(fragments, payload...)

		case opContinuation:
			if !assembling {
				w.protocolClose("continuation without a start frame")
				return 0, nil, errors.New("goalapi: websocket continuation frame without a start frame")
			}
			fragments = append(fragments, payload...)
			if int64(len(fragments)) > w.maxPayload {
				w.protocolClose("message too large")
				return 0, nil, errors.New("goalapi: websocket message exceeded the size limit")
			}
			if fin {
				return messageType, fragments, nil
			}

		default:
			w.protocolClose("unknown opcode")
			return 0, nil, fmt.Errorf("goalapi: unknown websocket opcode 0x%X", opcode)
		}
	}
}

// readFrame reads a single frame header and its payload.
func (w *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var header [2]byte
	if _, err := io.ReadFull(w.br, header[:]); err != nil {
		return false, 0, nil, err
	}

	fin = header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		// RSV1-3 must be zero: no extension was negotiated.
		w.protocolClose("reserved bits set")
		return false, 0, nil, errors.New("goalapi: websocket reserved bits set without an extension")
	}
	opcode = header[0] & 0x0F
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7F)

	isControl := opcode&0x8 != 0
	if isControl {
		if !fin {
			w.protocolClose("fragmented control frame")
			return false, 0, nil, errors.New("goalapi: websocket control frame was fragmented")
		}
		if length > maxControlPayload {
			w.protocolClose("oversized control frame")
			return false, 0, nil, errors.New("goalapi: websocket control frame exceeded 125 bytes")
		}
	}

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		value := binary.BigEndian.Uint64(ext[:])
		if value > 1<<62 {
			w.protocolClose("absurd frame length")
			return false, 0, nil, errors.New("goalapi: websocket frame length out of range")
		}
		length = int64(value)
	}

	// Check the advertised length before allocating, so a hostile header cannot make us
	// reserve gigabytes.
	if length > w.maxPayload {
		w.protocolClose("message too large")
		return false, 0, nil, errors.New("goalapi: websocket frame exceeded the size limit")
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}

	payload = make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return false, 0, nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}

	return fin, opcode, payload, nil
}

// protocolClose tells the peer why we are hanging up, then drops the socket. Best effort:
// the caller is already returning an error.
func (w *wsConn) protocolClose(reason string) {
	code := closeProtocolError
	if strings.Contains(reason, "too large") {
		code = closeTooLarge
	}
	_ = w.writeClose(code, reason)
	w.close()
}

// setReadDeadline bounds the next read, which is how the ping/pong liveness check detects
// a silent peer.
func (w *wsConn) setReadDeadline(t time.Time) error {
	return w.conn.SetReadDeadline(t)
}

// close drops the socket. Safe to call repeatedly and from several goroutines, which
// matters because both the reader and Close() reach it.
func (w *wsConn) close() {
	w.closeOnce.Do(func() {
		close(w.closed)
		_ = w.conn.Close()
	})
}

// gracefulClose sends a close frame and then drops the socket.
//
// It does not wait for the peer's echo. The reader goroutine is usually blocked in
// readMessage and will see the echo or an EOF; waiting here would just deadlock against
// it.
func (w *wsConn) gracefulClose(code int, reason string) error {
	err := w.writeClose(code, reason)
	w.close()
	return err
}
