// Command owp-relay runs the Open Watch Party relay. Every flag has an
// environment variable; relay/README.md describes them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/edde746/openwatchparty/relay/internal/relay"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg, l, err := parseConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "owp-relay:", err)
		os.Exit(2)
	}
	if err := run(cfg, l); err != nil {
		log.Fatalf("owp-relay: %v", err)
	}
}

// listen is how the relay serves: its listeners and, with tlsDomains, the
// certificates of the public one.
type listen struct {
	addr, adminAddr string
	tlsDomains      []string // empty: plain HTTP on addr
	acmeDirectory   string
	acmeEmail       string
}

func parseConfig(argv []string) (cfg relay.Config, l listen, err error) {
	fs := flag.NewFlagSet("owp-relay", flag.ContinueOnError)
	envErr := error(nil)
	str := func(name, env, def, usage string) *string {
		if v, ok := os.LookupEnv(env); ok {
			def = v
		}
		return fs.String(name, def, usage+" ($"+env+")")
	}
	boolean := func(name, env string, def bool, usage string) *bool {
		if v, ok := os.LookupEnv(env); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				envErr = fmt.Errorf("$%s: %q is not a boolean", env, v)
			}
			def = b
		}
		return fs.Bool(name, def, usage+" ($"+env+")")
	}
	integer := func(name, env string, def int, usage string) *int {
		if v, ok := os.LookupEnv(env); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				envErr = fmt.Errorf("$%s: %q is not an integer", env, v)
			}
			def = n
		}
		return fs.Int(name, def, usage+" ($"+env+")")
	}
	float := func(name, env string, def float64, usage string) *float64 {
		if v, ok := os.LookupEnv(env); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				envErr = fmt.Errorf("$%s: %q is not a number", env, v)
			}
			def = f
		}
		return fs.Float64(name, def, usage+" ($"+env+")")
	}

	a := str("addr", "OWP_ADDR", ":8080", "public listener: /v1/ws, /v1/info, /health")
	admin := str("admin-addr", "OWP_ADMIN_ADDR", "127.0.0.1:9090",
		"admin listener: /metrics, /admin/usage; empty disables it")
	id := str("id", "OWP_ID", "local", "this relay's ID")
	prefixes := str("prefixes", "OWP_PREFIXES", "", "first characters this relay creates codes with (default all 31)")
	deny := str("denylist", "OWP_DENYLIST", "", "client app deny list file, re-read on change")
	stateDir := str("state-dir", "OWP_STATE_DIR", "", "directory for room snapshots and usage counts (default: none)")
	trust := boolean("trust-proxy", "OWP_TRUST_PROXY", false, "take the client IP from the first X-Forwarded-For hop")
	d := relay.DefaultLimits
	maxConns := integer("max-connections", "OWP_MAX_CONNECTIONS", d.MaxConnections, "total WebSocket connections")
	maxParticipants := integer("max-participants", "OWP_MAX_PARTICIPANTS", d.MaxParticipants,
		"participants per room, disconnected ones included; at most 32")
	connsPerIP := integer("max-connections-per-ip", "OWP_MAX_CONNECTIONS_PER_IP", d.MaxConnectionsPerIP,
		"WebSocket connections per client IP or IPv6 /64")
	maxRooms := integer("max-rooms", "OWP_MAX_ROOMS", d.MaxRooms, "rooms the relay holds, restored ones included")
	roomsPerIP := integer("max-rooms-per-ip", "OWP_MAX_ROOMS_PER_IP", d.MaxRoomsPerIP,
		"rooms a client IP or IPv6 /64 created that the relay still holds")
	factor56 := integer("ipv6-factor-56", "OWP_IPV6_FACTOR_56", d.IPv6Factor56,
		"per-IP limits multiplier for an IPv6 /56")
	factor48 := integer("ipv6-factor-48", "OWP_IPV6_FACTOR_48", d.IPv6Factor48,
		"per-IP limits multiplier for an IPv6 /48")
	msgBurst := integer("message-burst", "OWP_MESSAGE_BURST", d.MessageBurst, "per-connection message bucket size")
	msgRate := float("messages-per-second", "OWP_MESSAGES_PER_SECOND", d.MessagesPerSecond,
		"per-connection message refill per second")
	joinBurst := integer("join-burst", "OWP_JOIN_BURST", d.JoinBurst,
		"per-IP join bucket size")
	joinRate := float("joins-per-second", "OWP_JOINS_PER_SECOND", d.JoinsPerSecond, "per-IP join refill per second")
	joinBurstTotal := integer("join-burst-total", "OWP_JOIN_BURST_TOTAL", d.JoinBurstTotal,
		"relay-wide join bucket size")
	joinRateTotal := float("joins-per-second-total", "OWP_JOINS_PER_SECOND_TOTAL", d.JoinsPerSecondTotal,
		"relay-wide join refill per second")
	scale := float("timer-scale", "OWP_TIMER_SCALE", 1.0, "test mode: multiplies every relay policy duration")
	tlsDomains := str("tls-domains", "OWP_TLS_DOMAINS", "",
		"comma-separated hostnames: serve TLS on --addr with ACME certificates for them (needs --state-dir)")
	acmeDirectory := str("acme-directory", "OWP_ACME_DIRECTORY", acme.LetsEncryptURL, "ACME directory URL")
	acmeEmail := str("acme-email", "OWP_ACME_EMAIL", "", "ACME account contact (default: none)")
	if envErr != nil {
		return cfg, l, envErr
	}
	if err := fs.Parse(argv); err != nil {
		return cfg, l, err
	}
	if fs.NArg() > 0 {
		return cfg, l, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	cfg = relay.Config{ID: *id, Prefixes: *prefixes, DenylistPath: *deny, StateDir: *stateDir, TrustProxy: *trust,
		TimerScale: *scale,
		Limits: relay.Limits{MaxParticipants: *maxParticipants, MaxConnections: *maxConns,
			MaxConnectionsPerIP: *connsPerIP, MaxRooms: *maxRooms, MaxRoomsPerIP: *roomsPerIP, IPv6Factor56: *factor56,
			IPv6Factor48: *factor48, MessageBurst: *msgBurst, MessagesPerSecond: *msgRate, JoinBurst: *joinBurst, JoinsPerSecond: *joinRate,
			JoinBurstTotal: *joinBurstTotal, JoinsPerSecondTotal: *joinRateTotal}}
	l = listen{addr: *a, adminAddr: *admin}
	if *tlsDomains == "" {
		return cfg, l, nil
	}
	if *stateDir == "" {
		return cfg, l, errors.New("--tls-domains needs --state-dir: the certificates are kept in <state-dir>/acme")
	}
	for name := range strings.SplitSeq(*tlsDomains, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if !validHostname(name) {
			return cfg, l, fmt.Errorf("--tls-domains: %q is not a hostname (ASCII, at least two labels, no IP address)", name)
		}
		l.tlsDomains = append(l.tlsDomains, name)
	}
	if u, err := url.Parse(*acmeDirectory); err != nil || u.Scheme != "https" || u.Host == "" {
		return cfg, l, errors.New("--acme-directory must be an https:// URL")
	}
	l.acmeDirectory, l.acmeEmail = *acmeDirectory, *acmeEmail
	return cfg, l, nil
}

// validHostname reports whether name is a DNS name ACME can issue a
// certificate for over TLS-ALPN-01: lower-case ASCII labels (xn-- form for
// others), at least two, no wildcard, not an IPv4 address.
func validHostname(name string) bool {
	labels := strings.Split(name, ".")
	if len(name) > 253 || len(labels) < 2 || strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range []byte(label) {
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func run(cfg relay.Config, l listen) error {
	srv, err := relay.New(cfg)
	if err != nil {
		return err
	}
	errorLog := newErrorLog(os.Stderr)
	public := httpServer(srv.Handler(), errorLog, readTimeout, idleTimeout)
	var admin *http.Server
	served := make(chan error, 2)
	if l.adminAddr != "" {
		ln, err := net.Listen("tcp", l.adminAddr)
		if err != nil {
			return fmt.Errorf("--admin-addr: %v", err)
		}
		admin = httpServer(srv.AdminHandler(version), errorLog, readTimeout, idleTimeout)
		go func() { served <- admin.Serve(ln) }()
	}
	ln, err := net.Listen("tcp", l.addr)
	if err != nil {
		return fmt.Errorf("--addr: %v", err)
	}
	var certs *autocert.Manager
	if len(l.tlsDomains) > 0 {
		certs = certManager(l, cfg.StateDir)
		ln = listenTLS(public, ln, tlsConfig(certs.GetCertificate), maxPending, srv.AdmitPending)
	}
	srv.Run()
	go func() { served <- public.Serve(ln) }()
	fmt.Println("listening", ln.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if certs != nil {
		go checkCertificates(ctx, certs, l.tlsDomains, certCheckInterval)
	}
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	public.Shutdown(shutdown) // stops accepting; upgraded connections are the relay's to close
	if admin != nil {
		admin.Shutdown(shutdown)
	}
	srv.Shutdown(shutdown)
	return nil
}

// newErrorLog is net/http's error log. It can name client addresses ("panic
// serving 1.2.3.4:…"); only accept errors, which name the listener, pass.
func newErrorLog(w io.Writer) *log.Logger {
	return log.New(acceptErrorsOnly{w}, "", log.LstdFlags|log.LUTC)
}

// acceptErrorsOnly passes net/http log lines about failing accepts and drops
// the rest, which may carry client addresses.
type acceptErrorsOnly struct{ w io.Writer }

func (a acceptErrorsOnly) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "http: Accept error") {
		return a.w.Write(p)
	}
	return len(p), nil
}

const (
	readTimeout = 10 * time.Second // a whole request, headers included
	idleTimeout = 60 * time.Second // a keep-alive connection between requests
)

// httpServer bounds every plain HTTP connection: a request must arrive within
// read, its response must be written within read, so a client that stops
// reading can't hold the connection, and an idle keep-alive connection closes
// after idle. An upgraded WebSocket is not affected: gorilla clears the
// deadlines when it takes the connection over, and the relay sets its own.
func httpServer(h http.Handler, errorLog *log.Logger, read, idle time.Duration) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: read, ReadTimeout: read, WriteTimeout: read,
		IdleTimeout: idle, MaxHeaderBytes: 16 << 10, ErrorLog: errorLog}
}
