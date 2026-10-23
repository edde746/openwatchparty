package relay

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCreateAndJoin(t *testing.T) {
	tr := startRelay(t, testConfig())
	host, member := tr.dial(), tr.dial()
	host.create("Host", "Test/1.0")
	r := host.last
	if !validCode(host.code) || r["code"] != host.code || num(r, "rev") != 1 || r["phase"] != "idle" ||
		num(r, "epoch") != 0 || hasKey(r, "action") || hasKey(r, "media") || len(waitingOn(r)) != 0 ||
		hostOf(r) != host.peerID {
		t.Fatalf("new room: %v", r)
	}
	member.join(strings.ToLower(host.code), "Member") // codes are case-insensitive
	if num(member.last, "rev") != 2 || len(member.last["participants"].([]any)) != 2 || member.peerID == host.peerID {
		t.Fatalf("joined: %v", member.last)
	}
	if r := host.expect("room"); num(r, "rev") != 2 || hostOf(r) != host.peerID || hasKey(r, "action") ||
		hasKey(r, "media") {
		t.Fatalf("host after the join: %v", r)
	}
}

// TestTypedCodes: join and resume reach the room with the code as a user
// typed it. Characters SPEC.md §5.1 doesn't list stay, so such a string, like
// one with too few symbols, names no room (§5.1, §7.2, §7.3).
func TestTypedCodes(t *testing.T) {
	cfg := testConfig()
	cfg.TrustProxy = true // each dial its own address, under the per-IP connection limit
	tr := startRelay(t, cfg)
	host := tr.dial()
	host.create("Host", "Test/1.0")
	code, lower := host.code, strings.ToLower(host.code)
	var member *client
	for _, typed := range []string{
		lower[:3] + "-" + lower[3:],
		code[:3] + " " + code[3:],
		" " + code[:3] + "\u2010" + code[3:] + "\n",
		code[:3] + "\u2212" + code[3:],
		"\t" + strings.Join(strings.Split(lower, ""), "\u00A0") + "\r",
	} {
		member = tr.dial()
		member.join(typed, "Member")
		if member.code != code {
			t.Fatalf("join %q: joined.code %q, want %q", typed, member.code, code)
		}
	}
	back := tr.dial()
	back.send(msg{"type": "resume", "code": lower[:3] + "\u2013" + lower[3:], "peerId": member.peerID, "token": member.token})
	back.expect("joined")
	if back.peerID != member.peerID || back.code != code {
		t.Fatalf("resume with a typed code: peer %q code %q", back.peerID, back.code)
	}

	fullWidth := strings.Map(func(r rune) rune { return r + 0xFEE0 }, code)
	for _, other := range []string{
		code[:3] + "\u2015" + code[3:], code[:3] + "_" + code[3:], code[:3] + "." + code[3:], fullWidth,
		code[:3] + "-" + code[3:5], code + "-" + code[:1],
	} {
		c := tr.dial()
		c.send(msg{"type": "join", "code": other, "name": "Lost", "client": "Test/1.0"})
		c.expectError(errRoomNotFound)
		c.send(msg{"type": "resume", "code": other, "peerId": back.peerID, "token": back.token})
		c.expectError(errResumeFailed)
	}
}

func TestBarrierWaitsForEveryoneReady(t *testing.T) {
	tr := startRelay(t, testConfig())
	host, member := tr.dial(), tr.dial()
	host.create("Host", "Test/1.0")
	member.join(host.code, "Member")
	host.status(0, "loading")
	member.status(0, "loading")
	host.until("both loading", func(r msg) bool { return strings.Count(jsonOf(r), `"loading"`) == 2 })

	host.setMedia(movie, 5000)
	r := host.expectMedia()
	if r["phase"] != "waiting" || num(r, "positionMs") != 5000 || num(r, "epoch") != 1 || actionKind(r) != "media" ||
		!slices.Equal(waitingOn(r), []string{host.peerID, member.peerID}) ||
		num(r["media"].(msg), "durationMs") != 7200000 {
		t.Fatalf("after setMedia: %v", r)
	}
	host.status(1, "ready")
	r = host.until("host ready", func(r msg) bool { return len(waitingOn(r)) == 1 })
	if waitingOn(r)[0] != member.peerID || r["phase"] != "waiting" || hasKey(r, "media") {
		t.Fatalf("after the host's ready: %v", r)
	}
	member.status(1, "ready")
	r = host.until("released", phase("playing"))
	if num(r, "positionMs") != 5000 || len(waitingOn(r)) != 0 || hasKey(r, "action") {
		t.Fatalf("after release: %v", r)
	}
}

func TestBarrierTimeoutExcludes(t *testing.T) {
	cfg := testConfig()
	cfg.TimerScale = 0.01 // mediaWaitMs 300, waitMs 150, exclusionMs 600
	tr := startRelay(t, cfg)
	host, member := tr.dial(), tr.dial()
	host.create("Host", "Test/1.0")
	member.join(host.code, "Member")
	host.status(0, "loading")
	member.status(0, "loading")
	host.setMedia(movie, 0)
	host.status(1, "ready")
	start := time.Now()
	host.until("member alone holds the barrier", func(r msg) bool { return slices.Equal(waitingOn(r), []string{member.peerID}) })
	host.until("barrier timeout", phase("playing"))
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("released after %v, before mediaWaitMs", d)
	}

	// Excluded: the member doesn't hold the next wait.
	host.send(msg{"type": "pause"})
	host.until("paused", phase("paused"))
	host.send(msg{"type": "play"})
	if r := host.until("play", func(r msg) bool { return actionKind(r) == "play" }); r["phase"] != "playing" {
		t.Fatalf("play with the member excluded: %v", r)
	}

	// The exclusion ends silently; the next wait counts the member again.
	time.Sleep(700 * time.Millisecond)
	host.send(msg{"type": "pause"})
	host.until("paused", phase("paused"))
	host.send(msg{"type": "play"})
	r := host.until("play", phase("waiting"))
	if !slices.Equal(waitingOn(r), []string{member.peerID}) {
		t.Fatalf("play after the exclusion: %v", r)
	}
}

func TestPauseKeepsNearbyPosition(t *testing.T) {
	tr := startRelay(t, testConfig())
	host, _ := playingRoom(t, tr) // at 1000, starting 750 ms from now
	host.send(msg{"type": "pause", "positionMs": 2500})
	if r := host.until("paused", phase("paused")); num(r, "positionMs") != 2500 || actionKind(r) != "pause" {
		t.Fatalf("pause within the tolerance: %v", r)
	}
	host.send(msg{"type": "play"})
	host.until("playing", phase("playing")) // starts at 2500, 750 ms from now
	host.send(msg{"type": "pause", "positionMs": 9000})
	if r := host.until("paused", phase("paused")); num(r, "positionMs") != 2500 {
		t.Fatalf("pause beyond the tolerance must use the relay's position: %v", r)
	}
}

func TestSeekCoalescing(t *testing.T) {
	tr := startRelay(t, testConfig())
	host, _ := playingRoom(t, tr)
	host.send(msg{"type": "pause"})
	host.until("paused", phase("paused"))
	start := time.Now()
	host.send(msg{"type": "seek", "positionMs": 10000})
	host.send(msg{"type": "seek", "positionMs": 20000})
	host.send(msg{"type": "seek", "positionMs": 30000})
	host.until("first seek", func(r msg) bool { return num(r, "positionMs") == 10000 })
	r := host.until("held seek", func(r msg) bool {
		if num(r, "positionMs") == 20000 {
			t.Fatalf("a replaced held seek was applied: %v", r)
		}
		return num(r, "positionMs") == 30000
	})
	if d := time.Since(start); d < 150*time.Millisecond || actionKind(r) != "seek" {
		t.Fatalf("held seek applied after %v: %v", d, r)
	}
}

func TestClearMedia(t *testing.T) {
	tr := startRelay(t, testConfig())
	host, member := playingRoom(t, tr)
	host.send(msg{"type": "clearMedia"})
	for _, c := range []*client{host, member} {
		r := c.expectMedia()
		if r["media"] != nil || r["phase"] != "idle" || num(r, "positionMs") != 0 || num(r, "epoch") != 2 ||
			actionKind(r) != "media" {
			t.Fatalf("room after clearMedia: %v", r)
		}
	}
	host.send(msg{"type": "play"})
	host.expectError(errInvalidMessage)
	host.send(msg{"type": "clearMedia"}) // no media: no effect, no snapshot
	host.send(msg{"type": "ping", "id": 1})
	host.expect("pong")
}

func TestStallWait(t *testing.T) {
	cfg := testConfig()
	cfg.TimerScale = 0.1 // stallGraceMs 50, stallReleaseMs 40
	tr := startRelay(t, cfg)
	host, member := playingRoom(t, tr)
	member.status(1, "buffering")
	r := host.until("stall", phase("waiting"))
	if a := r["action"].(msg); a["kind"] != "stall" || a["by"] != member.peerID ||
		!slices.Equal(waitingOn(r), []string{member.peerID}) {
		t.Fatalf("stall wait: %v", r)
	}
	member.status(1, "ready")
	r = host.until("member recovered", func(r msg) bool { return len(waitingOn(r)) == 0 })
	if r["phase"] != "waiting" {
		t.Fatalf("a stall wait must hold for stallReleaseMs after waitingOn empties: %v", r)
	}
	host.until("stall release", phase("playing"))
}

func TestNobodyInThePlayerPauses(t *testing.T) {
	tr := startRelay(t, testConfig())
	host, member := playingRoom(t, tr)
	host.status(1, "idle")
	member.status(1, "idle")
	if r := host.until("paused", phase("paused")); hasKey(r, "action") {
		t.Fatalf("a pause for nobody in the player sets no action: %v", r)
	}
}

func TestEndOfMedia(t *testing.T) {
	cfg := testConfig()
	cfg.TimerScale = 0.01 // start delay 7-20 ms; media time is never scaled
	tr := startRelay(t, cfg)
	host := tr.dial()
	host.create("Host", "Test/1.0")
	host.setMedia(`{"kind":"video","title":"Short","durationMs":1500}`, 1000)
	host.expectMedia()
	host.status(1, "ready")
	start := time.Now()
	r := host.until("ended", func(r msg) bool { return actionKind(r) == "ended" })
	if r["phase"] != "paused" || num(r, "positionMs") != 1500 || r["action"].(msg)["by"] != nil {
		t.Fatalf("end of media: %v", r)
	}
	if d := time.Since(start); d < 450*time.Millisecond {
		t.Fatalf("ended after %v; 500 ms of media remained", d)
	}
	rev := num(r, "rev")
	host.send(msg{"type": "play"}) // at the end: releases and ends again in the same event, one snapshot
	r = host.expect("room")
	if num(r, "rev") != rev+1 || r["phase"] != "paused" || num(r, "positionMs") != 1500 || actionKind(r) != "ended" {
		t.Fatalf("play at the end: %v", r)
	}
}

func TestHostPromotion(t *testing.T) {
	cfg := testConfig()
	cfg.TimerScale = 0.01 // hostPromotionMs 300
	tr := startRelay(t, cfg)
	host, member := tr.dial(), tr.dial()
	host.create("Host", "Test/1.0")
	member.join(host.code, "Member")
	host.ws.Close()
	start := time.Now()
	r := member.until("promotion", func(r msg) bool { return hostOf(r) == member.peerID })
	if a := r["action"].(msg); a["kind"] != "host" || a["by"] != nil {
		t.Fatalf("promotion: %v", r)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("promoted after %v", d)
	}
}

func TestPromotionWaitsForSomeoneConnected(t *testing.T) {
	cfg := testConfig()
	cfg.TimerScale = 0.01 // hostPromotionMs 300
	tr := startRelay(t, cfg)
	host, member := tr.dial(), tr.dial()
	host.create("Host", "Test/1.0")
	member.join(host.code, "Member")
	host.ws.Close()
	member.until("host gone", func(r msg) bool { return strings.Count(jsonOf(r), `"connected":false`) == 1 })
	member.ws.Close()
	time.Sleep(400 * time.Millisecond) // promotion time passes with nobody connected: the host stays

	back := tr.dial()
	back.send(msg{"type": "resume", "code": host.code, "peerId": member.peerID, "token": member.token})
	back.expect("joined")
	if hostOf(back.last) != member.peerID || hasKey(back.last, "action") {
		t.Fatalf("a resume after the promotion time must promote, and joined.room carries no action: %v", back.last)
	}
}

func TestResumeTakesOver(t *testing.T) {
	tr := startRelay(t, testConfig())
	host, member := tr.dial(), tr.dial()
	host.create("Host", "Test/1.0")
	member.join(host.code, "Member")

	bad := tr.dial()
	bad.send(msg{"type": "resume", "code": host.code, "peerId": member.peerID, "token": host.token})
	bad.expectError(errResumeFailed)

	again := tr.dial()
	again.send(msg{"type": "resume", "code": host.code, "peerId": member.peerID, "token": member.token})
	again.expect("joined")
	if again.peerID != member.peerID || again.token != member.token {
		t.Fatalf("resume must keep the peer ID and token: %v %v", again.peerID, again.token)
	}
	if code := member.closeCode(); code != closeTakenOver {
		t.Fatalf("taken-over connection closed with %d, want %d", code, closeTakenOver)
	}
	again.send(msg{"type": "pause"}) // the new connection is the participant
	again.expectError(errForbidden)
}

func TestKickTransferEnd(t *testing.T) {
	tr := startRelay(t, testConfig())
	host, a, b := tr.dial(), tr.dial(), tr.dial()
	host.create("Host", "Test/1.0")
	a.join(host.code, "A")
	b.join(host.code, "B")

	a.send(msg{"type": "kick", "peerId": b.peerID})
	a.expectError(errForbidden)
	host.send(msg{"type": "kick", "peerId": a.peerID})
	if m := a.expect("closed"); m["reason"] != "kicked" {
		t.Fatalf("kicked: %v", m)
	}
	a.send(msg{"type": "resume", "code": host.code, "peerId": a.peerID, "token": a.token})
	a.expectError(errResumeFailed) // the token stopped working; the connection stays open

	host.send(msg{"type": "transferHost", "peerId": b.peerID})
	r := b.until("transfer", func(r msg) bool { return hostOf(r) == b.peerID })
	if a := r["action"].(msg); a["kind"] != "host" || a["by"] != host.peerID {
		t.Fatalf("transferHost: %v", r)
	}
	b.send(msg{"type": "end"})
	for _, c := range []*client{host, b} {
		if m := c.expect("closed"); m["reason"] != "ended" {
			t.Fatalf("end: %v", m)
		}
	}
	a.send(msg{"type": "join", "code": host.code, "name": "A", "client": "Test/1.0"})
	a.expectError(errRoomNotFound)
}

func TestJoinRefusals(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxParticipants = 2
	tr := startRelay(t, cfg)
	host, member, third := tr.dial(), tr.dial(), tr.dial()

	host.send(msg{"type": "pause"})
	host.expectError(errNotInRoom)
	host.create("Host", "Test/1.0")
	host.send(msg{"type": "create", "name": "Host", "client": "Test/1.0"})
	host.expectError(errAlreadyInRoom)
	third.send(msg{"type": "join", "code": "ZZZZZZ", "name": "T", "client": "Test/1.0"})
	third.expectError(errRoomNotFound)
	third.send(msg{"type": "join", "code": "abc", "name": "T", "client": "Test/1.0"})
	third.expectError(errRoomNotFound)

	host.send(msg{"type": "lock", "locked": true})
	host.until("locked", func(r msg) bool { return r["locked"] == true })
	member.send(msg{"type": "join", "code": host.code, "name": "M", "client": "Test/1.0"})
	member.expectError(errRoomLocked)
	host.send(msg{"type": "lock", "locked": false})
	host.until("unlocked", func(r msg) bool { return r["locked"] == false })
	member.join(host.code, "M")
	third.send(msg{"type": "join", "code": host.code, "name": "T", "client": "Test/1.0"})
	third.expectError(errRoomFull)

	member.setMedia(movie, 0)
	member.expectError(errForbidden)
	host.send(msg{"type": "play"})
	host.expectError(errInvalidMessage) // idle
}

// TestJoinCreate: a join with create makes the room that a free code names,
// with the code as normalized and the sender as its host at rev 1. On a code
// that names a room, it joins as a join without create does. Without create,
// or with create false, a free code gets room_not_found, and create must be a
// boolean. A creating join counts as a room created and as a join (SPEC.md
// §7.2).
func TestJoinCreate(t *testing.T) {
	tr := startRelay(t, testConfig())
	a, b, c := tr.dial(), tr.dial(), tr.dial()
	a.send(msg{"type": "join", "code": "K7MQ2X", "name": "A", "client": "Test/1.0"})
	a.expectRefusal(errRoomNotFound, "join")
	a.send(msg{"type": "join", "code": "K7MQ2X", "name": "A", "client": "Test/1.0", "create": false})
	a.expectRefusal(errRoomNotFound, "join")
	for _, v := range []string{`"true"`, `1`, `null`, `{}`} {
		a.sendRaw(`{"type":"join","code":"K7MQ2X","name":"A","client":"Test/1.0","create":` + v + `}`)
		a.expectRefusal(errInvalidMessage, "join")
	}

	a.send(joinCreate("k7m-q2x", "A"))
	a.expect("joined")
	r := a.last
	if a.code != "K7MQ2X" || r["code"] != "K7MQ2X" || num(r, "rev") != 1 || r["phase"] != "idle" ||
		num(r, "epoch") != 0 || r["control"] != "host" || r["locked"] != false ||
		len(r["participants"].([]any)) != 1 || hostOf(r) != a.peerID {
		t.Fatalf("room made by a join with create: code %q, %v", a.code, r)
	}
	b.send(joinCreate("K7MQ2X", "B"))
	b.expect("joined")
	if num(b.last, "rev") != 2 || len(b.last["participants"].([]any)) != 2 || hostOf(b.last) != a.peerID {
		t.Fatalf("join with create of a held room: %v", b.last)
	}
	if r := a.expect("room"); num(r, "rev") != 2 || hostOf(r) != a.peerID {
		t.Fatalf("host after the join: %v", r)
	}
	c.join("K7MQ2X", "C")

	for i, x := range []*client{a, b, c} { // each answer comes after its join is counted
		x.send(msg{"type": "ping", "id": i})
		x.expect("pong")
	}
	var metrics strings.Builder
	tr.srv.writeMetrics(&metrics, "test")
	for _, want := range []string{"owp_rooms_created_total 1\n", "owp_joins_total 3\n"} {
		if !strings.Contains(metrics.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// TestJoinCreateRefusals: a join with create for a free code gets
// room_not_found when the code isn't one the relay creates, then
// rate_limited when the sender's address created its limit of rooms, then
// relay_full at maxRooms, and a refused one keeps no room slot. Every one of
// these joins takes a join token, as does one that makes or joins a room
// (SPEC.md §7.2, §11.10).
func TestJoinCreateRefusals(t *testing.T) {
	cfg := testConfig()
	cfg.TrustProxy, cfg.Prefixes = true, "2345"
	cfg.Limits.MaxRooms, cfg.Limits.MaxRoomsPerIP = 2, 1
	cfg.Limits.JoinBurstTotal, cfg.Limits.JoinsPerSecondTotal = 9, 0.001 // the relay's bucket counts every join here
	tr := startRelay(t, cfg)
	from := func(addr string) *client {
		t.Helper()
		c, resp, err := tr.dialWith(http.Header{"X-Forwarded-For": {addr}})
		if err != nil {
			t.Fatalf("dial: %v (%v)", err, resp)
		}
		return c
	}
	refused := func(c *client, code, want string) {
		t.Helper()
		c.send(joinCreate(code, "R"))
		c.expectRefusal(want, "join")
	}
	a, again, c := from("10.1.1.1"), from("10.1.1.1"), tr.dial()
	a.send(joinCreate("2AAAAA", "A")) // token 1
	a.expect("joined")
	refused(c, "K7MQ2X", errRoomNotFound)     // 2: K is not a creation prefix
	refused(c, "2AAAA", errRoomNotFound)      // 3: not a code
	refused(again, "K7MQ2Y", errRoomNotFound) // 4: the code is checked before the room limit
	refused(again, "2BBBBB", errRateLimited)  // 5: 10.1.1.1 made its 1 room with a join
	tr.dial().create("D", "Test/1.0")         // the relay holds 2 rooms
	refused(again, "2CCCCC", errRateLimited)  // 6: the room limit is checked before maxRooms
	refused(c, "2CCCCC", errRelayFull)        // 7
	refused(c, "2CCCCC", errRelayFull)        // 8: not rate_limited, so 7 kept no slot
	c.send(joinCreate("2aa-aaa", "C"))        // 9: a held room takes no slot
	c.expect("joined")
	if c.code != "2AAAAA" || hostOf(c.last) != a.peerID {
		t.Fatalf("join with create of a held room on a full relay: %v", c.last)
	}
	late := tr.dial()
	late.send(msg{"type": "join", "code": "2AAAAA", "name": "L", "client": "Test/1.0"})
	late.expectRefusal(errRateLimited, "join") // the relay's 9 join tokens are gone
}

// TestJoinCreateAfterEnd: once the host ends the room, a former member that
// read closed gets the code back with a join with create, as the host of a
// new room, even from the address whose 1 room slot the ended room held. The
// former host's join with create then joins that room (SPEC.md §7.2, §11.8).
func TestJoinCreateAfterEnd(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxRoomsPerIP = 1
	tr := startRelay(t, cfg)
	host, member := tr.dial(), tr.dial() // both from 127.0.0.1
	host.create("Host", "Test/1.0")
	member.join(host.code, "Member")
	code := host.code
	host.send(msg{"type": "end"})
	for _, c := range []*client{host, member} {
		c.expect("closed")
	}
	member.send(joinCreate(strings.ToLower(code[:3]+"-"+code[3:]), "Member"))
	member.expect("joined")
	if r := member.last; member.code != code || num(r, "rev") != 1 || len(r["participants"].([]any)) != 1 ||
		hostOf(r) != member.peerID {
		t.Fatalf("join with create of an ended room's code: code %q, %v", member.code, r)
	}
	host.send(joinCreate(code, "Host"))
	host.expect("joined")
	if num(host.last, "rev") != 2 || hostOf(host.last) != member.peerID {
		t.Fatalf("former host's join with create: %v", host.last)
	}
}

// TestJoinCreateRace: of two joins with create sent at once for a free code,
// one makes the room and the other joins it. One that races the end of the
// room its code names joins that room or makes a new one, and never gets
// room_not_found (SPEC.md §7.2).
func TestJoinCreateRace(t *testing.T) {
	cfg := testConfig()
	cfg.TrustProxy = true // each dial its own address, under the per-IP limits
	tr := startRelay(t, cfg)
	for range 20 {
		code := newRoomCode(codeAlphabet)
		a, b := tr.dial(), tr.dial()
		a.send(joinCreate(code, "A"))
		b.send(joinCreate(code, "B"))
		a.expect("joined")
		b.expect("joined")
		maker, guest := a, b
		if num(b.last, "rev") == 1 {
			maker, guest = b, a
		}
		if maker.code != code || guest.code != code || num(maker.last, "rev") != 1 ||
			hostOf(maker.last) != maker.peerID || num(guest.last, "rev") != 2 || hostOf(guest.last) != maker.peerID {
			t.Fatalf("two joins with create of %s: %v and %v", code, a.last, b.last)
		}

		host, c := tr.dial(), tr.dial()
		host.create("H", "Test/1.0")
		host.send(msg{"type": "end"})
		c.send(joinCreate(host.code, "C"))
		c.expect("joined")
		if num(c.last, "rev") != 1 { // it joined the room before the end
			c.expect("closed")
		}
	}
}

func TestDenyList(t *testing.T) {
	cfg := testConfig()
	cfg.DenylistPath = filepath.Join(t.TempDir(), "denylist")
	if err := os.WriteFile(cfg.DenylistPath, []byte("# comment\nblocked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := startRelay(t, cfg)
	host, member := tr.dial(), tr.dial()
	host.send(msg{"type": "create", "name": "Host", "client": "BLOCKED/2.0"}) // ASCII case-insensitive
	host.expectError(errClientBlocked)

	host.create("Host", "Fine/1.0")
	member.join(host.code, "Member")
	if err := os.WriteFile(cfg.DenylistPath, []byte("fine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := tr.dial()
	deadline := time.Now().Add(3 * time.Second)
	for {
		other.send(msg{"type": "create", "name": "Other", "client": "Blocked/1.0"})
		if m := other.next(); m["type"] == "joined" {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the changed deny list was not applied: %v", m)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Members already in rooms stay; a resume of a denied app is refused.
	host.send(msg{"type": "lock", "locked": true})
	host.until("host still in the room", func(r msg) bool { return r["locked"] == true })
	member.ws.Close()
	back := tr.dial()
	back.send(msg{"type": "resume", "code": host.code, "peerId": host.peerID, "token": host.token})
	back.expectError(errClientBlocked)
}

// TestMediaEndFarAwayWaits: a media end further away than time.Duration holds
// in nanoseconds (1e13 ms) waits instead of firing and re-arming at once.
// Descriptors can't carry such a duration (TestBounds), so the test sets the
// room's duration directly.
func TestMediaEndFarAwayWaits(t *testing.T) {
	s, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := newRoom(s, "K7MQ2X", s.now())
	c := &conn{srv: s, send: make(chan []byte, 64), done: make(chan struct{})}
	r.mu.Lock()
	p, _ := r.add(c, "Ann", "Test/1", s.now())
	setMedia, _ := parseCommand("setMedia", parseObject([]byte(`{"media":`+movie+`}`)))
	ready, _ := parseCommand("status", parseObject([]byte(`{"epoch":1,"player":"ready","match":null,"offsetMs":0}`)))
	r.apply(p, setMedia, s.now())
	r.durationMs = 10_000_000_000_000
	r.apply(p, ready, s.now())
	r.flush()
	gen := r.mediaEnd.gen
	r.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mediaEnd.gen != gen || !r.mediaEnd.pending() || r.phase != phasePlaying {
		t.Errorf("the media-end timer re-armed %d times in 100 ms; phase %s", r.mediaEnd.gen-gen, r.phase)
	}
	r.stopPlaying()
	r.closed = true
}

// TestLargestMessageFitsClients: a room of participantsBound members, every
// snapshot field at its longest, sends `joined` and `room` within the 32768
// bytes clients accept (SPEC.md §3.1, §13.2). U+2028 is the longest name and
// client character on the wire: encoding/json writes it as a 6-byte escape.
func TestLargestMessageFitsClients(t *testing.T) {
	s, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := newRoom(s, "K7MQ2X", s.now())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peerSeq = 1e15 // 16-character peer IDs
	delta := int64(-maxSafeInteger)
	name, client := strings.Repeat("\u2028", maxNameRunes), strings.Repeat("\u2028", maxClientRunes-1)+"/"
	var conns []*conn
	var p *participant
	for range participantsBound {
		c := &conn{srv: s, send: make(chan []byte, 1), done: make(chan struct{})}
		p, _ = r.add(c, name, client, s.now())
		p.player, p.offsetMs = playerBuffering, -maxDurationMs
		p.match = &matchState{State: matchUnavailable, DeltaMs: &delta}
		r.waitingOn = append(r.waitingOn, p)
		conns = append(conns, c)
	}
	r.rev, r.epoch, r.atMs, r.positionMs = maxSafeInteger-1, maxSafeInteger, maxSafeInteger, maxDurationMs
	r.phase, r.rate, r.control = phaseWaiting, 1.0000000000000002, controlEveryone
	r.media = []byte(`{"kind":"movie","title":"` + strings.Repeat("x", maxMediaBytes-27) + `"}`)
	r.act("media", p.peerID)
	r.newEpoch = true
	r.publish(p, newToken()) // the last one joins; everyone else gets `room` with the media
	sizes := map[string]int{}
	for _, c := range conns {
		msg := <-c.send
		var head struct{ Type string }
		if err := json.Unmarshal(msg, &head); err != nil {
			t.Fatal(err)
		}
		sizes[head.Type] = max(sizes[head.Type], len(msg))
	}
	if len(sizes) != 2 || sizes["joined"] > 32768 || sizes["room"] > 32768 {
		t.Errorf("largest messages %v, want joined and room within 32768 bytes", sizes)
	}
	r.closed = true
}

// TestTimerStepsRunOnceAtTheEnd: a delay longer than one timer step (2^31 − 1
// ms; 20 ms here) runs its function once, after the whole delay.
func TestTimerStepsRunOnceAtTheEnd(t *testing.T) {
	s, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := newRoom(s, "K7MQ2X", s.now())
	ran := make(chan time.Duration, 4)
	start := time.Now()
	r.mu.Lock()
	r.barrier.stop()
	r.armSteps(&r.barrier, r.barrier.gen, 70, 20, func(int64) { ran <- time.Since(start) })
	r.mu.Unlock()
	if d := <-ran; d < 70*time.Millisecond {
		t.Fatalf("ran after %v, before the whole delay", d)
	}
	time.Sleep(100 * time.Millisecond)
	if len(ran) != 0 {
		t.Fatal("ran more than once")
	}
	r.mu.Lock()
	if r.barrier.pending() {
		t.Error("the timer is still pending after it ran")
	}
	r.closed = true
	r.mu.Unlock()
}

// TestHugeTimerScale: --timer-scale can scale a policy duration past what
// int64 milliseconds and time.Duration hold. Scaled values cap at 2^53 − 1
// and a new room lives instead of expiring at once.
func TestHugeTimerScale(t *testing.T) {
	cfg := testConfig()
	cfg.TimerScale = 1e300
	tr := startRelay(t, cfg)
	info, _ := getJSON(t, tr.ts.URL+"/v1/info")
	if got := info["policy"].(msg)["maxRoomAgeMs"]; got != float64(maxSafeInteger) {
		t.Errorf("maxRoomAgeMs %v, want %d", got, int64(maxSafeInteger))
	}
	host := tr.dial()
	host.create("Host", "Test/1.0")
	time.Sleep(100 * time.Millisecond)
	host.send(msg{"type": "ping", "id": 1})
	host.expect("pong")
}

// TestFirstSeekAppliedAtOnce: a room's first seek is applied at once, in the
// relay's first seekCoalesceMs and right after a restore (SPEC.md §12.8).
func TestFirstSeekAppliedAtOnce(t *testing.T) {
	cfg := testConfig()
	cfg.TimerScale = 10 // seekCoalesceMs 2000
	cfg.StateDir = t.TempDir()
	seek := func(tr *testRelay, c *client, positionMs int64) {
		t.Helper()
		if now := tr.srv.now(); now >= tr.srv.scaledMs(seekCoalesceMs) {
			t.Fatalf("the seek goes out at relay time %d, after the first seekCoalesceMs", now)
		}
		start := time.Now()
		c.send(msg{"type": "seek", "positionMs": positionMs})
		c.until("the seek", func(r msg) bool { return actionKind(r) == "seek" && num(r, "positionMs") == positionMs })
		if d := time.Since(start); d > time.Second {
			t.Fatalf("the first seek was applied after %v", d)
		}
	}
	first := startRelay(t, cfg)
	host := first.dial()
	host.create("Host", "Test/1.0")
	host.setMedia(movie, 0)
	host.expectMedia()
	seek(first, host, 60_000)
	first.stop()

	second := startRelay(t, cfg)
	back := second.dial()
	back.send(msg{"type": "resume", "code": host.code, "peerId": host.peerID, "token": host.token})
	back.expect("joined")
	seek(second, back, 30_000)
}
