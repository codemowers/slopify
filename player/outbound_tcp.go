package main

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// tcpTuning holds the settings for outbound connections made by any playback
// backend through the shared transport. Zero leaves the system default,
// or no limit for DialTimeout.
type tcpTuning struct {
	DialTimeout       time.Duration
	UserTimeout       time.Duration // how long sent data may stay unacknowledged
	KeepAliveIdle     time.Duration
	KeepAliveInterval time.Duration
	KeepAliveCount    int
}

func (t tcpTuning) validate() error {
	if t.DialTimeout < 0 || t.UserTimeout < 0 || t.KeepAliveIdle < 0 || t.KeepAliveInterval < 0 || t.KeepAliveCount < 0 {
		return errors.New("tcp settings must not be negative")
	}
	if t.UserTimeout.Milliseconds() > math.MaxInt32 {
		return errors.New("tcp-user-timeout is too long")
	}
	if t.UserTimeout > 0 && t.UserTimeout < time.Millisecond {
		return errors.New("tcp-user-timeout must be at least 1ms")
	}
	for _, d := range []time.Duration{t.KeepAliveIdle, t.KeepAliveInterval} {
		if d > 0 && d < time.Second {
			return errors.New("tcp keepalive durations must be at least 1s")
		}
	}
	return nil
}

// keepAliveSetting maps zero to Go's negative value for "leave the system default".
func keepAliveSetting[T time.Duration | int](v T) T {
	if v == 0 {
		return -1
	}
	return v
}

func (t tcpTuning) dialer() *net.Dialer {
	return &net.Dialer{
		Timeout: t.DialTimeout,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     keepAliveSetting(t.KeepAliveIdle),
			Interval: keepAliveSetting(t.KeepAliveInterval),
			Count:    keepAliveSetting(t.KeepAliveCount),
		},
		Control: t.control,
	}
}

// configureOutboundHTTP applies the settings to the shared transport, and makes it
// probe idle HTTP/2 connections. Events, reports and media share one connection,
// so a path lost to a router restart would otherwise stall all of them until
// TCP gives up on retransmitting, about 15 minutes by default.
func configureOutboundHTTP(t tcpTuning) {
	transport := http.DefaultTransport.(*http.Transport)
	transport.DialContext = t.dialer().DialContext
	if metricsEnabled {
		transport.DialContext = outboundConns.dial(t.dialer())
		go outboundConns.sampleEvery(tcpSampleInterval)
	}
	transport.HTTP2 = &http.HTTP2Config{
		SendPingTimeout: 15 * time.Second,
		PingTimeout:     5 * time.Second,
	}
}

const tcpSampleInterval = 5 * time.Second

var (
	tcpRTTBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

	metricTCPDials = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_tcp_dials_total",
		Help: "Outbound connections attempted."}, []string{"source", "result"})
	metricTCPConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "slopify_tcp_connections",
		Help: "Open outbound connections."}, []string{"source"})
	metricTCPErrors = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_tcp_errors_total",
		Help: "Outbound connections ended by an error; timeout means unacknowledged data or failed keepalives."}, []string{"source", "reason"})
	metricTCPRetransmits = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_tcp_retransmits_total",
		Help: "Segments this speaker resent to the host (requests, reports): upload loss only. Download loss shows as out-of-order packets."}, []string{"source"})
	metricTCPOutOfOrder = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "slopify_tcp_out_of_order_packets_total",
		Help: "Packets from the host received out of order: download loss (or reordering). The only download-loss signal; a loss at the end of a burst is not seen."}, []string{"source"})
	metricTCPRTT = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "slopify_tcp_rtt_seconds",
		Help:    "The kernel's smoothed ACK round trip, sampled on connections with recent traffic.",
		Buckets: tcpRTTBuckets}, []string{"source"})
	metricTCPReceiveRTT = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "slopify_tcp_receive_rtt_seconds",
		Help:    "The kernel's receive-side round trip estimate, sampled on connections with recent traffic.",
		Buckets: tcpRTTBuckets}, []string{"source"})
)

func init() {
	nativeMetricsRegistry.MustRegister(metricTCPDials, metricTCPConnections, metricTCPErrors,
		metricTCPRetransmits, metricTCPOutOfOrder, metricTCPRTT, metricTCPReceiveRTT)
}

// tcpSample is one reading of a socket's TCP_INFO; the counters only increase.
type tcpSample struct {
	rtt, receiveRTT                   time.Duration
	retransmits, outOfOrder, segments uint32
}

// maxTCPHostLabels bounds the metric series for hosts not covered by a wildcard.
const maxTCPHostLabels = 16

// tcpHostWildcards are domains whose many hosts share one label, such as CDN nodes.
var tcpHostWildcards = []string{"spotify.com", "spotifycdn.com", "scdn.co"}

// tcpConnSet tracks open connections for sampling. The transport bounds its size.
type tcpConnSet struct {
	mu    sync.Mutex
	conns map[*trackedConn]struct{}
	hosts map[string]struct{}
}

func newTCPConnSet() *tcpConnSet {
	return &tcpConnSet{conns: map[*trackedConn]struct{}{}, hosts: map[string]struct{}{}}
}

// hostLabel names the dialled host, before resolution. Wildcard domains collapse
// to *.domain. Other hosts past the cap share "other"; seen ones keep their label.
func (s *tcpConnSet) hostLabel(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, domain := range tcpHostWildcards {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return "*." + domain
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.hosts[host]; !seen {
		if len(s.hosts) == maxTCPHostLabels {
			return "other"
		}
		s.hosts[host] = struct{}{}
	}
	return host
}

var outboundConns = newTCPConnSet()

func (s *tcpConnSet) dial(d *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if !metricsEnabled {
		return d.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host := s.hostLabel(address)
		conn, err := d.DialContext(ctx, network, address)
		if err != nil {
			metricTCPDials.WithLabelValues(host, "error").Inc()
			return nil, err
		}
		metricTCPDials.WithLabelValues(host, "ok").Inc()
		tracked := &trackedConn{Conn: conn, set: s, host: host}
		if sc, ok := conn.(syscall.Conn); ok {
			if raw, err := sc.SyscallConn(); err == nil {
				tracked.raw = raw
			}
		}
		s.mu.Lock()
		s.conns[tracked] = struct{}{}
		s.mu.Unlock()
		metricTCPConnections.WithLabelValues(host).Inc()
		return tracked, nil
	}
}

func (s *tcpConnSet) sampleEvery(interval time.Duration) {
	for range time.Tick(interval) {
		s.mu.Lock()
		conns := make([]*trackedConn, 0, len(s.conns))
		for c := range s.conns {
			conns = append(conns, c)
		}
		s.mu.Unlock()
		for _, c := range conns {
			c.sample()
		}
	}
}

// trackedConn observes one connection without changing what passes through it.
type trackedConn struct {
	net.Conn
	set  *tcpConnSet
	host string
	raw  syscall.RawConn

	mu   sync.Mutex
	last tcpSample
	once sync.Once
}

func (c *trackedConn) sample() {
	if c.raw == nil {
		return
	}
	now, ok := readTCPSample(c.raw)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	metricTCPRetransmits.WithLabelValues(c.host).Add(float64(now.retransmits - c.last.retransmits))
	metricTCPOutOfOrder.WithLabelValues(c.host).Add(float64(now.outOfOrder - c.last.outOfOrder))
	if now.segments != c.last.segments { // An idle socket only repeats its old estimate.
		if now.rtt > 0 {
			metricTCPRTT.WithLabelValues(c.host).Observe(now.rtt.Seconds())
		}
		if now.receiveRTT > 0 {
			metricTCPReceiveRTT.WithLabelValues(c.host).Observe(now.receiveRTT.Seconds())
		}
	}
	c.last = now
}

func (c *trackedConn) observe(err error) {
	if reason := tcpErrorReason(err); reason != "" {
		metricTCPErrors.WithLabelValues(c.host, reason).Inc()
	}
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.observe(err)
	return n, err
}

func (c *trackedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.observe(err)
	return n, err
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.sample() // The counters are gone once the socket is.
		c.set.mu.Lock()
		delete(c.set.conns, c)
		c.set.mu.Unlock()
		metricTCPConnections.WithLabelValues(c.host).Dec()
	})
	return c.Conn.Close()
}

// tcpErrorReason gives a bounded metric label, empty for an ordinary ending.
func tcpErrorReason(err error) string {
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed), errors.Is(err, os.ErrDeadlineExceeded):
		return ""
	case errors.Is(err, syscall.ETIMEDOUT):
		return "timeout"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	}
	return "other"
}
