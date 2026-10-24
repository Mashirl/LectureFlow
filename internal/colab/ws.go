// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

// A minimal RFC 6455 WebSocket client (text and binary messages,
// fragmentation, ping/pong, close), enough for the Jupyter kernel channels
// (upstream uses the websocket-client package through
// jupyter-kernel-client 0.9.0). Written here instead of adding a module
// dependency; the handshake goes through net/http, so HTTP(S)_PROXY is
// honoured like for every other request.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MessageConn is a message-oriented connection (the kernel channels).
type MessageConn interface {
	// ReadMessage returns the next data message (text or binary).
	ReadMessage(ctx context.Context) (binary bool, data []byte, err error)
	// WriteText sends one text message.
	WriteText(ctx context.Context, data []byte) error
	Close() error
}

// Dialer opens a MessageConn to a ws:// or wss:// URL with extra headers.
type Dialer func(ctx context.Context, rawURL string, header http.Header) (MessageConn, error)

const (
	wsGUID          = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxMessage    = 16 << 20
	wsPingInterval  = 30 * time.Second
	wsHandshakeTime = 30 * time.Second

	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xa
)

var errWSClosed = errors.New("websocket closed")

// wsHTTP1Transport is the transport of the handshake: HTTP/1.1 only (an
// upgrade cannot go over HTTP/2), proxies from the environment.
func wsHTTP1Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	p := new(http.Protocols)
	p.SetHTTP1(true)
	t.Protocols = p
	return t
}

// DialWebSocket is the default Dialer. tr nil = wsHTTP1Transport().
func DialWebSocket(tr http.RoundTripper) Dialer {
	if tr == nil {
		tr = wsHTTP1Transport()
	}
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context, rawURL string, header http.Header) (MessageConn, error) {
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, err
		}
		switch u.Scheme {
		case "wss":
			u.Scheme = "https"
		case "ws":
			u.Scheme = "http"
		default:
			return nil, fmt.Errorf("websocket: unsupported scheme %q", u.Scheme)
		}
		var kb [16]byte
		_, _ = rand.Read(kb[:])
		key := base64.StdEncoding.EncodeToString(kb[:])
		// The connection outlives the caller's handshake context: it is
		// cancelled only when the handshake takes too long or the
		// connection is closed.
		hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		timer := time.AfterFunc(wsHandshakeTime, cancel)
		stop := context.AfterFunc(ctx, cancel)
		req, err := http.NewRequestWithContext(hctx, http.MethodGet, u.String(), nil)
		if err != nil {
			cancel()
			return nil, err
		}
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Sec-WebSocket-Key", key)
		req.Header.Set("Sec-WebSocket-Version", "13")
		resp, err := hc.Do(req)
		timer.Stop()
		stop()
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &netError{text: "websocket handshake: " + netErrText(err)}
		}
		if resp.StatusCode != http.StatusSwitchingProtocols {
			resp.Body.Close()
			cancel()
			return nil, &httpError{endpoint: "api/kernels/channels", status: resp.StatusCode}
		}
		rwc, ok := resp.Body.(io.ReadWriteCloser)
		if !ok || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
			resp.Body.Close()
			cancel()
			return nil, errors.New("websocket handshake: invalid upgrade answer")
		}
		c := newWSConn(rwc, bufio.NewReaderSize(rwc, 64<<10), true)
		c.onClose = cancel
		c.startPings(wsPingInterval)
		return c, nil
	}
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

type wsMessage struct {
	binary bool
	data   []byte
}

// wsConn is one WebSocket connection; client = this side masks its frames.
type wsConn struct {
	rwc    io.ReadWriteCloser
	br     *bufio.Reader
	client bool

	wmu sync.Mutex // frame writes

	msgs    chan wsMessage
	done    chan struct{}
	errOnce sync.Once
	err     error

	closeOnce sync.Once
	onClose   func()
}

func newWSConn(rwc io.ReadWriteCloser, br *bufio.Reader, client bool) *wsConn {
	c := &wsConn{rwc: rwc, br: br, client: client, msgs: make(chan wsMessage, 64), done: make(chan struct{})}
	go c.readLoop()
	return c
}

func (c *wsConn) fail(err error) {
	c.errOnce.Do(func() {
		c.err = err
		close(c.done)
	})
}

func (c *wsConn) readLoop() {
	var (
		buf    []byte
		binary bool
		inMsg  bool
	)
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			c.fail(err)
			return
		}
		switch op {
		case opPing:
			_ = c.writeFrame(opPong, payload)
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeFrame(opClose, payload)
			c.fail(errWSClosed)
			return
		case opText, opBinary:
			if inMsg {
				c.fail(errors.New("websocket: new message inside a fragmented one"))
				return
			}
			buf, binary, inMsg = append(buf[:0:0], payload...), op == opBinary, true
		case opCont:
			if !inMsg {
				c.fail(errors.New("websocket: continuation without a message"))
				return
			}
			buf = append(buf, payload...)
		default:
			c.fail(fmt.Errorf("websocket: unknown opcode %d", op))
			return
		}
		if len(buf) > wsMaxMessage {
			c.fail(errors.New("websocket: message too large"))
			return
		}
		if fin { // the data frame that ends the message (control frames continue above)
			inMsg = false
			select {
			case c.msgs <- wsMessage{binary: binary, data: buf}:
			case <-c.done:
				return
			}
			buf = nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0f
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: reserved bits set (no extension was negotiated)")
	}
	masked := h[1]&0x80 != 0
	if masked == c.client {
		return false, 0, nil, errors.New("websocket: wrong masking")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= opClose && (n > 125 || !fin) {
		return false, 0, nil, errors.New("websocket: invalid control frame")
	}
	if n > wsMaxMessage {
		return false, 0, nil, errors.New("websocket: frame too large")
	}
	var key [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, key[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|op)
	mbit := byte(0)
	if c.client {
		mbit = 0x80
	}
	switch n := len(payload); {
	case n <= 125:
		hdr = append(hdr, mbit|byte(n))
	case n <= 0xffff:
		hdr = append(hdr, mbit|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, mbit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	out := payload
	if c.client {
		var key [4]byte
		_, _ = rand.Read(key[:])
		hdr = append(hdr, key[:]...)
		out = make([]byte, len(payload))
		for i := range payload {
			out[i] = payload[i] ^ key[i%4]
		}
	}
	if _, err := c.rwc.Write(append(hdr, out...)); err != nil {
		return err
	}
	return nil
}

func (c *wsConn) startPings(every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				if err := c.writeFrame(opPing, []byte("lf")); err != nil {
					c.fail(err)
					return
				}
			}
		}
	}()
}

func (c *wsConn) ReadMessage(ctx context.Context) (bool, []byte, error) {
	select {
	case m := <-c.msgs:
		return m.binary, m.data, nil
	default:
	}
	select {
	case m := <-c.msgs:
		return m.binary, m.data, nil
	case <-c.done:
		select { // messages that arrived before the failure come first
		case m := <-c.msgs:
			return m.binary, m.data, nil
		default:
		}
		return false, nil, c.err
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
}

func (c *wsConn) WriteText(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := c.writeFrame(opText, data); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func (c *wsConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.writeFrame(opClose, []byte{0x03, 0xe8}) // 1000 normal closure
		c.fail(errWSClosed)
		_ = c.rwc.Close()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return nil
}
