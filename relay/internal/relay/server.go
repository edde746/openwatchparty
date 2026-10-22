// Package relay is the Open Watch Party relay: rooms, their playback state
// machine, limits, persistence and the HTTP endpoints (spec/SPEC.md).
package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Config is the relay's configuration (relay/README.md lists the flags).
type Config struct {
	ID           string
	Prefixes     string // first characters this relay creates codes with; "" means all
	DenylistPath string
	StateDir     string // "" disables persistence
	TrustProxy   bool
	Limits       Limits
	TimerScale   float64
}

const (
	denyPollInterval = time.Second
	sweepInterval    = time.Minute
	shutdownWait     = 2 * time.Second
)

// Server holds the live rooms and connections. Lock order: Room.mu, then
// Server.mu, which is held only for map access and the room slots that go
// with a join's insertion and a removal (claim, removeRoom). Every other
// mutex (limits, store, usage, conns) is a leaf.
type Server struct {
	cfg      Config
	prefixes string
	start    time.Time

	limits   *ipLimits
	metrics  metrics
	usage    *usage
	store    *store // nil without a state directory
	deny     atomic.Pointer[map[string]bool]
	closing  atomic.Bool
	inflight sync.RWMutex // read-held while a message is handled; Shutdown write-locks it

	denyFile watchedFile // only New and then pollLoop touch it

	mu    sync.Mutex
	rooms map[string]*Room

	connMu sync.Mutex
	conns  map[*conn]struct{}

	stop chan struct{}
	done sync.WaitGroup
}

// New validates cfg, loads the deny list and restores saved rooms. The
// background loops start with Run.
func New(cfg Config) (*Server, error) {
	if !validRelayID(cfg.ID) {
		return nil, errors.New("--id must be 1-32 characters of a-z, 0-9 and -, starting with a letter or digit")
	}
	if !(cfg.TimerScale > 0) || math.IsInf(cfg.TimerScale, 0) {
		return nil, errors.New("--timer-scale must be a positive number")
	}
	if err := cfg.Limits.check(); err != nil {
		return nil, err
	}
	prefixes := codeAlphabet
	if cfg.Prefixes != "" {
		p, err := normalizePrefixes(cfg.Prefixes)
		if err != nil {
			return nil, fmt.Errorf("--prefixes: %v", err)
		}
		prefixes = p
	}
	s := &Server{cfg: cfg, prefixes: prefixes, start: time.Now(), limits: newIPLimits(cfg.Limits),
		rooms: make(map[string]*Room), conns: make(map[*conn]struct{}), stop: make(chan struct{}),
		denyFile: watchedFile{path: cfg.DenylistPath}}
	s.reloadDeny()
	if cfg.StateDir == "" {
		s.usage = newUsage("")
		return s, nil
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("--state-dir: %v", err)
	}
	s.usage = newUsage(filepath.Join(cfg.StateDir, usageFile))
	if err := s.usage.load(time.Now()); err != nil {
		log.Printf("relay: usage counts not loaded, starting from zero: %v", err)
	}
	s.store = newStore(s, filepath.Join(cfg.StateDir, roomsFile))
	s.restore()
	return s, nil
}

// Run starts the background loops: deny-list reloads, limit sweeps and state
// writes. Shutdown stops them.
func (s *Server) Run() {
	s.done.Add(1)
	go s.pollLoop()
	if s.store != nil {
		s.done.Add(1)
		go s.store.run()
	}
}

// now is relay time: integer milliseconds since start, monotonic.
func (s *Server) now() int64 { return time.Since(s.start).Milliseconds() }

// scaledMs applies --timer-scale to a policy duration, rounded down to whole
// milliseconds (SPEC.md §13.3) and capped at 2^53 − 1, so it stays an exact
// JSON integer and relay time plus it can't overflow.
func (s *Server) scaledMs(ms int64) int64 {
	return int64(min(math.Floor(float64(ms)*s.cfg.TimerScale), maxSafeInteger))
}

// lookup finds the room a received `join` or `resume` code names: the code
// normalized, then matched exactly (SPEC.md §5.1).
func (s *Server) lookup(code string) *Room {
	code = normalizeCode(code)
	if !validCode(code) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rooms[code]
}

// normalizeCode removes the separators of SPEC.md §5.1 from a received code
// and upper-cases ASCII a-z. Every other character stays as it is.
func normalizeCode(code string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'z':
			return r - 'a' + 'A'
		case isCodeSeparator(r):
			return -1
		}
		return r
	}, code)
}

// isCodeSeparator reports whether r is one of the characters SPEC.md §5.1
// removes from a received code: spaces, line breaks, hyphens, dashes and the
// minus sign.
func isCodeSeparator(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\u00A0', '\u2007', '\u202F',
		'-', '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2212':
		return true
	}
	return false
}

// create makes a room with a new code and c as its host, unless the relay
// already holds maxRooms rooms.
func (s *Server) create(c *conn, name, client string) bool {
	r := newRoom(s, "", s.now())
	r.owner = c.ip
	r.mu.Lock() // before the room becomes visible to join
	defer r.mu.Unlock()
	s.mu.Lock()
	if len(s.rooms) >= s.cfg.Limits.MaxRooms {
		s.mu.Unlock()
		return false
	}
	for r.code == "" || s.rooms[r.code] != nil {
		r.code = newRoomCode(s.prefixes)
	}
	s.rooms[r.code] = r
	s.mu.Unlock()
	r.open(c, name, client)
	return true
}

// join adds c to the room that code names (SPEC.md §7.2). With create, a
// code that names no room gets a new room with c as its host, and made
// reports that. A room that closed after the lookup has left the map, so its
// code is free and a join with create looks again.
func (s *Server) join(c *conn, code, name, client string, create bool) (made bool, err *relayError) {
	for {
		var r *Room
		if create {
			if r, err = s.lookupOrMake(c, code, name, client); r == nil {
				return err == nil, err
			}
		} else if r = s.lookup(code); r == nil {
			return false, &relayError{errRoomNotFound, "no room has that code"}
		}
		r.mu.Lock()
		if !create || !r.closed {
			err = r.join(c, name, client, s.now())
			r.mu.Unlock()
			return false, err
		}
		r.mu.Unlock()
	}
}

// lookupOrMake finds the room that code names, as lookup does, or else makes
// a room with that code and c as its host. It returns the room it found, or
// nil after making one or with the refusal.
func (s *Server) lookupOrMake(c *conn, code, name, client string) (*Room, *relayError) {
	code = normalizeCode(code)
	if !validCode(code) {
		return nil, &relayError{errRoomNotFound, "no room has that code"}
	}
	r := newRoom(s, code, s.now())
	r.owner = c.ip
	r.mu.Lock() // before the room becomes visible to join
	defer r.mu.Unlock()
	if found, err := s.claim(r); found != nil || err != nil {
		return found, err
	}
	r.open(c, name, client)
	return nil, nil
}

// claim puts the new room r in the map under its code, unless a room holds
// that code already, which it returns, or a refusal of SPEC.md §7.2 applies.
// The lookup and the insertion share one hold of s.mu, so of two joins with
// create for a free code one makes the room and the other finds it.
func (s *Server) claim(r *Room) (*Room, *relayError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if found := s.rooms[r.code]; found != nil {
		return found, nil
	}
	switch {
	case !containsByte(s.prefixes, r.code[0]):
		return nil, &relayError{errRoomNotFound, "no room has that code"}
	case !s.limits.holdRoom(r.owner):
		return nil, &relayError{errRateLimited, "too many rooms from this address"}
	case len(s.rooms) >= s.cfg.Limits.MaxRooms:
		s.limits.releaseRoom(r.owner)
		return nil, &relayError{errRelayFull, "the relay holds its maximum number of rooms"}
	}
	s.rooms[r.code] = r
	return nil, nil
}

// removeRoom forgets a closed room; the caller holds r.mu. The room leaves
// the map and gives back its creator's slot in one hold of s.mu, so claim
// never sees the code free while the slot is still taken.
func (s *Server) removeRoom(r *Room) {
	s.mu.Lock()
	if s.rooms[r.code] == r {
		delete(s.rooms, r.code)
	}
	s.limits.releaseRoom(r.owner) // a restored room's owner counts nowhere
	s.mu.Unlock()
	s.saveLater()
}

func (s *Server) roomList() []*Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	rooms := make([]*Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	return rooms
}

// saveLater asks the state writer for a write. It never blocks.
func (s *Server) saveLater() {
	if s.store != nil {
		s.store.mark()
	}
}

// newRoomCode draws a code whose first character is one of prefixes and whose
// other characters come from the whole alphabet, all uniformly.
func newRoomCode(prefixes string) string {
	code := make([]byte, codeLength)
	code[0] = prefixes[uniform(len(prefixes))]
	for i := 1; i < codeLength; i++ {
		code[i] = codeAlphabet[uniform(len(codeAlphabet))]
	}
	return string(code)
}

// uniform returns a cryptographically random int in [0, n) for n ≤ 256.
func uniform(n int) int {
	limit := 256 - 256%n // rejecting bytes at or above limit keeps every value equally likely
	var b [1]byte
	for {
		rand.Read(b[:])
		if int(b[0]) < limit {
			return int(b[0]) % n
		}
	}
}

func newToken() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (s *Server) track(c *conn) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.closing.Load() {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c *conn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	delete(s.conns, c)
}

// Deny list.

func (s *Server) pollLoop() {
	defer s.done.Done()
	poll := time.NewTicker(denyPollInterval)
	defer poll.Stop()
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-poll.C:
			s.reloadDeny()
		case now := <-sweep.C:
			s.limits.sweep(now)
			s.usage.sweep(now)
		case <-s.stop:
			return
		}
	}
}

// reloadDeny reads the deny list if it changed; a missing file is an empty
// list. Only New and pollLoop call it.
func (s *Server) reloadDeny() {
	if s.cfg.DenylistPath == "" {
		return
	}
	data, changed, err := s.denyFile.read()
	if err != nil {
		log.Printf("relay: --denylist: %v; keeping the previous deny list", err)
		return
	}
	if changed || s.deny.Load() == nil {
		deny := parseDenyList(data)
		s.deny.Store(&deny)
	}
}

// HTTP.

// Handler serves the public endpoints: /v1/ws, /v1/info and /health.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ws", s.serveWS)
	mux.HandleFunc("GET /v1/info", s.serveInfo)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok")
	})
	return mux
}

// AdmitPending counts a socket from addr that the relay's own TLS listener
// accepted and that is not a WebSocket yet. It refuses one beyond addr's
// WebSocket allowance (MaxConnectionsPerIP times the group's factor, SPEC.md
// §3.1), so that one address can't hold every pending slot; the listener
// closes it at once. Call release exactly once, when the socket upgrades or closes.
// Behind a proxy (TrustProxy) the socket's address is the proxy's, and every
// socket is admitted.
func (s *Server) AdmitPending(addr netip.Addr) (release func(), ok bool) {
	if s.cfg.TrustProxy {
		return func() {}, true
	}
	g := groupsOf(addr)
	if !s.limits.pend(g) {
		s.metrics.refusedPerIP.Add(1)
		return nil, false
	}
	return func() { s.limits.unpend(g) }, true
}

type infoLimits struct {
	MaxParticipants     int     `json:"maxParticipants"`
	MaxMessageBytes     int     `json:"maxMessageBytes"`
	MaxMediaBytes       int     `json:"maxMediaBytes"`
	MaxExtBytes         int     `json:"maxExtBytes"`
	MaxNameLength       int     `json:"maxNameLength"`
	MaxClientLength     int     `json:"maxClientLength"`
	MinRate             float64 `json:"minRate"`
	MaxRate             float64 `json:"maxRate"`
	MaxConnectionsPerIP int     `json:"maxConnectionsPerIp"`
	MaxRoomsPerIP       int     `json:"maxRoomsPerIp"`
	MessageBurst        int     `json:"messageBurst"`
	MessagesPerSecond   float64 `json:"messagesPerSecond"`
	JoinBurst           int     `json:"joinBurst"`
	JoinsPerSecond      float64 `json:"joinsPerSecond"`
	MaxConnections      int     `json:"maxConnections"`
	MaxRooms            int     `json:"maxRooms"`
	IPv6Factor56        int     `json:"ipv6Factor56"`
	IPv6Factor48        int     `json:"ipv6Factor48"`
	JoinBurstTotal      int     `json:"joinBurstTotal"`
	JoinsPerSecondTotal float64 `json:"joinsPerSecondTotal"`
}

type infoPolicy struct {
	StallGraceMs        int64   `json:"stallGraceMs"`
	StallReleaseMs      int64   `json:"stallReleaseMs"`
	WaitMs              int64   `json:"waitMs"`
	MediaWaitMs         int64   `json:"mediaWaitMs"`
	ExclusionMs         int64   `json:"exclusionMs"`
	StartDelayMinMs     int64   `json:"startDelayMinMs"`
	StartDelayMaxMs     int64   `json:"startDelayMaxMs"`
	StartDelayRttFactor float64 `json:"startDelayRttFactor"`
	DefaultRttMs        int64   `json:"defaultRttMs"`
	SeekCoalesceMs      int64   `json:"seekCoalesceMs"`
	PauseToleranceMs    int64   `json:"pauseToleranceMs"`
	HostPromotionMs     int64   `json:"hostPromotionMs"`
	ResumeMs            int64   `json:"resumeMs"`
	MaxRoomAgeMs        int64   `json:"maxRoomAgeMs"`
}

type info struct {
	Protocol int        `json:"protocol"`
	ID       string     `json:"id"`
	Limits   infoLimits `json:"limits"`
	Policy   infoPolicy `json:"policy"`
}

func (s *Server) serveInfo(w http.ResponseWriter, _ *http.Request) {
	l := s.cfg.Limits
	body := info{
		Protocol: 1,
		ID:       s.cfg.ID,
		Limits: infoLimits{
			MaxParticipants: l.MaxParticipants, MaxMessageBytes: maxMessageBytes, MaxMediaBytes: maxMediaBytes,
			MaxExtBytes: maxExtBytes, MaxNameLength: maxNameRunes, MaxClientLength: maxClientRunes,
			MinRate: minRate, MaxRate: maxRate, MaxConnectionsPerIP: l.MaxConnectionsPerIP,
			MaxRoomsPerIP: l.MaxRoomsPerIP, MessageBurst: l.MessageBurst, MessagesPerSecond: l.MessagesPerSecond,
			JoinBurst: l.JoinBurst, JoinsPerSecond: l.JoinsPerSecond, MaxConnections: l.MaxConnections,
			MaxRooms: l.MaxRooms, IPv6Factor56: l.IPv6Factor56, IPv6Factor48: l.IPv6Factor48,
			JoinBurstTotal: l.JoinBurstTotal, JoinsPerSecondTotal: l.JoinsPerSecondTotal,
		},
		Policy: infoPolicy{
			StallGraceMs: s.scaledMs(stallGraceMs), StallReleaseMs: s.scaledMs(stallReleaseMs),
			WaitMs: s.scaledMs(waitMs), MediaWaitMs: s.scaledMs(mediaWaitMs), ExclusionMs: s.scaledMs(exclusionMs),
			StartDelayMinMs: s.scaledMs(startDelayMinMs), StartDelayMaxMs: s.scaledMs(startDelayMaxMs),
			StartDelayRttFactor: startDelayRttRate, DefaultRttMs: defaultRttMs,
			SeekCoalesceMs: s.scaledMs(seekCoalesceMs), PauseToleranceMs: pauseToleranceMs,
			HostPromotionMs: s.scaledMs(hostPromotionMs), ResumeMs: s.scaledMs(resumeMs),
			MaxRoomAgeMs: s.scaledMs(maxRoomAgeMs),
		},
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(encode(body))
}

// AdminHandler serves /metrics and /admin/usage.
func (s *Server) AdminHandler(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.writeMetrics(w, version)
	})
	mux.HandleFunc("GET /admin/usage", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(encode(struct {
			ID   string                      `json:"id"`
			Days map[string]map[string]int64 `json:"days"`
		}{s.cfg.ID, s.usage.days()}))
	})
	return mux
}

// Shutdown saves every room, closes every connection with 1001 and stops the
// background loops. Call it after the HTTP servers stopped accepting.
func (s *Server) Shutdown(ctx context.Context) {
	s.connMu.Lock()
	s.closing.Store(true) // readers drop further frames; no new connection is tracked
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.connMu.Unlock()
	s.inflight.Lock() // waits for messages already being handled
	s.inflight.Unlock()

	close(s.stop)
	s.done.Wait()
	if s.store != nil {
		s.store.save()
	}
	if err := s.usage.save(); err != nil {
		log.Printf("relay: usage counts not saved: %v", err)
	}

	for _, c := range conns {
		c.closeWith(websocket.CloseGoingAway, "relay shutting down")
	}
	deadline := time.NewTimer(shutdownWait)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		s.connMu.Lock()
		n := len(s.conns)
		s.connMu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return
		case <-ctx.Done():
			return
		}
	}
}
