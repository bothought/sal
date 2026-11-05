package upgrade

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// systemResolverFile is where the standard library looks for the system's name
// servers, and the only path its resolver reads.
const systemResolverFile = "/etc/resolv.conf"

// alternateResolverFiles are searched in order when the system file does not
// exist. Android serves DNS through a system daemon and never writes
// /etc/resolv.conf; Termux keeps the configuration under its own prefix.
// Without this, a sal built for Android on an ordinary machine cannot resolve a
// name, and so cannot reach the release it is supposed to install.
var alternateResolverFiles = []string{
	"/data/data/com.termux/files/usr/etc/resolv.conf",
}

// newDialer returns a dialer that can look names up on this machine.
//
// Where /etc/resolv.conf exists this is the ordinary dialer. Where it does not
// — Android, and Termux in particular — the name servers are read from the
// alternate location and asked directly, because the standard resolver would
// otherwise fall back to a loopback address nothing is listening on.
func newDialer(timeout, keepAlive time.Duration) *net.Dialer {
	d := &net.Dialer{Timeout: timeout, KeepAlive: keepAlive}
	ns := findNameserver()
	if ns == "" {
		return d
	}
	d.Resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// The resolver retries over tcp itself when a response is
			// truncated, so both networks go to the same server.
			return d.DialContext(ctx, network, ns)
		},
	}
	return d
}

// findNameserver returns the name server to use, or "" when the system file
// exists (nothing to work around) or no alternate file names one.
func findNameserver() string {
	if _, err := os.Stat(systemResolverFile); err == nil {
		return ""
	}
	for _, path := range alternateResolverFiles {
		if ns := nameserverIn(path); ns != "" {
			return ns
		}
	}
	return ""
}

// nameserverIn parses the first nameserver line of a resolver configuration.
func nameserverIn(path string) string {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if net.ParseIP(fields[1]) == nil {
			continue
		}
		return net.JoinHostPort(fields[1], "53")
	}
	return ""
}

// newHTTPClient is the client an upgrade uses. It resolves names through the
// dialer above, and it has no overall timeout: the run is bounded by its
// context and by the download limit instead.
func newHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           newDialer(30*time.Second, 30*time.Second).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}
