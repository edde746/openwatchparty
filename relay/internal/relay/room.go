package relay

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"
)

const (
	phaseIdle    = "idle"
	phasePaused  = "paused"
	phaseWaiting = "waiting"
	phasePlaying = "playing"

	playerIdle      = "idle"
	playerLoading   = "loading"
	playerReady     = "ready"
	playerBuffering = "buffering"

	matchUnavailable = "unavailable"

	controlHost     = "host"
	controlEveryone = "everyone"
)

// Relay durations in milliseconds at --timer-scale 1 (SPEC.md, Policy).
const (
	stallGraceMs      = 500
	stallReleaseMs    = 400
	mediaWaitMs       = 30_000
	waitMs            = 15_000
	exclusionMs       = 60_000
	startDelayMinMs   = 750
	startDelayMaxMs   = 2_000
	seekCoalesceMs    = 200
	hostPromotionMs   = 30_000
	resumeMs          = 5 * 60_000
	maxRoomAgeMs      = 24 * 60 * 60_000
	startDelayRttRate = 1.5
)

const (
	defaultRttMs     = 500   // an rttMs, never scaled
	pauseToleranceMs = 2_000 // a media-position tolerance, never scaled
)

type participant struct {
	peerID, name, client string
	tokenHash            [sha256.Size]byte

	// conn is the live connection, nil while disconnected and after the
	// participant left, was kicked or the room closed. A connection is in the
	// room exactly while its peer's conn points back at it.
	conn *conn

	player   string
	match    *matchState
	offsetMs int64

	statusEpoch   int64 // -1 until the first status
	unavailable   bool  // the latest status reported match.state unavailable
	rttMs         int64
	hasRtt        bool
	excludedUntil int64
	stallGrace    timer
	removal       timer // runs while disconnected: the resume window
}

func (p *participant) send(msg []byte) {
	if p.conn != nil {
		p.conn.enqueue(msg)
	}
}

func hashToken(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

func (p *participant) tokenMatches(token string) bool {
	h := hashToken(token)
	return subtle.ConstantTimeCompare(h[:], p.tokenHash[:]) == 1
}

// timer is a room timer. Its callback runs under Room.mu and does nothing if
// the timer was stopped or re-armed between firing and taking the lock.
type timer struct {
	t   *time.Timer
	gen uint64
}

func (t *timer) stop() {
	if t.t != nil {
		t.t.Stop()
		t.t = nil
	}
	t.gen++
}

func (t *timer) pending() bool { return t.t != nil }

type pendingSeek struct {
	positionMs int64
	by         string
}

// Room is one room's state. Every field below mu is guarded by it; connection
// goroutines and timer callbacks take it for the whole of one event, mutate,
// then broadcast at most one snapshot (flush) before unlocking. Sends only
// enqueue, so no I/O happens under the lock.
type Room struct {
	srv       *Server
	code      string    // immutable
	owner     ipGroups  // the creator's address for the rooms-per-IP limit; zero after a restore
	createdAt time.Time // wall clock, for the 24 h limit across restarts

	mu              sync.Mutex
	closed          bool
	dirty           bool // a snapshot field changed since the last broadcast
	rev             int64
	epoch           int64
	phase           string
	positionMs      int64
	atMs            int64
	rate            float64
	control         string
	locked          bool
	waitingOn       []*participant
	participants    []*participant // join order
	host            *participant
	hostAbsentSince int64 // relay time the host lost its connection; meaningful while host.conn == nil
	media           json.RawMessage
	durationMs      int64
	peerSeq         int
	stallWait       bool  // the current wait was entered by a stall
	lastSeekAt      int64 // relay time the last seek was applied
	pendingSeek     *pendingSeek

	// What the next snapshot carries besides its fields: the action the event
	// set, and media when the event started a new epoch (SPEC.md §8.3, §9.1).
	action   *action
	newEpoch bool

	barrier, stallRelease, mediaEnd, hostPromotion, maxAge, seekTimer timer
}

func newRoom(s *Server, code string, now int64) *Room {
	return &Room{srv: s, code: code, createdAt: time.Now(), phase: phaseIdle, rate: 1, control: controlHost,
		atMs: now, lastSeekAt: math.MinInt64 / 2}
}

// set assigns a snapshot field and marks the room dirty if the value changed.
func set[T comparable](r *Room, field *T, v T) {
	if *field != v {
		*field = v
		r.dirty = true
	}
}

// act records the action the event sets; the snapshot it publishes carries
// the last one. An action always publishes a snapshot.
func (r *Room) act(kind, by string) {
	r.action = &action{Kind: kind}
	if by != "" {
		r.action.By = &by
	}
	r.dirty = true
}

// maxTimerMs is the longest wait of one room timer: 2^31 − 1 ms. A longer
// delay waits in steps and runs fn once all of it has passed.
const maxTimerMs = math.MaxInt32

// arm runs fn under r.mu ms milliseconds from now, unless t is stopped or
// re-armed first.
func (r *Room) arm(t *timer, ms int64, fn func(now int64)) {
	t.stop()
	r.armSteps(t, t.gen, max(ms, 0), maxTimerMs, fn)
}

// armSteps waits at most step ms at a time until ms have passed.
func (r *Room) armSteps(t *timer, gen uint64, ms, step int64, fn func(now int64)) {
	wait := min(ms, step)
	t.t = time.AfterFunc(time.Duration(wait)*time.Millisecond, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if t.gen != gen || r.closed {
			return
		}
		if ms > wait {
			r.armSteps(t, gen, ms-wait, step, fn)
			return
		}
		t.t = nil
		fn(r.srv.now())
		r.flush()
	})
}

func (r *Room) scaledMs(ms int64) int64 { return r.srv.scaledMs(ms) }

// flush broadcasts one snapshot if anything changed.
func (r *Room) flush() {
	if r.dirty && !r.closed {
		r.publish(nil, "")
	}
}

// publish sends the next snapshot to every connected member; joiner, if set,
// gets it inside `joined` with its token instead. A `room` carries the
// event's action and, after setMedia or clearMedia, the media; `joined`
// carries neither in its snapshot.
func (r *Room) publish(joiner *participant, token string) {
	r.rev++
	r.dirty = false
	snap := r.snapshot()
	act, newEpoch := r.action, r.newEpoch
	r.action, r.newEpoch = nil, false
	var room []byte
	for _, p := range r.participants {
		switch {
		case p.conn == nil:
		case p == joiner:
			p.send(spliceMedia(encode(joinedMsg{Type: "joined", Code: r.code, You: you{p.peerID, token},
				Room: snap, ServerTime: r.srv.now()}), r.media))
		default:
			if room == nil {
				room = encode(roomMsg{"room", snap, act})
				if newEpoch {
					room = spliceMedia(room, r.media)
				}
			}
			p.send(room)
		}
	}
	r.srv.saveLater()
}

func (r *Room) broadcast(msg []byte) {
	for _, p := range r.participants {
		p.send(msg)
	}
}

func (r *Room) snapshot() *snapshot {
	s := &snapshot{
		Rev: r.rev, Code: r.code, Epoch: r.epoch, Phase: r.phase, PositionMs: r.positionMs, AtMs: r.atMs,
		Rate: r.rate, Control: r.control, Locked: r.locked,
		WaitingOn:    make([]string, len(r.waitingOn)),
		Participants: make([]participantView, len(r.participants)),
	}
	for i, p := range r.waitingOn {
		s.WaitingOn[i] = p.peerID
	}
	for i, p := range r.participants {
		s.Participants[i] = participantView{p.peerID, p.name, p.client, p == r.host, p.conn != nil,
			p.player, p.match, p.offsetMs}
	}
	return s
}

func (r *Room) find(peerID string) *participant {
	for _, p := range r.participants {
		if p.peerID == peerID {
			return p
		}
	}
	return nil
}

func (r *Room) firstConnected() *participant {
	for _, p := range r.participants {
		if p.conn != nil {
			return p
		}
	}
	return nil
}

// Membership.

// add appends a new participant attached to c and returns it with its token.
// The first participant becomes the host.
func (r *Room) add(c *conn, name, client string, now int64) (*participant, string) {
	r.peerSeq++
	token := newToken()
	p := &participant{peerID: "p" + strconv.Itoa(r.peerSeq), name: name, client: client, tokenHash: hashToken(token),
		player: playerIdle, statusEpoch: -1}
	r.participants = append(r.participants, p)
	if r.host == nil {
		r.host = p
	}
	r.dirty = true
	r.attach(p, c, now)
	return p, token
}

func (r *Room) attach(p *participant, c *conn, now int64) {
	set(r, &p.conn, c)
	c.room, c.peer = r, p
	p.removal.stop()
	switch {
	case p == r.host:
		r.hostPromotion.stop()
	case r.host.conn == nil && now-r.hostAbsentSince >= r.scaledMs(hostPromotionMs):
		r.setHost(r.firstConnected(), "", now) // promotion was due while nobody was connected
	}
}

func (r *Room) join(c *conn, name, client string, now int64) *relayError {
	switch {
	case r.closed:
		return &relayError{errRoomNotFound, "no room has that code"}
	case len(r.participants) >= r.srv.cfg.Limits.MaxParticipants:
		return &relayError{errRoomFull, "the room is full"}
	case r.locked:
		return &relayError{errRoomLocked, "the room is locked"}
	}
	r.publish(r.add(c, name, client, now))
	return nil
}

// open starts a room that was just put in the map: it arms the age limit and
// adds c as the first participant and host. The caller holds r.mu.
func (r *Room) open(c *conn, name, client string) {
	r.arm(&r.maxAge, r.scaledMs(maxRoomAgeMs), func(int64) { r.close("expired") })
	r.publish(r.add(c, name, client, r.srv.now()))
}

// resume reattaches the participant peerID to c and returns it.
func (r *Room) resume(c *conn, peerID, token string, now int64) (*participant, *relayError) {
	p := r.find(peerID)
	if r.closed || p == nil || !p.tokenMatches(token) {
		return nil, &relayError{errResumeFailed, "unknown participant or wrong token"}
	}
	if r.srv.blocked(p.client) {
		return nil, &relayError{errClientBlocked, "this relay does not serve this app"}
	}
	if p.conn != nil {
		p.conn.closeWith(closeTakenOver, "") // taken over: closed without a message
	}
	r.attach(p, c, now)
	r.publish(p, token)
	return p, nil
}

func (r *Room) disconnect(p *participant, now int64) {
	set(r, &p.conn, nil)
	p.stallGrace.stop()
	r.arm(&p.removal, r.scaledMs(resumeMs), func(now int64) { r.remove(p, now) })
	if p == r.host {
		r.hostAbsentSince = now
		r.arm(&r.hostPromotion, r.scaledMs(hostPromotionMs), r.promoteHost)
	}
	r.afterPeerEvent(now)
}

// remove takes p out of the room: leave, kick, or the end of its resume window.
func (r *Room) remove(p *participant, now int64) {
	p.conn = nil
	p.stallGrace.stop()
	p.removal.stop()
	r.participants = slices.DeleteFunc(r.participants, func(q *participant) bool { return q == p })
	p.forget()
	r.dirty = true
	if len(r.participants) == 0 {
		r.close("")
		return
	}
	if p == r.host {
		next := r.firstConnected()
		if next == nil {
			next = r.participants[0]
		}
		r.setHost(next, "", now)
	}
	r.afterPeerEvent(now)
}

func (r *Room) setHost(h *participant, by string, now int64) {
	set(r, &r.host, h)
	r.act("host", by)
	if h.conn == nil {
		r.hostAbsentSince = now
		r.arm(&r.hostPromotion, r.scaledMs(hostPromotionMs), r.promoteHost)
	} else {
		r.hostPromotion.stop()
	}
}

func (r *Room) promoteHost(now int64) {
	if r.host.conn != nil {
		return
	}
	if next := r.firstConnected(); next != nil {
		r.setHost(next, "", now)
	} // with nobody connected the host stays until someone connects (attach)
}

// close deletes the room. Connected members get `closed` unless reason is "".
// Former members' connections may still point at the room and their
// participants, so both forget their data here (SPEC.md §11.8).
func (r *Room) close(reason string) {
	if reason != "" {
		r.broadcast(encode(closedMsg{"closed", reason}))
	}
	r.closed = true
	for _, t := range []*timer{&r.barrier, &r.stallRelease, &r.mediaEnd, &r.hostPromotion, &r.maxAge, &r.seekTimer} {
		t.stop()
	}
	for _, p := range r.participants {
		p.conn = nil
		p.stallGrace.stop()
		p.removal.stop()
		p.forget()
	}
	r.srv.removeRoom(r)
	r.participants, r.waitingOn, r.host, r.media, r.pendingSeek = nil, nil, nil, nil, nil
}

// forget drops a removed participant's data. Only its pointer identity stays
// meaningful.
func (p *participant) forget() {
	p.name, p.client, p.tokenHash, p.match = "", "", [sha256.Size]byte{}, nil
}

// Commands. apply runs one validated command from p; the caller holds r.mu
// and flushes afterwards.
func (r *Room) apply(p *participant, cmd *command, now int64) *relayError {
	switch cmd.typ {
	case "status":
		r.status(p, cmd, now)
	case "leave":
		r.remove(p, now)
	case "play", "pause", "seek", "rate":
		if r.control == controlHost && p != r.host {
			return &relayError{errForbidden, "only the host controls playback"}
		}
		if r.phase == phaseIdle {
			return invalid("the room has no media")
		}
		r.playback(p, cmd, now)
	default:
		if p != r.host {
			return &relayError{errForbidden, "only the host may send " + cmd.typ}
		}
		return r.hostCommand(p, cmd, now)
	}
	return nil
}

func (r *Room) playback(p *participant, cmd *command, now int64) {
	switch cmd.typ {
	case "play":
		if r.phase == phasePaused {
			r.act("play", p.peerID)
			r.enterWaiting(r.positionMs, waitMs, now, false)
		}
	case "pause":
		if r.phase == phasePaused {
			return
		}
		pos := r.position(now)
		if d := cmd.positionMs - pos; cmd.hasPosition && max(d, -d) <= pauseToleranceMs {
			pos = min(cmd.positionMs, r.durationMs)
		}
		r.pauseAt(pos)
		r.act("pause", p.peerID)
	case "seek":
		target := min(cmd.positionMs, r.durationMs)
		if due := r.lastSeekAt + r.scaledMs(seekCoalesceMs); now < due {
			// Throttled: the latest target wins and applies once the interval has passed.
			first := r.pendingSeek == nil
			r.pendingSeek = &pendingSeek{target, p.peerID}
			if first {
				r.arm(&r.seekTimer, due-now, r.applyPendingSeek)
			}
			return
		}
		r.dropPendingSeek() // a pending target is older than this one
		r.seek(target, p.peerID, now)
	case "rate":
		r.act("rate", p.peerID)
		if r.phase == phasePlaying && now >= r.atMs {
			set(r, &r.positionMs, r.position(now))
			set(r, &r.atMs, now)
		}
		set(r, &r.rate, cmd.rate)
		r.scheduleMediaEnd(now)
	}
}

func (r *Room) seek(target int64, by string, now int64) {
	r.lastSeekAt = now
	r.act("seek", by)
	if r.phase == phasePaused {
		set(r, &r.positionMs, target)
	} else {
		r.enterWaiting(target, waitMs, now, false)
	}
}

func (r *Room) applyPendingSeek(now int64) {
	ps := r.pendingSeek
	r.pendingSeek = nil
	if ps != nil && r.phase != phaseIdle {
		r.seek(min(ps.positionMs, r.durationMs), ps.by, now)
	}
}

func (r *Room) dropPendingSeek() {
	r.pendingSeek = nil
	r.seekTimer.stop()
}

func (r *Room) hostCommand(p *participant, cmd *command, now int64) *relayError {
	switch cmd.typ {
	case "setMedia":
		r.dropPendingSeek()
		r.epoch++
		r.media, r.durationMs, r.newEpoch = cmd.media, cmd.durationMs, true
		r.clearExclusions()
		r.act("media", p.peerID)
		r.enterWaiting(min(cmd.positionMs, r.durationMs), mediaWaitMs, now, false)
	case "clearMedia":
		if r.media == nil {
			return nil
		}
		r.dropPendingSeek()
		r.stopPlaying()
		r.epoch++
		r.media, r.durationMs, r.newEpoch = nil, 0, true
		r.clearExclusions()
		set(r, &r.phase, phaseIdle)
		set(r, &r.positionMs, 0)
		r.setWaitingOn(nil)
		r.stallWait = false
		r.act("media", p.peerID)
	case "setControl":
		set(r, &r.control, cmd.mode)
	case "lock":
		set(r, &r.locked, cmd.locked)
	case "transferHost", "kick":
		target := r.find(cmd.peerID)
		if target == nil {
			return invalid("peerId is not in the room")
		}
		if cmd.typ == "kick" {
			target.send(encode(closedMsg{"closed", "kicked"}))
			r.remove(target, now)
		} else if target != r.host {
			r.setHost(target, p.peerID, now)
		}
	case "end":
		r.close("ended")
	}
	return nil
}

func (r *Room) clearExclusions() {
	for _, q := range r.participants {
		q.excludedUntil = 0
	}
}

// Playback state machine.

// position is P(now) (SPEC.md §12.2): the floor of the whole sum in float64. The explicit float64 conversion of
// the product rounds it before the addition, which keeps the compiler from fusing them into one multiply-add
// (arm64 does otherwise), as §12.2 requires.
func (r *Room) position(now int64) int64 {
	if r.phase != phasePlaying {
		return r.positionMs
	}
	pos := int64(math.Floor(float64(r.positionMs) + float64(float64(max(0, now-r.atMs))*r.rate)))
	return min(pos, r.durationMs)
}

// effectivePlayer is p's reported player when its latest status has the
// room's epoch, else loading: after setMedia, or after a restore, everyone
// counts as loading until it reports on the current media (SPEC.md §12.3).
func (r *Room) effectivePlayer(p *participant) string {
	if p.statusEpoch == r.epoch {
		return p.player
	}
	return playerLoading
}

func (r *Room) counted(p *participant, now int64) bool {
	return p.conn != nil && now >= p.excludedUntil && r.effectivePlayer(p) != playerIdle &&
		!(p.unavailable && p.statusEpoch == r.epoch)
}

func (r *Room) notReady(now int64) []*participant {
	var waiting []*participant
	for _, p := range r.participants {
		if r.counted(p, now) && !(p.statusEpoch == r.epoch && p.player == playerReady) {
			waiting = append(waiting, p)
		}
	}
	return waiting
}

func (r *Room) setWaitingOn(w []*participant) {
	if !slices.Equal(r.waitingOn, w) {
		r.waitingOn, r.dirty = w, true
	}
}

func (r *Room) status(p *participant, cmd *command, now int64) {
	set(r, &p.player, cmd.player)
	if !p.match.equal(cmd.match) {
		p.match, r.dirty = cmd.match, true
	}
	set(r, &p.offsetMs, cmd.offsetMs)
	p.statusEpoch = cmd.epoch
	p.unavailable = cmd.match != nil && cmd.match.State == matchUnavailable
	if cmd.hasRtt {
		p.rttMs, p.hasRtt = cmd.rttMs, true
	}
	if cmd.player == playerReady {
		p.stallGrace.stop()
	}
	r.afterPeerEvent(now)
	if r.phase == phasePlaying && r.stalled(p, now) && !p.stallGrace.pending() {
		r.arm(&p.stallGrace, r.scaledMs(stallGraceMs), func(now int64) {
			if r.phase == phasePlaying && r.stalled(p, now) {
				r.act("stall", p.peerID)
				r.enterWaiting(r.position(now), waitMs, now, true)
			}
		})
	}
}

func (r *Room) stalled(p *participant, now int64) bool {
	return p.player == playerBuffering && p.statusEpoch == r.epoch && r.counted(p, now)
}

// afterPeerEvent runs after every status, disconnect, leave and kick.
func (r *Room) afterPeerEvent(now int64) {
	if r.phase != phasePlaying && r.phase != phaseWaiting {
		return
	}
	if !slices.ContainsFunc(r.participants, func(p *participant) bool {
		return p.conn != nil && r.effectivePlayer(p) != playerIdle
	}) {
		r.pauseAt(r.position(now)) // nobody in the player
		return
	}
	if r.phase != phaseWaiting {
		return
	}
	r.setWaitingOn(r.notReady(now))
	switch {
	case len(r.waitingOn) > 0:
		r.stallRelease.stop()
	case !r.stallWait:
		r.release(now)
	case !r.stallRelease.pending():
		r.arm(&r.stallRelease, r.scaledMs(stallReleaseMs), func(now int64) {
			if r.phase == phaseWaiting && len(r.waitingOn) == 0 {
				r.release(now)
			}
		})
	}
}

func (r *Room) enterWaiting(pos, timeoutMs, now int64, byStall bool) {
	r.stopPlaying()
	r.stallWait = byStall
	set(r, &r.phase, phaseWaiting)
	set(r, &r.positionMs, pos)
	r.setWaitingOn(r.notReady(now))
	r.arm(&r.barrier, r.scaledMs(timeoutMs), r.barrierTimeout)
	if len(r.waitingOn) == 0 {
		r.release(now)
	}
}

func (r *Room) barrierTimeout(now int64) {
	until := now + r.scaledMs(exclusionMs)
	for _, p := range r.waitingOn {
		p.excludedUntil = until
	}
	r.release(now)
}

func (r *Room) release(now int64) {
	r.barrier.stop()
	r.stallRelease.stop()
	r.stallWait = false
	set(r, &r.phase, phasePlaying)
	r.setWaitingOn(nil)
	maxRtt := int64(-1)
	for _, p := range r.participants {
		if r.counted(p, now) {
			rtt := int64(defaultRttMs)
			if p.hasRtt {
				rtt = p.rttMs
			}
			maxRtt = max(maxRtt, rtt)
		}
	}
	if maxRtt < 0 {
		maxRtt = defaultRttMs
	}
	lo, hi := float64(r.scaledMs(startDelayMinMs)), float64(r.scaledMs(startDelayMaxMs))
	set(r, &r.atMs, now+int64(min(max(math.Floor(startDelayRttRate*float64(maxRtt)), lo), hi)))
	r.scheduleMediaEnd(now)
}

// pauseAt enters paused at pos; the action is the caller's business.
func (r *Room) pauseAt(pos int64) {
	r.stopPlaying()
	r.stallWait = false
	set(r, &r.phase, phasePaused)
	set(r, &r.positionMs, pos)
	r.setWaitingOn(nil)
}

// stopPlaying cancels every timer that belongs to playing or waiting.
func (r *Room) stopPlaying() {
	r.barrier.stop()
	r.stallRelease.stop()
	r.mediaEnd.stop()
	for _, p := range r.participants {
		p.stallGrace.stop()
	}
}

// scheduleMediaEnd pauses the room with the action `ended` the moment P(t)
// reaches durationMs. Media time is never scaled.
func (r *Room) scheduleMediaEnd(now int64) {
	r.mediaEnd.stop()
	if r.phase != phasePlaying {
		return
	}
	if r.position(now) >= r.durationMs {
		r.pauseAt(r.durationMs)
		r.act("ended", "")
		return
	}
	end := r.atMs + int64(math.Ceil(float64(r.durationMs-r.positionMs)/r.rate))
	r.arm(&r.mediaEnd, max(1, end-now), r.scheduleMediaEnd)
}
