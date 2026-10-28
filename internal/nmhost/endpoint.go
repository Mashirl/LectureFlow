// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package nmhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const isWindows = runtime.GOOS == "windows"

// EndpointOff disables the endpoint (-nm-endpoint off, or the variable).
const EndpointOff = "off"

// pipePrefix starts every named pipe path.
const pipePrefix = `\\.\pipe\`

// Errors of Listen and Dial.
var (
	// ErrInUse: another process already serves the endpoint (a second
	// LectureFlow, or a squatter). The app logs it, starts anyway and
	// reports the endpoint as taken (ListenInUse): the extension cannot
	// reach it.
	ErrInUse = errors.New("nmhost: the endpoint is in use by another process")
	// ErrNotListening: nothing serves the endpoint (LectureFlow is not
	// running).
	ErrNotListening = errors.New("nmhost: nothing is listening on the endpoint")
	// ErrForeign: something serves the endpoint, but not a trusted process
	// of the current user (another user's; on Windows also one below
	// medium integrity or in an AppContainer). The host never sends it a
	// message.
	ErrForeign = errors.New("nmhost: the endpoint is not served by a trusted process of the current user")
)

// ResolveEndpoint picks the endpoint: flagValue (the app's -nm-endpoint),
// else LECTUREFLOW_NM_ENDPOINT, else the per-user default. "" with a nil
// error means "off".
func ResolveEndpoint(flagValue string, getenv func(string) string) (string, error) {
	v := strings.TrimSpace(flagValue)
	if v == "" {
		v = strings.TrimSpace(getenv(EnvEndpoint))
	}
	if strings.EqualFold(v, EndpointOff) {
		return "", nil
	}
	if v != "" {
		if !IsPipe(v) {
			abs, err := filepath.Abs(v)
			if err != nil {
				return "", err
			}
			v = abs
		}
		return v, nil
	}
	return DefaultEndpoint()
}

// DefaultEndpoint is the per-user endpoint: on Windows the named pipe
// \\.\pipe\org.lectureflow.nm.<user SID>, elsewhere the Unix socket
// ~/Library/Application Support/LectureFlow/run/nm.sock (macOS;
// {UserConfigDir}/LectureFlow/run/nm.sock on other systems).
func DefaultEndpoint() (string, error) {
	if isWindows {
		sid, err := currentUserSID()
		if err != nil {
			return "", fmt.Errorf("nmhost: current user: %w", err)
		}
		return pipePrefix + "org.lectureflow.nm." + sid, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "LectureFlow", "run", "nm.sock"), nil
}

// IsPipe reports whether the endpoint is a Windows named pipe path.
func IsPipe(endpoint string) bool {
	return len(endpoint) > len(pipePrefix) && strings.EqualFold(endpoint[:len(pipePrefix)], pipePrefix)
}

// Listen serves the endpoint. A named pipe admits only the current user
// and refuses to be a second instance of an existing pipe (ErrInUse); a
// socket gets a 0700 directory and mode 0600, and a leftover socket file
// of an earlier run is removed (one that still answers is ErrInUse).
func Listen(endpoint string) (net.Listener, error) {
	if IsPipe(endpoint) {
		return listenPipe(endpoint)
	}
	return listenUnix(endpoint)
}

// Dial connects to the endpoint. ErrNotListening when nothing serves it;
// ErrForeign when what serves it is not a trusted process of the current
// user (checkPipeServer).
func Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	if IsPipe(endpoint) {
		return dialPipe(ctx, endpoint)
	}
	return dialUnix(ctx, endpoint)
}

// ---- Unix sockets (macOS; tests on every system) ----

func listenUnix(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if def, err := DefaultEndpoint(); err == nil && !isWindows && filepath.Dir(def) == dir {
		// Our own run directory: nobody else may reach the socket.
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("nmhost: %s exists and is not a socket", path)
		}
		c, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			c.Close()
			return nil, ErrInUse
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if !isWindows {
		if err := os.Chmod(path, 0o600); err != nil {
			ln.Close()
			return nil, err
		}
	}
	return ln, nil
}

func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotListening
		}
		return nil, err
	}
	if !ownedByCurrentUser(fi) {
		return nil, ErrForeign
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A leftover socket file of a run that ended.
		return nil, fmt.Errorf("%w: %v", ErrNotListening, err)
	}
	return c, nil
}
