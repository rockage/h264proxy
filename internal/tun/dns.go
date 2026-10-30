package tun

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/roca/h264-tunnel-lab/internal/logging"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/proxy"
)

// dnsProxy sends wire-format DNS over HTTPS through SOCKS. A literal bootstrap
// address avoids using the system resolver to establish the encrypted DNS path.
// TLS still validates cloudflare-dns.com's certificate.
type dnsProxy struct {
	ipv4Only   bool
	log        *logging.Logger
	client     *http.Client
	transport  *http.Transport
	endpoint   string
	udp        net.PacketConn
	tcp        net.Listener
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	slots      chan struct{}
	lookupCtx  context.Context
	lookupWG   sync.WaitGroup
	resolverMu sync.Mutex
	cache      map[string]dnsCacheEntry
	flights    map[string]*dnsFlight
}

func startDNS(parent context.Context, address, socks string, ipv6 bool, log *logging.Logger) (*dnsProxy, error) {
	d, err := proxy.SOCKS5("tcp", socks, nil, &net.Dialer{Timeout: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("SOCKS dialer does not support cancellation")
	}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return cd.DialContext(ctx, "tcp", "1.1.1.1:443")
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxConnsPerHost:       8,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
	}
	ctx, cancel := context.WithCancel(parent)
	p := &dnsProxy{ipv4Only: !ipv6, log: log, transport: tr, client: &http.Client{Transport: tr, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		endpoint: "https://cloudflare-dns.com/dns-query", cancel: cancel, slots: make(chan struct{}, 64), lookupCtx: ctx}
	p.udp, err = net.ListenPacket("udp4", address)
	if err != nil {
		cancel()
		tr.CloseIdleConnections()
		return nil, fmt.Errorf("listen DNS UDP: %w", err)
	}
	p.tcp, err = net.Listen("tcp4", p.udp.LocalAddr().String())
	if err != nil {
		p.udp.Close()
		cancel()
		tr.CloseIdleConnections()
		return nil, fmt.Errorf("listen DNS TCP: %w", err)
	}
	p.wg.Add(2)
	go p.serveUDP(ctx)
	go p.serveTCP(ctx)
	return p, nil
}

func (p *dnsProxy) Close() {
	p.cancel()
	p.udp.Close()
	p.tcp.Close()
	p.wg.Wait()
	p.lookupWG.Wait()
	p.transport.CloseIdleConnections()
}

func (p *dnsProxy) exchange(ctx context.Context, query []byte) ([]byte, error) {
	var q dnsmessage.Message
	if err := q.Unpack(query); err != nil || q.Header.Response || len(q.Questions) != 1 || q.Header.OpCode != 0 {
		return nil, fmt.Errorf("invalid DNS query")
	}
	// An IPv4-only gateway cannot serve literal IPv6 destinations. Suppress
	// AAAA and service-binding records (which can carry IPv6 hints and aliases)
	// so applications resolve A records instead. This is local NODATA policy,
	// not NXDOMAIN: the name and its other record types remain valid.
	if p.ipv4Only && q.Questions[0].Class == dnsmessage.ClassINET {
		switch q.Questions[0].Type {
		case dnsmessage.TypeAAAA, dnsmessage.Type(64), dnsmessage.Type(65):
			return (&dnsmessage.Message{Header: dnsmessage.Header{
				ID: q.Header.ID, Response: true, RecursionDesired: q.Header.RecursionDesired,
				RecursionAvailable: true, RCode: dnsmessage.RCodeSuccess,
			}, Questions: q.Questions}).Pack()
		}
	}
	if !cacheableQuery(q) {
		return p.fetch(ctx, query, q)
	}
	return p.resolve(ctx, query, q)
}

func (p *dnsProxy) fetch(ctx context.Context, query []byte, q dnsmessage.Message) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(query))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH HTTP %d", resp.StatusCode)
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil || len(answer) > 65535 {
		return nil, fmt.Errorf("invalid DoH body: %v", err)
	}
	var a dnsmessage.Message
	if err := a.Unpack(answer); err != nil || !a.Header.Response || a.Header.ID != q.Header.ID || a.Header.OpCode != q.Header.OpCode || len(a.Questions) != 1 || a.Questions[0] != q.Questions[0] {
		return nil, fmt.Errorf("invalid DoH answer")
	}
	return answer, nil
}

func (p *dnsProxy) answer(ctx context.Context, query []byte, udp bool) []byte {
	started := time.Now()
	a, err := p.exchange(ctx, query)
	if err != nil {
		var q dnsmessage.Message
		if q.Unpack(query) != nil || q.Header.Response {
			return nil
		}
		if p.log != nil {
			var name string
			var queryType dnsmessage.Type
			if len(q.Questions) == 1 {
				name, queryType = q.Questions[0].Name.String(), q.Questions[0].Type
			}
			p.log.Warn("tun_dns_query_failed", "name", name, "query_type", uint16(queryType), "duration_ms", time.Since(started).Milliseconds(), "error", err.Error())
		}
		a, _ = (&dnsmessage.Message{Header: dnsmessage.Header{ID: q.Header.ID, Response: true, RecursionDesired: q.Header.RecursionDesired, RecursionAvailable: true, RCode: dnsmessage.RCodeServerFailure}, Questions: q.Questions}).Pack()
	}
	// Respect the client's advertised UDP size; retry over TCP on truncation.
	if udp {
		limit := 512
		var q dnsmessage.Message
		if q.Unpack(query) == nil {
			for _, r := range q.Additionals {
				if r.Header.Type == dnsmessage.TypeOPT {
					limit = int(r.Header.Class)
					if limit < 512 {
						limit = 512
					}
					if limit > 4096 {
						limit = 4096
					}
				}
			}
		}
		if len(a) > limit {
			var m dnsmessage.Message
			if m.Unpack(a) != nil {
				return nil
			}
			m.Header.Truncated = true
			m.Answers, m.Authorities, m.Additionals = nil, nil, nil
			a, _ = m.Pack()
		}
	}
	return a
}

func (p *dnsProxy) serveUDP(ctx context.Context) {
	defer p.wg.Done()
	b := make([]byte, 65535)
	for {
		n, addr, err := p.udp.ReadFrom(b)
		if err != nil {
			return
		}
		select {
		case p.slots <- struct{}{}:
		default:
			continue
		}
		query := append([]byte(nil), b[:n]...)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { <-p.slots }()
			if a := p.answer(ctx, query, true); len(a) > 0 {
				_, _ = p.udp.WriteTo(a, addr)
			}
		}()
	}
}

func (p *dnsProxy) serveTCP(ctx context.Context) {
	defer p.wg.Done()
	for {
		c, err := p.tcp.Accept()
		if err != nil {
			return
		}
		select {
		case p.slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { <-p.slots }()
			defer c.Close()
			hook := context.AfterFunc(ctx, func() { c.Close() })
			defer hook()
			for {
				c.SetDeadline(time.Now().Add(20 * time.Second))
				var size [2]byte
				if _, err := io.ReadFull(c, size[:]); err != nil {
					return
				}
				q := make([]byte, int(binary.BigEndian.Uint16(size[:])))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				a := p.answer(ctx, q, false)
				if len(a) == 0 {
					return
				}
				binary.BigEndian.PutUint16(size[:], uint16(len(a)))
				wire := append(size[:], a...)
				if _, err := io.Copy(c, bytes.NewReader(wire)); err != nil {
					return
				}
			}
		}()
	}
}

func (p *dnsProxy) check(ctx context.Context) error {
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 1, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	wire, _ := q.Pack()
	// Health checks must exercise the transport, including after route takeover;
	// a cached response would hide an ICE routing loop.
	var a []byte
	for attempt := 1; ; attempt++ {
		var err error
		a, err = p.fetch(ctx, wire, q)
		if err == nil {
			break
		}
		// Retry one transient network failure on a fresh connection. Never
		// accept cached data or bypass SOCKS to make startup appear healthy.
		var networkError net.Error
		transient := (errors.As(err, &networkError) && networkError.Timeout()) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if attempt == 2 || !transient || ctx.Err() != nil {
			return fmt.Errorf("DNS through tunnel health check (attempt %d): %w", attempt, err)
		}
		p.transport.CloseIdleConnections()
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("DNS through tunnel health check: %w", ctx.Err())
		case <-timer.C:
		}
	}
	var answer dnsmessage.Message
	if answer.Unpack(a) != nil || answer.Header.RCode != dnsmessage.RCodeSuccess || len(answer.Answers) == 0 {
		return fmt.Errorf("DNS health check returned no successful answer")
	}
	return nil
}
