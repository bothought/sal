package upgrade

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
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

// systemCertFiles are the bundles the Go TLS stack reads on its own.
var systemCertFiles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// alternateCertFiles are searched when no system bundle exists. Termux installs
// its certificates under its own prefix, which the Go TLS stack does not read;
// without this a sal built on an ordinary machine cannot complete a handshake
// with anything there.
var alternateCertFiles = []string{
	"/data/data/com.termux/files/usr/etc/tls/cert.pem",
}

// rootCAs returns the certificate pool to trust, or nil to use the system's.
//
// It only steps in when the system has no bundle at all, so an ordinary host
// keeps the standard behaviour, including any bundle the environment names.
func rootCAs() *x509.CertPool {
	if os.Getenv("SSL_CERT_FILE") != "" {
		return nil // the system already honors an explicit choice
	}
	for _, path := range systemCertFiles {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	for _, path := range alternateCertFiles {
		pem, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			continue
		}
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(pem) {
			return pool
		}
	}
	return nil
}

// nameserverIn parses the first nameserver line of a resolver configuration.
func nameserverIn(path string) string {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// A comments line can be long; the default 64 KiB would stop at it and
	// quietly ignore every nameserver after it.
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
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
// dialer above, trusts the platform's certificate bundle when the system one is
// missing, and has no overall timeout: the run is bounded by its context and by
// the download limit instead.
func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           newDialer(30*time.Second, 30*time.Second).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if pool := rootCAs(); pool != nil {
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &http.Client{Transport: transport}
}
