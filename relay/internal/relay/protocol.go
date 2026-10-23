package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Error codes (SPEC.md, Errors).
const (
	errInvalidMessage = "invalid_message"
	errUnsupported    = "unsupported"
	errRateLimited    = "rate_limited"
	errRoomNotFound   = "room_not_found"
	errRoomFull       = "room_full"
	errRoomLocked     = "room_locked"
	errForbidden      = "forbidden"
	errNotInRoom      = "not_in_room"
	errAlreadyInRoom  = "already_in_room"
	errResumeFailed   = "resume_failed"
	errClientBlocked  = "client_blocked"
	errRelayFull      = "relay_full"
)

var errorCodes = []string{errInvalidMessage, errUnsupported, errRateLimited, errRoomNotFound, errRoomFull,
	errRoomLocked, errForbidden, errNotInRoom, errAlreadyInRoom, errResumeFailed, errClientBlocked,
	errRelayFull}

// Protocol limits (SPEC.md §13.1) and code format (§5.1).
const (
	maxMessageBytes = 8192
	maxMediaBytes   = 4096
	maxExtBytes     = 1024
	maxNameRunes    = 32
	maxClientRunes  = 64
	minRate         = 0.5
	maxRate         = 2.0
	maxSafeInteger  = 1<<53 - 1
	maxDurationMs   = 604_800_000 // 7 days; also the largest offsetMs either way
	maxPeerIDLength = 16
	codeLength      = 6
	codeAlphabet    = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
)

// participantsBound is the largest MaxParticipants (SPEC.md §13.2): a bigger
// room could send snapshots over the 32768 bytes clients accept.
const participantsBound = 32

// Limits are the policy limits of SPEC.md §13.2, each a relay setting.
type Limits struct {
	MaxParticipants     int
	MaxConnections      int
	MaxConnectionsPerIP int
	MaxRooms            int
	MaxRoomsPerIP       int
	IPv6Factor56        int
	IPv6Factor48        int
	MessageBurst        int
	MessagesPerSecond   float64
	JoinBurst           int
	JoinsPerSecond      float64
	JoinBurstTotal      int
	JoinsPerSecondTotal float64
}

// DefaultLimits are the public relays' values.
var DefaultLimits = Limits{MaxParticipants: 32, MaxConnections: 10000, MaxConnectionsPerIP: 10, MaxRooms: 10000,
	MaxRoomsPerIP: 3, IPv6Factor56: 4, IPv6Factor48: 16, MessageBurst: 30, MessagesPerSecond: 10, JoinBurst: 20,
	JoinsPerSecond: 1, JoinBurstTotal: 2000, JoinsPerSecondTotal: 200}

// factors are the per-IP limit multipliers of the ipGroups entries: an IPv4
// address or IPv6 /64, a /56, a /48 (SPEC.md §3.1).
func (l Limits) factors() [3]int { return [3]int{1, l.IPv6Factor56, l.IPv6Factor48} }

func (l Limits) check() error {
	switch {
	case l.MaxParticipants < 1 || l.MaxParticipants > participantsBound:
		return errors.New("--max-participants must be 1-" + strconv.Itoa(participantsBound))
	case l.MaxConnections < 1:
		return errors.New("--max-connections must be at least 1")
	case l.MaxConnectionsPerIP < 1:
		return errors.New("--max-connections-per-ip must be at least 1")
	case l.MaxRoomsPerIP < 1:
		return errors.New("--max-rooms-per-ip must be at least 1")
	case l.MaxRooms < 1:
		return errors.New("--max-rooms must be at least 1")
	case l.IPv6Factor56 < 1:
		return errors.New("--ipv6-factor-56 must be at least 1")
	case l.IPv6Factor48 < 1:
		return errors.New("--ipv6-factor-48 must be at least 1")
	case l.MessageBurst < 1:
		return errors.New("--message-burst must be at least 1")
	case !(l.MessagesPerSecond > 0) || math.IsInf(l.MessagesPerSecond, 0):
		return errors.New("--messages-per-second must be a positive number")
	case l.JoinBurst < 1:
		return errors.New("--join-burst must be at least 1")
	case !(l.JoinsPerSecond > 0) || math.IsInf(l.JoinsPerSecond, 0):
		return errors.New("--joins-per-second must be a positive number")
	case l.JoinBurstTotal < 1:
		return errors.New("--join-burst-total must be at least 1")
	case !(l.JoinsPerSecondTotal > 0) || math.IsInf(l.JoinsPerSecondTotal, 0):
		return errors.New("--joins-per-second-total must be a positive number")
	}
	return nil
}

// relayError is an `error` reply to the sender; it never closes the connection.
type relayError struct {
	code, message string
}

func invalid(message string) *relayError { return &relayError{errInvalidMessage, message} }

// fields is a JSON object decoded down to its members. Keys match exactly,
// unlike encoding/json struct decoding, which matches them case-insensitively.
type fields map[string]json.RawMessage

func parseObject(data []byte) fields {
	var f fields
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	return f // nil for the literal null
}

func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	return s, json.Unmarshal(raw, &s) == nil
}

func isJSONNumber(raw json.RawMessage) bool {
	return len(raw) > 0 && (raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9')
}

// jsonInt accepts a JSON number with an integral value within ±(2^53−1), as
// JSON Schema's "integer": 3 and 3.0 are ints, 3.5 is not.
func jsonInt(raw json.RawMessage) (int64, bool) {
	if !isJSONNumber(raw) {
		return 0, false
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(string(raw), 64)
		if ferr != nil || f != math.Trunc(f) || math.Abs(f) > maxSafeInteger {
			return 0, false
		}
		v = int64(f)
	}
	if v > maxSafeInteger || v < -maxSafeInteger {
		return 0, false
	}
	return v, true
}

// hasControlChar reports a forbidden character as SPEC.md §2 defines it:
// Unicode Cc (C0, DEL, C1) or Bidi_Control (the direction marks, embeddings,
// overrides and isolates that let a name display as something else).
func hasControlChar(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) })
}

// duplicateMembers reports whether the JSON object data has two `type`
// members, and whether any object in it, at any depth, names a member twice
// (SPEC.md §6.2). encoding/json keeps the last of them and a client may keep
// the first, so the relay refuses both. Names compare after decoding escapes.
// data must be valid JSON. It reads the bytes once, without recursion and
// without decoding values, because it runs on every message before the rate
// limit: a message nested thousands deep costs no more stack than a flat one.
func duplicateMembers(data []byte) (typeTwice, any bool) {
	type member struct {
		object int
		name   string
	}
	seen := make(map[member]bool)
	var open []int // the containers around i: an object's number, or -1 for an array
	objects := 0
	name := false // the next string is a member name
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '{':
			open = append(open, objects)
			objects++
			name = true
		case '[':
			open = append(open, -1)
		case '}', ']':
			open = open[:len(open)-1]
		case ',':
			name = open[len(open)-1] >= 0
		case '"':
			end := i + 1
			for data[end] != '"' {
				if data[end] == '\\' {
					end++
				}
				end++
			}
			if name {
				m := member{open[len(open)-1], memberName(data[i : end+1])}
				any = any || seen[m]
				typeTwice = typeTwice || seen[m] && m.object == 0 && m.name == "type"
				seen[m] = true
				name = false
			}
			i = end
		}
	}
	return typeTwice, any
}

// memberName decodes the quoted member name raw.
func memberName(raw []byte) string {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1 : len(raw)-1])
	}
	var s string
	json.Unmarshal(raw, &s)
	return s
}

// hasLoneSurrogate reports a \u escape of a UTF-16 surrogate other than a high
// surrogate directly followed by an escaped low one (SPEC.md §6.2).
// encoding/json decodes such an escape to U+FFFD, but a json.RawMessage would
// forward it as written. raw must be valid JSON text, which holds backslashes
// only as escapes inside strings.
func hasLoneSurrogate(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++ // the escaped character: `\\` is no escape start
		if raw[i] != 'u' {
			continue
		}
		r := hex4(raw[i+1:])
		i += 4
		switch {
		case 0xDC00 <= r && r <= 0xDFFF:
			return true
		case 0xD800 <= r && r <= 0xDBFF:
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return true
			}
			if lo := hex4(raw[i+3:]); lo < 0xDC00 || lo > 0xDFFF {
				return true
			}
			i += 6
		}
	}
	return false
}

// hex4 reads the 4 hex digits of a \u escape.
func hex4(b []byte) rune {
	var r rune
	for _, c := range b[:4] {
		r <<= 4
		switch {
		case '0' <= c && c <= '9':
			r |= rune(c - '0')
		case 'a' <= c && c <= 'f':
			r |= rune(c - 'a' + 10)
		case 'A' <= c && c <= 'F':
			r |= rune(c - 'A' + 10)
		}
	}
	return r
}

// clientName is the app part of a `client` value: everything before the
// first `/`.
func clientName(client string) string {
	name, _, _ := strings.Cut(client, "/")
	return name
}

// args reads typed members out of fields and keeps the first validation error.
type args struct {
	f   fields
	err *relayError
}

func (a *args) fail(message string) {
	if a.err == nil {
		a.err = invalid(message)
	}
}

func (a *args) str(key string) string {
	s, ok := jsonString(a.f[key])
	if !ok {
		a.fail(key + " must be a string")
	}
	return s
}

// text is a display string: 1 to maxRunes code points, no control characters.
func (a *args) text(key string, maxRunes int) string {
	s := a.str(key)
	if n := utf8.RuneCountInString(s); n < 1 || n > maxRunes || hasControlChar(s) {
		a.fail(key + " must be 1-" + strconv.Itoa(maxRunes) + " characters without control characters")
	}
	return s
}

// client is `<app>/<version>`, both parts non-empty, the app without `/`.
func (a *args) client() string {
	s := a.text("client", maxClientRunes)
	if app, version, ok := strings.Cut(s, "/"); !ok || app == "" || version == "" {
		a.fail("client must be <app>/<version>")
	}
	return s
}

func (a *args) integer(key string, min int64) int64 {
	v, ok := jsonInt(a.f[key])
	switch {
	case !ok && min == math.MinInt64:
		a.fail(key + " must be an integer")
	case !ok || v < min:
		a.fail(key + " must be an integer >= " + strconv.FormatInt(min, 10))
	}
	return v
}

// between is an integer in lo..hi, both included.
func (a *args) between(key string, lo, hi int64) int64 {
	if v, ok := jsonInt(a.f[key]); ok && v >= lo && v <= hi {
		return v
	}
	a.fail(key + " must be an integer in " + strconv.FormatInt(lo, 10) + ".." + strconv.FormatInt(hi, 10))
	return 0
}

func (a *args) optInteger(key string, min int64) (int64, bool) {
	if _, present := a.f[key]; !present {
		return 0, false
	}
	return a.integer(key, min), true
}

func (a *args) boolean(key string) bool {
	switch string(a.f[key]) {
	case "true":
		return true
	case "false":
	default:
		a.fail(key + " must be a boolean")
	}
	return false
}

// optBoolean is an optional boolean, false when absent.
func (a *args) optBoolean(key string) bool {
	if _, present := a.f[key]; !present {
		return false
	}
	return a.boolean(key)
}

func (a *args) enum(key string, allowed ...string) string {
	if s, ok := jsonString(a.f[key]); ok {
		for _, v := range allowed {
			if s == v {
				return s
			}
		}
	}
	a.fail(key + " must be one of " + strings.Join(allowed, ", "))
	return ""
}

// Room commands, validated before the room lock is taken.

type matchState struct {
	State   string `json:"state"`
	DeltaMs *int64 `json:"deltaMs,omitempty"`
}

func (m *matchState) equal(o *matchState) bool {
	if m == nil || o == nil {
		return m == o
	}
	if m.State != o.State || (m.DeltaMs == nil) != (o.DeltaMs == nil) {
		return false
	}
	return m.DeltaMs == nil || *m.DeltaMs == *o.DeltaMs
}

type command struct {
	typ string

	// status
	epoch    int64
	player   string
	match    *matchState
	offsetMs int64
	rttMs    int64
	hasRtt   bool

	positionMs  int64 // pause, seek, setMedia
	hasPosition bool
	rate        float64
	media       json.RawMessage
	durationMs  int64
	mode        string
	peerID      string
	locked      bool
}

var roomCommands = map[string]bool{
	"status": true, "play": true, "pause": true, "seek": true, "rate": true, "setMedia": true, "clearMedia": true,
	"setControl": true, "transferHost": true, "kick": true, "lock": true, "leave": true, "end": true,
}

func parseCommand(typ string, f fields) (*command, *relayError) {
	cmd := &command{typ: typ}
	a := &args{f: f}
	switch typ {
	case "status":
		cmd.epoch = a.integer("epoch", 0)
		cmd.player = a.enum("player", playerIdle, playerLoading, playerReady, playerBuffering)
		cmd.match = a.match()
		cmd.offsetMs = a.between("offsetMs", -maxDurationMs, maxDurationMs)
		cmd.rttMs, cmd.hasRtt = a.optInteger("rttMs", 0)
	case "pause":
		cmd.positionMs, cmd.hasPosition = a.optInteger("positionMs", 0)
	case "seek":
		cmd.positionMs, cmd.hasPosition = a.integer("positionMs", 0), true
	case "rate":
		raw := f["rate"]
		v, err := strconv.ParseFloat(string(raw), 64)
		if !isJSONNumber(raw) || err != nil || v < minRate || v > maxRate {
			a.fail("rate must be a number in 0.5-2.0")
		}
		cmd.rate = v
	case "setMedia":
		cmd.media, cmd.durationMs = a.media()
		cmd.positionMs, cmd.hasPosition = a.optInteger("positionMs", 0)
	case "setControl":
		cmd.mode = a.enum("mode", controlHost, controlEveryone)
	case "transferHost", "kick":
		cmd.peerID = a.str("peerId")
	case "lock":
		cmd.locked = a.boolean("locked")
	}
	if a.err != nil {
		return nil, a.err
	}
	return cmd, nil
}

func (a *args) match() *matchState {
	raw, present := a.f["match"]
	if string(raw) == "null" {
		return nil
	}
	if !present || raw[0] != '{' {
		a.fail("match must be an object or null")
		return nil
	}
	m := &args{f: parseObject(raw)}
	state := &matchState{State: m.enum("state", "searching", "matched", "mismatched", matchUnavailable)}
	if delta, ok := m.optInteger("deltaMs", math.MinInt64); ok {
		state.DeltaMs = &delta
	}
	if m.err != nil {
		a.fail("match." + m.err.message)
	}
	return state
}

// media validates a descriptor and returns it exactly as received.
func (a *args) media() (json.RawMessage, int64) {
	raw := a.f["media"]
	if len(raw) == 0 || raw[0] != '{' {
		a.fail("media must be an object")
		return nil, 0
	}
	duration, err := validateMedia(raw)
	if err != nil {
		a.fail("media." + err.message)
	}
	return raw, duration
}

// validateMedia checks a descriptor against SPEC.md §10 and returns its
// durationMs. Unknown members pass through untouched.
func validateMedia(raw json.RawMessage) (int64, *relayError) {
	if len(raw) > maxMediaBytes {
		return 0, invalid("media must be at most 4096 bytes")
	}
	m := &args{f: parseObject(raw)}
	if m.f == nil {
		return 0, invalid("media must be an object")
	}
	kind := m.enum("kind", "movie", "episode", "video")
	m.printable("title")
	m.optInteger("year", math.MinInt64)
	m.ids("ids")
	if series, ok := m.object("series", kind == "episode"); ok {
		s := &args{f: series}
		s.printable("title")
		s.optInteger("year", math.MinInt64)
		s.ids("ids")
		m.nested("series", s)
	}
	m.optInteger("season", 0)
	m.optInteger("episode", 0)
	duration := m.between("durationMs", 1, maxDurationMs)
	if file, ok := m.object("file", false); ok {
		f := &args{f: file}
		f.optInteger("sizeBytes", 0)
		if _, present := file["oshash"]; present && !isOSHash(f.str("oshash")) {
			f.fail("oshash must be 16 lower-case hex digits")
		}
		if _, present := file["name"]; present {
			if name := f.printable("name"); name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
				f.fail(`name must be a file name: no / or \, not . or ..`)
			}
		}
		m.nested("file", f)
	}
	if ext, present := m.f["ext"]; present {
		if len(ext) > maxExtBytes {
			m.fail("ext must be at most 1024 bytes")
		} else if members, ok := m.object("ext", false); ok {
			for name := range members {
				if !isReverseDNS(name) {
					m.fail("ext member names must be reverse-DNS names")
				}
			}
		}
	}
	return duration, m.err
}

// printable is a descriptor string shown to people: at least one code point,
// no forbidden character (SPEC.md §2, §10).
func (a *args) printable(key string) string {
	s := a.str(key)
	if s == "" || hasControlChar(s) {
		a.fail(key + " must be at least one character without control characters")
	}
	return s
}

// object returns the member key as an object. An absent member fails only
// when required.
func (a *args) object(key string, required bool) (fields, bool) {
	raw, present := a.f[key]
	if !present {
		if required {
			a.fail(key + " is required")
		}
		return nil, false
	}
	if len(raw) == 0 || raw[0] != '{' {
		a.fail(key + " must be an object")
		return nil, false
	}
	return parseObject(raw), true
}

// ids checks an optional namespace → ID object: every value a non-empty string.
func (a *args) ids(key string) {
	ids, ok := a.object(key, false)
	if !ok {
		return
	}
	for _, v := range ids {
		if s, ok := jsonString(v); !ok || s == "" {
			a.fail(key + " values must be non-empty strings")
		}
	}
}

func (a *args) nested(key string, sub *args) {
	if sub.err != nil {
		a.fail(key + "." + sub.err.message)
	}
}

func isOSHash(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range []byte(s) {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// isReverseDNS accepts two or more dot-separated labels of a-z, 0-9 and -.
func isReverseDNS(s string) bool {
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if l == "" || strings.ContainsFunc(l, func(c rune) bool {
			return !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-')
		}) {
			return false
		}
	}
	return true
}

// Relay → client messages.

type errorMsg struct {
	Type    string  `json:"type"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Re      *string `json:"re"`
}

type pongMsg struct {
	Type       string          `json:"type"`
	ID         json.RawMessage `json:"id"`
	ServerTime int64           `json:"serverTime"`
}

type closedMsg struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type you struct {
	PeerID string `json:"peerId"`
	Token  string `json:"token"`
}

// joinedMsg and roomMsg leave out `media`: spliceMedia appends it, so the
// descriptor goes out byte for byte as the host sent it (SPEC.md §8.3).
type joinedMsg struct {
	Type       string    `json:"type"`
	Code       string    `json:"code"`
	You        you       `json:"you"`
	Room       *snapshot `json:"room"`
	ServerTime int64     `json:"serverTime"`
}

// spliceMedia adds a `media` member holding raw (null when nil) to the
// encoded object obj. encoding/json would compact raw; this keeps its bytes.
func spliceMedia(obj []byte, raw json.RawMessage) []byte {
	if raw == nil {
		raw = json.RawMessage("null")
	}
	out := make([]byte, 0, len(obj)+len(`,"media":`)+len(raw))
	out = append(out, obj[:len(obj)-1]...)
	out = append(out, `,"media":`...)
	out = append(out, raw...)
	return append(out, '}')
}

type roomMsg struct {
	Type string `json:"type"`
	*snapshot
	Action *action `json:"action,omitempty"`
}

type snapshot struct {
	Rev          int64             `json:"rev"`
	Code         string            `json:"code"`
	Epoch        int64             `json:"epoch"`
	Phase        string            `json:"phase"`
	PositionMs   int64             `json:"positionMs"`
	AtMs         int64             `json:"atMs"`
	Rate         float64           `json:"rate"`
	Control      string            `json:"control"`
	Locked       bool              `json:"locked"`
	WaitingOn    []string          `json:"waitingOn"`
	Participants []participantView `json:"participants"`
}

// action is what the event that published a `room` did (SPEC.md §9.1).
type action struct {
	Kind string  `json:"kind"`
	By   *string `json:"by"`
}

type participantView struct {
	PeerID    string      `json:"peerId"`
	Name      string      `json:"name"`
	Client    string      `json:"client"`
	Host      bool        `json:"host"`
	Connected bool        `json:"connected"`
	Player    string      `json:"player"`
	Match     *matchState `json:"match"`
	OffsetMs  int64       `json:"offsetMs"`
}

// encode marshals without HTML escaping, so pass-through values (ping ids)
// keep their characters; encoding/json only drops insignificant whitespace.
func encode(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // every outbound type is encodable
	}
	return buf.Bytes()[:buf.Len()-1] // drop Encode's newline
}
