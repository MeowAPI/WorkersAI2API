package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

type fingerprintClient interface {
	Do(*fhttp.Request) (*fhttp.Response, error)
	CloseIdleConnections()
}

type fingerprintCacheEntry struct {
	client fingerprintClient
	used   uint64
}

type fingerprintPool struct {
	mu        sync.Mutex
	clients   map[string]fingerprintCacheEntry
	clock     uint64
	newClient func(string, string) (fingerprintClient, error)
}

func newFingerprintClient(profileID, proxyURL string, options ...tlsclient.HttpClientOption) (fingerprintClient, error) {
	profile := findFingerprint(profileID)
	if profile == nil {
		return nil, fmt.Errorf("unsupported upstream fingerprint")
	}
	idleTimeout := 90 * time.Second
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	base := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(profile.profile),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithDisableHttp3(),
		// The request context owns the overall deadline, including streamed bodies.
		tlsclient.WithTimeoutSeconds(0),
		tlsclient.WithTransportOptions(&tlsclient.TransportOptions{
			IdleConnTimeout: &idleTimeout, MaxIdleConns: 32, MaxIdleConnsPerHost: 32,
		}),
	}
	if proxyURL != "" {
		base = append(base, tlsclient.WithProxyUrl(proxyURL))
	} else {
		base = append(base, tlsclient.WithDialContext(dialer.DialContext))
	}
	if profileID == "chrome_152" {
		base = append(base, tlsclient.WithRandomTLSExtensionOrder())
	}
	return tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), append(base, options...)...)
}

func (p *fingerprintPool) get(a account, proxyURL string) (fingerprintClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients = make(map[string]fingerprintCacheEntry)
	}
	key := a.ID + "\x00" + a.Fingerprint + "\x00" + proxyURL
	p.clock++
	if entry, ok := p.clients[key]; ok {
		entry.used = p.clock
		p.clients[key] = entry
		return entry.client, nil
	}
	factory := p.newClient
	if factory == nil {
		factory = func(profileID, proxyURL string) (fingerprintClient, error) {
			return newFingerprintClient(profileID, proxyURL)
		}
	}
	client, err := factory(a.Fingerprint, proxyURL)
	if err != nil {
		return nil, err
	}
	// Account/profile/proxy changes must not create an unbounded connection cache.
	if len(p.clients) >= 100 {
		oldestKey := ""
		var oldest uint64
		for key, entry := range p.clients {
			if oldestKey == "" || entry.used < oldest {
				oldestKey, oldest = key, entry.used
			}
		}
		p.clients[oldestKey].client.CloseIdleConnections()
		delete(p.clients, oldestKey)
	}
	p.clients[key] = fingerprintCacheEntry{client: client, used: p.clock}
	return client, nil
}

func (p *fingerprintPool) do(req *http.Request, a account) (*http.Response, error) {
	proxyURL := ""
	if proxy, err := http.ProxyFromEnvironment(req); err != nil {
		return nil, fmt.Errorf("invalid upstream proxy")
	} else if proxy != nil {
		proxyURL = proxy.String()
	}
	client, err := p.get(a, proxyURL)
	if err != nil {
		return nil, err
	}
	return doFingerprintRequest(client, req)
}

func doFingerprintRequest(client fingerprintClient, req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	headerTimer := time.AfterFunc(90*time.Second, cancel)
	defer headerTimer.Stop()
	forward, err := fhttp.NewRequestWithContext(ctx, req.Method, req.URL.String(), req.Body)
	if err != nil {
		cancel()
		return nil, err
	}
	forward.Header = fhttp.Header(req.Header.Clone())
	forward.Host = req.Host
	forward.ContentLength = req.ContentLength
	forward.GetBody = req.GetBody
	forward.Header[fhttp.HeaderOrderKey] = []string{"host", "user-agent", "accept", "authorization", "content-type", "content-length", "x-session-affinity", "accept-encoding"}
	resp, err := client.Do(forward)
	if err != nil {
		timedOut := ctx.Err() != nil
		cancel()
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		if timedOut {
			return nil, fmt.Errorf("upstream response header timeout: %w", context.DeadlineExceeded)
		}
		return nil, err
	}
	return &http.Response{
		Status: resp.Status, StatusCode: resp.StatusCode,
		Proto: resp.Proto, ProtoMajor: resp.ProtoMajor, ProtoMinor: resp.ProtoMinor,
		Header: http.Header(resp.Header), Body: &fingerprintResponseBody{ReadCloser: resp.Body, cancel: cancel},
		ContentLength: resp.ContentLength, TransferEncoding: resp.TransferEncoding,
		Close: resp.Close, Uncompressed: resp.Uncompressed, Trailer: http.Header(resp.Trailer), Request: req,
	}, nil
}

type fingerprintResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *fingerprintResponseBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}
