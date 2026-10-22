package main

// Party traces of a session's life (spec/SDK.md §3, §4, §7, §8, §15): the relay list as the party reads and refreshes
// it, code routing, create and join with their failures and cancellation, the end, the checks of relay input, and
// the participant and action events. As for every party trace, the outputs are written by hand from SDK.md.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const roninMs = 7_320_000

func ronin() *Media {
	return &Media{Kind: "movie", Title: "Ronin", Year: new(1998), Ids: map[string]string{"imdb": "tt0122690"},
		DurationMs: roninMs}
}

func lifeList(relays ...obj) obj { return o("relays", objs(relays...)) }
func lifeRelay(id, url, prefixes string) obj {
	return o("id", id, "name", "Relay "+id, "url", url, "prefixes", prefixes)
}

// embeddedURL is the url of the relay owning symbol c in the repository's relays.json, the list every SDK embeds and
// Relays.public() starts from (SDK.md §3.1). A trace that routes before any refresh succeeded depends on it, so the
// table is regenerated when relays.json changes.
func embeddedURL(c byte) string {
	_, here, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "..", "relays.json"))
	if err != nil {
		panic(err)
	}
	relays, err := ParseRelayList(raw)
	if err != nil {
		panic(err)
	}
	for _, r := range relays {
		if strings.IndexByte(r.Prefixes, c) >= 0 {
			return r.URL
		}
	}
	panic("embeddedURL: relays.json gives no relay symbol " + string(c))
}

func lifeJoinMsg() obj { return o("type", "join", "code", trCode, "name", trName, "client", trClient) }
func lifeResumeMsg(peerID string) obj {
	return o("type", "resume", "code", trCode, "peerId", peerID, "token", token(peerID))
}
func lifeLeave() obj               { return o("type", "leave") }
func lifeClosedMsg(reason any) obj { return o("type", "closed", "reason", reason) }
func lifeRelayError(code string, re any) obj {
	return o("type", "error", "code", code, "message", "refused: "+code, "re", re)
}
func lifeEvError(code string, re any) obj { return o("type", "error", "code", code, "re", re) }

// lifeFailed is call id failing with an error the SDK raises itself: its re is null (SDK.md §15).
func lifeFailed(id, code string) obj { return done(id, "error", code, "re", nil) }

func lifeMalformed(msg any) obj { return o("recv", msg, "malformed", true) }
func lifeText(text string) obj  { return o("recvText", text) }

// lifeWith is m with member k set to v, added at the end when m lacks it; m itself is unchanged.
func lifeWith(m obj, k string, v any) obj {
	m = slices.Clone(m)
	for i := range m {
		if m[i].k == k {
			m[i].v = v
			return m
		}
	}
	return append(m, kv{k, v})
}

// lifeAsMap is v's JSON as a map, so a trace can give a message members the generator's types don't have.
func lifeAsMap(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		panic(err)
	}
	return m
}

// lifeAction is an Action event whose byPeerId has no entry in the snapshot, so by is null (SDK.md §15).
func lifeAction(kind string, byPeerID any, rev int64) obj {
	return o("type", "action", "kind", kind, "byPeerId", byPeerID, "by", nil, "byMe", false, "room", rev)
}

// lifeEnded is what ending by the relay or by the SDK leaves in state (SDK.md §7.4).
func lifeEnded(reason string) obj {
	return state("connection", "ended", "endReason", reason, "peerId", nil, "room", nil)
}

// lifeJoin joins trCode again with call id, the list already read, and answers with room and no media: the attempt
// clears endReason; status follows joined with the last-sent values cleared, the offset 0 (SDK.md §7.2, §7.3, §13).
func (t *trace) lifeJoin(id string, room snap) {
	t.do(call("join", "id", id, "code", trCode), out{
		Net:   objs(opened()),
		Send:  objs(lifeJoinMsg()),
		State: state("connection", "connecting", "endReason", nil),
	})
	t.do(recv(joinedMsg("ann", room, nil, t.relayNow())), out{
		Done:  objs(done(id)),
		Send:  objs(t.status(0, "idle", nil)),
		State: state("connection", "connected", "room", room.Rev, "offsetMs", 0, "endReason", nil),
	})
}

// lifeDropResumed drops the member connection, resumes 250 ms later (draw 0) and answers with room: status follows
// the joined (SDK.md §14, §7.3); out is what the joined does besides.
func (t *trace) lifeDropResumed(room snap, res out) {
	t.do(drop(1006), out{State: state("connection", "reconnecting")})
	t.do(advance(250), out{Net: objs(opened()), Send: objs(lifeResumeMsg("ann"))})
	res.Send = append(objs(t.status(0, "idle", nil)), res.Send...)
	res.State = append(state("connection", "connected", "room", room.Rev), res.State...)
	t.do(recv(joinedMsg("ann", room, nil, t.relayNow())), res)
}

func partyLifeTraces() []Case {
	var cases []Case
	add := func(tr *trace) { cases = append(cases, tr.cas()) }
	dan := newPeer("dan", "Dan", false)
	eve := newPeer("eve", "Eve", false)
	fay := newPeer("fay", "Fay", false)
	guestRoom := func(rev int64, peers ...peer) snap {
		return newRoom(rev, 990_000, append([]peer{ben, ann}, peers...)...)
	}

	// ---------------------------------------------------------------------------------------------------------------
	// §3 The relay list

	// SDK.md §3.1, §6, §7.1, §7.2: Relays.at fetches nothing; a failed connection to its one relay fails create and
	// join with unreachable, each back to idle.
	{
		tr := newTrace("a failed connection to the one relay fails create and join with unreachable", setup())
		tr.do(nextOpen("fail"), out{})
		tr.do(call("create", "id", "c2"), out{
			Net:   objs(opened()),
			Done:  objs(lifeFailed("c2", "unreachable")),
			State: state("connection", "idle"),
		})
		tr.do(nextOpen("fail"), out{})
		tr.do(call("join", "id", "c3", "code", trCode), out{
			Net:   objs(opened()),
			Done:  objs(lifeFailed("c3", "unreachable")),
			State: state("connection", "idle"),
		})
		add(tr)
	}

	// SDK.md §3.1, §3.3, §14: Relays.public() routes with its embedded list until a refresh succeeds, and joined
	// starts a background refresh from https://openwatch.party/relays.json only. One still running (hanging until
	// the 5 s limit) delays no join and the next joined starts no second. A failed one doesn't count, so the next
	// joined, a reconnect's included, starts another; once one succeeded, no joined starts one, and routing uses
	// its list.
	{
		s := pickSetup()
		s.Random = []float64{0, 0}
		tr := newTrace("joined refreshes the public list in the background until a refresh succeeds; a hanging one delays no join and a joined starts no second", s)
		embedded := embeddedURL(trCode[0])
		room := guestRoom(3)
		joinEmbedded := func(id string) {
			tr.do(call("join", "id", id, "code", trCode), out{
				Net:   objs(openedOn(embedded)),
				Send:  objs(lifeJoinMsg()),
				State: state("connection", "connecting", "endReason", nil),
			})
		}
		joined := func(id string, get bool) out {
			res := out{
				Done:  objs(done(id)),
				Send:  objs(tr.status(0, "idle", nil)),
				State: state("connection", "connected", "room", room.Rev),
			}
			if get {
				res.Net = objs(got())
			}
			return res
		}
		left := out{
			Send:  objs(lifeLeave()),
			Net:   objs(closedOn(embedded, 1000)),
			Event: objs(evEnded("left", nil)),
			State: lifeEnded("left"),
		}
		tr.do(nextGet("hang"), out{})
		joinEmbedded("c1")
		tr.do(recv(joinedMsg("ann", room, nil, tr.relayNow())), joined("c1", true))
		tr.do(call("leave"), left)
		joinEmbedded("c2")
		tr.do(recv(joinedMsg("ann", room, nil, tr.relayNow())), joined("c2", false))
		tr.do(advance(5000), out{})
		for _, get := range []bool{true, false} {
			tr.do(drop(1006), out{State: state("connection", "reconnecting")})
			tr.do(advance(250), out{Net: objs(openedOn(embedded)), Send: objs(lifeResumeMsg("ann"))})
			res := out{Send: objs(tr.status(0, "idle", nil)), State: state("connection", "connected")}
			if get {
				res.Net = objs(got())
			}
			tr.do(recv(joinedMsg("ann", room, nil, tr.relayNow())), res)
		}
		tr.do(call("leave"), left)
		tr.do(call("join", "id", "c3", "code", trCode), out{
			Net:   objs(openedOn(urlB)),
			Send:  objs(sendOn(urlB, lifeJoinMsg())),
			State: state("connection", "connecting", "endReason", nil),
		})
		tr.do(recvOn(urlB, joinedMsg("ann", room, nil, tr.relayNow())), out{
			Done:  objs(done("c3")),
			Send:  objs(sendOn(urlB, tr.status(0, "idle", nil))),
			State: state("connection", "connected"),
		})
		add(tr)
	}

	// ---------------------------------------------------------------------------------------------------------------
	// §4 Codes and routing

	// SDK.md §4.1, §7.2, §15: join normalizes first: a code that isn't 6 symbols fails invalid_code, re null, before
	// any network use and before already_in_room; nothing changes.
	{
		tr := newTrace("join refuses an invalid code with invalid_code before any network use, also while in a room", setup())
		tr.do(call("join", "id", "c1", "code", "K7MQ2"), out{
			Done:  objs(lifeFailed("c1", "invalid_code")),
			State: state("connection", "idle"),
		})
		tr.created("c2", opened())
		tr.do(call("join", "id", "c3", "code", "K7MQ2O"), out{
			Done:  objs(lifeFailed("c3", "invalid_code")),
			State: state("connection", "connected", "room", 1),
		})
		tr.do(call("join", "id", "c4", "code", trCode), out{Done: objs(lifeFailed("c4", "already_in_room"))})
		add(tr)
	}

	// SDK.md §3.3, §4.1, §4.2, §7.2, §10.3: join removes separators and upper-cases, asks the relay whose prefixes hold
	// the first symbol (K: b) and sends the normalized code; status follows joined. The refresh before join counts as
	// the last one, so joined starts none.
	{
		tr := newTrace("join normalizes the code and asks the relay that owns its first symbol", pickSetup())
		tr.pickRefreshed()
		tr.do(call("join", "id", "c1", "code", " k7m-q2x\t"), out{
			Net:   objs(openedOn(urlB)),
			Send:  objs(sendOn(urlB, lifeJoinMsg())),
			State: state("connection", "connecting"),
		})
		tr.do(recvOn(urlB, joinedMsg("ann", guestRoom(4), nil, tr.relayNow())), out{
			Done:  objs(done("c1")),
			Send:  objs(sendOn(urlB, tr.status(0, "idle", nil))),
			State: state("connection", "connected", "room", 4),
		})
		add(tr)
	}

	// SDK.md §3.2, §3.3, §4.2, §15: refresh() returns false and changes nothing when the GET fails or the list is
	// invalid (a ws:// relay on the public list). A code whose first symbol no relay owns forces a refresh; still none
	// fails room_not_found from the SDK (re null) with no connection, also when the refresh fails. When the refresh
	// gives K an owner, join goes there; that relay's room_not_found is then final, since the call already forced its
	// one refresh.
	{
		s := pickSetup()
		s.List = lifeList(lifeRelay("a", urlA, "23456789A"), lifeRelay("c", urlC, "MNPQRSTUVWXYZ"))
		tr := newTrace("refresh() is false on a failed GET or an invalid list; a code no relay owns forces one refresh, then fails room_not_found", s)
		tr.pickRefreshed()
		tr.do(nextGet("fail"), out{})
		tr.do(call("refresh", "id", "r1"), out{Net: objs(got()), Done: objs(done("r1", "value", false))})
		tr.do(nextGet(lifeList(lifeRelay("x", "ws://x.relay.test/v1/ws", Alphabet))), out{})
		tr.do(call("refresh", "id", "r2"), out{Net: objs(got()), Done: objs(done("r2", "value", false))})
		tr.do(call("join", "id", "c1", "code", trCode), out{
			Net:   objs(got()),
			Done:  objs(lifeFailed("c1", "room_not_found")),
			State: state("connection", "idle"),
		})
		tr.do(nextGet("fail"), out{})
		tr.do(call("join", "id", "c2", "code", trCode), out{
			Net:   objs(got()),
			Done:  objs(lifeFailed("c2", "room_not_found")),
			State: state("connection", "idle"),
		})
		tr.do(nextGet(pickList()), out{})
		tr.do(call("join", "id", "c3", "code", trCode), out{
			Net:   objs(got(), openedOn(urlB)),
			Send:  objs(sendOn(urlB, lifeJoinMsg())),
			State: state("connection", "connecting"),
		})
		tr.do(recvOn(urlB, refusal("room_not_found", "no room with this code", "join")), out{
			Net:   objs(closedOn(urlB, 1000)),
			Done:  objs(done("c3", "error", "room_not_found", "re", "join", "message", "no room with this code")),
			State: state("connection", "idle"),
		})
		add(tr)
	}

	// SDK.md §3.3, §4.2, §7.2: a relay's room_not_found forces a refresh from the public source, not from the relay
	// that answered; the code's symbol now belongs to another URL, so join goes there once and joins.
	{
		tr := newTrace("room_not_found refreshes from the public source, not the relay, and joins once where the code moved", pickSetup())
		tr.pickRefreshed()
		tr.do(call("join", "id", "c1", "code", trCode), out{
			Net:   objs(openedOn(urlB)),
			Send:  objs(sendOn(urlB, lifeJoinMsg())),
			State: state("connection", "connecting"),
		})
		tr.do(nextGet(lifeList(lifeRelay("a", urlA, "23456789A"), lifeRelay("b", urlB, "BCDEFGH"),
			lifeRelay("c", urlC, "JKMNPQRSTUVWXYZ"))), out{})
		tr.do(recvOn(urlB, refusal("room_not_found", "no room with this code", "join")), out{
			Net:   objs(closedOn(urlB, 1000), got(), openedOn(urlC)),
			Send:  objs(sendOn(urlC, lifeJoinMsg())),
			State: state("connection", "connecting"),
		})
		tr.do(recvOn(urlC, joinedMsg("ann", guestRoom(4), nil, tr.relayNow())), out{
			Done:  objs(done("c1")),
			Send:  objs(sendOn(urlC, tr.status(0, "idle", nil))),
			State: state("connection", "connected", "room", 4),
		})
		add(tr)
	}

	// SDK.md §4.2, §15: the move is by URL, not id: when the refreshed list gives the symbol to a relay with another
	// id at the same URL, the relay's room_not_found is final, with its message and re; one GET, one attempt.
	{
		tr := newTrace("room_not_found is final when the refreshed list gives the code the same URL under another id", pickSetup())
		tr.pickRefreshed()
		tr.do(call("join", "id", "c1", "code", trCode), out{
			Net:   objs(openedOn(urlB)),
			Send:  objs(sendOn(urlB, lifeJoinMsg())),
			State: state("connection", "connecting"),
		})
		tr.do(nextGet(lifeList(lifeRelay("a", urlA, "23456789A"), lifeRelay("k", urlB, "BCDEFGHJK"),
			lifeRelay("c", urlC, "MNPQRSTUVWXYZ"))), out{})
		tr.do(recvOn(urlB, refusal("room_not_found", "no room with this code", "join")), out{
			Net:   objs(closedOn(urlB, 1000), got()),
			Done:  objs(done("c1", "error", "room_not_found", "re", "join", "message", "no room with this code")),
			State: state("connection", "idle"),
		})
		add(tr)
	}

	// SDK.md §3.1, §4.2: Relays.at sends every code to its one relay, whatever the first symbol (Z belongs to no public
	// relay), and its room_not_found is final: no GET, no second attempt.
	{
		tr := newTrace("Relays.at joins any code on its one relay and its room_not_found is final, with no GET", setup())
		joinZ := lifeWith(lifeJoinMsg(), "code", "Z7MQ2X")
		tr.do(call("join", "id", "c1", "code", "z7m-q2x"), out{
			Net:   objs(opened()),
			Send:  objs(joinZ),
			State: state("connection", "connecting"),
		})
		tr.do(recv(refusal("room_not_found", "no room with this code", "join")), out{
			Net:   objs(closed(1000)),
			Done:  objs(done("c1", "error", "room_not_found", "re", "join", "message", "no room with this code")),
			State: state("connection", "idle"),
		})
		tr.do(advance(10_000), out{})
		add(tr)
	}

	// SDK.md §4.2, §7.2, SPEC.md §7.2: join with create sends create: true. A relay's room_not_found still forces the
	// one refresh, and the join goes once, create kept, to the relay the code moved to. That relay's relay_full is
	// final: unlike create, join never tries another relay.
	{
		tr := newTrace("join with create keeps create where room_not_found moves the code, and relay_full is final", pickSetup())
		tr.pickRefreshed()
		joinC := lifeWith(lifeJoinMsg(), "create", true)
		tr.do(call("join", "id", "c1", "code", trCode, "create", true), out{
			Net:   objs(openedOn(urlB)),
			Send:  objs(sendOn(urlB, joinC)),
			State: state("connection", "connecting"),
		})
		tr.do(nextGet(lifeList(lifeRelay("a", urlA, "23456789A"), lifeRelay("b", urlB, "BCDEFGH"),
			lifeRelay("c", urlC, "JKMNPQRSTUVWXYZ"))), out{})
		tr.do(recvOn(urlB, refusal("room_not_found", "no room with this code", "join")), out{
			Net:   objs(closedOn(urlB, 1000), got(), openedOn(urlC)),
			Send:  objs(sendOn(urlC, joinC)),
			State: state("connection", "connecting"),
		})
		tr.do(recvOn(urlC, refusal("relay_full", "the relay is full", "join")), out{
			Net:   objs(closedOn(urlC, 1000)),
			Done:  objs(done("c1", "error", "relay_full", "re", "join", "message", "the relay is full")),
			State: state("connection", "idle"),
		})
		tr.do(advance(10_000), out{})
		add(tr)
	}

	// SDK.md §3.1, §7.2, SPEC.md §7.2: a join with create false sends no create member, and Relays.at's room_not_found
	// stays final. With create true, Relays.at sends the join to its one relay whatever the first symbol, and the
	// joined that answers makes the party the host of a new room.
	{
		tr := newTrace("join without create sends no create member; with it, Relays.at joins the room it makes as host", setup())
		joinZ := lifeWith(lifeJoinMsg(), "code", "Z7MQ2X")
		tr.do(call("join", "id", "c1", "code", "z7m-q2x", "create", false), out{
			Net:   objs(opened()),
			Send:  objs(joinZ),
			State: state("connection", "connecting"),
		})
		tr.do(recv(refusal("room_not_found", "no room with this code", "join")), out{
			Net:   objs(closed(1000)),
			Done:  objs(done("c1", "error", "room_not_found", "re", "join", "message", "no room with this code")),
			State: state("connection", "idle"),
		})
		host := ann
		host.Host = true
		room := newRoom(1, tr.relayNow(), host)
		room.Code = "Z7MQ2X"
		tr.do(call("join", "id", "c2", "code", "z7m-q2x", "create", true), out{
			Net:   objs(opened()),
			Send:  objs(lifeWith(joinZ, "create", true)),
			State: state("connection", "connecting"),
		})
		tr.do(recv(joinedMsg("ann", room, nil, tr.relayNow())), out{
			Done:  objs(done("c2")),
			Send:  objs(tr.status(0, "idle", nil)),
			State: state("connection", "connected", "peerId", "ann", "room", 1, "isHost", true, "player", "idle"),
		})
		add(tr)
	}

	// ---------------------------------------------------------------------------------------------------------------
	// §7 Session lifecycle

	// SDK.md §7.1, §7.2: joined must answer within the connect timeout (10000 ms) of sending join; without it join
	// fails unreachable, its connection closes with 1000, back to idle.
	{
		tr := newTrace("a join without an answer in 10000 ms fails unreachable and closes its connection with 1000", setup())
		tr.do(call("join", "id", "c1", "code", trCode), out{
			Net:  objs(opened()),
			Send: objs(lifeJoinMsg()),
		})
		tr.do(advance(9999), out{})
		tr.do(advance(1), out{
			Net:   objs(closed(1000)),
			Done:  objs(lifeFailed("c1", "unreachable")),
			State: state("connection", "idle"),
		})
		add(tr)
	}

	// SDK.md §2, §7.1: the connect timeout (10000 ms) bounds opening the connection: one that never opens fails join
	// at 10000 ms, back to idle, with nothing sent.
	{
		tr := newTrace("a connection that doesn't open within the connect timeout fails join unreachable", setup())
		tr.do(nextOpen("hang"), out{})
		tr.do(call("join", "id", "c1", "code", trCode), out{
			Net:   objs(opened()),
			State: state("connection", "connecting"),
		})
		tr.do(advance(9999), out{})
		tr.do(advance(1), out{Done: objs(lifeFailed("c1", "unreachable")), State: state("connection", "idle")})
		add(tr)
	}

	// SDK.md §7.2: create and join fail already_in_room while an attempt runs and while connected or reconnecting;
	// the attempt and the room go on untouched.
	{
		s := setup()
		s.Random = []float64{0}
		tr := newTrace("create and join fail already_in_room while connecting, connected or reconnecting", s)
		room := guestRoom(4)
		tr.do(call("join", "id", "c1", "code", trCode), out{
			Net:   objs(opened()),
			Send:  objs(lifeJoinMsg()),
			State: state("connection", "connecting"),
		})
		tr.do(call("join", "id", "c2", "code", trCode), out{
			Done:  objs(lifeFailed("c2", "already_in_room")),
			State: state("connection", "connecting"),
		})
		tr.do(call("create", "id", "c3"), out{Done: objs(lifeFailed("c3", "already_in_room"))})
		tr.do(recv(joinedMsg("ann", room, nil, tr.relayNow())), out{
			Done:  objs(done("c1")),
			Send:  objs(tr.status(0, "idle", nil)),
			State: state("connection", "connected"),
		})
		tr.do(call("create", "id", "c4"), out{Done: objs(lifeFailed("c4", "already_in_room"))})
		tr.do(drop(1006), out{State: state("connection", "reconnecting")})
		tr.do(call("join", "id", "c5", "code", trCode), out{
			Done:  objs(lifeFailed("c5", "already_in_room")),
			State: state("connection", "reconnecting"),
		})
		tr.do(call("create", "id", "c6"), out{Done: objs(lifeFailed("c6", "already_in_room"))})
		tr.do(advance(250), out{Net: objs(opened()), Send: objs(lifeResumeMsg("ann"))})
		room.Rev = 5
		tr.do(recv(joinedMsg("ann", room, nil, tr.relayNow())), out{
			Send:  objs(tr.status(0, "idle", nil)),
			State: state("connection", "connected", "room", 5),
		})
		add(tr)
	}

	// SDK.md §7.2, SPEC.md §11.5: leave() after join was sent cancels it at once (cancelled, re null), back to idle
	// without Ended; the connection stays for the answer: a late joined gets leave, then a close with 1000, and the
	// party stays idle with no room and no status.
	{
		tr := newTrace("leave() after join was sent cancels at once; a late joined gets leave and a close, no Ended", setup())
		tr.do(call("join", "id", "c1", "code", trCode), out{Net: objs(opened()), Send: objs(lifeJoinMsg())})
		tr.do(call("leave"), out{Done: objs(lifeFailed("c1", "cancelled")), State: state("connection", "idle")})
		tr.do(recv(joinedMsg("ann", guestRoom(4), nil, tr.relayNow())), out{
			Send:  objs(lifeLeave()),
			Net:   objs(closed(1000)),
			State: state("connection", "idle", "room", nil, "peerId", nil),
		})
		tr.do(advance(15_000), out{})
		add(tr)
	}

	// SDK.md §7.1, §7.2: a cancelled join's connection waits for its answer only until the connect timeout from
	// sending join, then closes with 1000.
	{
		tr := newTrace("a cancelled join's connection closes with 1000 at its answer timeout", setup())
		tr.do(call("join", "id", "c1", "code", trCode), out{Net: objs(opened()), Send: objs(lifeJoinMsg())})
		tr.do(advance(4000), out{})
		tr.do(call("leave"), out{Done: objs(lifeFailed("c1", "cancelled")), State: state("connection", "idle")})
		tr.do(advance(5999), out{})
		tr.do(advance(1), out{Net: objs(closed(1000))})
		add(tr)
	}

	// SDK.md §7.2: leave() while the connection is still opening cancels join; that connection hasn't sent join, so
	// it goes at once and nothing is ever sent.
	{
		tr := newTrace("leave() while the connection is still opening cancels join and sends nothing", setup())
		tr.do(nextOpen("hang"), out{})
		tr.do(call("join", "id", "c1", "code", trCode), out{
			Net:   objs(opened()),
			State: state("connection", "connecting"),
		})
		tr.do(call("leave"), out{Done: objs(lifeFailed("c1", "cancelled")), State: state("connection", "idle")})
		tr.do(advance(10_000), out{})
		add(tr)
	}

	// SDK.md §7.2, §7.4, SPEC.md §11.5: close() during join rejects it with cancelled and completes at once (no
	// adapter); the connection that sent join waits for the answer, and a refusal only closes it, with no leave.
	{
		tr := newTrace("close() during join rejects it with cancelled; a late refusal only closes", setup())
		tr.do(call("join", "id", "c1", "code", trCode), out{Net: objs(opened()), Send: objs(lifeJoinMsg())})
		tr.do(call("close", "id", "x1"), out{
			Done:  objs(lifeFailed("c1", "cancelled"), done("x1")),
			State: state("connection", "idle"),
		})
		tr.do(recv(refusal("room_full", "the room is full", "join")), out{Net: objs(closed(1000))})
		add(tr)
	}
	// SDK.md §7.1, §8, §15: while waiting for joined, an error whose re isn't the request's (another type, null) is
	// ignored and is no Error event; so are unknown types and texts that aren't objects. A connection closing before
	// joined fails join and create with unreachable, back to idle.
	{
		tr := newTrace("while waiting for joined, errors for other requests are ignored; a close before joined is unreachable", setup())
		tr.do(call("join", "id", "c1", "code", trCode), out{Net: objs(opened()), Send: objs(lifeJoinMsg())})
		tr.do(recv(refusal("rate_limited", "slow down", "status")), out{})
		tr.do(recv(lifeRelayError("room_full", nil)), out{})
		tr.do(recv(refusal("relay_full", "this relay is full", "create")), out{})
		tr.do(recv(o("type", "welcome", "motd", "hi")), out{})
		tr.do(lifeText("[1,2]"), out{State: state("connection", "connecting")})
		tr.do(recv(joinedMsg("ann", guestRoom(4), nil, tr.relayNow())), out{
			Done:  objs(done("c1")),
			Send:  objs(tr.status(0, "idle", nil)),
			State: state("connection", "connected"),
		})
		tr.do(call("leave"), out{
			Send:  objs(lifeLeave()),
			Net:   objs(closed(1000)),
			Event: objs(evEnded("left", nil)),
		})
		tr.do(call("join", "id", "c2", "code", trCode), out{Net: objs(opened()), Send: objs(lifeJoinMsg())})
		tr.do(drop(1006), out{Done: objs(lifeFailed("c2", "unreachable")), State: state("connection", "idle")})
		tr.do(call("create", "id", "c3"), out{Net: objs(opened()), Send: objs(createMsg())})
		tr.do(drop(nil), out{Done: objs(lifeFailed("c3", "unreachable")), State: state("connection", "idle")})
		add(tr)
	}

	// SDK.md §7.4: leave() in a room sends leave, closes with 1000, clears the room fields and emits Ended(left)
	// without an error; nothing follows. leave() while idle or ended does nothing.
	{
		tr := newTrace("leave() in a room sends leave, closes with 1000 and ends left; leave() idle or ended does nothing", setup())
		tr.do(call("leave"), out{State: state("connection", "idle", "endReason", nil)})
		tr.hostJoined()
		tr.do(call("leave"), out{
			Send:  objs(lifeLeave()),
			Net:   objs(closed(1000)),
			Event: objs(evEnded("left", nil)),
			State: append(lifeEnded("left"), o("isHost", false, "canControl", false)...),
		})
		tr.do(advance(20_000), out{})
		tr.do(call("leave"), out{State: state("connection", "ended", "endReason", "left")})
		add(tr)
	}

	// SDK.md §7.4: close() in a room is leave() then the release: leave, a close with 1000 and Ended(left), then the
	// completion (no adapter: at once). close() again changes nothing and completes at once.
	{
		tr := newTrace("close() in a room leaves and ends left; a second close() only completes", setup())
		tr.hostJoined()
		tr.do(call("close", "id", "x1"), out{
			Send:  objs(lifeLeave()),
			Net:   objs(closed(1000)),
			Event: objs(evEnded("left", nil)),
			Done:  objs(done("x1")),
			State: lifeEnded("left"),
		})
		tr.do(call("close", "id", "x2"), out{Done: objs(done("x2")), State: state("connection", "ended")})
		add(tr)
	}

	// SDK.md §7.2, §7.4, §13: closed ends the party with its reason, kicked, expired, or ended for a reason the SDK
	// doesn't know; the SDK closes with 1000 and never resumes. An ended party may join again: the attempt clears
	// endReason, the joined resets an offset set meanwhile, and status goes out again as after any joined.
	{
		tr := newTrace("closed ends the party with its reason, unknown ones as ended, never resumes; the next join starts over", setup())
		tr.guestJoined(trCode, guestRoom(4), nil)
		tr.do(recv(lifeClosedMsg("kicked")), out{
			Net:   objs(closed(1000)),
			Event: objs(evEnded("kicked", nil)),
			State: lifeEnded("kicked"),
		})
		tr.do(advance(20_000), out{})
		tr.do(call("setOffset", "ms", 1200), out{State: state("offsetMs", 1200)})
		tr.lifeJoin("c2", guestRoom(5))
		tr.do(recv(lifeClosedMsg("expired")), out{
			Net:   objs(closed(1000)),
			Event: objs(evEnded("expired", nil)),
			State: lifeEnded("expired"),
		})
		tr.lifeJoin("c3", guestRoom(6))
		tr.do(recv(lifeClosedMsg("archived")), out{
			Net:   objs(closed(1000)),
			Event: objs(evEnded("ended", nil)),
			State: lifeEnded("ended"),
		})
		tr.do(advance(20_000), out{})
		add(tr)
	}

	// SDK.md §7.1, §8, AGENTS.md v1 invariant: unknown message types are ignored, before and after joined; so are
	// texts that aren't JSON objects or have no string type; unknown members at any depth of joined and room are
	// ignored and the message applies.
	{
		tr := newTrace("unknown message types and members and messages that aren't objects are ignored", setup())
		room := guestRoom(4)
		tr.do(call("join", "id", "c1", "code", trCode), out{Net: objs(opened()), Send: objs(lifeJoinMsg())})
		tr.do(recv(o("type", "welcome", "motd", "hi")), out{})
		inner := lifeAsMap(room)
		inner["theme"] = "dark"
		inner["participants"].([]any)[0].(map[string]any)["badge"] = "gold"
		joined := joinedMsg("ann", room, nil, tr.relayNow())
		joined = lifeWith(joined, "you", o("peerId", "ann", "token", token("ann"), "color", "blue"))
		joined = lifeWith(joined, "room", inner)
		joined = lifeWith(joined, "region", o("name", "x"))
		tr.do(recv(joined), out{
			Done:  objs(done("c1")),
			Send:  objs(tr.status(0, "idle", nil)),
			State: state("connection", "connected", "room", 4),
		})
		tr.do(recv(o("type", "announcement", "rev", 9)), out{})
		for _, text := range []string{`[{"type":"room"}]`, `"room"`, `{"type":7}`, `{"type":null}`, `null`, `not json`} {
			tr.do(lifeText(text), out{})
		}
		next := guestRoom(5, cat)
		msg := lifeAsMap(next.acted("media", "ben").msg())
		msg["theme"] = "dark"
		msg["participants"].([]any)[2].(map[string]any)["badge"] = "gold"
		msg["action"].(map[string]any)["note"] = "cleared"
		tr.do(recv(msg), out{
			Event: objs(evJoined("cat"), evAction("media", "ben", false, 5)),
			State: state("connection", "connected", "room", 5),
		})
		add(tr)
	}

	// SDK.md §7.4, §14, §15: client_blocked answering a reconnect attempt ends the party lost with that error, after
	// a close with 1000, and it never joins again on its own; an answer to resume is no Error event.
	{
		s := setup()
		s.Random = []float64{0}
		tr := newTrace("client_blocked answering a reconnect attempt ends the party lost with that error", s)
		tr.hostJoined()
		tr.do(drop(1006), out{State: state("connection", "reconnecting")})
		tr.do(advance(250), out{Net: objs(opened()), Send: objs(lifeResumeMsg("ann"))})
		tr.do(recv(refusal("client_blocked", "this client is blocked", "resume")), out{
			Net:   objs(closed(1000)),
			Event: objs(evEnded("lost", "client_blocked")),
			State: lifeEnded("lost"),
		})
		tr.do(advance(300_000), out{})
		add(tr)
	}

	// ---------------------------------------------------------------------------------------------------------------
	// §8 Room state

	// SDK.md §8: a room failing the snapshot check (rate 3, an idle room at 5 ms, no host) is dropped whole: the last
	// good snapshot stays, and so does its rev, so a valid rev 5 after a malformed rev 6 applies.
	{
		tr := newTrace("a malformed room is dropped and the last good snapshot and its rev stay", setup())
		tr.guestJoined(trCode, guestRoom(4), nil)
		fast := guestRoom(5, cat)
		fast.Rate = 3
		tr.do(lifeMalformed(fast.msg()), out{State: state("room", 4)})
		moved := guestRoom(6, cat)
		moved.PositionMs = 5
		tr.do(lifeMalformed(moved.msg()), out{State: state("room", 4)})
		headless := newRoom(6, 990_000, newPeer("ben", "Ben", false), ann, cat)
		tr.do(lifeMalformed(headless.msg()), out{State: state("room", 4)})
		tr.do(recv(guestRoom(5, cat).msg()), out{Event: objs(evJoined("cat")), State: state("room", 5)})
		add(tr)
	}

	// SDK.md §7.4, §8: a room of the party's own epoch applies whatever its media member holds, a descriptor or a
	// malformed one, and media stays null. A room starting a new epoch with a malformed media (durationMs 0), or
	// without media, sends leave, closes with 1000 and ends the party lost with invalid_message, with no Action; the
	// descriptor never reaches state and the party never resumes.
	{
		tr := newTrace("media on a room of the party's epoch is ignored; a new epoch without valid media ends the party lost with invalid_message", setup())
		tr.guestJoined(trCode, guestRoom(4), nil)
		bad := *heat()
		bad.DurationMs = 0
		tr.do(recv(guestRoom(5).withMedia(heat()).msg()), out{State: state("room", 5, "media", nil, "match", nil)})
		tr.do(recv(guestRoom(6).withMedia(&bad).msg()), out{State: state("room", 6, "media", nil)})
		next := guestRoom(7)
		next.Epoch, next.Phase = 1, "paused"
		lost := out{
			Send:  objs(lifeLeave()),
			Net:   objs(closed(1000)),
			Event: objs(evEnded("lost", "invalid_message")),
			State: append(lifeEnded("lost"), o("media", nil)...),
		}
		tr.do(recv(next.withMedia(&bad).acted("media", "ben").msg()), lost)
		tr.do(advance(20_000), out{})
		tr.lifeJoin("c2", guestRoom(5))
		next.Rev = 6
		tr.do(recv(next.acted("media", "ben").msg()), lost)
		tr.do(advance(20_000), out{})
		add(tr)
	}

	// SDK.md §6, §7.2, §8: a malformed joined answering join or create gets leave and a close with 1000 and fails the
	// call with invalid_message, re null, with no fallback to another relay. Back to idle, no Ended.
	{
		tr := newTrace("a malformed joined fails join and create with invalid_message after leave, with no fallback", pickSetup())
		tr.pickRefreshed()
		joined := joinedMsg("ann", guestRoom(4), nil, tr.relayNow())
		tr.do(call("join", "id", "j1", "code", trCode), out{
			Net:  objs(openedOn(urlB)),
			Send: objs(sendOn(urlB, lifeJoinMsg())),
		})
		tr.do(lifeMalformed(lifeWith(joined, "you", o("peerId", "ann", "token", token("ann")[1:]))), out{
			Send:  objs(sendOn(urlB, lifeLeave())),
			Net:   objs(closedOn(urlB, 1000)),
			Done:  objs(lifeFailed("j1", "invalid_message")),
			State: state("connection", "idle", "room", nil),
		})
		tr.pickCreate(10, 20, 30)
		tr.do(advance(10), out{
			Net:  objs(closedOn(urlB, 1000), closedOn(urlC, 1000)),
			Send: objs(sendOn(urlA, createMsg())),
		})
		host := ann
		host.Host = true
		created := newRoom(1, tr.relayNow(), host)
		created.Code = codeOn[urlA]
		tr.do(o("recv", lifeWith(joinedMsg("ann", created, nil, tr.relayNow()), "serverTime", -1), "malformed", true,
			"on", urlA), out{
			Send:  objs(sendOn(urlA, lifeLeave())),
			Net:   objs(closedOn(urlA, 1000)),
			Done:  objs(lifeFailed("c1", "invalid_message")),
			State: state("connection", "idle", "room", nil, "endReason", nil),
		})
		tr.do(advance(10_000), out{})
		add(tr)
	}

	// SDK.md §8, §15: a malformed error (empty code, no message) or closed (empty or non-string reason) is ignored;
	// every valid relay error on the member connection is an Error event with its code and re: a refused command,
	// one about status or ping, one with re null, an unknown code, unknown members ignored. The party stays.
	{
		tr := newTrace("relay errors on the member connection are Error events with code and re; a malformed error or closed is ignored", setup())
		tr.guestJoined(trCode, guestRoom(4), nil)
		tr.do(lifeMalformed(o("type", "error", "code", "", "message", "refused", "re", "play")), out{})
		tr.do(lifeMalformed(o("type", "error", "code", "forbidden", "re", "play")), out{})
		tr.do(lifeMalformed(lifeClosedMsg("")), out{})
		tr.do(lifeMalformed(lifeClosedMsg(5)), out{State: state("connection", "connected")})
		tr.do(recv(refusal("forbidden", "only a controller may play", "play")), out{Event: objs(lifeEvError("forbidden", "play"))})
		tr.do(recv(refusal("rate_limited", "too many status messages", "status")), out{
			Event: objs(lifeEvError("rate_limited", "status")),
		})
		tr.do(recv(refusal("rate_limited", "too many pings", "ping")), out{Event: objs(lifeEvError("rate_limited", "ping"))})
		tr.do(recv(lifeWith(lifeRelayError("maintenance", nil), "retryMs", 5000)), out{
			Event: objs(lifeEvError("maintenance", nil)),
			State: state("connection", "connected", "room", 4),
		})
		add(tr)
	}

	// SDK.md §8, §13: roomPositionMs() is P(now) on the reference timeline: null before a snapshot, then the room's
	// position, unchanged by the offset. Without a resolver the other media is unavailable and the player idle (§9.2,
	// §10.2); the offset change goes out once the 250 ms spacing is up (§10.3).
	{
		s := setup()
		s.Resolver = false
		tr := newTrace("roomPositionMs is the reference timeline, without the offset", s)
		tr.do(call("join", "id", "c1", "code", trCode), out{
			Net:   objs(opened()),
			Send:  objs(lifeJoinMsg()),
			State: state("roomPositionMs", nil),
		})
		tr.do(recv(joinedMsg("ann", playingRoom("host"), heat(), tr.relayNow())), out{
			Done:  objs(done("c1")),
			Send:  objs(tr.status(2, "idle", matchOf("unavailable"))),
			State: state("match", matchOf("unavailable"), "player", "idle", "roomPositionMs", 610_000),
		})
		tr.do(call("setOffset", "ms", 5000), out{State: state("offsetMs", 5000, "roomPositionMs", 610_000)})
		tr.do(advance(250), out{
			Send:  objs(lifeWith(tr.status(2, "idle", matchOf("unavailable")), "offsetMs", 5000)),
			State: state("roomPositionMs", 610_250),
		})
		add(tr)
	}

	// SDK.md §5, §7.3, §8, §14, §15: a reconnect switches to the new connection once it has joined and a clock sample;
	// until then the old snapshot stays. A pong failing its check (serverTime −1) is no sample, so the switch waits
	// for the real one 200 ms after the open. A room that arrived before the switch is handled after it, in order:
	// its new participant, then its action; the status after the switch carries the new clock's round trip.
	{
		s := setup()
		s.Random = []float64{0}
		tr := newTrace("a reconnect waits for a valid pong; a room that came before the switch is handled after it", s)
		tr.guestJoined(trCode, guestRoom(4), nil)
		tr.do(o("rtt", 200), out{})
		tr.do(drop(1006), out{State: state("connection", "reconnecting")})
		tr.do(advance(250), out{Net: objs(opened()), Send: objs(lifeResumeMsg("ann"))})
		tr.do(recv(joinedMsg("ann", guestRoom(2), nil, tr.relayNow())), out{
			State: state("connection", "reconnecting", "room", 4),
		})
		tr.do(lifeMalformed(o("type", "pong", "id", 1, "serverTime", -1)), out{State: state("connection", "reconnecting")})
		tr.do(recv(guestRoom(3, cat).acted("media", "ben").msg()), out{State: state("connection", "reconnecting", "room", 4)})
		tr.do(advance(199), out{})
		tr.do(advance(1), out{
			Send:  objs(lifeWith(tr.status(0, "idle", nil), "rttMs", 200)),
			Event: objs(evJoined("cat"), evAction("media", "ben", false, 3)),
			State: state("connection", "connected", "room", 3),
		})
		add(tr)
	}

	// SDK.md §7.3, §14, §15: the joined answering join emits no participant event; a reconnect's joined reports who
	// joined and who left against the snapshot before the drop and emits no Action; the next room with an action
	// does.
	{
		s := setup()
		s.Random = []float64{0}
		tr := newTrace("a reconnect's joined reports who joined and left since the drop and emits no Action", s)
		tr.guestJoined(trCode, guestRoom(4, dan), nil)
		back := newRoom(2, 990_000, ben, cat, ann)
		tr.lifeDropResumed(back, out{Event: objs(evJoined("cat"), evLeft("dan"))})
		back.Rev = 3
		tr.do(recv(back.msg()), out{State: state("room", 3)})
		back.Rev = 4
		tr.do(recv(back.acted("media", "ben").msg()), out{Event: objs(evAction("media", "ben", false, 4)), State: state("room", 4)})
		add(tr)
	}

	// SDK.md §8, §9.1, §10.2, §11.1, §11.2: a room starting an epoch with other media replaces the snapshot, then
	// unbinds and pauses the player once and starts the search; attached with the new choice's ref the player binds
	// (the bind's setRate) and is loading, held far from the hold point, until the seek cooldown (2000 ms after the
	// last seek, at local 0) lets the tick seek it to the held 0; then it is ready.
	{
		tr := newTrace("a room starting new media pauses the player once; bound again it waits out the seek cooldown, then seeks to the hold point", setup())
		room := tr.guestFollowing("host", false)
		room.Rev, room.Epoch, room.Phase, room.PositionMs, room.AtMs = 8, 3, "waiting", 0, tr.relayNow()
		room.WaitingOn = []string{"ann"}
		room = room.withPeer(1, ann.reported("ready", matchOf("matched", 500)))
		tr.do(recv(room.withMedia(ronin()).acted("media", "ben").msg()), out{
			Event:    objs(evAction("media", "ben", false, 8)),
			Command:  objs(cmd("p1", "pause")),
			Resolver: objs(o("resolve", "Ronin")),
			Send:     objs(tr.status(3, "loading", matchOf("searching"))),
			State: state("room", 8, "media", "Ronin", "match", matchOf("searching"), "choice", nil,
				"player", "loading"),
		})
		tr.do(answer(candidate("ronin-a", "durationMs", roninMs)), out{
			State: state("match", matchOf("matched", 0), "choice", choice("ronin-a", "ronin-a"), "player", "loading"),
		})
		tr.do(call("attachPlayer", "player", "p1", "ref", "ronin-a"), out{
			Command: objs(cmd("p1", "setRate", "rate", 1)),
			State:   state("player", "loading"),
		})
		tr.do(advance(250), out{Send: objs(tr.status(3, "loading", matchOf("matched", 0)))})
		tr.do(advance(1499), out{State: state("player", "loading")})
		tr.do(advance(1), out{
			Command: objs(cmd("p1", "seek", "positionMs", 0)),
			Send:    objs(tr.status(3, "ready", matchOf("matched", 0))),
			State:   state("player", "ready"),
		})
		add(tr)
	}

	// ---------------------------------------------------------------------------------------------------------------
	// §15 Events

	// SDK.md §8, §15: rooms apply only above the rev held; Action fires for each room carrying an action, never from
	// joined, with kind, byPeerId, by (the entry, null when absent) and byMe, after the snapshot's ParticipantJoined;
	// a removal is ParticipantLeft; an unknown kind passes through. A malformed action (empty kind, by not a peer ID,
	// null) drops the whole room, and its rev with it.
	{
		s := setup()
		s.Resolver = false
		tr := newTrace("rooms apply by rev; a room's action fires Action once with its peer, never joined; a malformed action drops the room", s)
		paused := func(rev int64, peers ...peer) snap {
			r := newRoom(rev, 990_000, append([]peer{ben.reported("ready", matchOf("matched", 0)),
				ann.reported("idle", matchOf("unavailable"))}, peers...)...)
			r.Epoch, r.Phase, r.PositionMs = 1, "paused", 600_000
			return r
		}
		tr.do(call("join", "id", "c1", "code", trCode), out{Net: objs(opened()), Send: objs(lifeJoinMsg())})
		tr.do(recv(joinedMsg("ann", paused(4), heat(), tr.relayNow())), out{
			Done:  objs(done("c1")),
			Send:  objs(tr.status(1, "idle", matchOf("unavailable"))),
			State: state("connection", "connected", "room", 4, "match", matchOf("unavailable")),
		})
		tr.do(recv(paused(4, cat).acted("pause", "cat").msg()), out{State: state("room", 4)})
		tr.do(recv(paused(3).msg()), out{State: state("room", 4)})
		tr.do(recv(paused(5).msg()), out{State: state("room", 5)})
		r := paused(6, cat)
		tr.do(recv(r.acted("pause", "cat").msg()), out{
			Event: objs(evJoined("cat"), evAction("pause", "cat", false, 6)),
			State: state("room", 6),
		})
		r.Rev = 7
		tr.do(recv(r.msg()), out{State: state("room", 7)})
		r = paused(8)
		tr.do(recv(r.acted("seek", "dan").msg()), out{Event: objs(evLeft("cat"), lifeAction("seek", "dan", 8)), State: state("room", 8)})
		r.Rev = 9
		tr.do(recv(r.acted("rate", "ann").msg()), out{Event: objs(evAction("rate", "ann", true, 9))})
		r.Rev = 10
		tr.do(recv(r.acted("rewind", nil).msg()), out{Event: objs(lifeAction("rewind", nil, 10))})
		r.Rev = 11
		tr.do(lifeMalformed(r.acted("", "ben").msg()), out{State: state("room", 10)})
		tr.do(lifeMalformed(r.acted("seek", "not a peer").msg()), out{State: state("room", 10)})
		nullAction := lifeAsMap(r.msg())
		nullAction["action"] = nil
		tr.do(lifeMalformed(nullAction), out{State: state("room", 10)})
		tr.do(recv(r.msg()), out{State: state("room", 11)})
		add(tr)
	}

	// SDK.md §15: for one snapshot the events are every ParticipantJoined in the new snapshot's order, then every
	// ParticipantLeft in the old snapshot's order, then Action.
	{
		tr := newTrace("one snapshot's events: joins in new order, then leaves in old order, then the action", setup())
		tr.guestJoined(trCode, guestRoom(4, dan, cat), nil)
		next := newRoom(5, 990_000, ben, fay, ann, eve)
		tr.do(recv(next.acted("media", "ben").msg()), out{
			Event: objs(evJoined("fay"), evJoined("eve"), evLeft("dan"), evLeft("cat"), evAction("media", "ben", false, 5)),
			State: state("room", 5),
		})
		add(tr)
	}

	return cases
}
