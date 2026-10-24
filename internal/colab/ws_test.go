// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package colab

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wsServer upgrades every request and hands the raw connection to fn.
func wsServer(t *testing.T, fn func(r *http.Request, conn net.Conn, c *wsConn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj := w.(http.Hijacker)
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			acceptKey(r.Header.Get("Sec-WebSocket-Key")))
		_ = brw.Flush()
		c := newWSConn(conn, brw.Reader, false)
		fn(r, conn, c)
		c.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/kernels/k/channels?token=x"
}

func TestWebSocketEchoAndSizes(t *testing.T) {
	srv := wsServer(t, func(r *http.Request, _ net.Conn, c *wsConn) {
		if r.Header.Get("X-Colab-Runtime-Proxy-Token") != "tok" || r.URL.Query().Get("token") != "x" {
			_ = c.WriteText(context.Background(), []byte("bad headers"))
			return
		}
		for {
			_, data, err := c.ReadMessage(context.Background())
			if err != nil {
				return
			}
			if err := c.WriteText(context.Background(), data); err != nil {
				return
			}
		}
	})
	ctx := context.Background()
	conn, err := DialWebSocket(nil)(ctx, wsURL(srv), http.Header{"X-Colab-Runtime-Proxy-Token": {"tok"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, n := range []int{0, 5, 125, 126, 65535, 65536, 300000} {
		msg := bytes.Repeat([]byte{'a' + byte(n%26)}, n)
		if err := conn.WriteText(ctx, msg); err != nil {
			t.Fatal(err)
		}
		_, got, err := conn.ReadMessage(ctx)
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("size %d: %v (got %d bytes)", n, err, len(got))
		}
	}
}

// rawFrame writes an unmasked server frame.
func rawFrame(fin bool, op byte, payload []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	out := []byte{b0}
	switch n := len(payload); {
	case n <= 125:
		out = append(out, byte(n))
	case n <= 0xffff:
		out = append(out, 126, byte(n>>8), byte(n))
	default:
		out = append(out, 127)
		out = binary.BigEndian.AppendUint64(out, uint64(n))
	}
	return append(out, payload...)
}

func TestWebSocketFragmentsPingClose(t *testing.T) {
	pong := make(chan []byte, 1)
	srv := wsServer(t, func(_ *http.Request, conn net.Conn, c *wsConn) {
		// a fragmented text message with a ping in the middle, then a
		// binary message, then close
		var w bytes.Buffer
		w.Write(rawFrame(false, opText, []byte("Hel")))
		w.Write(rawFrame(true, opPing, []byte("p1")))
		w.Write(rawFrame(false, opCont, []byte("lo, ")))
		w.Write(rawFrame(true, opCont, []byte("world")))
		w.Write(rawFrame(true, opBinary, []byte{1, 2, 3}))
		_, _ = conn.Write(w.Bytes())
		// the client's pong arrives (control frames are not messages)
		time.Sleep(50 * time.Millisecond)
		_, _ = conn.Write(rawFrame(true, opClose, []byte{0x03, 0xe8}))
		_, _, _ = c.ReadMessage(context.Background())
	})
	_ = pong
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialWebSocket(nil)(ctx, wsURL(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	bin, got, err := conn.ReadMessage(ctx)
	if err != nil || bin || string(got) != "Hello, world" {
		t.Fatalf("fragmented: %q %v", got, err)
	}
	bin, got, err = conn.ReadMessage(ctx)
	if err != nil || !bin || !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("binary: %v %v", got, err)
	}
	if _, _, err := conn.ReadMessage(ctx); err == nil {
		t.Fatal("no error after close")
	}
}

func TestWebSocketReadHonoursContext(t *testing.T) {
	srv := wsServer(t, func(_ *http.Request, _ net.Conn, c *wsConn) {
		_, _, _ = c.ReadMessage(context.Background()) // until the client goes
	})
	conn, err := DialWebSocket(nil)(context.Background(), wsURL(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := conn.ReadMessage(ctx); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("read did not stop with the context: %v", err)
	}
}

func TestWebSocketRejectsBadHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/forbidden/api/kernels/k/channels" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Sec-WebSocket-Accept", "wrong")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	if _, err := DialWebSocket(nil)(context.Background(), base+"/forbidden/api/kernels/k/channels", nil); statusOf(err) != 403 {
		t.Errorf("403: %v", err)
	}
	if _, err := DialWebSocket(nil)(context.Background(), base+"/api/kernels/k/channels", nil); err == nil {
		t.Error("a wrong Sec-WebSocket-Accept was accepted")
	}
	if _, err := DialWebSocket(nil)(context.Background(), "ftp://x/y", nil); err == nil {
		t.Error("scheme ftp accepted")
	}
}

func TestDecodeKernelMsgBinary(t *testing.T) {
	js := []byte(`{"header":{"msg_type":"stream","msg_id":"m"},"parent_header":{"msg_id":"p"},"metadata":{},"content":{"name":"stdout","text":"x"},"channel":"iopub"}`)
	buf := []byte{0, 0, 0, 2}
	off := 4 * 3
	buf = binary.BigEndian.AppendUint32(buf, uint32(off))
	buf = binary.BigEndian.AppendUint32(buf, uint32(off+len(js)))
	buf = append(buf, js...)
	buf = append(buf, 9, 9, 9) // a buffer
	m, err := decodeKernelMsg(true, buf)
	if err != nil || m.msgType() != "stream" || m.parentID() != "p" || m.Channel != "iopub" {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := decodeKernelMsg(true, []byte{0, 0, 0, 9, 0}); err == nil {
		t.Error("short binary message accepted")
	}
}
