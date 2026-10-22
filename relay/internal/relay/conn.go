package relay

import (
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second // not scaled
	pingInterval   = 30 * time.Second // not scaled
	sendQueue      = 64               // frames; a client this far behind is dropped
	closeTakenOver = 4000             // a resume moved the participant to another connection
)

// Per-connection memory: net/http's hijacked 4 KiB bufio buffers are dropped
// (serveWS returns), the read buffer is 1 KiB, which holds a typical message,
// and write buffers come from a pool, so an idle connection holds none.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	WriteBufferPool: &sync.Pool{},
	CheckOrigin:     func(*http.Request) bool { return true },
}

// conn is one WebSocket connection: readPump reads and dispatches, writePump
// owns every data write.
type conn struct {
	srv       *Server
	ws        *websocket.Conn
	ip        ipGroups // the client address, as the per-IP limits count it
	send      chan []byte
	done      chan struct{}
	closeOnce sync.Once

	// Owned by the reading goroutine. The connection is in room while
	// peer.conn == c, which only room.mu can tell: another goroutine may have
	// kicked it, closed the room or resumed the participant elsewhere.
	room    *Room
	peer    *participant
	limiter tokenBucket
}

func (s *Server) serveWS(w http.ResponseWriter, req *http.Request) {
	addr, err := clientIP(req, s.cfg.TrustProxy)
	if err != nil {
		http.Error(w, "invalid client address", http.StatusBadRequest)
		return
	}
	ip := groupsOf(addr)
	switch s.limits.connect(ip) {
	case connPerIP:
		s.metrics.refusedPerIP.Add(1)
		http.Error(w, "too many connections from this address", http.StatusTooManyRequests)
		return
	case connTotal:
		s.metrics.refusedTotal.Add(1)
		http.Error(w, "relay is full", http.StatusServiceUnavailable)
		return
	}
	ws, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		s.limits.disconnect(ip)
		return // Upgrade has answered with an HTTP error
	}
	c := &conn{srv: s, ws: ws, ip: ip, send: make(chan []byte, sendQueue), done: make(chan struct{}),
		limiter: newTokenBucket(float64(s.cfg.Limits.MessageBurst), s.cfg.Limits.MessagesPerSecond, time.Now())}
	if !s.track(c) {
		s.limits.disconnect(ip)
		c.closeWith(websocket.CloseGoingAway, "")
		return
	}
	// Both pumps run in their own goroutines so the handler returns and net/http
	// frees its per-connection state (request, bufio buffers, the serve
	// goroutine's stack), as in gorilla's chat example.
	go c.writePump()
	go c.readPump()
}

func (c *conn) readPump() {
	defer c.srv.untrack(c)
	defer c.srv.limits.disconnect(c.ip)
	defer c.disconnect()
	defer c.close()
	defer func() {
		if v := recover(); v != nil {
			log.Printf("relay: internal error while handling a message: %v\n%s", v, debug.Stack())
		}
	}()
	c.ws.SetReadLimit(maxMessageBytes) // larger messages get close 1009
	// Any frame (data, ping or pong) keeps the connection for another pongWait.
	c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error { return c.ws.SetReadDeadline(time.Now().Add(pongWait)) })
	c.ws.SetPingHandler(func(data string) error {
		c.ws.SetReadDeadline(time.Now().Add(pongWait))
		c.ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeWait))
		return nil // a failed write shows up as a failed read
	})
	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		select {
		case <-c.done: // closed while the frame was read: taken over, too slow or invalid UTF-8
			return
		default:
		}
		c.ws.SetReadDeadline(time.Now().Add(pongWait))
		c.handleUnlessClosing(kind, data)
	}
}

func (c *conn) handleUnlessClosing(kind int, data []byte) {
	c.srv.inflight.RLock()
	defer c.srv.inflight.RUnlock()
	if !c.srv.closing.Load() { // shutting down: the state is saved, nothing may change it
		c.handle(kind, data)
	}
}

// pongMarker starts a queued pong: the marker byte, then the ping's id. The
// writer fills in serverTime as late as possible (SPEC.md §7.4). Encoded
// messages always start with '{'.
const pongMarker = 0

func (c *conn) writePump() {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	defer c.close()
	for {
		select {
		case msg := <-c.send:
			if msg[0] == pongMarker {
				msg = encode(pongMsg{"pong", msg[1:], c.srv.now()})
			}
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if c.ws.WriteMessage(websocket.TextMessage, msg) != nil {
				return
			}
		case <-ping.C:
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if c.ws.WriteMessage(websocket.PingMessage, nil) != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// enqueue never blocks, so it is safe under a room lock.
func (c *conn) enqueue(msg []byte) {
	select {
	case <-c.done:
	case c.send <- msg:
	default:
		c.close() // slow consumer
	}
}

func (c *conn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.ws.Close()
	})
}

// closeWith sends a close frame with code, then closes the socket. The write
// runs in its own goroutine, so this is safe under a room lock.
func (c *conn) closeWith(code int, text string) {
	c.closeOnce.Do(func() {
		close(c.done)
		go func() {
			c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text),
				time.Now().Add(time.Second))
			c.ws.Close()
		}()
	})
}

func (c *conn) reply(err *relayError, re *string) {
	c.srv.metrics.countError(err.code)
	c.enqueue(encode(errorMsg{"error", err.code, err.message, re}))
}

func (c *conn) handle(kind int, data []byte) {
	c.srv.metrics.messages.Add(1)
	if kind == websocket.TextMessage && !utf8.Valid(data) {
		// RFC 6455 §8.1: a text frame must be UTF-8. Clients fail on one, so
		// nothing invalid may pass through to them (SPEC.md §3.1).
		c.closeWith(websocket.CloseInvalidFramePayloadData, "")
		return
	}
	var f fields
	var typ string
	var re *string
	var dup, surrogate bool
	if kind == websocket.TextMessage {
		if f = parseObject(data); f != nil {
			var typeTwice bool
			typeTwice, dup = duplicateMembers(data)
			surrogate = hasLoneSurrogate(data)
			if t, ok := jsonString(f["type"]); ok && !typeTwice && !hasLoneSurrogate(f["type"]) {
				typ, re = t, &t
			}
		}
	}
	if !c.limiter.allow(time.Now()) {
		c.reply(&relayError{errRateLimited, "too many messages"}, re)
		return
	}
	// SPEC.md §6.2: envelope, type, then shape.
	var err *relayError
	switch {
	case kind != websocket.TextMessage:
		err = invalid("binary messages are not supported")
	case f == nil:
		err = invalid("a message must be one JSON object")
	case re == nil:
		err = invalid("type must be one string member")
	case !knownType(typ):
		err = &relayError{errUnsupported, "unknown message type"}
	case dup:
		err = invalid("an object names a member twice")
	case surrogate:
		err = invalid("a \\u escape holds an unpaired surrogate")
	default:
		err = c.dispatch(typ, f)
	}
	if err != nil {
		c.reply(err, re)
	}
}

func knownType(typ string) bool {
	switch typ {
	case "ping", "create", "join", "resume":
		return true
	}
	return roomCommands[typ]
}

func (c *conn) dispatch(typ string, f fields) *relayError {
	switch typ {
	case "ping":
		id := f["id"]
		if !isJSONNumber(id) && (len(id) == 0 || id[0] != '"') {
			return invalid("id must be a number or a string")
		}
		c.enqueue(append([]byte{pongMarker}, id...))
		return nil
	case "create", "join", "resume":
		return c.enter(typ, f)
	}
	cmd, err := parseCommand(typ, f)
	if err != nil {
		return err
	}
	r := c.room
	if r == nil {
		return &relayError{errNotInRoom, "join a room first"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.peer.conn != c {
		c.room, c.peer = nil, nil
		return &relayError{errNotInRoom, "join a room first"}
	}
	err = r.apply(c.peer, cmd, c.srv.now())
	r.flush()
	return err
}

func (c *conn) enter(typ string, f fields) *relayError {
	a := &args{f: f}
	var code, name, client, peerID, token string
	var create bool
	if typ == "resume" {
		code, peerID, token = a.str("code"), a.str("peerId"), a.str("token")
	} else {
		if typ == "join" {
			code, create = a.str("code"), a.optBoolean("create")
		}
		name, client = a.text("name", maxNameRunes), a.client()
	}
	if a.err != nil {
		return a.err
	}
	if c.inRoom() {
		return &relayError{errAlreadyInRoom, "leave the current room first"}
	}
	s := c.srv
	if typ != "resume" && s.blocked(client) {
		return &relayError{errClientBlocked, "this relay does not serve this app"}
	}
	switch typ {
	case "create":
		if !s.limits.holdRoom(c.ip) {
			return &relayError{errRateLimited, "too many rooms from this address"}
		}
		if !s.create(c, name, client) {
			s.limits.releaseRoom(c.ip)
			return &relayError{errRelayFull, "the relay holds its maximum number of rooms; create on another relay"}
		}
		s.metrics.created.Add(1)
	case "join":
		if !s.limits.lookup(c.ip, time.Now()) {
			return &relayError{errRateLimited, "too many joins from this address"}
		}
		made, err := s.join(c, code, name, client, create)
		if err != nil {
			return err
		}
		if made {
			s.metrics.created.Add(1)
		}
		s.metrics.joins.Add(1)
	case "resume":
		r := s.lookup(code)
		if r == nil {
			return &relayError{errResumeFailed, "unknown participant or wrong token"}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		p, err := r.resume(c, peerID, token, s.now())
		if err != nil {
			return err
		}
		client = p.client
		s.metrics.resumes.Add(1)
	}
	s.usage.add(clientName(client), c.ip[0], time.Now())
	return nil
}

func (c *conn) inRoom() bool {
	if c.room == nil {
		return false
	}
	c.room.mu.Lock()
	in := c.peer.conn == c
	c.room.mu.Unlock()
	if !in {
		c.room, c.peer = nil, nil
	}
	return in
}

// disconnect runs once the socket is gone.
func (c *conn) disconnect() {
	r := c.room
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.peer.conn == c {
		r.disconnect(c.peer, c.srv.now())
		r.flush()
	}
}

// blocked reports whether the deny list names client's app.
func (s *Server) blocked(client string) bool {
	deny := s.deny.Load()
	return deny != nil && (*deny)[asciiLower(clientName(client))]
}
