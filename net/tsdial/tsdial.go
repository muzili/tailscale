// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package tsdial provides a Dialer type that can dial out of tailscaled.
package tsdial

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"tailscale.com/net/dnscache"
	"tailscale.com/net/interfaces"
	"tailscale.com/net/netaddr"
	"tailscale.com/net/netknob"
	"tailscale.com/net/netns"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/util/mak"
	"tailscale.com/wgengine/monitor"
)

// Dialer dials out of tailscaled, while taking care of details while
// handling the dozens of edge cases depending on the server mode
// (TUN, netstack), the OS network sandboxing style (macOS/iOS
// Extension, none), user-selected route acceptance prefs, etc.
type Dialer struct {
	Logf logger.Logf
	// UseNetstackForIP if non-nil is whether NetstackDialTCP (if
	// it's non-nil) should be used to dial the provided IP.
	UseNetstackForIP func(netip.Addr) bool

	// NetstackDialTCP dials the provided IPPort using netstack.
	// If nil, it's not used.
	NetstackDialTCP func(context.Context, netip.AddrPort) (net.Conn, error)

	peerDialControlFuncAtomic atomic.Value // of func() func(network, address string, c syscall.RawConn) error

	peerClientOnce sync.Once
	peerClient     *http.Client

	peerDialerOnce sync.Once
	peerDialer     *net.Dialer

	netnsDialerOnce sync.Once
	netnsDialer     netns.Dialer

	mu                sync.Mutex
	closed            bool
	dns               dnsMap
	tunName           string // tun device name
	linkMon           *monitor.Mon
	linkMonUnregister func()
	exitDNSDoHBase    string                 // non-empty if DoH-proxying exit node in use; base URL+path (without '?')
	dnsCache          *dnscache.MessageCache // nil until first first non-empty SetExitDNSDoH
	nextSysConnID     int
	activeSysConns    map[int]net.Conn // active connections not yet closed
}

// sysConn wraps a net.Conn that was created using d.SystemDial.
// It exists to track which connections are still open, and should be
// closed on major link changes.
type sysConn struct {
	net.Conn
	id int
	d  *Dialer
}

func (c sysConn) Close() error {
	c.d.closeSysConn(c.id)
	return nil
}

// SetTUNName sets the name of the tun device in use ("tailscale0", "utun6",
// etc). This is needed on some platforms to set sockopts to bind
// to the same interface index.
func (d *Dialer) SetTUNName(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tunName = name
}

// TUNName returns the name of the tun device in use, if any.
// Example format ("tailscale0", "utun6").
func (d *Dialer) TUNName() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tunName
}

// SetExitDNSDoH sets (or clears) the exit node DNS DoH server base URL to use.
// The doh URL should contain the scheme, authority, and path, but without
// a '?' and/or query parameters.
//
// For example, "http://100.68.82.120:47830/dns-query".
func (d *Dialer) SetExitDNSDoH(doh string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.exitDNSDoHBase == doh {
		return
	}
	d.exitDNSDoHBase = doh
	if doh != "" && d.dnsCache == nil {
		d.dnsCache = new(dnscache.MessageCache)
	}
	if d.dnsCache != nil {
		d.dnsCache.Flush()
	}
}

func (d *Dialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.linkMonUnregister != nil {
		d.linkMonUnregister()
		d.linkMonUnregister = nil
	}
	for _, c := range d.activeSysConns {
		c.Close()
	}
	d.activeSysConns = nil
	return nil
}

func (d *Dialer) SetLinkMonitor(mon *monitor.Mon) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.linkMonUnregister != nil {
		go d.linkMonUnregister()
		d.linkMonUnregister = nil
	}
	d.linkMon = mon
	d.linkMonUnregister = d.linkMon.RegisterChangeCallback(d.linkChanged)
}

func (d *Dialer) linkChanged(major bool, state *interfaces.State) {
	if !major {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, c := range d.activeSysConns {
		go c.Close()
		delete(d.activeSysConns, id)
	}
}

func (d *Dialer) closeSysConn(id int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.activeSysConns[id]
	if !ok {
		return
	}
	delete(d.activeSysConns, id)
	go c.Close() // ignore the error
}

func (d *Dialer) interfaceIndexLocked(ifName string) (index int, ok bool) {
	if d.linkMon == nil {
		return 0, false
	}
	st := d.linkMon.InterfaceState()
	iface, ok := st.Interface[ifName]
	if !ok {
		return 0, false
	}
	return iface.Index, true
}

// peerDialControlFunc is non-nil on platforms that require a way to
// bind to dial out to other peers.
var peerDialControlFunc func(*Dialer) func(network, address string, c syscall.RawConn) error

// PeerDialControlFunc returns a function
// that can assigned to net.Dialer.Control to set sockopts or whatnot
// to make a dial escape the current platform's network sandbox.
//
// On many platforms the returned func will be nil.
//
// Notably, this is non-nil on iOS and macOS when run as a Network or
// System Extension (the GUI variants).
func (d *Dialer) PeerDialControlFunc() func(network, address string, c syscall.RawConn) error {
	if peerDialControlFunc == nil {
		return nil
	}
	return peerDialControlFunc(d)
}

// SetNetMap sets the current network map and notably, the DNS names
// in its DNS configuration.
func (d *Dialer) SetNetMap(nm *netmap.NetworkMap) {
	m := dnsMapFromNetworkMap(nm)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.dns = m
}

func (d *Dialer) userDialResolve(ctx context.Context, network, addr string) (netip.AddrPort, error) {
	d.mu.Lock()
	dns := d.dns
	exitDNSDoH := d.exitDNSDoHBase
	d.mu.Unlock()

	// MagicDNS or otherwise baked in to the NetworkMap? Try that first.
	ipp, err := dns.resolveMemory(ctx, network, addr)
	if err != errUnresolved {
		return ipp, err
	}

	// Otherwise, hit the network.

	// TODO(bradfitz): wire up net/dnscache too.

	host, port, err := splitHostPort(addr)
	if err != nil {
		// addr is malformed.
		return netip.AddrPort{}, err
	}

	var r net.Resolver
	if exitDNSDoH != "" && runtime.GOOS != "windows" { // Windows: https://github.com/golang/go/issues/33097
		r.PreferGo = true
		r.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return &dohConn{
				ctx:      ctx,
				baseURL:  exitDNSDoH,
				hc:       d.PeerAPIHTTPClient(),
				dnsCache: d.dnsCache,
			}, nil
		}
	}

	ips, err := r.LookupIP(ctx, ipNetOfNetwork(network), host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if len(ips) == 0 {
		return netip.AddrPort{}, fmt.Errorf("DNS lookup returned no results for %q", host)
	}
	ip, _ := netaddr.FromStdIP(ips[0])
	return netip.AddrPortFrom(ip, port), nil
}

// ipNetOfNetwork returns "ip", "ip4", or "ip6" corresponding
// to the input value of "tcp", "tcp4", "udp6" etc network
// names.
func ipNetOfNetwork(n string) string {
	if strings.HasSuffix(n, "4") {
		return "ip4"
	}
	if strings.HasSuffix(n, "6") {
		return "ip6"
	}
	return "ip"
}

// SystemDial connects to the provided network address without going over
// Tailscale. It prefers going over the default interface and closes existing
// connections if the default interface changes. It is used to connect to
// Control and (in the future, as of 2022-04-27) DERPs..
func (d *Dialer) SystemDial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}

	d.netnsDialerOnce.Do(func() {
		logf := d.Logf
		if logf == nil {
			logf = logger.Discard
		}
		d.netnsDialer = netns.NewDialer(logf)
	})
	c, err := d.netnsDialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.nextSysConnID
	d.nextSysConnID++
	mak.Set(&d.activeSysConns, id, c)

	return sysConn{
		id:   id,
		d:    d,
		Conn: c,
	}, nil
}

// UserDial connects to the provided network address as if a user were initiating the dial.
// (e.g. from a SOCKS or HTTP outbound proxy)
func (d *Dialer) UserDial(ctx context.Context, network, addr string) (net.Conn, error) {
	ipp, err := d.userDialResolve(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if d.UseNetstackForIP != nil && d.UseNetstackForIP(ipp.Addr()) {
		if d.NetstackDialTCP == nil {
			return nil, errors.New("Dialer not initialized correctly")
		}
		return d.NetstackDialTCP(ctx, ipp)
	}
	// TODO(bradfitz): netns, etc
	var stdDialer net.Dialer
	return stdDialer.DialContext(ctx, network, ipp.String())
}

// dialPeerAPI connects to a Tailscale peer's peerapi over TCP.
//
// network must a "tcp" type, and addr must be an ip:port. Name resolution
// is not supported.
func (d *Dialer) dialPeerAPI(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp6", "tcp4":
	default:
		return nil, fmt.Errorf("peerAPI dial requires tcp; %q not supported", network)
	}
	ipp, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, fmt.Errorf("peerAPI dial requires ip:port, not name resolution: %w", err)
	}
	if d.UseNetstackForIP != nil && d.UseNetstackForIP(ipp.Addr()) {
		if d.NetstackDialTCP == nil {
			return nil, errors.New("Dialer not initialized correctly")
		}
		return d.NetstackDialTCP(ctx, ipp)
	}
	return d.getPeerDialer().DialContext(ctx, network, addr)
}

// getPeerDialer returns the *net.Dialer to use to dial peers to use
// peer API.
//
// This is not used in netstack mode.
//
// The primary function of this is to work on macOS & iOS's in the
// Network/System Extension so it can mark the dialer as staying
// withing the network namespace/sandbox.
func (d *Dialer) getPeerDialer() *net.Dialer {
	d.peerDialerOnce.Do(func() {
		d.peerDialer = &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: netknob.PlatformTCPKeepAlive(),
			Control:   d.PeerDialControlFunc(),
		}
	})
	return d.peerDialer
}

// PeerAPIHTTPClient returns an HTTP Client to call peers' peerapi
// endpoints.                                                                                                                                                                                                                      //
// The returned Client must not be mutated; it's owned by the Dialer
// and shared by callers.
func (d *Dialer) PeerAPIHTTPClient() *http.Client {
	d.peerClientOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Dial = nil
		t.DialContext = d.dialPeerAPI
		d.peerClient = &http.Client{Transport: t}
	})
	return d.peerClient
}

// PeerAPITransport returns a Transport to call peers' peerapi
// endpoints.
//
// The returned value must not be mutated; it's owned by the Dialer
// and shared by callers.
func (d *Dialer) PeerAPITransport() *http.Transport {
	return d.PeerAPIHTTPClient().Transport.(*http.Transport)
}
