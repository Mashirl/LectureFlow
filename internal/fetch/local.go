// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"lectureflow/internal/blocklist"
)

// Local addresses (security review L1, blind SSRF). The URLs a download
// requests come from a web page (through the extension) and from the
// playlists and MPDs that page's servers return; a public page must not
// make LectureFlow request something on this computer or the user's
// network. The classes (classify*):
//
//   - this computer: loopback (127.0.0.0/8, ::1, localhost, *.localhost),
//     always refused (DevLoopbackEnv and this package's tests aside);
//     unspecified addresses (0.0.0.0/8, ::) and hosts that resolvers read as
//     a number ("2130706433", "0x7f.1", "127.1") are refused always;
//   - the local network: private (RFC 1918, fc00::/7, the old fec0::/10),
//     link-local (169.254.0.0/16, fe80::/10) and *.local (mDNS) are reached
//     only when the recording's page is on the local network itself (its
//     host is such an address or name, or resolves to such addresses only;
//     lead 2026-10-27: a self-hosted Opencast behind the school's VPN has
//     its media there; a public page sending LectureFlow there is the
//     attack, also when the media host is on the page's own site, whose
//     owner can point any of its names at a private address);
//   - everything else is public.
//
// Where (the same rule everywhere, per task: taskAddr):
//
//   - enqueue and refresh: the track URLs by their host as written, this
//     computer only (request.invalid {field: "tracks.url", reason:
//     "local_address"}); the local network needs the page's addresses,
//     looked up when the task runs;
//   - Go's own requests (playlists, MPDs, the redirect probes): the URL and
//     every redirect hop as written, and every connection by the address it
//     actually dials (net.Dialer.Control: a name resolving to this computer,
//     or answering differently the second time, is refused all the same);
//     each task has its own transport, so a connection one task may use is
//     never lent to another; a connection to the user's proxy is not
//     checked, the proxy resolves the names it is given;
//   - what aria2c gets (segments, keys, files after Go's redirects): the
//     host as written, and its addresses when no proxy applies.
//
// What stays open, and why (NOTES "sec-fetch"): aria2c resolves the name
// again (a DNS answer that changes between Go's lookup and aria2c's), and
// follows redirects by itself (aria2c has no option to stop that) for a
// task without cookies, whose URLs Go does not resolve first. Closing
// that would mean putting a proxy of ours between aria2c and every server.

// DevLoopbackEnv is the development switch of the native end-to-end test
// (extension/test/native, whose fake platform serves its media on
// 127.0.0.1): "1" lets downloads reach loopback addresses (127.0.0.0/8,
// ::1, localhost). The product never sets it; the other classes keep their
// rules. Like LECTUREFLOW_DEV_POWER, the variable itself is the switch.
const DevLoopbackEnv = "LECTUREFLOW_DEV_DOWNLOAD_LOOPBACK"

// addrClass is where an address is.
type addrClass int

const (
	classPublic   addrClass = iota
	classLocalNet           // private, link-local, *.local
	classLoopback           // this computer: loopback, localhost
	classUnsafe             // unspecified, or a number resolvers read as an address
)

// classifyIP tells where ip is.
func classifyIP(ip netip.Addr) addrClass {
	ip = ip.Unmap().WithZone("")
	switch {
	case ip.IsLoopback():
		return classLoopback
	case ip.IsUnspecified(), ip.Is4() && ip.As4()[0] == 0: // 0.0.0.0/8, "this network"
		return classUnsafe
	case ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return classLocalNet
	case ip.Is6() && ip.As16()[0] == 0xfe && ip.As16()[1]&0xc0 == 0xc0: // fec0::/10, site-local
		return classLocalNet
	}
	return classPublic
}

// classifyHost tells where host is as written; final is false when its
// addresses decide (a name).
func classifyHost(host string) (class addrClass, final bool) {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(h); err == nil {
		return classifyIP(ip), true
	}
	switch {
	case h == "localhost" || strings.HasSuffix(h, ".localhost"):
		return classLoopback, true
	case endsInNumber(h):
		return classUnsafe, true
	case h == "local" || strings.HasSuffix(h, ".local"):
		return classLocalNet, true
	}
	return classPublic, false
}

// endsInNumber is the URL standard's test for a host that is an IPv4
// address: its last label is a decimal or 0x hexadecimal number. Those
// that are not plain dotted quads ("2130706433", "0x7f.1", "127.1",
// "0177.0.0.1") mean 127.0.0.1 and the like to most resolvers, aria2c's
// included.
func endsInNumber(h string) bool {
	last := h[strings.LastIndexByte(h, '.')+1:]
	if last == "" {
		return false
	}
	if x, ok := strings.CutPrefix(last, "0x"); ok {
		if x == "" {
			return true
		}
		for _, c := range x {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return false
			}
		}
		return true
	}
	for _, c := range last {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// addrPolicy is what is the same for every task.
type addrPolicy struct {
	allowLoopback bool
	// lookup resolves a name (default net.DefaultResolver; tests).
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// proxy tells the proxy a URL goes through (default
	// http.ProxyFromEnvironment; tests).
	proxy func(*url.URL) (*url.URL, error)
}

func (p *addrPolicy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if p.lookup != nil {
		return p.lookup(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (p *addrPolicy) proxied(u *url.URL) bool {
	proxy := p.proxy
	if proxy == nil {
		proxy = func(u *url.URL) (*url.URL, error) { return http.ProxyFromEnvironment(&http.Request{URL: u}) }
	}
	pu, _ := proxy(u)
	return pu != nil
}

// forTask is the policy of a task whose recording page is on pageHost (""
// = unknown: the local network stays out of reach).
func (p *addrPolicy) forTask(pageHost string) *taskAddr {
	return &taskAddr{p: p, pageHost: strings.TrimSuffix(strings.ToLower(pageHost), ".")}
}

// taskAddr is the policy of one task: its page decides whether the local
// network may be reached.
type taskAddr struct {
	p        *addrPolicy
	pageHost string

	once     sync.Once
	pageNear bool // the page is on the local network
}

// localAddrError: host is, or resolves to, an address the task must not
// reach; loopback when it is this computer (CodeLoopbackAddress), else the
// local network from a public page (CodeLocalAddress).
type localAddrError struct {
	host     string
	loopback bool
}

func (e *localAddrError) Error() string {
	if e.loopback {
		return "refused: " + e.host + " is this computer"
	}
	return "refused: " + e.host + " is on the local network and the recording's page is not"
}

// pageOnLocalNet: the page's host is on the local network, by address or
// name, or every address it resolves to is (looked up once per task).
func (t *taskAddr) pageOnLocalNet(ctx context.Context) bool {
	t.once.Do(func() {
		if t.pageHost == "" {
			return
		}
		class, final := classifyHost(t.pageHost)
		if final {
			t.pageNear = class == classLocalNet
			return
		}
		ips, err := t.p.resolve(ctx, t.pageHost)
		if err != nil || len(ips) == 0 {
			return
		}
		for _, ip := range ips {
			if classifyIP(ip) != classLocalNet {
				return
			}
		}
		t.pageNear = true
	})
	return t.pageNear
}

// allow judges host (the URL's host) found to be of class.
func (t *taskAddr) allow(ctx context.Context, host string, class addrClass) error {
	switch class {
	case classLoopback:
		if t.p.allowLoopback {
			return nil
		}
		return &localAddrError{host: host, loopback: true}
	case classUnsafe:
		return &localAddrError{host: host, loopback: true}
	case classLocalNet:
		if t.pageOnLocalNet(ctx) {
			return nil
		}
		return &localAddrError{host: host}
	}
	return nil
}

// blockedSiteError: a URL of the task is on a site LectureFlow does not
// handle (internal/blocklist), wherever it came from (a playlist's entry,
// an MPD, a redirect).
type blockedSiteError struct {
	host, category string
}

func (e *blockedSiteError) Error() string {
	return "refused: " + e.host + " is a site LectureFlow does not handle (" + e.category + ")"
}

// checkURL judges raw's host as written and, with resolve and no proxy
// for raw, every address it resolves to (a lookup error is left to the
// request itself). A URL of a site on the do-not-handle list is refused
// first, whatever its address.
func (t *taskAddr) checkURL(ctx context.Context, raw string, resolve bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		// Allowed here only because every caller has already refused such a
		// URL: enqueue / refresh by validHTTPURL, playlist and manifest URLs
		// by resolve(), which requires a host (fuzz decision 4). A new caller
		// must check the URL itself first.
		return nil
	}
	if c := blocklist.URLCategory(raw); c != "" {
		return &blockedSiteError{host: u.Hostname(), category: c}
	}
	host := u.Hostname()
	class, final := classifyHost(host)
	if final {
		return t.allow(ctx, host, class)
	}
	if !resolve || t.p.proxied(u) {
		return nil // (a proxy resolves it)
	}
	ips, err := t.p.resolve(ctx, host)
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if err := t.allow(ctx, host, classifyIP(ip)); err != nil {
			return err
		}
	}
	return nil
}

// checkDialed is the dialer's check of the address it connects to for the
// URL host name.
func (t *taskAddr) checkDialed(ctx context.Context, name, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return &localAddrError{host: name, loopback: true}
	}
	return t.allow(ctx, name, classifyIP(ip))
}

// transport is the HTTP transport of Go's own requests for the task: the
// default one whose connections pass checkDialed, but for those to the
// user's proxy (HTTP_PROXY and the like: a school's proxy may well have a
// private address).
func (t *taskAddr) transport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	var proxies sync.Map // "host:port" of the proxies the transport was given
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		var u *url.URL
		var err error
		if t.p.proxy != nil {
			u, err = t.p.proxy(req.URL)
		} else {
			u, err = http.ProxyFromEnvironment(req)
		}
		if u != nil {
			proxies.Store(proxyAddr(u), true)
		}
		return u, err
	}
	direct := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, ok := proxies.Load(addr); ok {
			return direct.DialContext(ctx, network, addr)
		}
		name, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error { return t.checkDialed(ctx, name, address) }}
		return guarded.DialContext(ctx, network, addr)
	}
	return tr
}

// proxyAddr is the address net/http dials for a proxy URL.
func proxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[u.Scheme]
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// localFailure is the task failure of a refused address (nil when err is
// not one): the host of the URL being requested when the error tells it,
// else the host refused. A site on the do-not-handle list is
// downloads.site_blocked {category, host}.
func localFailure(err error) *failure {
	var be *blockedSiteError
	if errors.As(err, &be) {
		return &failure{code: CodeSiteBlocked, params: map[string]any{"category": be.category, "host": be.host},
			detail: "refused a site LectureFlow does not handle: " + be.host}
	}
	var le *localAddrError
	if !errors.As(err, &le) {
		return nil
	}
	host := le.host
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
	}
	if le.loopback {
		return &failure{code: CodeLoopbackAddress, params: map[string]any{"host": host},
			detail: "refused an address of this computer: " + host}
	}
	return &failure{code: CodeLocalAddress, params: map[string]any{"host": host},
		detail: "refused an address of the local network for a recording whose page is not on it: " + host}
}

// checkTrackHosts refuses track URLs whose host as written is this
// computer (the enqueue and refresh check; the local network is judged
// when the task runs, with the page's addresses).
func (p *addrPolicy) checkTrackHosts(tracks []liveTrack) error {
	t := p.forTask("")
	for _, tr := range tracks {
		u, err := url.Parse(tr.url)
		if err != nil {
			continue
		}
		class, final := classifyHost(u.Hostname())
		if final && (class == classLoopback || class == classUnsafe) {
			if t.allow(context.Background(), u.Hostname(), class) != nil {
				return localRequestError()
			}
		}
	}
	return nil
}

// taskNet is the network side of a task run: its address policy and the
// HTTP client of Go's own requests, whose connections are the task's
// alone (so a connection to the local network one task may make is never
// lent to another).
type taskNet struct {
	addr *taskAddr
	hc   *http.Client
	tr   *http.Transport // nil when the client came from Options.HTTPClient
}

// newTaskNet is the network side of a run whose page is on pageHost.
func (q *Queue) newTaskNet(pageHost string) *taskNet {
	n := &taskNet{addr: q.local.forTask(pageHost)}
	if q.o.HTTPClient != nil {
		n.hc = q.o.HTTPClient
		return n
	}
	n.tr = n.addr.transport()
	n.hc = &http.Client{Timeout: q.hc.Timeout, Transport: n.tr}
	return n
}

func (n *taskNet) close() {
	if n != nil && n.tr != nil {
		n.tr.CloseIdleConnections()
	}
}

// netOf is s's network side (a snapshot made outside a run: an unknown
// page).
func (q *Queue) netOf(s runSnapshot) *taskNet {
	if s.net != nil {
		return s.net
	}
	return q.newTaskNet("")
}

// pageHostOf is the host of the recording's page: from the referer, else
// the stored page_host.
func pageHostOf(referer, stored string) string {
	if u, err := url.Parse(referer); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	if h, _, err := net.SplitHostPort(stored); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(stored, "[]")
}
