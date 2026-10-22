package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"
)

// SPEC §7 (client → relay messages), §8 (relay → client messages) and §9 (snapshot).

// scCreate: create answers joined with the code, the participant's peer ID and token, a new room (§9.3), no media
// and serverTime.
func scCreate(x *sc) error {
	c, j, err := x.create("Alice")
	if err != nil {
		return err
	}
	if !codeRe.MatchString(c.Code) {
		return fmt.Errorf("code %q", c.Code)
	}
	if !peerRe.MatchString(c.PeerID) {
		return fmt.Errorf("peerId %q", c.PeerID)
	}
	tok, err := base64.RawURLEncoding.DecodeString(c.Token)
	if err != nil || len(tok) != 32 || len(c.Token) != 43 {
		return fmt.Errorf("token %q: %d bytes, %v; want 32 bytes in unpadded base64url", c.Token, len(tok), err)
	}
	if v, ok := j["media"]; !ok || v != nil {
		return fmt.Errorf("joined.media = %v (present %v), want null", v, ok)
	}
	if st, ok := j["serverTime"].(float64); !ok || st < 0 || st != math.Trunc(st) {
		return fmt.Errorf("serverTime %v", j["serverTime"])
	}
	r := obj(j, "room")
	if _, typed := r["type"]; typed || num(r, "rev") != 1 || num(r, "epoch") != 0 || str(r, "phase") != "idle" ||
		num(r, "positionMs") != 0 || num(r, "rate") != 1 || str(r, "control") != "host" || r["locked"] != false ||
		str(r, "code") != c.Code {
		return fmt.Errorf("new room = %v", r)
	}
	if at, ok := r["atMs"].(float64); !ok || at < 0 || at != math.Trunc(at) {
		return fmt.Errorf("room.atMs = %v, want the relay time of creation", r["atMs"])
	}
	if w, ok := r["waitingOn"].([]any); !ok || len(w) != 0 {
		return fmt.Errorf("room.waitingOn = %v", r["waitingOn"])
	}
	ps := participants(r)
	if len(ps) != 1 {
		return fmt.Errorf("participants = %v", r["participants"])
	}
	p := ps[0]
	if str(p, "peerId") != c.PeerID || str(p, "name") != "Alice" || str(p, "client") != clientName ||
		p["host"] != true || p["connected"] != true || str(p, "player") != "idle" || p["offsetMs"] != 0.0 {
		return fmt.Errorf("participant = %v", p)
	}
	if v, ok := p["match"]; !ok || v != nil {
		return fmt.Errorf("participant.match = %v (present %v), want null", v, ok)
	}
	return nil
}

// scJoin: join answers joined with the room (the new participant last, not host); the others get a snapshot.
func scJoin(x *sc) error {
	a, _, err := x.create("A")
	if err != nil {
		return err
	}
	b, j, err := x.join(a.Code, "B")
	if err != nil {
		return err
	}
	r := obj(j, "room")
	ps := participants(r)
	if str(j, "code") != a.Code || num(r, "rev") != 2 || len(ps) != 2 || b.PeerID == a.PeerID ||
		!peerRe.MatchString(b.PeerID) || hostOf(r) != a.PeerID || str(ps[1], "peerId") != b.PeerID ||
		ps[1]["host"] != false || str(ps[1], "player") != "idle" || ps[1]["match"] != nil {
		return fmt.Errorf("joined: code %s room %v", str(j, "code"), r)
	}
	ar, err := a.waitFor(reply, "room with B", isType("room"))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(ar, withType(r)) {
		return fmt.Errorf("A's room after the join %v, want the snapshot in B's joined %v", ar, r)
	}
	return nil
}

// scJoinErrors: unknown codes, membership refusals (§6.2 step 5).
func scJoinErrors(x *sc) error {
	a, _, err := x.create("A")
	if err != nil {
		return err
	}
	c, err := x.dial("C")
	if err != nil {
		return err
	}
	_ = c.send(joinMsg("ZZZZZZ", "C"))
	if err := expectErr(c, "room_not_found", "join"); err != nil {
		return err
	}
	for _, m := range []Msg{createMsg("A"), joinMsg(a.Code, "A"), resumeMsg(a)} {
		_ = a.send(m)
		if err := expectErr(a, "already_in_room", str(m, "type")); err != nil {
			return err
		}
	}
	for _, m := range []Msg{{"type": "play"}, statusMsg(0, "idle", nil, -1), {"type": "leave"}, {"type": "end"},
		{"type": "lock", "locked": true}} {
		_ = c.send(m)
		if err := expectErr(c, "not_in_room", str(m, "type")); err != nil {
			return err
		}
	}
	return nil
}

// scJoinCreate: a join with create for a free code makes the room with that code, normalized from the way a user typed
// it, and the sender as its host at rev 1 with a new room's state (§9.3). For a held room, a join with create joins it
// as join does, and two at once for a free code make one room that both are in. Without create, with create false, or
// with a code the relay doesn't create (not a code, or a first symbol outside its creation prefixes), a free code gets
// room_not_found, and a create that isn't a boolean is invalid_message. After end, a former member that read closed
// makes the room again with a join with create, and the former host's join with create joins it (§5.1, §7.2, §11.8).
func scJoinCreate(x *sc) error {
	a, _, err := x.create("A") // its code starts with one of the relay's creation prefixes
	if err != nil {
		return err
	}
	code := freeCode(a.Code[0])
	e, err := x.dial("E")
	if err != nil {
		return err
	}
	refused := func(m Msg, what string) error {
		j, err := enter(e, m)
		return refusedWithRe(j, err, "room_not_found", "join", what)
	}
	createFalse := joinMsg(code, "E")
	createFalse["create"] = false
	if err := errs(refused(joinMsg(code, "E"), "join without create of a free code"),
		refused(createFalse, "join with create false of a free code")); err != nil {
		return err
	}
	lost := []string{"ABC", "IIIIII", code + "2"}
	if f := x.foreignCode(); f != "" {
		lost = append(lost, f)
	}
	for _, l := range lost {
		if err := refused(joinCreateMsg(l, "E"), fmt.Sprintf("join with create of %q", l)); err != nil {
			return err
		}
	}
	head := `{"type":"join","code":"` + code + `","name":"E","client":"` + clientName + `","create":`
	if err := sendRawCases(e, []rawCase{
		{head + `"true"}`, "invalid_message", "join"},
		{head + `1}`, "invalid_message", "join"},
		{head + `null}`, "invalid_message", "join"},
	}); err != nil {
		return err
	}

	b, err := x.dial("B")
	if err != nil {
		return err
	}
	typed := lowerCode(code[:3]) + "-" + lowerCode(code[3:])
	j, err := enter(b, joinCreateMsg(typed, "B"))
	if err != nil {
		return fmt.Errorf("join with create of a free code typed %q: %w", typed, err)
	}
	r := obj(j, "room")
	if str(j, "code") != code || str(r, "code") != code || num(r, "rev") != 1 || num(r, "epoch") != 0 ||
		str(r, "phase") != "idle" || num(r, "positionMs") != 0 || num(r, "rate") != 1 || str(r, "control") != "host" ||
		r["locked"] != false || len(participants(r)) != 1 || hostOf(r) != b.PeerID {
		return fmt.Errorf("join with create of a free code typed %q: code %s, room %v; want code %s and a new room "+
			"with B as host", typed, str(j, "code"), r, code)
	}
	if v, ok := j["media"]; !ok || v != nil {
		return fmt.Errorf("joined.media = %v (present %v), want null", v, ok)
	}
	c, err := x.dial("C")
	if err != nil {
		return err
	}
	j, err = enter(c, joinCreateMsg(code, "C"))
	if err != nil {
		return fmt.Errorf("join with create of a held room: %w", err)
	}
	if r := obj(j, "room"); num(r, "rev") != 2 || len(participants(r)) != 2 || hostOf(r) != b.PeerID {
		return fmt.Errorf("join with create of a held room: %v; want rev 2 with B as host", r)
	}
	d, _, err := x.join(code, "D")
	if err != nil {
		return fmt.Errorf("join of a room that a join with create made: %w", err)
	}

	// Two at once for a free code: one makes the room, the other joins it.
	two := freeCode(a.Code[0])
	p, err := x.dial("P")
	if err != nil {
		return err
	}
	q, err := x.dial("Q")
	if err != nil {
		return err
	}
	if err := errs(p.send(joinCreateMsg(two, "P")), q.send(joinCreateMsg(two, "Q"))); err != nil {
		return err
	}
	jp, errP := entered(p, "join")
	jq, errQ := entered(q, "join")
	if err := errs(errP, errQ); err != nil {
		return fmt.Errorf("two joins with create at once for a free code: %w", err)
	}
	maker, rm, rg := p, obj(jp, "room"), obj(jq, "room")
	if num(rg, "rev") == 1 {
		maker, rm, rg = q, rg, rm
	}
	if str(jp, "code") != two || str(jq, "code") != two || num(rm, "rev") != 1 || hostOf(rm) != maker.PeerID ||
		num(rg, "rev") != 2 || hostOf(rg) != maker.PeerID {
		return fmt.Errorf("two joins with create at once for %s: P got %v, Q got %v; want one room that one of them "+
			"made and the other joined", two, obj(jp, "room"), obj(jq, "room"))
	}

	// After end the code is free: C, reading closed, makes the room again, and B joins it.
	_ = b.send(Msg{"type": "end"})
	for _, m := range []*Client{b, c, d} {
		if _, err := m.waitFor(reply, "closed ended", isType("closed")); err != nil {
			return err
		}
	}
	j, err = enter(c, joinCreateMsg(typed, "C"))
	if err != nil {
		return fmt.Errorf("join with create of an ended room's code, sent on reading closed: %w", err)
	}
	if r := obj(j, "room"); str(j, "code") != code || num(r, "rev") != 1 || len(participants(r)) != 1 ||
		hostOf(r) != c.PeerID {
		return fmt.Errorf("join with create of an ended room's code: code %s, room %v; want a new room with C as host",
			str(j, "code"), r)
	}
	j, err = enter(b, joinCreateMsg(code, "B"))
	if err != nil {
		return fmt.Errorf("the former host's join with create: %w", err)
	}
	if r := obj(j, "room"); num(r, "rev") != 2 || hostOf(r) != c.PeerID {
		return fmt.Errorf("the former host's join with create: %v; want rev 2 with C as host", r)
	}
	return nil
}

// scIPJoins: every address group of a client IP (§3.1) may look up joinBurst codes at once and joinsPerSecond after
// that, times the group's factor, across its connections: an IPv4 address and an IPv6 /64 the per-IP values, a /56
// ipv6Factor56 times them, a /48 ipv6Factor48 times them. A join over the allowance gets rate_limited and takes no
// token from any group; an IPv4-mapped IPv6 address is its IPv4 address (§7.2).
func scIPJoins(x *sc) error {
	addr := nextAddr()
	burst, rate := int(x.lim("joinBurst")), x.lim("joinsPerSecond")
	f56, f48 := int(x.lim("ipv6Factor56")), int(x.lim("ipv6Factor48"))
	room := int(x.lim("messageBurst")) - 2 // joins one connection sends at once
	n := burst + 5
	if n > room {
		return skipf("joinBurst %d doesn't fit in one connection's messageBurst", burst)
	}
	c, err := x.dialFrom("C", addr)
	if err != nil {
		return err
	}
	msgs := make([]Msg, n)
	for i := range msgs {
		msgs[i] = joinMsg("ZZZZZZ", "C")
	}
	t0 := time.Now()
	if err := c.sendBatch(msgs...); err != nil {
		return err
	}
	found, limited := 0, 0
	for found+limited < n {
		m, err := c.waitFor(reply, "answer to a join", isType("error"))
		if err != nil {
			return fmt.Errorf("after %d room_not_found and %d rate_limited: %w", found, limited, err)
		}
		switch {
		case isError("room_not_found")(m) && limited == 0:
			found++
		case isError("room_not_found")(m):
			return fmt.Errorf("room_not_found after %d rate_limited: the allowance refilled within the batch", limited)
		case isError("rate_limited")(m):
			limited++
		default:
			return fmt.Errorf("answer to a join of an unknown code: %s", brief(m))
		}
		if str(m, "re") != "join" {
			return fmt.Errorf("%s re %v, want join", str(m, "code"), m["re"])
		}
	}
	refill := int(rate*time.Since(t0).Seconds()) + 1
	if found < burst || found > burst+refill || limited == 0 {
		return fmt.Errorf("%d joins at once: %d looked up, %d rate_limited; want %d–%d looked up, the rest rate_limited",
			n, found, limited, burst, burst+refill)
	}
	// The allowance belongs to the IP, not the connection, and the IPv4-mapped form is the same IP.
	d, err := x.dialFrom("D", mapped(addr))
	if err != nil {
		return err
	}
	_ = d.send(joinMsg("ZZZZZZ", "D"))
	if err := expectErr(d, "rate_limited", "join"); err != nil {
		return fmt.Errorf("another connection from the IPv4-mapped form of the same IP: %w", err)
	}
	e, err := x.dial("E")
	if err != nil {
		return err
	}
	_ = e.send(joinMsg("ZZZZZZ", "E"))
	if err := expectErr(e, "room_not_found", "join"); err != nil {
		return fmt.Errorf("another IP: %w", err)
	}
	time.Sleep(time.Duration(float64(time.Second)/rate) + 100*time.Millisecond)
	_ = d.send(joinMsg("ZZZZZZ", "D"))
	if err := expectErr(d, "room_not_found", "join"); err != nil {
		return fmt.Errorf("once the allowance refilled: %w", err)
	}
	if err := ipv6Joins56(x, burst, rate, f56, room); err != nil {
		return err
	}
	return ipv6Joins48(x, burst, rate, f56, f48)
}

// ipv6Joins56 checks a /56's join bucket: ipv6Factor56 times joinBurst, refilled at ipv6Factor56 times joinsPerSecond.
// Its first /64 overruns its own bucket; the refused joins must take nothing from the /56, so its next /64s get their
// whole burst, and one more /64 then finds the /56 empty.
func ipv6Joins56(x *sc, burst int, rate float64, f56, room int) error {
	n := newV6Net()
	var cs []*Client
	for h := range f56 + 1 {
		c, err := x.dialFrom(fmt.Sprintf("H%d", h), n.at(0, h, 0))
		if err != nil {
			return err
		}
		cs = append(cs, c)
	}
	t0 := time.Now()
	found, limited, err := lookupBatch(cs[0], room)
	if err != nil {
		return err
	}
	if found < burst || limited == 0 {
		return fmt.Errorf("%d joins at once from a /64: %d looked up, %d rate_limited; want %d looked up, the rest "+
			"rate_limited", room, found, limited, burst)
	}
	sum := found
	for h := 1; h < f56; h++ {
		f, _, err := lookupBatch(cs[h], burst)
		if err != nil {
			return err
		}
		if f != burst {
			return fmt.Errorf("/64 number %d of a /56 whose first /64 had %d joins refused: %d of %d joins looked up; "+
				"want all (the /56 allows %d, and refused joins take no token)", h+1, limited, f, burst, burst*f56)
		}
		sum += f
	}
	last := cs[f56]
	tSend := time.Now()
	found, limited, err = lookupBatch(last, burst)
	if err != nil {
		return err
	}
	tEmpty := time.Now()
	allow := max(0, burst*f56+int(rate*float64(f56)*time.Since(t0).Seconds())+1-sum)
	if limited == 0 || found > allow {
		return fmt.Errorf("a new /64 in a /56 after %d joins (ipv6Factor56 %d): %d looked up, %d rate_limited; want at "+
			"most %d looked up (refill), the rest rate_limited", sum, f56, found, limited, allow)
	}
	// The /56 refills at f56 × joinsPerSecond: after wait, about that many joins more pass.
	const wait = 1500 * time.Millisecond
	r56 := rate * float64(f56)
	probe := int(r56*wait.Seconds()*2) + 2
	if probe > burst-found || probe > room || r56*wait.Seconds() >= float64(burst*f56) {
		return nil // the refill can't be told apart with these limits
	}
	time.Sleep(time.Until(tEmpty.Add(wait)))
	t2 := time.Now()
	found2, _, err := lookupBatch(last, probe)
	if err != nil {
		return err
	}
	lo := int(r56 * t2.Sub(tEmpty).Seconds())
	hi := int(1 + r56*time.Since(tSend).Seconds())
	if found2 < lo || found2 > hi {
		return fmt.Errorf("%d joins from a /64 of an empty /56 %v later: %d looked up; want %d–%d (the /56 refills at "+
			"%g per second, ipv6Factor56 × joinsPerSecond)", probe, t2.Sub(tEmpty).Round(time.Millisecond), found2, lo, hi, r56)
	}
	return nil
}

// ipv6Joins48 checks a /48's join bucket: ipv6Factor48 times joinBurst, spread over /56s at their own limit.
func ipv6Joins48(x *sc, burst int, rate float64, f56, f48 int) error {
	n := newV6Net()
	var cs []*Client
	for k := range f48 + 1 {
		at := n.at(k/f56, k%f56, 0)
		if k == f48 {
			at = n.at(255, 0, 0)
		}
		c, err := x.dialFrom(fmt.Sprintf("H%d", k), at)
		if err != nil {
			return err
		}
		cs = append(cs, c)
	}
	t0 := time.Now()
	for _, c := range cs[:f48] {
		if err := sendLookups(c, burst); err != nil {
			return err
		}
	}
	sum := 0
	for k, c := range cs[:f48] {
		f, _, err := countLookups(c, burst)
		if err != nil {
			return err
		}
		if f != burst {
			return fmt.Errorf("/64 number %d of a /48 that allows %d joins: %d of %d joins looked up, want all", k+1,
				burst*f48, f, burst)
		}
		sum += f
	}
	found, limited, err := lookupBatch(cs[f48], burst)
	if err != nil {
		return err
	}
	allow := max(0, burst*f48+int(rate*float64(f48)*time.Since(t0).Seconds())+1-sum)
	if limited == 0 || found > allow {
		return fmt.Errorf("a new /56 in a /48 after %d joins (ipv6Factor48 %d): %d looked up, %d rate_limited; want at "+
			"most %d looked up (refill), the rest rate_limited", sum, f48, found, limited, allow)
	}
	return nil
}

// lookupBatch sends n joins of an unknown code from c at once and counts the answers: room_not_found (looked up) and
// rate_limited.
func lookupBatch(c *Client, n int) (found, limited int, err error) {
	if err := sendLookups(c, n); err != nil {
		return 0, 0, err
	}
	return countLookups(c, n)
}

func sendLookups(c *Client, n int) error {
	msgs := make([]Msg, n)
	for i := range msgs {
		msgs[i] = joinMsg("ZZZZZZ", c.name)
	}
	return c.sendBatch(msgs...)
}

// countLookups reads the answers to n joins of an unknown code, each room_not_found or rate_limited with re join.
func countLookups(c *Client, n int) (found, limited int, err error) {
	for found+limited < n {
		m, err := c.waitFor(reply, "answer to a join", isType("error"))
		if err != nil {
			return found, limited, fmt.Errorf("after %d room_not_found and %d rate_limited: %w", found, limited, err)
		}
		switch {
		case isError("room_not_found")(m):
			found++
		case isError("rate_limited")(m):
			limited++
		default:
			return found, limited, fmt.Errorf("%s: answer to a join of an unknown code: %s", c.name, brief(m))
		}
		if str(m, "re") != "join" {
			return found, limited, fmt.Errorf("%s: %s re %v, want join", c.name, str(m, "code"), m["re"])
		}
	}
	return found, limited, nil
}

// scJoinTotal: besides the per-IP buckets, the relay has one join bucket of joinBurstTotal tokens refilled at
// joinsPerSecondTotal. A join takes a token from every bucket or from none: one refused by its IP's bucket leaves the
// relay's untouched, and one refused by the relay's leaves its IP's untouched (§7.2). It runs on a relay of its own
// with 3 joins per IP, 5 on the relay, the relay's refilling at 2 per second and the IPs' hardly at all.
func scJoinTotal(x *sc) error {
	p, err := x.startRelay(x.scale, "--trust-proxy", "--join-burst", "3", "--joins-per-second", "0.01",
		"--join-burst-total", "5", "--joins-per-second-total", "2")
	if err != nil {
		return err
	}
	y := x.on(p, x.scale)
	if t, r := y.limits["joinBurstTotal"], y.limits["joinsPerSecondTotal"]; t != 5.0 || r != 2.0 {
		return fmt.Errorf("/v1/info limits.joinBurstTotal = %v, joinsPerSecondTotal = %v with --join-burst-total 5 "+
			"--joins-per-second-total 2", t, r)
	}
	var cs []*Client
	for _, name := range []string{"A", "B", "C"} {
		c, err := y.dialFrom(name, nextAddr())
		if err != nil {
			return err
		}
		cs = append(cs, c)
	}
	a, b, c := cs[0], cs[1], cs[2]
	expect := func(cl *Client, n, want int, what string) error {
		found, limited, err := lookupBatch(cl, n)
		if err != nil {
			return err
		}
		if found != want {
			return fmt.Errorf("%s: %d of %d joins looked up, %d rate_limited; want %d looked up", what, found, n, limited, want)
		}
		return nil
	}
	t0 := time.Now()
	if err := expect(a, 5, 3, "5 joins from A (IP bucket 3, relay bucket 5)"); err != nil {
		return err
	}
	if err := expect(b, 3, 2, "3 joins from B right after (A's 2 refused joins took no relay token)"); err != nil {
		return fmt.Errorf("%w; %v after the first join", err, time.Since(t0).Round(time.Millisecond))
	}
	// The relay's bucket refills to 5 within 2.5 s; B's own holds the one token the refused join didn't take.
	time.Sleep(2800 * time.Millisecond)
	if err := expect(b, 3, 1, "3 joins from B once the relay's bucket refilled (B's join refused by the relay's "+
		"bucket took no token of B's)"); err != nil {
		return err
	}
	return expect(c, 3, 3, "3 joins from C right after (B's 2 refused joins took no relay token)")
}

// scResume: resume refuses a wrong code, peer ID or token alike; it restores the participant with its fields and
// place, and publishes a snapshot; a resume of a connected participant takes over and closes the old connection with
// 4000 and nothing else (§7.3).
func scResume(x *sc) error {
	cs, err := x.roomOf(3)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	_ = b.send(Msg{"type": "status", "epoch": 0, "player": "loading", "match": Msg{"state": "searching"}, "offsetMs": 250, "rttMs": 30})
	if _, err := a.waitFor(reply, "B loading", playerIs(b.PeerID, "loading")); err != nil {
		return err
	}
	b.drop()
	if _, err := a.waitFor(reply, "B disconnected", connectedIs(b.PeerID, false)); err != nil {
		return err
	}
	bad := []byte(b.Token)
	bad[len(bad)/2] ^= 1
	for _, m := range []Msg{
		{"type": "resume", "code": b.Code, "peerId": b.PeerID, "token": string(bad)},
		{"type": "resume", "code": b.Code, "peerId": b.PeerID + "x", "token": b.Token},
		{"type": "resume", "code": "ZZZZZZ", "peerId": b.PeerID, "token": b.Token},
		{"type": "resume", "code": b.Code, "peerId": a.PeerID, "token": b.Token},
	} {
		r, err := x.dial("R")
		if err != nil {
			return err
		}
		j, err := enter(r, m)
		if err := refusedWith(j, err, "resume_failed", fmt.Sprintf("resume %v", m)); err != nil {
			return err
		}
		if str(j, "re") != "resume" {
			return fmt.Errorf("resume_failed re %v", j["re"])
		}
	}
	b2, j, err := x.resume(b, "B2")
	if err != nil {
		return err
	}
	r := obj(j, "room")
	ps := participants(r)
	if str(obj(j, "you"), "peerId") != b.PeerID || str(obj(j, "you"), "token") != b.Token || len(ps) != 3 ||
		str(ps[1], "peerId") != b.PeerID {
		return fmt.Errorf("joined by resume: you %v, participants %v; want B second, same peer ID and token", j["you"], ps)
	}
	if p := ps[1]; p["connected"] != true || p["host"] != false || str(p, "player") != "loading" ||
		str(obj(p, "match"), "state") != "searching" || num(p, "offsetMs") != 250 {
		return fmt.Errorf("resumed B = %v; want connected with player, match and offsetMs kept", p)
	}
	if err := mediaNull(j); err != nil {
		return err
	}
	ar, err := a.waitFor(reply, "B connected again", isType("room"))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(ar, withType(r)) {
		return fmt.Errorf("A's room after the resume %v, want the snapshot in B's joined %v", brief(ar), brief(withType(r)))
	}
	// Takeover: B is connected (as b2); a resume on another connection takes it over.
	b3, j, err := x.resume(b, "B3")
	if err != nil {
		return err
	}
	if err := closedWith(b2, 4000, true); err != nil {
		return fmt.Errorf("old connection after a takeover: %w", err)
	}
	r = obj(j, "room")
	// No snapshot field changed, yet the resume publishes one (§9.2).
	ar, err = a.waitFor(reply, "snapshot after the takeover", isType("room"))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(ar, withType(r)) || participant(r, b.PeerID)["connected"] != true {
		return fmt.Errorf("A's room after the takeover %v, want the snapshot in B3's joined %v", brief(ar), brief(withType(r)))
	}
	_ = b3.send(Msg{"type": "ping", "id": "b3"})
	_, err = b3.waitFor(reply, "pong on the new connection", isType("pong"))
	return err
}

func mediaNull(j Msg) error {
	if v, ok := j["media"]; !ok || v != nil {
		return fmt.Errorf("joined.media = %v (present %v), want null", v, ok)
	}
	return nil
}

// scPing: pong echoes id unchanged and carries an int serverTime from a monotonic clock (§7.4, §8.4).
func scPing(x *sc) error {
	c, err := x.dial("C")
	if err != nil {
		return err
	}
	last := -1.0
	for _, id := range []any{42, "abc", 7.5, -3, "", 9007199254740991} {
		if err := c.send(Msg{"type": "ping", "id": id}); err != nil {
			return err
		}
		p, err := c.waitFor(reply, "pong", isType("pong"))
		if err != nil {
			return err
		}
		want := normalize(Msg{"id": id})["id"]
		if !reflect.DeepEqual(p["id"], want) {
			return fmt.Errorf("pong id %v (%T), want %v", p["id"], p["id"], want)
		}
		st, ok := p["serverTime"].(float64)
		if !ok || st != math.Trunc(st) || st < last {
			return fmt.Errorf("serverTime %v after %v", p["serverTime"], last)
		}
		last = st
	}
	return nil
}

// scStatusFields: status stores player, match (deltaMs absent when not sent) and offsetMs; a status that changes only
// epoch or rttMs publishes nothing; offsetMs is within ±604800000, both ends included, and a status past either end
// is refused with invalid_message and changes nothing (§7.5).
func scStatusFields(x *sc) error {
	cs, err := x.roomOf(2)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	_ = a.send(Msg{"type": "status", "epoch": 0, "player": "loading", "match": Msg{"state": "searching"}, "offsetMs": 250, "rttMs": 10})
	r, err := b.waitFor(reply, "A loading", playerIs(a.PeerID, "loading"))
	if err != nil {
		return err
	}
	p := participant(r, a.PeerID)
	if _, has := obj(p, "match")["deltaMs"]; str(obj(p, "match"), "state") != "searching" || has || num(p, "offsetMs") != 250 {
		return fmt.Errorf("participant after status: %v; want match {state: searching} without deltaMs, offsetMs 250", p)
	}
	_ = a.send(Msg{"type": "status", "epoch": 0, "player": "loading", "match": Msg{"state": "searching"}, "offsetMs": 250, "rttMs": 99})
	_ = a.send(Msg{"type": "status", "epoch": 3, "player": "loading", "match": Msg{"state": "searching"}, "offsetMs": 250})
	// The next snapshot B gets must be the one for this mismatched status, not one for the two above.
	_ = a.send(Msg{"type": "status", "epoch": 3, "player": "loading", "match": Msg{"state": "mismatched", "deltaMs": -120}, "offsetMs": -40})
	r, err = b.waitFor(reply, "room after the statuses", isType("room"))
	if err != nil {
		return err
	}
	p = participant(r, a.PeerID)
	if m := obj(p, "match"); str(m, "state") != "mismatched" || num(m, "deltaMs") != -120 || num(p, "offsetMs") != -40 {
		return fmt.Errorf("first room after an rttMs-only and an epoch-only status, then a mismatched one: %v", p)
	}
	for _, o := range []int{604_800_001, 604_800_000, -604_800_001, -604_800_000} {
		_ = a.send(Msg{"type": "status", "epoch": 3, "player": "loading", "match": nil, "offsetMs": o})
		if o > 604_800_000 || o < -604_800_000 {
			if err := expectErr(a, "invalid_message", "status"); err != nil {
				return fmt.Errorf("status with offsetMs %d: %w", o, err)
			}
			continue
		}
		// The next snapshot B gets is the one for this status, not one for the refused status before it.
		r, err = b.waitFor(reply, fmt.Sprintf("room after offsetMs %d", o), isType("room"))
		if err != nil {
			return err
		}
		if got := num(participant(r, a.PeerID), "offsetMs"); got != float64(o) {
			return fmt.Errorf("first room after a status with offsetMs %d (after one past the bound): offsetMs %v", o, got)
		}
	}
	return nil
}

// scPause: pause keeps the sender's position within pauseToleranceMs of P(now), capped at durationMs, else uses
// P(now); a pause while paused and a play while playing change nothing (§7.6, §7.7).
func scPause(x *sc) error {
	cs, err := x.playing(2, 10, 7_200_000)
	if err != nil {
		return err
	}
	a := cs[0]
	clk, err := a.clock()
	if err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)
	room := a.lastRoom()
	t0 := time.Now()
	_ = a.send(Msg{"type": "pause"})
	r, err := a.waitFor(reply, "paused", isPhase("paused"))
	if err != nil {
		return err
	}
	t1 := time.Now()
	if k, by := actionOf(r); k != "pause" || by != a.PeerID {
		return fmt.Errorf("pause action %s/%v", k, by)
	}
	if num(r, "atMs") != num(room, "atMs") || num(r, "rate") != num(room, "rate") || len(strList(r, "waitingOn")) != 0 {
		return fmt.Errorf("pause changed atMs or rate: %s after %s", brief(r), brief(room))
	}
	if err := posBetween(num(r, "positionMs"), room, clk.early(t0), clk.late(t1), "pause position"); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "pause"})
	if err := a.expectNone(300*time.Millisecond, "snapshot for a pause while paused", isType("room")); err != nil {
		return err
	}
	if _, err := playNow(a, a); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "play"})
	if err := a.expectNone(300*time.Millisecond, "snapshot for play while playing", isType("room")); err != nil {
		return err
	}
	// 150 ms ahead of the room is well within the tolerance.
	cur := posAt(a.lastRoom(), clk.mid(time.Now()))
	given := int(cur) + 150
	_ = a.send(Msg{"type": "pause", "positionMs": given})
	r, err = a.waitFor(reply, "paused at the given position", isPhase("paused"))
	if err != nil {
		return err
	}
	if num(r, "positionMs") != float64(given) {
		return fmt.Errorf("pause positionMs %v, want the given %d (within pauseToleranceMs of the room)", r["positionMs"], given)
	}
	if _, err := playNow(a, a); err != nil {
		return err
	}
	time.Sleep(200 * time.Millisecond)
	room = a.lastRoom()
	tol := x.pol("pauseToleranceMs")
	t0 = time.Now()
	_ = a.send(Msg{"type": "pause", "positionMs": int(posAt(room, clk.mid(t0)) + 2.5*tol)})
	r, err = a.waitFor(reply, "paused", isPhase("paused"))
	if err != nil {
		return err
	}
	t1 = time.Now()
	if err := posBetween(num(r, "positionMs"), room, clk.early(t0), clk.late(t1),
		"pause position when the given one is 2.5 × pauseToleranceMs off"); err != nil {
		return err
	}
	// Near the end, a given position past durationMs but within the tolerance is capped at durationMs.
	_ = a.send(Msg{"type": "seek", "positionMs": 7_200_000 - 1000})
	if _, err := a.waitFor(reply, "seek near the end", isRoom(func(m Msg) bool { return num(m, "positionMs") == 7_199_000 })); err != nil {
		return err
	}
	if _, err := playNow(a, a); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "pause", "positionMs": 7_200_000 + 500})
	r, err = a.waitFor(reply, "paused", isPhase("paused"))
	if err != nil {
		return err
	}
	if num(r, "positionMs") != 7_200_000 {
		return fmt.Errorf("pause at durationMs + 500 (within the tolerance): positionMs %v, want durationMs 7200000", r["positionMs"])
	}
	return nil
}

// scSeek: seek is capped at durationMs; paused stays paused at the target; playing and waiting enter waiting at it;
// play while waiting changes nothing; pause from waiting pauses at positionMs (§7.8, §12.1).
func scSeek(x *sc) error {
	cs, err := x.playing(2, 10, 7_200_000)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	// Everyone ready: waitingOn is empty, so the seek releases at once, in its one snapshot.
	_ = a.send(Msg{"type": "seek", "positionMs": 25000})
	r, err := a.waitFor(reply, "room after seek", isType("room"))
	if err != nil {
		return err
	}
	if k, by := actionOf(r); str(r, "phase") != "playing" || num(r, "positionMs") != 25000 ||
		len(strList(r, "waitingOn")) != 0 || k != "seek" || by != a.PeerID {
		return fmt.Errorf("seek while playing with everyone ready: %s; want playing at 25000", brief(r))
	}
	_ = a.send(Msg{"type": "pause"})
	if _, err := a.waitFor(reply, "paused", isPhase("paused")); err != nil {
		return err
	}
	for _, target := range []float64{20000, 20000} { // a repeated action publishes again (§9.1)
		_ = a.send(Msg{"type": "seek", "positionMs": target})
		r, err = a.waitFor(reply, "seek while paused", isType("room"))
		if err != nil {
			return err
		}
		if k, by := actionOf(r); str(r, "phase") != "paused" || num(r, "positionMs") != target || k != "seek" || by != a.PeerID {
			return fmt.Errorf("seek while paused: %s", brief(r))
		}
	}
	_ = a.send(Msg{"type": "seek", "positionMs": 99_999_999})
	r, err = a.waitFor(reply, "seek past the end", isType("room"))
	if err != nil {
		return err
	}
	if num(r, "positionMs") != 7_200_000 {
		return fmt.Errorf("seek past the end: positionMs %v, want durationMs 7200000", r["positionMs"])
	}
	// Playing from durationMs would end at once (§12.11), so come back from the end first.
	_ = a.send(Msg{"type": "seek", "positionMs": 20000})
	if _, err := a.waitFor(reply, "seek back", isRoom(func(m Msg) bool { return num(m, "positionMs") == 20000 })); err != nil {
		return err
	}
	if _, err := playNow(a, a); err != nil {
		return err
	}
	_ = status(b, 1, "loading", matched(), 10)
	if _, err := a.waitFor(reply, "B loading", playerIs(b.PeerID, "loading")); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "seek", "positionMs": 30000})
	r, err = a.waitFor(reply, "waiting after seek", isType("room"))
	if err != nil {
		return err
	}
	if str(r, "phase") != "waiting" || num(r, "positionMs") != 30000 || !equalStrings(strList(r, "waitingOn"), []string{b.PeerID}) {
		return fmt.Errorf("seek while playing: %s", brief(r))
	}
	_ = a.send(Msg{"type": "play"})
	if err := a.expectNone(300*time.Millisecond, "snapshot for play while waiting", isType("room")); err != nil {
		return err
	}
	_ = status(b, 1, "ready", matched(), 10)
	r, err = a.waitFor(reply, "room after B ready", isType("room"))
	if err != nil {
		return err
	}
	if k, _ := actionOf(r); str(r, "phase") != "playing" || num(r, "positionMs") != 30000 || k != "" {
		return fmt.Errorf("first room after the last ready: %s; want playing at 30000, no action", brief(r))
	}
	_ = status(b, 1, "loading", matched(), 10)
	if _, err := a.waitFor(reply, "B loading", playerIs(b.PeerID, "loading")); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "seek", "positionMs": 35000})
	if _, err := a.waitFor(reply, "waiting on B", waitingOnIs(b.PeerID)); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "pause"})
	r, err = a.waitFor(reply, "room after pause in waiting", isType("room"))
	if err != nil {
		return err
	}
	if k, by := actionOf(r); str(r, "phase") != "paused" || num(r, "positionMs") != 35000 || k != "pause" ||
		by != a.PeerID || len(strList(r, "waitingOn")) != 0 {
		return fmt.Errorf("pause while waiting: %s; want paused at 35000 (P(now) in waiting), waitingOn []", brief(r))
	}
	return nil
}

// scRate: in playing after atMs, rate re-anchors positionMs and atMs to now; in paused and waiting only rate changes;
// each rate carries action rate, a repeated one too (§7.9, §9.1).
func scRate(x *sc) error {
	cs, err := x.playing(2, 10, 7_200_000)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	clk, err := a.clock()
	if err != nil {
		return err
	}
	time.Sleep(x.polD("startDelayMaxMs") + 200*time.Millisecond) // past the scheduled start
	room := a.lastRoom()
	t0 := time.Now()
	_ = a.send(Msg{"type": "rate", "rate": 1.5})
	r, err := a.waitFor(reply, "rate 1.5", isRoom(func(m Msg) bool { return num(m, "rate") == 1.5 }))
	if err != nil {
		return err
	}
	t1 := time.Now()
	if k, by := actionOf(r); k != "rate" || by != a.PeerID || str(r, "phase") != "playing" {
		return fmt.Errorf("rate snapshot %s", brief(r))
	}
	if err := errs(
		posBetween(num(r, "positionMs"), room, clk.early(t0), clk.late(t1), "re-anchored positionMs"),
		timeBetween(num(r, "atMs"), clk.early(t0), clk.late(t1), "re-anchored atMs"),
	); err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)
	t2 := time.Now()
	_ = a.send(Msg{"type": "pause"})
	p, err := a.waitFor(reply, "paused", isPhase("paused"))
	if err != nil {
		return err
	}
	t3 := time.Now()
	if err := posBetween(num(p, "positionMs"), r, clk.early(t2), clk.late(t3), "position after 400 ms at 1.5×"); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "rate", "rate": 0.5})
	r, err = a.waitFor(reply, "rate while paused", isType("room"))
	if err != nil {
		return err
	}
	if k, _ := actionOf(r); k != "rate" || num(r, "rate") != 0.5 || str(r, "phase") != "paused" ||
		num(r, "positionMs") != num(p, "positionMs") || num(r, "atMs") != num(p, "atMs") {
		return fmt.Errorf("rate while paused: %s after %s; want only rate changed, action rate", brief(r), brief(p))
	}
	_ = status(b, 1, "loading", matched(), 10)
	if _, err := a.waitFor(reply, "B loading", playerIs(b.PeerID, "loading")); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "play"})
	w, err := a.waitFor(reply, "waiting on B", waitingOnIs(b.PeerID))
	if err != nil {
		return err
	}
	for range 2 { // the same rate again changes nothing but still publishes its action
		_ = a.send(Msg{"type": "rate", "rate": 2.0})
		r, err = a.waitFor(reply, "rate while waiting", isType("room"))
		if err != nil {
			return err
		}
		if k, by := actionOf(r); k != "rate" || by != a.PeerID || num(r, "rate") != 2 || str(r, "phase") != "waiting" ||
			num(r, "positionMs") != num(w, "positionMs") || num(r, "atMs") != num(w, "atMs") {
			return fmt.Errorf("rate 2 while waiting: %s after %s; want only rate changed, action rate by A", brief(r), brief(w))
		}
	}
	return nil
}

// scMediaBarrier: setMedia publishes one room with the new epoch, the media, action media and a wait at the given
// position; everyone in the player is waited for until it reports the new epoch; the last ready releases with atMs
// = now + the start delay and no action (§7.10, §8.3, §9.1, §12.4, §12.6).
func scMediaBarrier(x *sc) error {
	cs, err := x.roomOf(2)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	clk, err := a.clock()
	if err != nil {
		return err
	}
	_ = status(a, 0, "loading", nil, 40)
	_ = status(b, 0, "loading", nil, 100)
	if err := waitPlayers(cs, "loading"); err != nil {
		return err
	}
	media := sampleMedia(7_200_000)
	if err := a.send(Msg{"type": "setMedia", "media": media, "positionMs": 5000}); err != nil {
		return err
	}
	for _, c := range cs {
		r, err := c.waitFor(reply, "room after setMedia", isType("room"))
		if err != nil {
			return err
		}
		if err := mediaDiff(r["media"], media); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		k, by := actionOf(r)
		if num(r, "epoch") != 1 || str(r, "phase") != "waiting" || num(r, "positionMs") != 5000 ||
			!equalStrings(strList(r, "waitingOn"), []string{a.PeerID, b.PeerID}) || k != "media" || by != a.PeerID {
			return fmt.Errorf("%s: after setMedia %s", c.name, brief(r))
		}
	}
	_ = status(a, 1, "ready", matched(), 40)
	for _, c := range cs {
		if _, err := c.waitFor(reply, "waitingOn [B]", waitingOnIs(b.PeerID)); err != nil {
			return err
		}
	}
	t0 := time.Now()
	_ = status(b, 1, "ready", matched(), 100)
	r, err := a.waitFor(reply, "room after B ready", isType("room"))
	if err != nil {
		return err
	}
	t1 := time.Now()
	if k, _ := actionOf(r); str(r, "phase") != "playing" || num(r, "positionMs") != 5000 || len(strList(r, "waitingOn")) != 0 || k != "" {
		return fmt.Errorf("first room after the last ready: %s; want the release, with no action", brief(r))
	}
	want := x.startDelay(100)
	return timeBetween(num(r, "atMs"), clk.early(t0)+want, clk.late(t1)+want, "atMs (release time + start delay)")
}

// scMediaEnd: setMedia caps positionMs at durationMs; a release at durationMs ends in the same event, in one snapshot
// (§7.10, §12.3, §12.6 step 5).
func scMediaEnd(x *sc) error {
	a, _, err := x.create("A")
	if err != nil {
		return err
	}
	media := Msg{"kind": "movie", "title": "Short", "durationMs": 5000}
	_ = a.send(Msg{"type": "setMedia", "media": media, "positionMs": 99_999})
	// A has sent no status, so it counts as loading until it reports epoch 1 (§12.3).
	r, err := a.waitFor(reply, "room after setMedia", isType("room"))
	if err != nil {
		return err
	}
	if !hasMedia(r) || str(r, "phase") != "waiting" || num(r, "positionMs") != 5000 || !equalStrings(strList(r, "waitingOn"), []string{a.PeerID}) {
		return fmt.Errorf("setMedia at 99999 of 5000: %s; want the media, waiting on A at 5000", brief(r))
	}
	_ = status(a, 1, "ready", matched(), 10)
	r, err = a.waitFor(reply, "room after A ready", isType("room"))
	if err != nil {
		return err
	}
	if k, by := actionOf(r); str(r, "phase") != "paused" || num(r, "positionMs") != 5000 || k != "ended" || by != nil {
		return fmt.Errorf("release at durationMs: %s; want one snapshot paused at 5000, ended by null", brief(r))
	}
	return a.expectNone(300*time.Millisecond, "a second snapshot for one status", isType("room"))
}

// scClearMedia: clearMedia (host only) bumps epoch and publishes one room with media null, action media and idle, rate
// and atMs kept; without media it does nothing; it drops a held seek (§7.11, §8.3, §12.8).
func scClearMedia(x *sc) error {
	cs, err := x.playing(2, 10, 7_200_000)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	_ = b.send(Msg{"type": "clearMedia"})
	if err := expectErr(b, "forbidden", "clearMedia"); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "rate", "rate": 1.25})
	before, err := a.waitFor(reply, "rate 1.25", isRoom(func(m Msg) bool { return num(m, "rate") == 1.25 }))
	if err != nil {
		return err
	}
	if _, err := b.waitFor(reply, "rate 1.25", isRoom(func(m Msg) bool { return num(m, "rate") == 1.25 })); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "clearMedia"})
	for _, c := range cs {
		r, err := c.waitFor(reply, "room after clearMedia", isType("room"))
		if err != nil {
			return err
		}
		k, by := actionOf(r)
		if v, ok := r["media"]; !ok || v != nil || num(r, "epoch") != 2 || str(r, "phase") != "idle" ||
			num(r, "positionMs") != 0 || len(strList(r, "waitingOn")) != 0 || num(r, "rate") != 1.25 ||
			num(r, "atMs") != num(before, "atMs") || k != "media" || by != a.PeerID {
			return fmt.Errorf("%s: after clearMedia %s (media %v); want media null, epoch 2, idle at 0, rate and atMs "+
				"kept, media by host", c.name, brief(r), r["media"])
		}
	}
	_ = a.send(Msg{"type": "clearMedia"})
	if err := a.expectNone(300*time.Millisecond, "an answer to clearMedia without media", func(m Msg) bool {
		return isType("room")(m) || isType("error")(m)
	}); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "play"})
	if err := expectErr(a, "invalid_message", "play"); err != nil {
		return err
	}
	_, j, err := x.join(a.Code, "C")
	if err != nil {
		return err
	}
	if err := mediaNull(j); err != nil {
		return err
	}
	if _, err := a.waitFor(reply, "C joined", isRoom(func(m Msg) bool { return len(participants(m)) == 3 })); err != nil {
		return err
	}
	// A held seek is dropped by clearMedia.
	if err := setMediaAll([]*Client{a}, a, sampleMedia(7_200_000)); err != nil {
		return err
	}
	time.Sleep(x.polD("seekCoalesceMs") + 50*time.Millisecond)
	if err := a.sendBatch(Msg{"type": "seek", "positionMs": 1000}, Msg{"type": "seek", "positionMs": 2000},
		Msg{"type": "clearMedia"}); err != nil {
		return err
	}
	if _, err := a.waitFor(reply, "idle after clearMedia", isPhase("idle")); err != nil {
		return err
	}
	return a.expectNone(x.polD("seekCoalesceMs")+300*time.Millisecond, "the held seek applied after clearMedia",
		isType("room"))
}

// scTransferHost: transferHost (host only) to a participant makes it host with action host by the sender; to the
// current host or to nobody it changes nothing or is refused (§7.13).
func scTransferHost(x *sc) error {
	cs, err := x.roomOf(2)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	_ = a.send(Msg{"type": "transferHost", "peerId": "nope"})
	if err := expectErr(a, "invalid_message", "transferHost"); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "transferHost", "peerId": a.PeerID})
	if err := a.expectNone(300*time.Millisecond, "snapshot for a transfer to the current host", isType("room")); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "transferHost", "peerId": b.PeerID})
	for _, c := range cs {
		r, err := c.waitFor(reply, "room after transferHost", isType("room"))
		if err != nil {
			return err
		}
		if k, by := actionOf(r); hostOf(r) != b.PeerID || k != "host" || by != a.PeerID {
			return fmt.Errorf("%s: after transferHost %s, host %s; want B host, action host by A", c.name, brief(r), hostOf(r))
		}
	}
	_ = a.send(Msg{"type": "setMedia", "media": sampleMedia(1000)})
	if err := expectErr(a, "forbidden", "setMedia"); err != nil {
		return err
	}
	media := sampleMedia(7_200_000)
	_ = b.send(Msg{"type": "setMedia", "media": media})
	for _, c := range cs {
		m, err := c.waitFor(reply, "media from the new host", hasMedia)
		if err != nil {
			return err
		}
		if err := mediaDiff(m["media"], media); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
	}
	// joined carries the media exactly as the host sent it, for a join and for a resume (§8.1).
	_, j, err := x.join(a.Code, "C")
	if err != nil {
		return err
	}
	if err := mediaDiff(j["media"], media); err != nil {
		return fmt.Errorf("joined.media of a join: %w", err)
	}
	a.drop()
	if _, err := b.waitFor(reply, "A disconnected", connectedIs(a.PeerID, false)); err != nil {
		return err
	}
	_, j, err = x.resume(a, "A2")
	if err != nil {
		return err
	}
	if err := mediaDiff(j["media"], media); err != nil {
		return fmt.Errorf("joined.media of a resume: %w", err)
	}
	return nil
}

// scKick: kick (host only) sends closed kicked to a connected target, removes it (its token stops working, its
// connection stays open outside the room), removes a disconnected target silently, and lets a kicked person join
// again (§7.14).
func scKick(x *sc) error {
	cs, err := x.roomOf(4)
	if err != nil {
		return err
	}
	a, b, c, d := cs[0], cs[1], cs[2], cs[3]
	_ = c.send(Msg{"type": "kick", "peerId": a.PeerID})
	if err := expectErr(c, "forbidden", "kick"); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "kick", "peerId": "nope"})
	if err := expectErr(a, "invalid_message", "kick"); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "kick", "peerId": b.PeerID})
	m, err := b.waitFor(reply, "closed kicked", func(m Msg) bool { return isType("closed")(m) || isType("room")(m) })
	if err != nil {
		return err
	}
	if str(m, "type") != "closed" || str(m, "reason") != "kicked" {
		return fmt.Errorf("kicked B got %s, want closed kicked", brief(m))
	}
	r, err := a.waitFor(reply, "room without B", isType("room"))
	if err != nil {
		return err
	}
	if participant(r, b.PeerID) != nil || len(participants(r)) != 3 {
		return fmt.Errorf("room after the kick %s, want B gone", brief(r))
	}
	if err := b.expectNone(200*time.Millisecond, "message to B after closed", func(Msg) bool { return true }); err != nil {
		return err
	}
	_ = b.send(Msg{"type": "play"})
	if err := expectErr(b, "not_in_room", "play"); err != nil {
		return err
	}
	_, j, err := x.resume(b, "B2")
	if err := refusedWith(j, err, "resume_failed", "resume of a kicked participant"); err != nil {
		return err
	}
	// A disconnected participant is removed without a closed.
	d.drop()
	if _, err := a.waitFor(reply, "D disconnected", connectedIs(d.PeerID, false)); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "kick", "peerId": d.PeerID})
	if _, err := c.waitFor(reply, "room without D", isRoom(func(m Msg) bool { return participant(m, d.PeerID) == nil })); err != nil {
		return err
	}
	// The kicked person joins again, as a new participant with a peer ID the room never had.
	used := map[string]bool{}
	for _, c := range cs {
		used[c.PeerID] = true
	}
	j, err = enter(b, joinMsg(a.Code, "B"))
	if err != nil {
		return fmt.Errorf("join after a kick: %w", err)
	}
	if id := str(obj(j, "you"), "peerId"); used[id] {
		return fmt.Errorf("rejoined with peer ID %q: the relay reused a peer ID", id)
	}
	return nil
}

// scKickSelf: the host may kick itself: it gets closed kicked and the host passes at once; a room left empty by a
// kick is deleted (§7.14, §11.6).
func scKickSelf(x *sc) error {
	cs, err := x.roomOf(3)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	_ = a.send(Msg{"type": "kick", "peerId": a.PeerID})
	m, err := a.waitFor(reply, "closed kicked", func(m Msg) bool { return isType("closed")(m) || isType("room")(m) })
	if err != nil {
		return err
	}
	if str(m, "type") != "closed" || str(m, "reason") != "kicked" {
		return fmt.Errorf("host kicking itself got %s, want closed kicked", brief(m))
	}
	r, err := b.waitFor(reply, "room after the host kicked itself", isType("room"))
	if err != nil {
		return err
	}
	if k, by := actionOf(r); participant(r, a.PeerID) != nil || hostOf(r) != b.PeerID || k != "host" || by != nil {
		return fmt.Errorf("after the host kicked itself: %s, host %s; want B host, action host by null", brief(r), hostOf(r))
	}
	solo, _, err := x.create("Solo")
	if err != nil {
		return err
	}
	_ = solo.send(Msg{"type": "kick", "peerId": solo.PeerID})
	if _, err := solo.waitFor(reply, "closed kicked", isType("closed")); err != nil {
		return err
	}
	_, j, err := x.join(solo.Code, "Late")
	return refusedWith(j, err, "room_not_found", "join a room whose only participant was kicked")
}

// scLock: lock (host only) refuses join with room_locked, keeps resume working, and publishes only when it changes
// (§7.15, §11.7).
func scLock(x *sc) error {
	cs, err := x.roomOf(2)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	_ = b.send(Msg{"type": "lock", "locked": true})
	if err := expectErr(b, "forbidden", "lock"); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "lock", "locked": true})
	if _, err := a.waitFor(reply, "locked", isRoom(func(m Msg) bool { return m["locked"] == true })); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "lock", "locked": true})
	if err := a.expectNone(300*time.Millisecond, "snapshot for an unchanged lock", isType("room")); err != nil {
		return err
	}
	_, j, err := x.join(a.Code, "C")
	if err := refusedWith(j, err, "room_locked", "join a locked room"); err != nil {
		return err
	}
	b.drop()
	if _, err := a.waitFor(reply, "B disconnected", connectedIs(b.PeerID, false)); err != nil {
		return err
	}
	if _, _, err := x.resume(b, "B2"); err != nil {
		return fmt.Errorf("resume into a locked room: %w", err)
	}
	_ = a.send(Msg{"type": "lock", "locked": false})
	if _, err := a.waitFor(reply, "unlocked", isRoom(func(m Msg) bool { return m["locked"] == false })); err != nil {
		return err
	}
	_, _, err = x.join(a.Code, "C2")
	return err
}

// scLeave: leave gets no reply and removes the sender; a leaving host passes the host at once to the connected
// participant earliest in join order, or to the earliest one when nobody is connected; the last one leaving deletes
// the room (§7.16, §11.6).
func scLeave(x *sc) error {
	cs, err := x.roomOf(4)
	if err != nil {
		return err
	}
	a, b, c, d := cs[0], cs[1], cs[2], cs[3]
	b.drop()
	for _, o := range []*Client{a, c, d} {
		if _, err := o.waitFor(reply, "B disconnected", connectedIs(b.PeerID, false)); err != nil {
			return err
		}
	}
	_ = a.send(Msg{"type": "leave"})
	r, err := c.waitFor(reply, "room after the host left", isType("room"))
	if err != nil {
		return err
	}
	if k, by := actionOf(r); participant(r, a.PeerID) != nil || hostOf(r) != c.PeerID || k != "host" || by != nil {
		return fmt.Errorf("after the host left: %s, host %s; want C (first connected), action host by null", brief(r), hostOf(r))
	}
	if err := a.expectNone(200*time.Millisecond, "message after leave", func(Msg) bool { return true }); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "play"})
	if err := expectErr(a, "not_in_room", "play"); err != nil {
		return err
	}
	_, j, err := x.resume(a, "A2")
	if err := refusedWith(j, err, "resume_failed", "resume of a participant that left"); err != nil {
		return err
	}
	if _, err := enter(a, createMsg("A")); err != nil {
		return fmt.Errorf("create after leave: %w", err)
	}
	// Nobody connected but B: the host passes to B, disconnected; B resumes as host. leave gets no reply, so the next
	// steps go out on the same connection, which the relay handles in order.
	_ = d.send(Msg{"type": "leave"})
	if _, err := c.waitFor(reply, "room without D", isRoom(func(m Msg) bool { return participant(m, d.PeerID) == nil })); err != nil {
		return err
	}
	_ = c.send(Msg{"type": "leave"})
	j, err = enter(c, resumeMsg(b))
	if err != nil {
		return err
	}
	if r := obj(j, "room"); hostOf(r) != b.PeerID || len(participants(r)) != 1 {
		return fmt.Errorf("after the connected host left with only disconnected B: %v; want B host", r)
	}
	_ = c.send(Msg{"type": "leave"})
	j, err = enter(c, joinMsg(b.Code, "Late"))
	return refusedWith(j, err, "room_not_found", "join after the last participant left")
}

// scEnd: end (host only) sends closed ended to every connected member and deletes the room (§7.17).
func scEnd(x *sc) error {
	cs, err := x.roomOf(3)
	if err != nil {
		return err
	}
	_ = cs[1].send(Msg{"type": "end"})
	if err := expectErr(cs[1], "forbidden", "end"); err != nil {
		return err
	}
	_ = cs[0].send(Msg{"type": "end"})
	for _, c := range cs {
		m, err := c.waitFor(reply, "closed ended", func(m Msg) bool { return isType("closed")(m) || isType("room")(m) })
		if err != nil {
			return err
		}
		if str(m, "type") != "closed" || str(m, "reason") != "ended" {
			return fmt.Errorf("%s: got %s, want closed ended", c.name, brief(m))
		}
	}
	_, j, err := x.join(cs[0].Code, "Late")
	if err := refusedWith(j, err, "room_not_found", "join an ended room"); err != nil {
		return err
	}
	_, j, err = x.resume(cs[1], "B2")
	if err := refusedWith(j, err, "resume_failed", "resume into an ended room"); err != nil {
		return err
	}
	_ = cs[1].send(Msg{"type": "ping", "id": "open"})
	if _, err := cs[1].waitFor(reply, "pong after closed", isType("pong")); err != nil {
		return errors.New("connection not open after closed ended")
	}
	return nil
}
