package main

import (
	"fmt"
	"math"
	"os"
	"time"
)

// SPEC §11: rooms and roles.

// scControl: with control host only the host may play, pause, seek and rate; host-only commands are the host's; with
// control everyone anyone controls; setControl publishes only a change, and sets no action (§7.12, §9.1, §11.3).
func scControl(x *sc) error {
	cs, err := x.playing(2, 10, 7_200_000)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	for _, m := range []Msg{
		{"type": "pause"}, {"type": "seek", "positionMs": 1000}, {"type": "rate", "rate": 1.5},
		{"type": "setMedia", "media": sampleMedia(1000)}, {"type": "clearMedia"}, {"type": "setControl", "mode": "everyone"},
		{"type": "transferHost", "peerId": b.PeerID}, {"type": "kick", "peerId": a.PeerID}, {"type": "lock", "locked": true},
		{"type": "end"},
	} {
		_ = b.send(m)
		if err := expectErr(b, "forbidden", str(m, "type")); err != nil {
			return err
		}
	}
	_ = a.send(Msg{"type": "pause"})
	if _, err := a.waitFor(reply, "paused", isPhase("paused")); err != nil {
		return err
	}
	// play is valid from paused, so this forbidden can only come from the permission check.
	_ = b.send(Msg{"type": "play"})
	if err := expectErr(b, "forbidden", "play"); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "setControl", "mode": "host"})
	if err := a.expectNone(300*time.Millisecond, "snapshot for an unchanged control", isType("room")); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "setControl", "mode": "everyone"})
	for _, c := range cs {
		r, err := c.waitFor(reply, "room after setControl", isType("room"))
		if err != nil {
			return err
		}
		if k, _ := actionOf(r); str(r, "control") != "everyone" || k != "" {
			return fmt.Errorf("%s: after setControl everyone %s; want control everyone, no action", c.name, brief(r))
		}
	}
	if _, err := playNow(a, b); err != nil {
		return err
	}
	_ = b.send(Msg{"type": "pause"})
	if _, err := a.waitFor(reply, "paused by B", isRoom(func(m Msg) bool {
		k, by := actionOf(m)
		return str(m, "phase") == "paused" && k == "pause" && by == b.PeerID
	})); err != nil {
		return err
	}
	_ = b.send(Msg{"type": "setControl", "mode": "host"})
	return expectErr(b, "forbidden", "setControl")
}

// scPromotion: after hostPromotionMs of host absence the connected participant earliest in join order becomes host,
// with action host by null; the old host resumes as a plain participant (§11.4).
func scPromotion(x *sc) error {
	cs, err := x.roomOf(3)
	if err != nil {
		return err
	}
	a, b, c := cs[0], cs[1], cs[2]
	t := time.Now()
	a.drop()
	r, err := b.waitFor(x.polD("hostPromotionMs")+3*time.Second, "promotion", isRoom(func(m Msg) bool { return hostOf(m) != a.PeerID }))
	if err != nil {
		return err
	}
	if k, by := actionOf(r); hostOf(r) != b.PeerID || k != "host" || by != nil {
		return fmt.Errorf("promotion: %s, host %s; want B, action host by null", brief(r), hostOf(r))
	}
	if err := within(time.Since(t), x.polD("hostPromotionMs")-slackD, x.polD("hostPromotionMs")+800*time.Millisecond, "host promotion"); err != nil {
		return err
	}
	if _, err := c.waitFor(reply, "B host", isRoom(func(m Msg) bool { return hostOf(m) == b.PeerID })); err != nil {
		return err
	}
	_, j, err := x.resume(a, "A2")
	if err != nil {
		return err
	}
	if p := participant(obj(j, "room"), a.PeerID); p["connected"] != true || p["host"] != false || hostOf(obj(j, "room")) != b.PeerID {
		return fmt.Errorf("A resumed after the promotion: %v; want connected, not host", p)
	}
	return nil
}

// scPromotionRules: promotion skips disconnected participants; with nobody connected the host stays; once someone
// connects (join or resume) after hostPromotionMs of absence, promotion runs then (§11.4).
func scPromotionRules(x *sc) error {
	hp := x.polD("hostPromotionMs")
	// Room 2: host and member both gone; the member resumes after hostPromotionMs and becomes host.
	r2, err := x.roomOf(2)
	if err != nil {
		return err
	}
	// Room 3: host and member both gone; the host resumes after hostPromotionMs and is still host.
	r3, err := x.roomOf(2)
	if err != nil {
		return err
	}
	// Room 4: the host alone, gone; someone joins after hostPromotionMs and becomes host.
	r4, _, err := x.create("H4")
	if err != nil {
		return err
	}
	for _, cs := range [][]*Client{r2, r3} {
		cs[0].drop()
		if _, err := cs[1].waitFor(reply, "host disconnected", connectedIs(cs[0].PeerID, false)); err != nil {
			return err
		}
		cs[1].drop()
	}
	r4.drop()
	emptied := time.Now()
	// Room 1: the first joiner (B) is disconnected, so the host passes to C.
	cs, err := x.roomOf(3)
	if err != nil {
		return err
	}
	a, b, c := cs[0], cs[1], cs[2]
	b.drop()
	for _, o := range []*Client{a, c} {
		if _, err := o.waitFor(reply, "B disconnected", connectedIs(b.PeerID, false)); err != nil {
			return err
		}
	}
	t := time.Now()
	a.drop()
	r, err := c.waitFor(hp+3*time.Second, "promotion", isRoom(func(m Msg) bool { return hostOf(m) != a.PeerID }))
	if err != nil {
		return err
	}
	if k, by := actionOf(r); hostOf(r) != c.PeerID || k != "host" || by != nil {
		return fmt.Errorf("promotion with the first joiner disconnected: %s, host %s; want C (%s)", brief(r), hostOf(r), c.PeerID)
	}
	if err := within(time.Since(t), hp-slackD, hp+800*time.Millisecond, "host promotion"); err != nil {
		return err
	}
	time.Sleep(time.Until(emptied.Add(hp + 500*time.Millisecond)))
	_, j, err := x.resume(r2[1], "M2")
	if err != nil {
		return err
	}
	if hostOf(obj(j, "room")) != r2[1].PeerID {
		return fmt.Errorf("member resuming after hostPromotionMs with nobody connected: host %q; want it promoted",
			hostOf(obj(j, "room")))
	}
	_, j, err = x.resume(r3[0], "H3")
	if err != nil {
		return err
	}
	if hostOf(obj(j, "room")) != r3[0].PeerID {
		return fmt.Errorf("host resuming after hostPromotionMs with nobody connected: host %q, want it to stay %q",
			hostOf(obj(j, "room")), r3[0].PeerID)
	}
	j4, j, err := x.join(r4.Code, "J4")
	if err != nil {
		return err
	}
	if hostOf(obj(j, "room")) != j4.PeerID {
		return fmt.Errorf("join after hostPromotionMs of host absence: host %q; want the joiner promoted",
			hostOf(obj(j, "room")))
	}
	return nil
}

// scTransferToAbsent: transferHost to a disconnected participant starts its absence then; hostPromotionMs later the
// connected participant earliest in join order is promoted (§7.13, §11.4).
func scTransferToAbsent(x *sc) error {
	cs, err := x.roomOf(3)
	if err != nil {
		return err
	}
	a, b, c := cs[0], cs[1], cs[2]
	c.drop()
	for _, o := range []*Client{a, b} {
		if _, err := o.waitFor(reply, "C disconnected", connectedIs(c.PeerID, false)); err != nil {
			return err
		}
	}
	t := time.Now()
	_ = a.send(Msg{"type": "transferHost", "peerId": c.PeerID})
	r, err := b.waitFor(reply, "C host", isType("room"))
	if err != nil {
		return err
	}
	if k, by := actionOf(r); hostOf(r) != c.PeerID || k != "host" || by != a.PeerID {
		return fmt.Errorf("transfer to disconnected C: %s, host %s; want C, action host by A", brief(r), hostOf(r))
	}
	r, err = b.waitFor(x.polD("hostPromotionMs")+3*time.Second, "promotion", isRoom(func(m Msg) bool { return hostOf(m) != c.PeerID }))
	if err != nil {
		return err
	}
	if k, by := actionOf(r); hostOf(r) != a.PeerID || k != "host" || by != nil {
		return fmt.Errorf("promotion after the transfer: %s, host %s; want A, action host by null", brief(r), hostOf(r))
	}
	return within(time.Since(t), x.polD("hostPromotionMs")-slackD, x.polD("hostPromotionMs")+800*time.Millisecond,
		"promotion from the transfer")
}

// scReservation: a participant disconnected for resumeMs is removed as by leave; its token stops working (§11.5,
// §11.6).
func scReservation(x *sc) error {
	cs, err := x.roomOf(2)
	if err != nil {
		return err
	}
	a, b := cs[0], cs[1]
	t := time.Now()
	b.drop()
	if _, err := a.waitFor(reply, "B disconnected", connectedIs(b.PeerID, false)); err != nil {
		return err
	}
	rm := x.polD("resumeMs")
	r, err := a.waitFor(rm+3*time.Second, "B removed", isRoom(func(m Msg) bool { return participant(m, b.PeerID) == nil }))
	if err != nil {
		return err
	}
	if err := within(time.Since(t), rm-slackD, rm+time.Second, "removal of a disconnected participant"); err != nil {
		return err
	}
	if len(participants(r)) != 1 || hostOf(r) != a.PeerID {
		return fmt.Errorf("after B's reservation ran out: %s", brief(r))
	}
	_, j, err := x.resume(b, "B2")
	return refusedWith(j, err, "resume_failed", "resume after resumeMs")
}

// scRoomFull: a room holds maxParticipants, disconnected ones included; one more join gets room_full, checked before
// the lock (§7.2, §11.1).
func scRoomFull(x *sc) error {
	a, _, err := x.create("A")
	if err != nil {
		return err
	}
	max := int(x.lim("maxParticipants"))
	var ps []*Client
	for i := 1; i < max; i++ {
		p, _, err := x.join(a.Code, fmt.Sprintf("P%d", i))
		if err != nil {
			return err
		}
		ps = append(ps, p)
	}
	ps[0].drop()
	if _, err := a.waitFor(reply, "P1 disconnected", connectedIs(ps[0].PeerID, false)); err != nil {
		return err
	}
	_, j, err := x.join(a.Code, "Extra")
	if err := refusedWith(j, err, "room_full", fmt.Sprintf("join number %d (one of %d disconnected)", max+1, max)); err != nil {
		return err
	}
	_ = a.send(Msg{"type": "lock", "locked": true})
	if _, err := a.waitFor(reply, "locked", isRoom(func(m Msg) bool { return m["locked"] == true })); err != nil {
		return err
	}
	_, j, err = x.join(a.Code, "Extra")
	return refusedWith(j, err, "room_full", "join a full, locked room")
}

// scEmptyRoom: a room with nobody connected stays until its last reservation runs out after resumeMs, which removes
// the last participant and so deletes the room (§11.5, §11.6). The room is locked so a join can probe it without
// connecting anyone.
func scEmptyRoom(x *sc) error {
	a, _, err := x.create("A")
	if err != nil {
		return err
	}
	_ = a.send(Msg{"type": "lock", "locked": true})
	if _, err := a.waitFor(reply, "locked", isRoom(func(m Msg) bool { return m["locked"] == true })); err != nil {
		return err
	}
	t := time.Now()
	a.drop()
	rm := x.polD("resumeMs")
	early := time.Duration(0.8 * float64(rm))
	time.Sleep(time.Until(t.Add(early)))
	_, j, err := x.join(a.Code, "Probe")
	if err := refusedWith(j, err, "room_locked", fmt.Sprintf("join %v after the last disconnect", early)); err != nil {
		return err
	}
	time.Sleep(time.Until(t.Add(rm + time.Second)))
	_, j, err = x.join(a.Code, "Late")
	return refusedWith(j, err, "room_not_found", fmt.Sprintf("join %v after the last disconnect", rm+time.Second))
}

// scRoomMaxAge: maxRoomAgeMs after creation every connected member gets closed expired and the room is deleted
// (§11.8). 24 h is hours at the usual test scale, so with --relay-bin a relay of its own runs at a scale where it is
// seconds.
func scRoomMaxAge(x *sc) error {
	url, age := x.url, x.pol("maxRoomAgeMs")
	if x.relay != nil {
		const scale = 0.0001
		p, err := x.startRelay(scale)
		if err != nil {
			return err
		}
		url, age = p.url(), scaledPolicy("maxRoomAgeMs", scale)
	} else if age > 60_000 {
		return skipf("maxRoomAgeMs is %.0f ms; needs --relay-bin or a timer scale where it is under 60 s", age)
	}
	a, err := x.dialAt(url, "A", nextAddr())
	if err != nil {
		return err
	}
	t0 := time.Now()
	if _, err := enter(a, createMsg("A")); err != nil {
		return err
	}
	t1 := time.Now()
	b, err := x.dialAt(url, "B", nextAddr())
	if err != nil {
		return err
	}
	if _, err := enter(b, joinMsg(a.Code, "B")); err != nil {
		return err
	}
	ageD := time.Duration(age * float64(time.Millisecond))
	for _, c := range []*Client{a, b} {
		m, err := c.waitFor(ageD+5*time.Second, "closed expired", isType("closed"))
		if err != nil {
			return err
		}
		if str(m, "reason") != "expired" {
			return fmt.Errorf("%s: closed reason %q, want expired", c.name, str(m, "reason"))
		}
	}
	if err := within(time.Since(t0), ageD-slackD, ageD+t1.Sub(t0)+time.Duration(math.Max(float64(ageD)/20, float64(time.Second))),
		"closed expired after create"); err != nil {
		return err
	}
	j, err := enter(b, joinMsg(a.Code, "Late"))
	return refusedWith(j, err, "room_not_found", "join an expired room")
}

// scClientBlocked: an <app> added to the deny list gets client_blocked on create, join and resume, without a
// restart and without affecting connections already in rooms; removing it lets the app in again (§11.9).
func scClientBlocked(x *sc) error {
	const blocked = "Blockme/2.0"
	h, _, err := x.create("H")
	if err != nil {
		return err
	}
	inRoom := func(name string) (*Client, error) {
		c, err := x.dial(name)
		if err != nil {
			return nil, err
		}
		if _, err := enter(c, Msg{"type": "join", "code": h.Code, "name": name, "client": blocked}); err != nil {
			return nil, err
		}
		return c, nil
	}
	b, err := inRoom("B")
	if err != nil {
		return err
	}
	k, err := inRoom("K")
	if err != nil {
		return err
	}
	b.drop()
	if _, err := h.waitFor(reply, "B disconnected", connectedIs(b.PeerID, false)); err != nil {
		return err
	}
	if err := writeFileAtomic(x.relay.denylist(), []byte("BLOCKME\n")); err != nil {
		return err
	}
	defer func() { _ = writeFileAtomic(x.relay.denylist(), []byte("# owp-conform\n")) }()
	p, err := x.dial("P")
	if err != nil {
		return err
	}
	createBlocked := Msg{"type": "create", "name": "P", "client": blocked}
	if err := poll(reloadWait, func() (bool, error) {
		j, err := enter(p, createBlocked)
		if err == nil {
			_ = p.send(Msg{"type": "end"})
			_, err := p.waitFor(reply, "closed", isType("closed"))
			return false, err
		}
		if j != nil && str(j, "code") == "client_blocked" {
			return str(j, "re") == "create", nil
		}
		return false, err
	}); err != nil {
		return fmt.Errorf("create from a denied app within %v of the deny-list change: %w", reloadWait, err)
	}
	q, err := x.dial("Q")
	if err != nil {
		return err
	}
	j, err := enter(q, Msg{"type": "join", "code": h.Code, "name": "Q", "client": "blockme/9"})
	if err := refusedWith(j, err, "client_blocked", "join from a denied app (another version, other case)"); err != nil {
		return err
	}
	j, err = enter(q, Msg{"type": "join", "code": "ZZZZZZ", "name": "Q", "client": blocked})
	if err := refusedWith(j, err, "client_blocked", "join of an unknown code from a denied app (checked first)"); err != nil {
		return err
	}
	j, err = enter(q, Msg{"type": "join", "code": h.Code, "name": "Q", "client": "Blockme2/2.0"})
	if err != nil {
		return fmt.Errorf("join from an app whose name only starts with a denied one: %w", err)
	}
	_, j, err = x.resume(b, "B2")
	if err := refusedWith(j, err, "client_blocked", "resume of a participant of a denied app"); err != nil {
		return err
	}
	wrong := resumeMsg(b)
	wrong["token"] = "x"
	r, err := x.dial("R")
	if err != nil {
		return err
	}
	j, err = enter(r, wrong)
	if err := refusedWith(j, err, "resume_failed", "resume with a wrong token for a denied app (token checked first)"); err != nil {
		return err
	}
	// A connection already in the room is unaffected.
	_ = status(k, 0, "loading", nil, -1)
	if _, err := h.waitFor(reply, "K loading", playerIs(k.PeerID, "loading")); err != nil {
		return fmt.Errorf("member of a denied app after the change: %w", err)
	}
	if err := writeFileAtomic(x.relay.denylist(), []byte("# owp-conform\n")); err != nil {
		return err
	}
	return poll(reloadWait, func() (bool, error) {
		j, err := enter(p, createBlocked)
		if err == nil {
			return true, nil
		}
		if j != nil && str(j, "code") == "client_blocked" {
			return false, nil
		}
		return false, err
	})
}

// scIPRooms: while an address group of a client IP (§3.1) is the creator of maxRoomsPerIp rooms the relay holds,
// times the group's factor, create from it gets rate_limited: an IPv4 address and an IPv6 /64 the limit, a /56
// ipv6Factor56 times it, a /48 ipv6Factor48 times it. Deleting a room frees its place; an IPv4-mapped IPv6 address is
// its IPv4 address; a refused create counts in no group (§7.1, §11.10). A join with create counts like create: from
// an IP at its limit, it gets rate_limited for a free code, after the check of the code, and joins a held room; the
// room it makes counts for the IP (§7.2).
func scIPRooms(x *sc) error {
	max := int(x.lim("maxRoomsPerIp"))
	f56, f48 := int(x.lim("ipv6Factor56")), int(x.lim("ipv6Factor48"))
	fill := func(n int, addrs func(i int) string) ([]*Client, error) {
		var hosts []*Client
		for i := range n {
			c, _, err := x.createFrom(fmt.Sprintf("H%d", i), addrs(i))
			if err != nil {
				return nil, err
			}
			hosts = append(hosts, c)
		}
		return hosts, nil
	}
	refused := func(name, addr, what string) error {
		_, j, err := x.createFrom(name, addr)
		return refusedWithRe(j, err, "rate_limited", "create", what)
	}
	end := func(c *Client) error {
		_ = c.send(Msg{"type": "end"})
		_, err := c.waitFor(reply, "closed ended", isType("closed"))
		return err
	}
	// IPv4, half of it in the IPv4-mapped form.
	addr := nextAddr()
	hosts, err := fill(max, func(i int) string {
		if i%2 == 1 {
			return mapped(addr)
		}
		return addr
	})
	if err != nil {
		return err
	}
	if err := errs(refused("Over", addr, fmt.Sprintf("create number %d from one IP", max+1)),
		refused("OverMapped", mapped(addr), "create from the IPv4-mapped form of an IP at its limit")); err != nil {
		return err
	}
	jc, err := x.dialFrom("J", addr)
	if err != nil {
		return err
	}
	j, err := enter(jc, joinCreateMsg(freeCode(hosts[0].Code[0]), "J"))
	if err := refusedWithRe(j, err, "rate_limited", "join",
		"join with create of a free code from an IP at its limit"); err != nil {
		return err
	}
	if f := x.foreignCode(); f != "" {
		j, err := enter(jc, joinCreateMsg(f, "J"))
		if err := refusedWithRe(j, err, "room_not_found", "join",
			"join with create of a code the relay doesn't create, from an IP at its limit"); err != nil {
			return err
		}
	}
	if _, err := enter(jc, joinCreateMsg(hosts[0].Code, "J")); err != nil {
		return fmt.Errorf("join with create of a held room from an IP at its limit: %w", err)
	}
	if _, _, err := x.create("Elsewhere"); err != nil {
		return fmt.Errorf("create from another IP: %w", err)
	}
	if err := end(hosts[0]); err != nil {
		return err
	}
	again, _, err := x.createFrom("Again", mapped(addr))
	if err != nil {
		return fmt.Errorf("create after ending one of the IP's rooms: %w", err)
	}
	// The ended room's code is free once its host reads closed, and the room that J makes with it counts for the IP.
	if err := end(again); err != nil {
		return err
	}
	if _, err := enter(jc, joinCreateMsg(again.Code, "J")); err != nil {
		return fmt.Errorf("join with create of an ended room's code from an IP below its limit: %w", err)
	}
	if err := refused("OverAfterJoin", addr,
		"create from an IP at its limit with a room that a join with create made"); err != nil {
		return err
	}
	// IPv6: the first /64 of a /56 reaches its limit and is refused once more, which must count nowhere; the /56's
	// next /64s then bring it to its own limit exactly.
	n := newV6Net()
	if _, err := fill(max, func(i int) string { return n.at(0, 0, i) }); err != nil {
		return err
	}
	if err := refused("Over6", n.at(0, 0, max), "create from a /64 at its limit"); err != nil {
		return err
	}
	var second []*Client
	for h := 1; h < f56; h++ {
		cs, err := fill(max, func(i int) string { return n.at(0, h, i) })
		if err != nil {
			return fmt.Errorf("/64 number %d of a /56 that holds %d rooms, after a refused create: %w", h+1, max*f56, err)
		}
		second = append(second, cs...)
	}
	if err := refused("Over56", n.at(0, f56, 0),
		fmt.Sprintf("create from a new /64 in a /56 with %d rooms (ipv6Factor56 %d)", max*f56, f56)); err != nil {
		return err
	}
	if _, _, err := x.createFrom("Other56", n.at(1, 0, 0)); err != nil {
		return fmt.Errorf("create from another /56 of the same /48: %w", err)
	}
	if len(second) > 0 {
		if err := end(second[0]); err != nil {
			return err
		}
		if _, _, err := x.createFrom("Again56", n.at(0, f56, 0)); err != nil {
			return fmt.Errorf("create from a new /64 after ending a room of its full /56: %w", err)
		}
	}
	// The /48 holds ipv6Factor48 times the limit, spread over its /56s at their own limit.
	m := newV6Net()
	for k := range f48 {
		if _, err := fill(max, func(i int) string { return m.at(k/f56, k%f56, i) }); err != nil {
			return fmt.Errorf("/64 number %d of a /48 that holds %d rooms: %w", k+1, max*f48, err)
		}
	}
	if err := refused("Over48", m.at(255, 0, 0),
		fmt.Sprintf("create from a new /56 in a /48 with %d rooms (ipv6Factor48 %d)", max*f48, f48)); err != nil {
		return err
	}
	if _, _, err := x.createFrom("Other48", newV6Net().at(0, 0, 0)); err != nil {
		return fmt.Errorf("create from another /48: %w", err)
	}
	return nil
}

// scRelayFull: a relay holding maxRooms rooms answers create with relay_full, checked after client_blocked and
// rate_limited, and answers join as usual; a deleted room frees a place. A join with create for a free code gets
// relay_full there in the same order, and one for a held room joins it. Restored rooms count: restarted with a lower
// cap than the rooms it holds, the relay restores every one and refuses create until it holds fewer than the cap
// (§7.1, §7.2, §11.10, §14). It runs on a relay of its own with --max-rooms 3.
func scRelayFull(x *sc) error {
	p, err := x.newRelay(x.scale, "--trust-proxy", "--max-rooms", "3", "--max-rooms-per-ip", "3")
	if err != nil {
		return err
	}
	if err := os.Mkdir(p.stateDir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p.denylist(), []byte("Blocked\n"), 0o644); err != nil {
		return err
	}
	p.args = append(p.args, "--state-dir", p.stateDir(), "--denylist", p.denylist())
	if err := p.start(); err != nil {
		return err
	}
	y := x.on(p, x.scale)
	if v := y.limits["maxRooms"]; v != 3.0 {
		return fmt.Errorf("/v1/info limits.maxRooms = %v with --max-rooms 3", v)
	}
	end := func(c *Client) error {
		_ = c.send(Msg{"type": "end"})
		_, err := c.waitFor(reply, "closed ended", isType("closed"))
		return err
	}
	a := nextAddr()
	var hosts []*Client
	for i := range 3 {
		h, _, err := y.createFrom(fmt.Sprintf("A%d", i), a)
		if err != nil {
			return err
		}
		hosts = append(hosts, h)
	}
	_, j, err := y.createFrom("A3", a)
	if err := refusedWithRe(j, err, "rate_limited", "create", "create from an IP at maxRoomsPerIp on a full relay "+
		"(rate_limited is checked first)"); err != nil {
		return err
	}
	free := freeCode(hosts[0].Code[0])
	_, j, err = y.joinCreateFrom("A4", a, free)
	if err := refusedWithRe(j, err, "rate_limited", "join", "join with create of a free code from an IP at "+
		"maxRoomsPerIp on a full relay (rate_limited is checked first)"); err != nil {
		return err
	}
	b := nextAddr()
	c, err := y.dialFrom("B", b)
	if err != nil {
		return err
	}
	j, err = enter(c, Msg{"type": "create", "name": "B", "client": "Blocked/1.0"})
	if err := refusedWithRe(j, err, "client_blocked", "create", "create from a denied app on a full relay "+
		"(client_blocked is checked first)"); err != nil {
		return err
	}
	j, err = enter(c, createMsg("B"))
	if err := refusedWithRe(j, err, "relay_full", "create", "create on a relay holding maxRooms rooms"); err != nil {
		return err
	}
	j, err = enter(c, Msg{"type": "join", "code": free, "name": "B", "client": "Blocked/1.0", "create": true})
	if err := refusedWithRe(j, err, "client_blocked", "join", "join with create from a denied app on a full relay "+
		"(client_blocked is checked first)"); err != nil {
		return err
	}
	j, err = enter(c, joinCreateMsg(free, "B"))
	if err := refusedWithRe(j, err, "relay_full", "join",
		"join with create of a free code on a relay holding maxRooms rooms"); err != nil {
		return err
	}
	if _, _, err := y.joinCreateFrom("E", nextAddr(), hosts[1].Code); err != nil {
		return fmt.Errorf("join with create of a held room on a full relay: %w", err)
	}
	if _, err := enter(c, joinMsg(hosts[0].Code, "B")); err != nil {
		return fmt.Errorf("join on a full relay: %w", err)
	}
	if err := end(hosts[2]); err != nil {
		return err
	}
	b3, _, err := y.createFrom("B3", b)
	if err != nil {
		return fmt.Errorf("create after a room of a full relay ended: %w", err)
	}
	// Restart with a cap below the 3 rooms held: restoring is never refused, and the restored rooms count.
	if err := p.stop(); err != nil {
		return err
	}
	p.setFlag("--max-rooms", "2")
	if err := p.start(); err != nil {
		return err
	}
	var back []*Client
	for _, h := range []*Client{hosts[0], hosts[1], b3} {
		r, _, err := y.resume(h, h.name+"r")
		if err != nil {
			return fmt.Errorf("resume in a room restored by a relay restarted with --max-rooms 2: %w", err)
		}
		back = append(back, r)
	}
	d := nextAddr()
	_, j, err = y.createFrom("D", d)
	if err := refusedWithRe(j, err, "relay_full", "create", "create with 3 restored rooms and --max-rooms 2"); err != nil {
		return err
	}
	_, j, err = y.joinCreateFrom("D", d, free)
	if err := refusedWithRe(j, err, "relay_full", "join",
		"join with create of a free code with 3 restored rooms and --max-rooms 2"); err != nil {
		return err
	}
	if err := end(back[0]); err != nil {
		return err
	}
	_, j, err = y.createFrom("D", d)
	if err := refusedWithRe(j, err, "relay_full", "create", "create with 2 restored rooms and --max-rooms 2"); err != nil {
		return err
	}
	if err := end(back[1]); err != nil {
		return err
	}
	if _, _, err := y.createFrom("D", d); err != nil {
		return fmt.Errorf("create with 1 restored room and --max-rooms 2: %w", err)
	}
	return nil
}
