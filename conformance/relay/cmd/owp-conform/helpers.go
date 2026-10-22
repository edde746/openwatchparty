package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const reply = 3 * time.Second // how long any direct answer may take

// slack is the allowance on every timing bound: relay timestamps are floored to integer milliseconds and timers fire
// a little late. Network latency is not in it: every bound runs from the send of the cause to the arrival of the
// answer.
const (
	slack  = 10.0 // ms
	slackD = 10 * time.Millisecond
)

// clientName is the `client` every scenario sends unless it tests that field.
const clientName = "owp-conform/1.0"

const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

var (
	codeRe = regexp.MustCompile(`^[` + alphabet + `]{6}$`)
	peerRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)
)

func (c *Client) setJoined(j Msg) {
	you := obj(j, "you")
	c.PeerID, c.Token, c.Code = str(you, "peerId"), str(you, "token"), str(j, "code")
	recordIssued(issued.codes, c.Code)
	recordIssued(issued.tokens, c.Token)
}

// enter sends create, join or resume on c and waits for joined or an error. An error answer is returned as j with a
// non-nil error.
func enter(c *Client, m Msg) (Msg, error) {
	if err := c.send(m); err != nil {
		return nil, err
	}
	return entered(c, str(m, "type"))
}

// entered waits for the answer to a create, join or resume of type typ that c sent, as enter does.
func entered(c *Client, typ string) (Msg, error) {
	j, err := c.waitFor(reply, "joined", func(m Msg) bool { return isType("joined")(m) || isType("error")(m) })
	if err != nil {
		return nil, err
	}
	if str(j, "type") == "error" {
		return j, fmt.Errorf("%s: %s failed: %s", c.name, typ, brief(j))
	}
	c.setJoined(j)
	return j, nil
}

// Every create, join and status carries "xFuture": receivers ignore unknown fields.

func createMsg(name string) Msg {
	return Msg{"type": "create", "name": name, "client": clientName, "xFuture": true}
}

func joinMsg(code, name string) Msg {
	return Msg{"type": "join", "code": code, "name": name, "client": clientName, "xFuture": true}
}

func joinCreateMsg(code, name string) Msg {
	m := joinMsg(code, name)
	m["create"] = true
	return m
}

func resumeMsg(c *Client) Msg {
	return Msg{"type": "resume", "code": c.Code, "peerId": c.PeerID, "token": c.Token}
}

func (x *sc) create(name string) (*Client, Msg, error) {
	c, err := x.dial(name)
	if err != nil {
		return nil, nil, err
	}
	j, err := enter(c, createMsg(name))
	if err != nil {
		return nil, nil, err
	}
	return c, j, nil
}

// createFrom creates a room on a new connection from addr. It returns the client and the answer also when the relay
// refused the create.
func (x *sc) createFrom(name, addr string) (*Client, Msg, error) {
	c, err := x.dialFrom(name, addr)
	if err != nil {
		return nil, nil, err
	}
	j, err := enter(c, createMsg(name))
	return c, j, err
}

// join returns the client and the answer also when the relay refused the join.
func (x *sc) join(code, name string) (*Client, Msg, error) {
	c, err := x.dial(name)
	if err != nil {
		return nil, nil, err
	}
	j, err := enter(c, joinMsg(code, name))
	return c, j, err
}

// joinCreateFrom sends a join with create for code on a new connection from addr. It returns the client and the
// answer also when the relay refused the join.
func (x *sc) joinCreateFrom(name, addr, code string) (*Client, Msg, error) {
	c, err := x.dialFrom(name, addr)
	if err != nil {
		return nil, nil, err
	}
	j, err := enter(c, joinCreateMsg(code, name))
	return c, j, err
}

// resume reconnects the participant old was, on a new connection.
func (x *sc) resume(old *Client, name string) (*Client, Msg, error) {
	c, err := x.dial(name)
	if err != nil {
		return nil, nil, err
	}
	j, err := enter(c, resumeMsg(old))
	return c, j, err
}

// roomOf makes a room of n participants (P0 hosts) and waits until every member has seen every join.
func (x *sc) roomOf(n int) ([]*Client, error) {
	host, _, err := x.create("P0")
	if err != nil {
		return nil, err
	}
	cs := []*Client{host}
	for i := 1; i < n; i++ {
		c, _, err := x.join(host.Code, fmt.Sprintf("P%d", i))
		if err != nil {
			return nil, err
		}
		for _, prev := range cs {
			want := i + 1
			if _, err := prev.waitFor(reply, fmt.Sprintf("room with %d participants", want),
				isRoom(func(m Msg) bool { return len(participants(m)) == want })); err != nil {
				return nil, err
			}
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// statusMsg is a status message; rtt < 0 leaves rttMs out.
func statusMsg(epoch int, player string, match any, rtt int) Msg {
	m := Msg{"type": "status", "epoch": epoch, "player": player, "match": match, "offsetMs": 0, "xFuture": true}
	if rtt >= 0 {
		m["rttMs"] = rtt
	}
	return m
}

func status(c *Client, epoch int, player string, match any, rtt int) error {
	return c.send(statusMsg(epoch, player, match, rtt))
}

func matched() Msg { return Msg{"state": "matched", "deltaMs": 0} }

func sampleMedia(durationMs int) Msg {
	return normalize(Msg{
		"kind": "episode", "title": "Pilot", "durationMs": durationMs, "year": 2008,
		"ids":    Msg{"imdb": "tt0959621", "tvdb": "349232"},
		"series": Msg{"title": "Breaking Bad", "year": 2008, "ids": Msg{"tmdb": "1396", "imdb": "tt0903747"}},
		"season": 1, "episode": 1,
		"file": Msg{"sizeBytes": 2147483648, "oshash": "8e245d9679d31e12", "name": "Breaking.Bad.S01E01.mkv"},
		"ext":  Msg{"com.example": Msg{"server": "abc", "item": "42", "nested": []any{1, "two", nil, true}}},
	})
}

func normalize(v Msg) Msg {
	b, _ := json.Marshal(v)
	var out Msg
	_ = json.Unmarshal(b, &out)
	return out
}

// mediaDiff compares a media object with the one sent, field by field: the relay passes the descriptor through.
func mediaDiff(got any, want Msg) error {
	g, ok := got.(map[string]any)
	if !ok {
		return fmt.Errorf("media = %v, want an object", got)
	}
	keys := map[string]bool{}
	for k := range g {
		keys[k] = true
	}
	for k := range want {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		gv, gok := g[k]
		wv, wok := want[k]
		if gok != wok || !reflect.DeepEqual(gv, wv) {
			gs, _ := json.Marshal(gv)
			ws, _ := json.Marshal(wv)
			return fmt.Errorf("media.%s = %s (present %v), want %s (present %v)", k, truncate(string(gs), 80), gok,
				truncate(string(ws), 80), wok)
		}
	}
	return nil
}

func allPlayers(room Msg, player string) bool {
	ps := participants(room)
	for _, p := range ps {
		if str(p, "player") != player {
			return false
		}
	}
	return len(ps) > 0
}

// waitPlayers waits, on every client, for the snapshot in which every participant reports player.
func waitPlayers(cs []*Client, player string) error {
	for _, c := range cs {
		if _, err := c.waitFor(reply, "every player "+player, isRoom(func(m Msg) bool { return allPlayers(m, player) })); err != nil {
			return err
		}
	}
	return nil
}

func isPhase(phase string) func(Msg) bool {
	return isRoom(func(m Msg) bool { return str(m, "phase") == phase })
}

func waitingOnIs(ids ...string) func(Msg) bool {
	return isRoom(func(m Msg) bool { return str(m, "phase") == "waiting" && equalStrings(strList(m, "waitingOn"), ids) })
}

func playerIs(peerID, player string) func(Msg) bool {
	return isRoom(func(m Msg) bool { return str(participant(m, peerID), "player") == player })
}

func connectedIs(peerID string, connected bool) func(Msg) bool {
	return isRoom(func(m Msg) bool { p := participant(m, peerID); return p != nil && p["connected"] == connected })
}

// hasMedia reports whether a room carries media: the snapshot that starts an epoch (§8.3).
func hasMedia(m Msg) bool {
	_, ok := m["media"]
	return isType("room")(m) && ok
}

// setMediaAll sends setMedia from host and waits until every client got the room that carries the media.
func setMediaAll(cs []*Client, host *Client, media Msg) error {
	if err := host.send(Msg{"type": "setMedia", "media": media}); err != nil {
		return err
	}
	for _, c := range cs {
		if _, err := c.waitFor(reply, "room with media", hasMedia); err != nil {
			return err
		}
	}
	return nil
}

// playing makes a room of n with media of durationMs playing, everyone ready on epoch 1.
func (x *sc) playing(n int, rtt int, durationMs int) ([]*Client, error) {
	cs, err := x.roomOf(n)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		if err := status(c, 0, "loading", nil, rtt); err != nil {
			return nil, err
		}
	}
	if err := waitPlayers(cs, "loading"); err != nil {
		return nil, err
	}
	if err := setMediaAll(cs, cs[0], sampleMedia(durationMs)); err != nil {
		return nil, err
	}
	for _, c := range cs {
		if err := status(c, 1, "ready", matched(), rtt); err != nil {
			return nil, err
		}
	}
	for _, c := range cs {
		if _, err := c.waitFor(reply, "playing with everyone ready", isRoom(func(m Msg) bool {
			return str(m, "phase") == "playing" && len(strList(m, "waitingOn")) == 0 && allPlayers(m, "ready")
		})); err != nil {
			return nil, err
		}
	}
	return cs, nil
}

// paused is [sc.playing] followed by a pause from the host, seen by every client.
func (x *sc) paused(n int, durationMs int) ([]*Client, error) {
	cs, err := x.playing(n, 10, durationMs)
	if err != nil {
		return nil, err
	}
	if err := cs[0].send(Msg{"type": "pause"}); err != nil {
		return nil, err
	}
	for _, c := range cs {
		if _, err := c.waitFor(reply, "paused", isPhase("paused")); err != nil {
			return nil, err
		}
	}
	return cs, nil
}

// playNow sends play from by while the room is paused and everyone is ready. waitingOn is then empty, so the room
// releases at once, and the command produces one snapshot: the first room c receives must be playing.
func playNow(c, by *Client) (Msg, error) {
	if err := by.send(Msg{"type": "play"}); err != nil {
		return nil, err
	}
	r, err := c.waitFor(reply, "room after play", isType("room"))
	if err != nil {
		return nil, err
	}
	k, who := actionOf(r)
	if str(r, "phase") != "playing" || len(strList(r, "waitingOn")) != 0 || k != "play" || who != by.PeerID {
		return nil, fmt.Errorf("%s: first room after play with everyone ready: %s; want playing (play by %s)", c.name,
			brief(r), by.PeerID)
	}
	return r, nil
}

// clock bounds relay time − local time in ms. Each pong's serverTime was taken between the ping's send and the pong's
// arrival, and floored, so the offset lies in [serverTime − arrival, serverTime + 1 − send]; more pings narrow it.
type clock struct{ offLo, offHi float64 }

// early and late bound the relay time at local time t; mid is the best single estimate.
func (k clock) early(t time.Time) float64 { return msNow(t) + k.offLo }
func (k clock) late(t time.Time) float64  { return msNow(t) + k.offHi }
func (k clock) mid(t time.Time) float64   { return msNow(t) + (k.offLo+k.offHi)/2 }

// clock measures the relay clock with five pings. Call only while the client expects nothing else: other messages
// are discarded.
func (c *Client) clock() (clock, error) {
	k := clock{offLo: math.Inf(-1), offHi: math.Inf(1)}
	for i := range 5 {
		id := fmt.Sprintf("clk-%d-%d", i, time.Now().UnixNano())
		t0 := time.Now()
		if err := c.send(Msg{"type": "ping", "id": id}); err != nil {
			return k, err
		}
		p, err := c.waitFor(reply, "pong", func(m Msg) bool { return isType("pong")(m) && str(m, "id") == id })
		if err != nil {
			return k, err
		}
		t1 := time.Now()
		st := num(p, "serverTime")
		k.offLo = math.Max(k.offLo, st-msNow(t1))
		k.offHi = math.Min(k.offHi, st+1-msNow(t0))
	}
	if k.offLo > k.offHi {
		return k, fmt.Errorf("%s: pong serverTime outside its ping's send-to-arrival interval (offset %.1f > %.1f)",
			c.name, k.offLo, k.offHi)
	}
	return k, nil
}

// posAt is the room's position at relay time t, uncapped.
func posAt(room Msg, t float64) float64 {
	if str(room, "phase") != "playing" {
		return num(room, "positionMs")
	}
	return num(room, "positionMs") + math.Max(0, t-num(room, "atMs"))*num(room, "rate")
}

// timeBetween checks a relay time that must lie in [lo, hi] (relay clock), within [slack].
func timeBetween(got, lo, hi float64, what string) error {
	if got < lo-slack || got > hi+slack {
		return fmt.Errorf("%s = %.0f, want %.0f–%.0f ± %.0f (relay clock, from the send to the arrival)", what, got,
			lo, hi, slack)
	}
	return nil
}

// posBetween checks a position the relay computed as P(now) of room, for some relay time now in [lo, hi], with
// [slack] on the time.
func posBetween(got float64, room Msg, lo, hi float64, what string) error {
	plo, phi := math.Floor(posAt(room, lo-slack)), posAt(room, hi+slack)
	if got < plo || got > phi {
		return fmt.Errorf("%s = %.0f, want %.0f–%.0f (P(now) from the send to the arrival, ± %.0f ms)", what, got,
			plo, phi, slack)
	}
	return nil
}

func within(elapsed, lo, hi time.Duration, what string) error {
	if elapsed < lo || elapsed > hi {
		return fmt.Errorf("%s took %v, want %v–%v", what, elapsed.Round(time.Millisecond), lo, hi)
	}
	return nil
}

func equalStrings(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

// expectErrMsg waits for the next error and checks its code.
func expectErrMsg(c *Client, code string) (Msg, error) {
	m, err := c.waitFor(reply, "error "+code, isType("error"))
	if err != nil {
		return nil, err
	}
	if str(m, "code") != code {
		return nil, fmt.Errorf("%s: got %s, want error %s", c.name, brief(m), code)
	}
	return m, nil
}

// expectErr waits for the next error and checks its code and re ("": null, "*": anything).
func expectErr(c *Client, code, re string) error {
	m, err := expectErrMsg(c, code)
	if err != nil {
		return err
	}
	if re != "*" {
		if re == "" {
			if v, ok := m["re"]; ok && v != nil {
				return fmt.Errorf("%s: error re = %v, want null", c.name, v)
			}
		} else if str(m, "re") != re {
			return fmt.Errorf("%s: error re = %v, want %s", c.name, m["re"], re)
		}
	}
	return nil
}

// refusedWith checks the answer to a create, join or resume that must fail with code.
func refusedWith(j Msg, err error, code, what string) error {
	if err == nil {
		return fmt.Errorf("%s: joined, want error %s", what, code)
	}
	if j == nil || str(j, "code") != code {
		return fmt.Errorf("%s: %v, want error %s", what, err, code)
	}
	return nil
}

// refusedWithRe is [refusedWith] that also checks the refusal's re.
func refusedWithRe(j Msg, err error, code, re, what string) error {
	if err := refusedWith(j, err, code, what); err != nil {
		return err
	}
	if str(j, "re") != re {
		return fmt.Errorf("%s: %s re %v, want %s", what, code, j["re"], re)
	}
	return nil
}

func errs(es ...error) error {
	for _, e := range es {
		if e != nil {
			return e
		}
	}
	return nil
}

// getJSON fetches an HTTP endpoint of the relay and decodes its JSON body.
func getJSON(url string) (Msg, http.Header, error) {
	hc := &http.Client{Timeout: reply}
	resp, err := hc.Get(url)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	var m Msg
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, nil, fmt.Errorf("GET %s: %v", url, err)
	}
	return m, resp.Header, nil
}

// poll calls f every 100 ms until it returns true or d passes.
func poll(d time.Duration, f func() (bool, error)) error {
	deadline := time.Now().Add(d)
	for {
		ok, err := f()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errTimeout
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func lowerCode(code string) string { return strings.ToLower(code) }
