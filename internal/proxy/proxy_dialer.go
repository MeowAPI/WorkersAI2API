package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	xproxy "golang.org/x/net/proxy"
)

func fingerprintProxyDialer(raw string, dialer *net.Dialer, proxyTLS *tls.Config) (func(context.Context, string, string) (net.Conn, error), error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream proxy")
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		if u.Port() == "" {
			port := "80"
			if u.Scheme == "https" {
				port = "443"
			}
			u.Host = net.JoinHostPort(u.Hostname(), port)
		}
		return (&httpProxyDialer{proxy: u, dialer: dialer, tlsConfig: proxyTLS}).DialContext, nil
	}
	socks, err := xproxy.FromURL(u, dialer)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream proxy")
	}
	contextDialer, ok := socks.(xproxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("upstream proxy does not support request cancellation")
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return contextDialer.DialContext(ctx, network, address)
	}, nil
}

type httpProxyDialer struct {
	proxy     *url.URL
	dialer    *net.Dialer
	tlsConfig *tls.Config
}

func (d *httpProxyDialer) DialContext(ctx context.Context, network, address string) (conn net.Conn, err error) {
	// Limit only tunnel establishment; streamed responses keep the request's deadline.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := d.dialer.DialContext(ctx, network, d.proxy.Host)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer func() {
		if !stop() && err == nil {
			err = ctx.Err()
		}
		if err != nil {
			raw.Close()
			conn = nil
		}
	}()
	conn = raw
	if deadline, ok := ctx.Deadline(); ok {
		if err = conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	if d.proxy.Scheme == "https" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if d.tlsConfig != nil {
			cfg = d.tlsConfig.Clone()
		}
		cfg.ServerName = d.proxy.Hostname()
		cfg.NextProtos = []string{"http/1.1"}
		secure := tls.Client(raw, cfg)
		if err = secure.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = secure
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if d.proxy.User != nil {
		credential, _ := d.proxy.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(d.proxy.User.Username() + ":" + credential))
		req.Header.Set("Proxy-Authorization", "Basic "+auth)
	}
	if err = req.Write(conn); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy CONNECT failed (HTTP %d)", resp.StatusCode)
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if reader.Buffered() > 0 {
		conn = &bufferedProxyConn{Conn: conn, reader: reader}
	}
	return conn, nil
}

type bufferedProxyConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedProxyConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
