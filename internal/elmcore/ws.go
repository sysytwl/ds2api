package elmcore

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
	"net/url"
	"strings"
	"sync"
	"time"
)

const websocketAcceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	wsOpContinuation byte = 0x0
	wsOpText         byte = 0x1
	wsOpBinary       byte = 0x2
	wsOpClose        byte = 0x8
	wsOpPing         byte = 0x9
	wsOpPong         byte = 0xA
)

const (
	wsHandshakeTimeout = 20 * time.Second
	maxWebSocketFrame  = 16 << 20
)

var (
	// ErrWebSocketClosed is returned when the peer closed the connection.
	ErrWebSocketClosed = errors.New("elm web: websocket closed")
)

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + websocketAcceptGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// dialWebSocket opens a RFC 6455 client connection. The ELM web chat speaks
// STOMP over this socket, so only the small subset the broker needs is
// implemented: the handshake, text frames, fragmentation, ping/pong and close.
func dialWebSocket(ctx context.Context, rawURL string, headers map[string]string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("elm web: parse websocket url: %w", err)
	}
	switch u.Scheme {
	case "wss", "ws":
	default:
		return nil, fmt.Errorf("elm web: unsupported websocket scheme %q", u.Scheme)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	dialer := &net.Dialer{Timeout: wsHandshakeTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("elm web: dial websocket: %w", err)
	}
	conn := net.Conn(raw)
	if u.Scheme == "wss" {
		tlsConn := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("elm web: websocket tls handshake: %w", err)
		}
		conn = tlsConn
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("elm web: websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", u.RequestURI())
	fmt.Fprintf(&b, "Host: %s\r\n", u.Host)
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", key)
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	_ = conn.SetDeadline(time.Now().Add(wsHandshakeTimeout))
	if _, err := io.WriteString(conn, b.String()); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("elm web: websocket handshake write: %w", err)
	}
	br := bufio.NewReaderSize(conn, 64*1024)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("elm web: websocket handshake read: %w", err)
	}
	if !strings.Contains(statusLine, " 101 ") && !strings.HasSuffix(strings.TrimSpace(statusLine), " 101") {
		_ = conn.Close()
		return nil, fmt.Errorf("elm web: websocket upgrade refused: %s", strings.TrimSpace(statusLine))
	}
	accept := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("elm web: websocket handshake headers: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "Sec-WebSocket-Accept") {
			accept = strings.TrimSpace(value)
		}
	}
	if accept != websocketAccept(key) {
		_ = conn.Close()
		return nil, errors.New("elm web: websocket accept mismatch")
	}
	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br}, nil
}

func (c *wsConn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *wsConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *wsConn) WriteMessage(payload string) error {
	return c.writeFrame(wsOpText, []byte(payload))
}

func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n := len(payload)
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126, byte(n>>8), byte(n))
	default:
		header = append(header, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header = append(header, mask...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := c.conn.Write(append(header, masked...))
	return err
}

// ReadMessage returns the next complete text/binary message, transparently
// handling fragmentation and ping/close control frames.
func (c *wsConn) ReadMessage() (string, error) {
	var payload []byte
	for {
		fin, opcode, data, err := c.readFrame()
		if err != nil {
			return "", err
		}
		switch opcode {
		case wsOpPing:
			if err := c.writeFrame(wsOpPong, data); err != nil {
				return "", err
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			_ = c.writeFrame(wsOpClose, data)
			return "", ErrWebSocketClosed
		case wsOpText, wsOpBinary:
			payload = append(payload[:0], data...)
		case wsOpContinuation:
			payload = append(payload, data...)
		default:
			return "", fmt.Errorf("elm web: unsupported websocket opcode %d", opcode)
		}
		if fin {
			return string(payload), nil
		}
	}
}

func (c *wsConn) readFrame() (bool, byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin := head[0]&0x80 != 0
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length < 0 || length > maxWebSocketFrame {
		return false, 0, nil, fmt.Errorf("elm web: websocket frame too large: %d", length)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(c.br, data); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}
	return fin, opcode, data, nil
}
