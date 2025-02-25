// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tshttpproxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/alexbrainman/sspi/negotiate"
	"golang.org/x/sys/windows"
	"tailscale.com/hostinfo"
	"tailscale.com/types/logger"
	"tailscale.com/util/cmpver"
)

func init() {
	sysProxyFromEnv = proxyFromWinHTTPOrCache
	sysAuthHeader = sysAuthHeaderWindows
}

var cachedProxy struct {
	sync.Mutex
	val *url.URL
}

// proxyErrorf is a rate-limited logger specifically for errors asking
// WinHTTP for the proxy information. We don't want to log about
// errors often, otherwise the log message itself will generate a new
// HTTP request which ultimately will call back into us to log again,
// forever. So for errors, we only log a bit.
var proxyErrorf = logger.RateLimitedFn(log.Printf, 10*time.Minute, 2 /* burst*/, 10 /* maxCache */)

func proxyFromWinHTTPOrCache(req *http.Request) (*url.URL, error) {
	if req.URL == nil {
		return nil, nil
	}
	urlStr := req.URL.String()

	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()

	type result struct {
		proxy *url.URL
		err   error
	}
	resc := make(chan result, 1)
	go func() {
		proxy, err := proxyFromWinHTTP(ctx, urlStr)
		resc <- result{proxy, err}
	}()

	select {
	case res := <-resc:
		err := res.err
		if err == nil {
			cachedProxy.Lock()
			defer cachedProxy.Unlock()
			if was, now := fmt.Sprint(cachedProxy.val), fmt.Sprint(res.proxy); was != now {
				log.Printf("tshttpproxy: winhttp: updating cached proxy setting from %v to %v", was, now)
			}
			cachedProxy.val = res.proxy
			return res.proxy, nil
		}

		// See https://docs.microsoft.com/en-us/windows/win32/winhttp/error-messages
		const (
			ERROR_WINHTTP_AUTODETECTION_FAILED      = 12180
			ERROR_WINHTTP_UNABLE_TO_DOWNLOAD_SCRIPT = 12167
		)
		if err == syscall.Errno(ERROR_WINHTTP_AUTODETECTION_FAILED) {
			setNoProxyUntil(10 * time.Second)
			return nil, nil
		}
		if err == windows.ERROR_INVALID_PARAMETER {
			// Seen on Windows 8.1. (https://github.com/tailscale/tailscale/issues/879)
			// TODO(bradfitz): figure this out.
			setNoProxyUntil(time.Hour)
			proxyErrorf("tshttpproxy: winhttp: GetProxyForURL(%q): ERROR_INVALID_PARAMETER [unexpected]", urlStr)
			return nil, nil
		}
		proxyErrorf("tshttpproxy: winhttp: GetProxyForURL(%q): %v/%#v", urlStr, err, err)
		if err == syscall.Errno(ERROR_WINHTTP_UNABLE_TO_DOWNLOAD_SCRIPT) {
			setNoProxyUntil(10 * time.Second)
			return nil, nil
		}
		return nil, err
	case <-ctx.Done():
		cachedProxy.Lock()
		defer cachedProxy.Unlock()
		proxyErrorf("tshttpproxy: winhttp: GetProxyForURL(%q): timeout; using cached proxy %v", urlStr, cachedProxy.val)
		return cachedProxy.val, nil
	}
}

func proxyFromWinHTTP(ctx context.Context, urlStr string) (proxy *url.URL, err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	whi, err := httpOpen()
	if err != nil {
		proxyErrorf("winhttp: Open: %v", err)
		return nil, err
	}
	defer whi.Close()

	t0 := time.Now()
	v, err := whi.GetProxyForURL(urlStr)
	td := time.Since(t0).Round(time.Millisecond)
	if err := ctx.Err(); err != nil {
		log.Printf("tshttpproxy: winhttp: context canceled, ignoring GetProxyForURL(%q) after %v", urlStr, td)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if v == "" {
		return nil, nil
	}
	// Discard all but first proxy value for now.
	if i := strings.Index(v, ";"); i != -1 {
		v = v[:i]
	}
	if !strings.HasPrefix(v, "https://") {
		v = "http://" + v
	}
	return url.Parse(v)
}

var userAgent = windows.StringToUTF16Ptr("Tailscale")

const (
	winHTTP_ACCESS_TYPE_DEFAULT_PROXY   = 0
	winHTTP_ACCESS_TYPE_AUTOMATIC_PROXY = 4
	winHTTP_AUTOPROXY_ALLOW_AUTOCONFIG  = 0x00000100
	winHTTP_AUTOPROXY_AUTO_DETECT       = 1
	winHTTP_AUTO_DETECT_TYPE_DHCP       = 0x00000001
	winHTTP_AUTO_DETECT_TYPE_DNS_A      = 0x00000002
)

// Windows 8.1 is actually Windows 6.3 under the hood. Yay, marketing!
const win8dot1Ver = "6.3"

// accessType is the flag we must pass to WinHttpOpen for proxy resolution
// depending on whether or not we're running Windows < 8.1
var accessType atomic.Value // of uint32

func getAccessFlag() uint32 {
	if flag, ok := accessType.Load().(uint32); ok {
		return flag
	}
	var flag uint32
	if cmpver.Compare(hostinfo.GetOSVersion(), win8dot1Ver) < 0 {
		flag = winHTTP_ACCESS_TYPE_DEFAULT_PROXY
	} else {
		flag = winHTTP_ACCESS_TYPE_AUTOMATIC_PROXY
	}
	accessType.Store(flag)
	return flag
}

func httpOpen() (winHTTPInternet, error) {
	return winHTTPOpen(
		userAgent,
		getAccessFlag(),
		nil, /* WINHTTP_NO_PROXY_NAME */
		nil, /* WINHTTP_NO_PROXY_BYPASS */
		0,
	)
}

type winHTTPInternet windows.Handle

func (hi winHTTPInternet) Close() error {
	return winHTTPCloseHandle(hi)
}

// WINHTTP_AUTOPROXY_OPTIONS
// https://docs.microsoft.com/en-us/windows/win32/api/winhttp/ns-winhttp-winhttp_autoproxy_options
type winHTTPAutoProxyOptions struct {
	DwFlags                uint32
	DwAutoDetectFlags      uint32
	AutoConfigUrl          *uint16
	_                      uintptr
	_                      uint32
	FAutoLogonIfChallenged int32 // BOOL
}

// WINHTTP_PROXY_INFO
// https://docs.microsoft.com/en-us/windows/win32/api/winhttp/ns-winhttp-winhttp_proxy_info
type winHTTPProxyInfo struct {
	AccessType  uint32
	Proxy       *uint16
	ProxyBypass *uint16
}

type winHGlobal windows.Handle

func globalFreeUTF16Ptr(p *uint16) error {
	return globalFree((winHGlobal)(unsafe.Pointer(p)))
}

func (pi *winHTTPProxyInfo) free() {
	if pi.Proxy != nil {
		globalFreeUTF16Ptr(pi.Proxy)
		pi.Proxy = nil
	}
	if pi.ProxyBypass != nil {
		globalFreeUTF16Ptr(pi.ProxyBypass)
		pi.ProxyBypass = nil
	}
}

var proxyForURLOpts = &winHTTPAutoProxyOptions{
	DwFlags:           winHTTP_AUTOPROXY_ALLOW_AUTOCONFIG | winHTTP_AUTOPROXY_AUTO_DETECT,
	DwAutoDetectFlags: winHTTP_AUTO_DETECT_TYPE_DHCP, // | winHTTP_AUTO_DETECT_TYPE_DNS_A,
}

func (hi winHTTPInternet) GetProxyForURL(urlStr string) (string, error) {
	var out winHTTPProxyInfo
	err := winHTTPGetProxyForURL(
		hi,
		windows.StringToUTF16Ptr(urlStr),
		proxyForURLOpts,
		&out,
	)
	if err != nil {
		return "", err
	}
	defer out.free()
	return windows.UTF16PtrToString(out.Proxy), nil
}

func sysAuthHeaderWindows(u *url.URL) (string, error) {
	spn := "HTTP/" + u.Hostname()
	creds, err := negotiate.AcquireCurrentUserCredentials()
	if err != nil {
		return "", fmt.Errorf("negotiate.AcquireCurrentUserCredentials: %w", err)
	}
	defer creds.Release()

	secCtx, token, err := negotiate.NewClientContext(creds, spn)
	if err != nil {
		return "", fmt.Errorf("negotiate.NewClientContext: %w", err)
	}
	defer secCtx.Release()

	return "Negotiate " + base64.StdEncoding.EncodeToString(token), nil
}
