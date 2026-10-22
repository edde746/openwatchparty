package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests drive the relay through its public handler with real WebSocket
// clients, so they check what a client sees. The conformance suite
// (conformance/relay) checks the whole of SPEC.md against the built binary.

type msg = map[string]any

const movie = `{"kind":"movie","title":"Test","durationMs":7200000}`

func testConfig() Config {
	return Config{ID: "test", TimerScale: 1, Limits: DefaultLimits}
}

type testRelay struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	ips atomic.Int64
}

// startRelay runs a relay with cfg until the test ends.
func startRelay(t *testing.T, cfg Config) *testRelay {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.Run()
	tr := &testRelay{t: t, srv: srv, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(tr.stop)
	return tr
}

// stop shuts the relay down as SIGTERM does: rooms saved, connections closed.
func (tr *testRelay) stop() {
	if tr.ts == nil {
		return
	}
	tr.ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr.srv.Shutdown(ctx)
	tr.ts = nil
}

func (tr *testRelay) url() string { return "ws" + strings.TrimPrefix(tr.ts.URL, "http") + "/v1/ws" }

type client struct {
	t  *testing.T
	ws *websocket.Conn

	code, peerID, token string
	last                msg // the latest room snapshot seen
}

// dial connects; with TrustProxy every dial comes from its own address.
func (tr *testRelay) dial() *client {
	tr.t.Helper()
	h := http.Header{}
	if tr.srv.cfg.TrustProxy {
		h.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", tr.ips.Add(1)))
	}
	c, resp, err := tr.dialWith(h)
	if err != nil {
		tr.t.Fatalf("dial: %v (%v)", err, resp)
	}
	return c
}

func (tr *testRelay) dialWith(h http.Header) (*client, *http.Response, error) {
	ws, resp, err := websocket.DefaultDialer.Dial(tr.url(), h)
	if err != nil {
		return nil, resp, err
	}
	tr.t.Cleanup(func() { ws.Close() })
	return &client{t: tr.t, ws: ws}, resp, nil
}

func (c *client) send(m msg) {
	c.t.Helper()
	if err := c.ws.WriteJSON(m); err != nil {
		c.t.Fatalf("send %v: %v", m["type"], err)
	}
}

// next reads one message; a close or timeout fails the test.
func (c *client) next() msg {
	c.t.Helper()
	m, err := c.read(2 * time.Second)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return m
}

func (c *client) read(wait time.Duration) (msg, error) {
	c.ws.SetReadDeadline(time.Now().Add(wait))
	_, data, err := c.ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	var m msg
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	switch m["type"] {
	case "room": // the snapshot's fields beside `type`
		c.last = m
	case "joined":
		c.last = m["room"].(msg)
		c.code = m["code"].(string)
		you := m["you"].(msg)
		c.peerID, c.token = you["peerId"].(string), you["token"].(string)
	}
	return m, nil
}

// expect reads up to the next message of type typ. Snapshots before it are
// skipped unless typ is "room"; any other message fails the test.
func (c *client) expect(typ string) msg {
	c.t.Helper()
	for {
		m := c.next()
		if m["type"] == typ {
			return m
		}
		if m["type"] != "room" {
			c.t.Fatalf("got %v, want a %q message", m, typ)
		}
	}
}

// expectError reads up to the next non-snapshot message and requires an
// error with code.
func (c *client) expectError(code string) {
	c.t.Helper()
	if m := c.expect("error"); m["code"] != code {
		c.t.Fatalf("got error %v, want %q", m, code)
	}
}

// expectMedia reads up to the next snapshot that carries media: the one that
// starts a new epoch.
func (c *client) expectMedia() msg {
	c.t.Helper()
	for {
		if m := c.expect("room"); hasKey(m, "media") {
			return m
		}
	}
}

func hasKey(m msg, key string) bool {
	_, ok := m[key]
	return ok
}

// until reads messages until a snapshot satisfies ok and returns it.
func (c *client) until(what string, ok func(room msg) bool) msg {
	c.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, err := c.read(time.Until(deadline))
		if err != nil {
			c.t.Fatalf("waiting for %s: %v (last room %v)", what, err, c.last)
		}
		if t := m["type"]; (t == "room" || t == "joined") && ok(c.last) {
			return c.last
		}
	}
}

// closeCode waits for the relay to close the connection and returns the code.
func (c *client) closeCode() int {
	c.t.Helper()
	for {
		_, err := c.read(3 * time.Second)
		if ce, ok := err.(*websocket.CloseError); ok {
			return ce.Code
		}
		if err != nil {
			c.t.Fatalf("waiting for a close: %v", err)
		}
	}
}

func (c *client) create(name, app string) {
	c.t.Helper()
	c.send(msg{"type": "create", "name": name, "client": app})
	c.expect("joined")
}

func (c *client) join(code, name string) {
	c.t.Helper()
	c.send(msg{"type": "join", "code": code, "name": name, "client": "Test/1.0"})
	c.expect("joined")
}

// joinCreate is a join with create true.
func joinCreate(code, name string) msg {
	return msg{"type": "join", "code": code, "name": name, "client": "Test/1.0", "create": true}
}

func (c *client) status(epoch int, player string) {
	c.t.Helper()
	c.send(msg{"type": "status", "epoch": epoch, "player": player, "match": nil, "offsetMs": 0})
}

// setMedia sends the descriptor; a negative positionMs is left out.
func (c *client) setMedia(media string, positionMs int) {
	c.t.Helper()
	m := msg{"type": "setMedia", "media": json.RawMessage(media)}
	if positionMs >= 0 {
		m["positionMs"] = positionMs
	}
	c.send(m)
}

func num(m msg, key string) int64 { return int64(m[key].(float64)) }

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func phase(want string) func(msg) bool { return func(r msg) bool { return r["phase"] == want } }

// actionKind is the kind of the action a room message carries, "" for none.
func actionKind(r msg) string {
	if a, ok := r["action"].(msg); ok {
		return a["kind"].(string)
	}
	return ""
}

func hostOf(r msg) string {
	for _, p := range r["participants"].([]any) {
		if p := p.(msg); p["host"] == true {
			return p["peerId"].(string)
		}
	}
	return ""
}

func waitingOn(r msg) []string {
	var ids []string
	for _, id := range r["waitingOn"].([]any) {
		ids = append(ids, id.(string))
	}
	return ids
}

// playingRoom creates a room with a host and one member, both ready in the
// player with the room's media playing from 1000.
func playingRoom(t *testing.T, tr *testRelay) (host, member *client) {
	t.Helper()
	host, member = tr.dial(), tr.dial()
	host.create("Host", "Test/1.0")
	member.join(host.code, "Member")
	host.setMedia(movie, 1000)
	for _, c := range []*client{host, member} {
		c.expectMedia()
		c.status(1, "ready")
	}
	for _, c := range []*client{host, member} {
		c.until("both ready and playing", func(r msg) bool {
			return r["phase"] == "playing" && strings.Count(jsonOf(r), `"player":"ready"`) == 2
		})
	}
	return host, member
}
