package managedproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const feedLimit int64 = 8 << 20

// Also block special-use ranges that IsPrivate does not include (CGNAT,
// benchmarking, documentation, multicast and IPv4-mapped IPv6 included).
var deniedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, p := range deniedNetworks {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func validateRemoteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 16384 || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return nil, errors.New("managed proxy: URL must be HTTP(S), without credentials or fragment")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.Contains(host, "%") {
		return nil, errors.New("managed proxy: subscription address is not public")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicIP(ip) {
		return nil, errors.New("managed proxy: subscription address is not public")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("managed proxy: invalid URL port")
		}
	}
	return u, nil
}

func publicAddresses(ctx context.Context, host string, lookup func(context.Context, string) ([]netip.Addr, error)) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !publicIP(ip) {
			return nil, errors.New("address blocked")
		}
		return []netip.Addr{ip}, nil
	}
	ips, err := lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("public DNS lookup failed")
	}
	// Reject the entire answer if any address is private. Pin the subsequent dial
	// to an inspected literal, preventing a second resolution / DNS rebinding.
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, errors.New("address blocked")
		}
	}
	return ips, nil
}

func remoteClient() (*http.Client, *http.Transport) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableKeepAlives: true}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, errors.New("invalid remote address")
		}
		ips, err := publicAddresses(ctx, host, func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		})
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				break
			}
		}
		return nil, errors.New("remote connection failed")
	}
	client := &http.Client{Transport: transport, Timeout: 45 * time.Second}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("redirect limit exceeded")
		}
		if len(via) != 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("HTTPS downgrade blocked")
		}
		_, err := validateRemoteURL(req.URL.String())
		return err
	}
	return client, transport
}

func fetchRemote(ctx context.Context, raw string, limit int64) ([]byte, error) {
	client, transport := remoteClient()
	defer transport.CloseIdleConnections()
	return download(ctx, client, raw, limit)
}

func download(ctx context.Context, client *http.Client, raw string, limit int64) ([]byte, error) {
	if _, err := validateRemoteURL(raw); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, errors.New("managed proxy: invalid download request")
	}
	req.Header.Set("User-Agent", "managedproxy/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("managed proxy: public download failed (network, DNS, redirect or timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("managed proxy: download returned an unsuccessful status")
	}
	if resp.ContentLength > limit {
		return nil, errors.New("managed proxy: download exceeds size limit")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(b)) > limit || len(b) == 0 {
		return nil, errors.New("managed proxy: download is empty, truncated or exceeds size limit")
	}
	return b, nil
}
