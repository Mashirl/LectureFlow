// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build realnet

package fetch

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Security review H1 with the real aria2c: 127.0.0.1:P is taken before it
// starts (it then prints "IPv4 RPC: failed to bind" and listens on [::1]:P
// alone). It is started again on another port; the program holding P sees
// no connection. Nothing leaves this computer.
func TestRealAria2cTakenPort(t *testing.T) {
	aria := realBinary(t, "LF_REAL_ARIA2C", "aria2c")
	sq := newSquatter(t, 0)
	logs := &syncBuffer{}
	var mu sync.Mutex
	picks := 0
	eng := &engine{exe: aria, launch: execLauncher, idle: time.Minute, hc: &http.Client{Timeout: 5 * time.Second},
		log: slog.New(slog.NewTextHandler(logs, nil)),
		pickPort: func() (int, error) {
			mu.Lock()
			defer mu.Unlock()
			picks++
			if picks == 1 {
				return sq.port(), nil
			}
			return freePort()
		}}
	defer eng.close()
	in, err := eng.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v\n%s", err, logs)
	}
	defer eng.release()
	if v, err := in.rpc.version(context.Background()); err != nil || v == "" {
		t.Fatalf("version %q %v", v, err)
	}
	if rpcPort(t, in) == sq.port() || picks != 2 {
		t.Fatalf("port %d after %d picks", rpcPort(t, in), picks)
	}
	time.Sleep(200 * time.Millisecond)
	if conns, got := sq.seen(); conns != 0 || got != "" {
		t.Fatalf("the program on the taken port saw %d connections: %q", conns, got)
	}
	if !strings.Contains(logs.String(), "seen_by=\"aria2c output\"") && !strings.Contains(logs.String(), "seen_by=\"socket table\"") {
		t.Fatalf("log:\n%s", logs)
	}
	t.Logf("log:\n%s", logs)
}
