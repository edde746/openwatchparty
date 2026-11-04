package relay

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	roomsFile    = "rooms.json"
	usageFile    = "usage.json"
	stateVersion = 1
	// A change waits saveInterval before the write that captures it, so a
	// burst of changes makes one write. The interval grows with the room
	// count, since each write holds every room. While a room plays, writes
	// also come every checkpointInterval, so its saved position moves on. Both
	// stay below SPEC.md §14's 5 s with time left for the write.
	minSaveInterval    = time.Second
	maxSaveInterval    = 4 * time.Second
	saveIntervalRoom   = time.Millisecond // per room held
	checkpointInterval = 4 * time.Second
	usageSaveInterval  = time.Minute
	errorLogInterval   = time.Minute
)

// saveInterval is the wait between a change and the write with rooms rooms.
func saveInterval(rooms int) time.Duration {
	return min(max(time.Duration(rooms)*saveIntervalRoom, minSaveInterval), maxSaveInterval)
}

// stateFile is rooms.json: every room the relay holds (SPEC.md §14). It holds
// no IPs, rtts, exclusions or timers, and token hashes instead of tokens.
type stateFile struct {
	Version int          `json:"version"`
	Rooms   []roomRecord `json:"rooms"`
	Reserved []string `json:"reserved,omitempty"`
}

type roomRecord struct {
	Code         string              `json:"code"`
	CreatedAt    int64               `json:"createdAt"` // Unix milliseconds
	Rev          int64               `json:"rev"`
	Epoch        int64               `json:"epoch"`
	Media        *string             `json:"media"`      // the descriptor's bytes as received, as a string
	PositionMs   int64               `json:"positionMs"` // P at the time of writing
	Rate         float64             `json:"rate"`
	Control      string              `json:"control"`
	Locked       bool                `json:"locked"`
	PeerSeq      int                 `json:"peerSeq"`
	Participants []participantRecord `json:"participants"` // join order
}

type participantRecord struct {
	PeerID    string      `json:"peerId"`
	TokenHash string      `json:"tokenHash"` // base64url SHA-256 of the token
	Name      string      `json:"name"`
	Client    string      `json:"client"`
	Host      bool        `json:"host"`
	Match     *matchState `json:"match"`
	OffsetMs  int64       `json:"offsetMs"`
}

// store writes rooms.json after changes. Only its run goroutine and
// Shutdown (after run has returned) write.
type store struct {
	srv  *Server
	path string
	wake chan struct{}

	lastErrorLog time.Time
}

func newStore(s *Server, path string) *store {
	return &store{srv: s, path: path, wake: make(chan struct{}, 1)}
}

// mark asks for a write soon. It never blocks, so it is safe under a room lock.
func (st *store) mark() {
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

func (st *store) run() {
	defer st.srv.done.Done()
	usageTick := time.NewTicker(usageSaveInterval)
	defer usageTick.Stop()
	write := time.NewTimer(0)
	write.Stop()
	defer write.Stop()
	var due time.Time // the next write; zero while none is due
	for {
		select {
		case <-st.wake:
			if at := time.Now().Add(saveInterval(st.srv.roomCount())); due.IsZero() || at.Before(due) {
				due = at
				write.Reset(time.Until(due))
			}
		case <-write.C:
			// Changes before this point are in the state captured below; later
			// ones wake the loop again.
			select {
			case <-st.wake:
			default:
			}
			due = time.Time{}
			if st.save() {
				due = time.Now().Add(checkpointInterval)
				write.Reset(checkpointInterval)
			}
		case <-usageTick.C:
			if err := st.srv.usage.save(); err != nil {
				st.logError("usage counts not saved", err)
			}
		case <-st.srv.stop:
			return // Shutdown writes the final state
		}
	}
}

// save writes every room the relay holds and reports whether one of them is
// playing. Each room's lock is held only to copy its record; encoding and the
// write run without any room lock.
func (st *store) save() (playing bool) {
	rooms := st.srv.roomList()
	now := st.srv.now()
	file := stateFile{Version: stateVersion, Rooms: make([]roomRecord, 0, len(rooms))}
	if st.srv.cfg.PermanentRooms {
		st.srv.mu.Lock()
		for code := range st.srv.reserved {
			file.Reserved = append(file.Reserved, code)
		}
		st.srv.mu.Unlock()
		slices.Sort(file.Reserved)
	}
	for _, r := range rooms {
		r.mu.Lock()
		if !r.closed {
			file.Rooms = append(file.Rooms, r.record(now))
			playing = playing || r.phase == phasePlaying
		}
		r.mu.Unlock()
	}
	if err := writeFileAtomic(st.path, encode(file)); err != nil {
		st.logError("room state not saved", err)
	}
	return playing
}

func (st *store) logError(what string, err error) {
	if time.Since(st.lastErrorLog) >= errorLogInterval {
		st.lastErrorLog = time.Now()
		log.Printf("relay: %s: %v", what, err)
	}
}

// record captures r for rooms.json; the caller holds r.mu.
func (r *Room) record(now int64) roomRecord {
	rec := roomRecord{
		Code: r.code, CreatedAt: r.createdAt.UnixMilli(), Rev: r.rev, Epoch: r.epoch,
		PositionMs: r.position(now), Rate: r.rate, Control: r.control, Locked: r.locked, PeerSeq: r.peerSeq,
		Participants: make([]participantRecord, len(r.participants)),
	}
	if r.media != nil {
		media := string(r.media)
		rec.Media = &media
	}
	for i, p := range r.participants {
		rec.Participants[i] = participantRecord{PeerID: p.peerID,
			TokenHash: base64.RawURLEncoding.EncodeToString(p.tokenHash[:]), Name: p.name, Client: p.client,
			Host: p == r.host, Match: p.match, OffsetMs: p.offsetMs}
	}
	return rec
}

// writeFileAtomic replaces path with data: a synced temporary file renamed
// over it, readable by the relay's user only.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o600) // in case a stale temporary file had other permissions
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync() // makes the rename durable; best effort
		d.Close()
	}
	return nil
}

// restore loads rooms.json (SPEC.md §14): every room whose maximum age hasn't
// passed comes back paused at its saved position, everyone disconnected.
func (s *Server) restore() {
	data, err := os.ReadFile(s.store.path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("relay: saved rooms not restored: %v", err)
		return
	}
	var file stateFile
	if err := json.Unmarshal(data, &file); err != nil || file.Version != stateVersion {
		log.Printf("relay: saved rooms not restored: %s is not a version %d state file", roomsFile, stateVersion)
		return
	}
	now, wall := s.now(), time.Now()
	if s.cfg.PermanentRooms {
		for _, code := range file.Reserved {
			if validCode(code) {
				s.reserved[code] = true
			}
		}
	}
	invalid, expired := 0, 0
	for i := range file.Rooms {
		r, err := s.restoreRoom(&file.Rooms[i], now, wall)
		switch {
		case err != nil:
			invalid++
		case r == nil:
			expired++
		default:
			s.rooms[r.code] = r
			if s.cfg.PermanentRooms {
				s.reserved[r.code] = true
			}
		}
	}
	if invalid > 0 {
		log.Printf("relay: skipped %d invalid rooms in %s", invalid, roomsFile)
	}
	if invalid+expired > 0 {
		s.store.mark() // the dropped rooms leave the file at the first write
	}
}

// restoreRoom rebuilds one saved room; nil without error when it has expired.
func (s *Server) restoreRoom(rec *roomRecord, now int64, wall time.Time) (*Room, error) {
	if !validCode(rec.Code) || s.rooms[rec.Code] != nil {
		return nil, errors.New("bad or duplicate code")
	}
	// In milliseconds, so no createdAt overflows. One in the future means the
	// clock went back: it counts as now, so the room lives at most maxRoomAgeMs.
	wallMs, maxAgeMs := wall.UnixMilli(), s.scaledMs(maxRoomAgeMs)
	if rec.CreatedAt <= wallMs-maxAgeMs {
		return nil, nil
	}
	createdMs := min(rec.CreatedAt, wallMs)
	age := maxAgeMs - (wallMs - createdMs)
	if rec.Rev < 1 || rec.Epoch < 0 || !(rec.Rate >= minRate && rec.Rate <= maxRate) ||
		(rec.Control != controlHost && rec.Control != controlEveryone) || rec.PeerSeq < 0 ||
		len(rec.Participants) < 1 {
		return nil, errors.New("bad room fields")
	}
	r := newRoom(s, rec.Code, now)
	r.createdAt = time.UnixMilli(createdMs)
	r.rev, r.epoch, r.rate, r.control, r.locked, r.peerSeq = rec.Rev, rec.Epoch, rec.Rate, rec.Control, rec.Locked, rec.PeerSeq
	if rec.Media != nil {
		media := json.RawMessage(*rec.Media)
		duration, err := validateMedia(media)
		if err != nil {
			return nil, errors.New("bad media")
		}
		r.media, r.durationMs, r.phase = media, duration, phasePaused
		r.positionMs = min(max(rec.PositionMs, 0), duration)
	}
	for _, pr := range rec.Participants {
		hash, err := base64.RawURLEncoding.DecodeString(pr.TokenHash)
		if err != nil || len(hash) != 32 || !validPeerID(pr.PeerID) || r.find(pr.PeerID) != nil ||
			pr.OffsetMs < -maxDurationMs || pr.OffsetMs > maxDurationMs {
			return nil, fmt.Errorf("bad participant")
		}
		p := &participant{peerID: pr.PeerID, name: pr.Name, client: pr.Client, player: playerIdle,
			match: pr.Match, offsetMs: pr.OffsetMs, statusEpoch: -1}
		copy(p.tokenHash[:], hash)
		if pr.Host {
			if r.host != nil {
				return nil, errors.New("two hosts")
			}
			r.host = p
		}
		r.participants = append(r.participants, p)
	}
	if r.host == nil {
		return nil, errors.New("no host")
	}
	for _, p := range r.participants {
		r.arm(&p.removal, r.scaledMs(resumeMs), func(now int64) { r.remove(p, now) })
	}
	r.hostAbsentSince = now
	r.arm(&r.hostPromotion, r.scaledMs(hostPromotionMs), r.promoteHost)
	r.arm(&r.maxAge, age, func(int64) { r.close("expired") })
	return r, nil
}

func validCode(code string) bool {
	if len(code) != codeLength {
		return false
	}
	for _, c := range []byte(code) {
		if !isCodeSymbol(c) {
			return false
		}
	}
	return true
}

func isCodeSymbol(c byte) bool {
	return c < 0x80 && containsByte(codeAlphabet, c)
}

func containsByte(s string, c byte) bool {
	for i := range len(s) {
		if s[i] == c {
			return true
		}
	}
	return false
}

func validPeerID(id string) bool {
	if id == "" || len(id) > maxPeerIDLength {
		return false
	}
	for _, c := range []byte(id) {
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// usage keeps the daily connection counts per <app> name (SPEC.md §15), by
// UTC day, for the last usageDays days. A name new to the day counts under
// otherApp when the day already has maxAppsPerDay names or when its address
// (an IPv4 address or IPv6 /64) already added namesPerAddress names that day,
// so no client fills the record with names of its own.
type usage struct {
	path string // "" keeps the counts in memory only

	mu       sync.Mutex
	counts   map[string]map[string]int64
	dirty    bool
	addedDay string         // the UTC day added counts
	added    map[string]int // names each address added to addedDay; memory only
}

const (
	maxAppsPerDay   = 1000
	namesPerAddress = 3
	otherApp        = "(other)"
	usageDays       = 400 // days kept, today included
)

func newUsage(path string) *usage {
	return &usage{path: path, counts: make(map[string]map[string]int64)}
}

// add counts one accepted create, join or resume by app from address addr
// (ipGroups' first entry).
func (u *usage) add(app, addr string, t time.Time) {
	day := t.UTC().Format(time.DateOnly)
	u.mu.Lock()
	defer u.mu.Unlock()
	apps := u.counts[day]
	if apps == nil {
		apps = make(map[string]int64)
		u.counts[day] = apps
		dropOldDays(u.counts, t) // a new day: the oldest one may have run out
	}
	if u.addedDay != day {
		u.addedDay, u.added = day, make(map[string]int)
	}
	if _, known := apps[app]; !known && app != otherApp {
		if len(apps) >= maxAppsPerDay || u.added[addr] >= namesPerAddress {
			app = otherApp
		} else {
			u.added[addr]++
		}
	}
	apps[app]++
	u.dirty = true
}

// sweep applies the day boundary without waiting for a counted request: it
// deletes the days that ran out and, once now's UTC day has begun, forgets
// which names each address added the day before.
func (u *usage) sweep(now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if dropOldDays(u.counts, now) {
		u.dirty = true
	}
	if u.addedDay != now.UTC().Format(time.DateOnly) {
		u.addedDay, u.added = "", nil
	}
}

// dropOldDays deletes every day before the usageDays days ending with now's
// day, and keys that aren't days. It reports whether it deleted any.
func dropOldDays(counts map[string]map[string]int64, now time.Time) bool {
	oldest := now.UTC().AddDate(0, 0, -(usageDays - 1)).Format(time.DateOnly)
	dropped := false
	for day := range counts {
		if _, err := time.Parse(time.DateOnly, day); err != nil || day < oldest {
			delete(counts, day)
			dropped = true
		}
	}
	return dropped
}

// days returns a copy of every day's counts.
func (u *usage) days() map[string]map[string]int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return copyCounts(u.counts)
}

func copyCounts(counts map[string]map[string]int64) map[string]map[string]int64 {
	out := make(map[string]map[string]int64, len(counts))
	for day, apps := range counts {
		out[day] = maps.Clone(apps)
	}
	return out
}

// topApps returns the n names with the highest counts on day, otherApp left
// out; ties go to the name that sorts first.
func (u *usage) topApps(day string, n int) map[string]bool {
	u.mu.Lock()
	counts := maps.Clone(u.counts[day])
	u.mu.Unlock()
	names := make([]string, 0, len(counts))
	for app := range counts {
		if app != otherApp {
			names = append(names, app)
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		if c := cmp.Compare(counts[b], counts[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	top := make(map[string]bool, min(n, len(names)))
	for _, app := range names[:min(n, len(names))] {
		top[app] = true
	}
	return top
}

// load reads usage.json, dropping days older than usageDays before now.
func (u *usage) load(now time.Time) error {
	data, err := os.ReadFile(u.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	counts := make(map[string]map[string]int64)
	if err := json.Unmarshal(data, &counts); err != nil {
		return fmt.Errorf("%s: %v", usageFile, err)
	}
	dropped := dropOldDays(counts, now)
	u.mu.Lock()
	u.counts, u.dirty = counts, dropped
	u.mu.Unlock()
	return nil
}

// save writes the counts if they changed since the last save. It encodes a
// copy, so add never waits for the encoding or the write.
func (u *usage) save() error {
	if u.path == "" {
		return nil
	}
	u.mu.Lock()
	if !u.dirty {
		u.mu.Unlock()
		return nil
	}
	counts := copyCounts(u.counts)
	u.dirty = false
	u.mu.Unlock()
	if err := writeFileAtomic(u.path, encode(counts)); err != nil {
		u.mu.Lock()
		u.dirty = true
		u.mu.Unlock()
		return err
	}
	return nil
}
