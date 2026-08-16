package web

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
)

// assertPublicHost rejects SSRF-flavoured hosts before any request is made.
// A model-provided URL must not reach the loopback interface, private RFC1918
// ranges, link-local (incl. the 169.254.169.254 cloud metadata service),
// multicast, or the unspecified address. The host is resolved and every
// resolved IP is checked, so a hostname that resolves to an internal address
// is also refused (DNS-rebinding prevention at the entry point). When
// allowLocal is true the internal-host check is skipped (opt-in for reading a
// local dev server — never the default in production).
func assertPublicHost(u *url.URL, allowLocal bool) error {
	if allowLocal {
		return nil
	}
	host := u.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h // strip :port for the IP check below
	}
	// Literal IP (IPv4 or IPv6 in brackets).
	if ip := net.ParseIP(host); ip != nil {
		return checkPublicIP(ip, u.String())
	}
	// Named host: resolve and check every answer. A single internal answer is
	// enough to refuse (defense in depth against fast-flux to an internal IP).
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("fetch %s: cannot resolve host: %w", u.String(), err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("fetch %s: host resolved to no addresses", u.String())
	}
	for _, ip := range ips {
		if err := checkPublicIP(ip, u.String()); err != nil {
			return err
		}
	}
	return nil
}

// checkPublicIP refuses any non-public IP range.
func checkPublicIP(ip net.IP, raw string) error {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("fetch %s: host %s is not a public address (SSRF guard)", raw, ip)
	}
	return nil
}

// safeFetchClient returns an *http.Client that re-validates the resolved host
// on every redirect hop. Go's default client follows redirects, so a server
// that answers 302 -> http://169.254.169.254/... would otherwise bypass the
// entry-point check. We intercept CheckRedirect, parse the next URL, and
// refuse internal/loopback/link-local targets. Up to 10 public hops are
// allowed (default client behaviour). allowLocal opts the internal-host check
// out entirely (only set when reading a trusted local dev server).
func safeFetchClient(base *http.Client, allowLocal bool) *http.Client {
	c := &http.Client{Timeout: base.Timeout}
	if t, ok := base.Transport.(*http.Transport); ok {
		c.Transport = t.Clone()
	} else if base.Transport != nil {
		c.Transport = base.Transport
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := assertPublicHost(req.URL, allowLocal); err != nil {
			return err
		}
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return c
}
