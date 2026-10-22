package main

// checkTraces refuses a party table that a driver couldn't run as the README's party section defines it: unknown step
// kinds, calls or members, an outputs count other than the steps count, outputs on unknown channels or of the wrong
// shape, players and calls that don't exist, a call whose completion is never reported, relay input the SDK would
// refuse, and duplicate names.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
)

// callArgs lists each call's members besides `call` and `id`: required ones first, then optional ones after "?".
var callArgs = map[string][]string{
	"create": {}, "join": {"code", "?", "create"}, "leave": {}, "end": {}, "close": {},
	"setMedia": {"media", "ref", "?", "positionMs"}, "clearMedia": {}, "play": {}, "pause": {}, "seek": {"ms"},
	"setRate": {"rate"}, "setOffset": {"ms"}, "setControl": {"mode"}, "lock": {"locked"},
	"kick": {"peerId"}, "transferHost": {"peerId"}, "choose": {"candidate"}, "acceptMismatch": {},
	"declineMismatch": {}, "resolveAgain": {}, "attachPlayer": {"player", "ref"}, "detachPlayer": {"?", "player"},
	"playerChanged": {}, "refresh": {},
}

// idRequired lists the calls whose result only a `done` output shows.
var idRequired = []string{"create", "join", "close", "choose", "detachPlayer", "refresh"}

// stepMembers lists each step kind's members besides the kind itself.
var stepMembers = map[string][]string{
	"recv": {"on", "malformed"}, "recvText": {"on"}, "drop": {"on"}, "nextOpen": {"on"}, "rtt": {"on"}, "nextGet": {}, "advance": {}, "hold": {},
	"finish": {"throws"}, "answer": {}, "answerThrows": {}, "probeAnswer": {"durationMs"}, "probeThrows": {},
	"player": {"positionMs", "paused", "buffering", "durationMs", "firstFrameShown", "canSetRate", "seekAccuracyMs",
		"playThrows"},
}

var channels = []string{"send", "command", "event", "resolver", "net", "done", "state"}

var stateFields = []string{"connection", "peerId", "room", "media", "match", "choice", "mismatch", "candidates",
	"offsetMs", "player", "endReason", "isHost", "canControl", "roomPositionMs"}

// eventMembers lists each event type's members besides `type`.
var eventMembers = map[string][]string{
	"action": {"kind", "byPeerId", "by", "byMe", "room"}, "participantJoined": {"peerId"},
	"participantLeft": {"peerId"}, "error": {"code", "re"}, "resolveFailed": {"timeout"}, "playBlocked": {},
	"ended": {"reason", "error"},
}

var basePattern = regexp.MustCompile(`^https?://[^/?#]+`)

// wsURL: the URL of a relay connection, as `on` names it.
var wsURL = stringWith(regexp.MustCompile(`^wss?://[^/?#]+`).MatchString)

var setupMembers = []string{"startMs", "relayOffsetMs", "rttMs", "random", "base", "list", "name", "client",
	"resolver"}

func checkTraces(cases []Case) error {
	names := map[string]bool{}
	for _, c := range cases {
		if names[c.Name] {
			return fmt.Errorf("party: duplicate case name %q", c.Name)
		}
		names[c.Name] = true
		if err := checkTrace(c); err != nil {
			return fmt.Errorf("party: %s: %w", c.Name, err)
		}
	}
	return nil
}

// generic decodes v's JSON as a driver sees it, numbers kept as json.Number.
func generic(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var g any
	err = d.Decode(&g)
	return g, err
}

// traceState is what checking a trace has seen so far.
type traceState struct {
	players map[string]bool
	calls   map[string]int // call id → step index
	done    map[string]bool
}

func checkTrace(c Case) error {
	in, err := generic(c.Input)
	if err != nil {
		return err
	}
	exp, err := generic(c.Expect)
	if err != nil {
		return err
	}
	input, ok := in.(object)
	if !ok || !hasOnly(input, "setup", "steps") {
		return fmt.Errorf("input must be {setup, steps}")
	}
	if err := checkSetup(input["setup"]); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	expect, ok := exp.(object)
	if !ok || !hasOnly(expect, "outputs") {
		return fmt.Errorf("expect must be {outputs}")
	}
	steps, ok := input["steps"].([]any)
	if !ok || len(steps) == 0 {
		return fmt.Errorf("steps must be a non-empty array")
	}
	outputs, ok := expect["outputs"].([]any)
	if !ok || len(outputs) != len(steps) {
		return fmt.Errorf("%d steps but %d outputs", len(steps), len(outputs))
	}
	st := traceState{players: map[string]bool{}, calls: map[string]int{}, done: map[string]bool{}}
	for i, s := range steps {
		if err := st.step(s, i); err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		if err := st.outputs(outputs[i], i); err != nil {
			return fmt.Errorf("outputs %d: %w", i, err)
		}
	}
	for id := range st.calls {
		if !st.done[id] {
			return fmt.Errorf("call %q never reports done", id)
		}
	}
	return nil
}

func checkSetup(v any) error {
	s, ok := v.(object)
	if !ok {
		return fmt.Errorf("not an object")
	}
	if err := fieldsOK(s, setupMembers); err != nil {
		return err
	}
	ints := []string{"startMs", "relayOffsetMs", "rttMs"}
	for _, k := range ints {
		if _, ok := intOf(s[k]); !ok {
			return fmt.Errorf("%s must be an int", k)
		}
	}
	if rtt, _ := intOf(s["rttMs"]); rtt < 0 || rtt > 1000 {
		return fmt.Errorf("rttMs must be 0 to 1000")
	}
	draws, ok := s["random"].([]any)
	if !ok {
		return fmt.Errorf("random must be an array")
	}
	for _, d := range draws {
		// Swift reads u as Double.random(in: 0..<1, using:), which yields only multiples of 2^-53.
		if f, ok := numberOf(d); !ok || f < 0 || f >= 1 || math.Trunc(f*(1<<53)) != f*(1<<53) {
			return fmt.Errorf("random draws must be multiples of 2^-53 in [0, 1)")
		}
	}
	base, custom := s["base"].(string)
	if s["base"] != nil && (!custom || !basePattern.MatchString(base) || strings.HasSuffix(base, "/")) {
		return fmt.Errorf("base must be null (the public list) or an http(s) base URL without a trailing /")
	}
	if custom {
		if s["list"] != nil {
			return fmt.Errorf("list must be null with a base: Relays.at never fetches")
		}
	} else if list, _ := json.Marshal(s["list"]); !validList(list) {
		return fmt.Errorf("list must be a valid public list (SDK.md §3.2)")
	}
	if !stringWith(ValidName)(s["name"]) || !stringWith(ValidClient)(s["client"]) {
		return fmt.Errorf("name or client fails SDK.md §7.1")
	}
	if !isBool(s["resolver"]) {
		return fmt.Errorf("resolver must be a boolean")
	}
	return nil
}

func (st *traceState) step(v any, i int) error {
	s, ok := v.(object)
	if !ok {
		return fmt.Errorf("not an object")
	}
	if _, isCall := s["call"]; isCall {
		return st.call(s, i)
	}
	kind := ""
	for k := range s {
		if _, isStep := stepMembers[k]; isStep {
			if kind != "" {
				return fmt.Errorf("two step kinds, %s and %s", kind, k)
			}
			kind = k
		}
	}
	if kind == "" {
		return fmt.Errorf("no known step kind in %v", keys(s))
	}
	if err := fieldsOK(s, append([]string{kind, "?"}, stepMembers[kind]...)); err != nil {
		return err
	}
	if !optional(s, "on", wsURL) {
		return fmt.Errorf("on takes a ws:// or wss:// URL")
	}
	switch arg := s[kind]; kind {
	case "recv":
		if !optional(s, "malformed", func(v any) bool { return v == true }) {
			return fmt.Errorf("malformed takes true")
		}
		return checkRecv(arg, s["malformed"] == true)
	case "recvText":
		if _, isText := arg.(string); !isText {
			return fmt.Errorf("recvText takes a string")
		}
	case "drop":
		if arg != nil && !intIn(1000, 4999)(arg) {
			return fmt.Errorf("drop takes a close code or null")
		}
	case "nextOpen":
		if arg != "fail" && arg != "hang" {
			return fmt.Errorf("nextOpen takes fail or hang")
		}
	case "nextGet":
		if _, isDoc := arg.(object); !isDoc && arg != "fail" && arg != "hang" {
			return fmt.Errorf("nextGet takes a document (an object), fail or hang")
		}
	case "advance":
		if !intAtLeast(1)(arg) {
			return fmt.Errorf("advance takes an int ≥ 1")
		}
	case "rtt":
		if !nullOr(intAtLeast(0))(arg) {
			return fmt.Errorf("rtt takes an int ≥ 0 or null")
		}
	case "player":
		id, ok := arg.(string)
		if !ok || id == "" {
			return fmt.Errorf("player takes an id")
		}
		st.players[id] = true
		for _, k := range []string{"paused", "buffering", "firstFrameShown", "canSetRate", "playThrows"} {
			if !optional(s, k, isBool) {
				return fmt.Errorf("%s must be a boolean", k)
			}
		}
		if !optional(s, "positionMs", intAtLeast(0)) || !optional(s, "seekAccuracyMs", intAtLeast(0)) ||
			!optional(s, "durationMs", nullOr(intAtLeast(1))) {
			return fmt.Errorf("positionMs, seekAccuracyMs or durationMs out of range")
		}
	case "hold", "finish":
		if !st.players[str(arg)] {
			return fmt.Errorf("%s names unknown player %v", kind, arg)
		}
		if !optional(s, "throws", isBool) {
			return fmt.Errorf("throws must be a boolean")
		}
	case "answer":
		return checkCandidates(arg)
	case "answerThrows":
		if arg != true {
			return fmt.Errorf("answerThrows takes true")
		}
	case "probeAnswer":
		if str(arg) == "" || !required(s, "durationMs", nullOr(anyInt)) {
			return fmt.Errorf("probeAnswer takes a candidate id and durationMs (an int or null)")
		}
	case "probeThrows":
		if str(arg) == "" {
			return fmt.Errorf("probeThrows takes a candidate id")
		}
	}
	return nil
}

func (st *traceState) call(s object, i int) error {
	api := str(s["call"])
	args, ok := callArgs[api]
	if !ok {
		return fmt.Errorf("unknown call %q", s["call"])
	}
	need, maybe, _ := cut(args)
	names := append(append(append([]string{"call"}, need...), "?", "id"), maybe...)
	if err := fieldsOK(s, names); err != nil {
		return fmt.Errorf("%s: %w", api, err)
	}
	if id, has := s["id"]; has {
		name := str(id)
		if name == "" {
			return fmt.Errorf("%s: id must be a non-empty string", api)
		}
		if _, dup := st.calls[name]; dup {
			return fmt.Errorf("%s: call id %q used twice", api, name)
		}
		st.calls[name] = i
	} else if slices.Contains(idRequired, api) {
		return fmt.Errorf("%s needs an id: only its done output shows the result", api)
	}
	if p, has := s["player"]; has && !st.players[str(p)] {
		return fmt.Errorf("%s names unknown player %v", api, p)
	}
	if m, has := s["media"]; has {
		raw, _ := json.Marshal(m)
		if _, hasID := s["id"]; !hasID && !CheckMedia(raw, true) {
			return fmt.Errorf("setMedia: a descriptor failing the send checks needs an id for its done")
		}
	}
	if c, has := s["candidate"]; has {
		if err := checkCandidates([]any{c}); err != nil {
			return err
		}
	}
	return nil
}

// checkRecv refuses relay input that doesn't say what the SDK does with it (SDK.md §8): a `joined`, `room`, `error` or
// `closed` must pass its check, unless the step marks it malformed, and then it must fail it. A `room`'s `media` is
// not part of its check. Pongs come from the driver, so a trace scripts only a malformed one.
func checkRecv(v any, malformed bool) error {
	m, ok := v.(object)
	if !ok || str(m["type"]) == "" {
		return fmt.Errorf("recv takes a message with a string type; recvText sends anything else")
	}
	var valid bool
	switch m["type"] {
	case "joined":
		valid = validJoined(m)
	case "room":
		raw, _ := json.Marshal(m)
		valid = CheckRoom(raw)
	case "pong":
		valid = required(m, "serverTime", intAtLeast(0))
		if valid {
			return fmt.Errorf("the driver answers pings; a trace scripts only a malformed pong")
		}
	case "error":
		valid = required(m, "code", text) && required(m, "message", stringWith(func(string) bool { return true })) &&
			required(m, "re", nullOr(stringWith(func(string) bool { return true })))
	case "closed":
		valid = required(m, "reason", text)
	case "ping":
		return fmt.Errorf("a relay never sends ping")
	default:
		if malformed {
			return fmt.Errorf("malformed applies to joined, room, pong, error and closed")
		}
		return nil
	}
	if valid == malformed {
		if malformed {
			return fmt.Errorf("%s marked malformed passes its check", m["type"])
		}
		return fmt.Errorf("%s fails its check; mark it malformed", m["type"])
	}
	return nil
}

// validJoined is SDK.md §8's check of a `joined`.
func validJoined(m object) bool {
	you, ok := m["you"].(object)
	room, _ := json.Marshal(m["room"])
	return ok && required(m, "code", stringWith(codePattern.MatchString)) &&
		required(you, "peerId", stringWith(peerIDPattern.MatchString)) &&
		required(you, "token", stringWith(tokenPattern.MatchString)) &&
		required(m, "serverTime", intAtLeast(0)) && m["room"] != nil && CheckSnapshot(room) && validMedia(m)
}

// validMedia: m has a member media that is null or passes the receive checks (SDK.md §9.7).
func validMedia(m object) bool {
	media, ok := m["media"]
	raw, _ := json.Marshal(media)
	return ok && (media == nil || CheckMedia(raw, false))
}

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func checkCandidates(v any) error {
	list, ok := v.([]any)
	if !ok {
		return fmt.Errorf("candidates must be an array")
	}
	ids := map[string]bool{}
	for _, item := range list {
		c, ok := item.(object)
		if !ok {
			return fmt.Errorf("a candidate must be an object")
		}
		if err := fieldsOK(c, []string{"id", "?", "ref", "durationMs", "sizeBytes", "oshash", "name", "probe"}); err != nil {
			return fmt.Errorf("candidate: %w", err)
		}
		id, ok := c["id"].(string)
		if !ok || ids[id] {
			return fmt.Errorf("candidate ids must be unique strings")
		}
		ids[id] = true
		if !optional(c, "ref", stringWith(func(s string) bool { return s != "" })) ||
			!optional(c, "durationMs", anyInt) || !optional(c, "sizeBytes", anyInt) ||
			!optional(c, "oshash", stringWith(func(string) bool { return true })) ||
			!optional(c, "name", stringWith(func(string) bool { return true })) ||
			!optional(c, "probe", func(v any) bool { return v == true }) {
			return fmt.Errorf("candidate %s: a member has the wrong type", id)
		}
	}
	return nil
}

func (st *traceState) outputs(v any, i int) error {
	m, ok := v.(object)
	if !ok {
		return fmt.Errorf("not an object")
	}
	if err := fieldsOK(m, append([]string{"?"}, channels...)); err != nil {
		return err
	}
	for _, ch := range channels[:len(channels)-1] {
		entries, has := m[ch]
		if !has {
			continue
		}
		list, ok := entries.([]any)
		if !ok || len(list) == 0 {
			return fmt.Errorf("%s must be a non-empty array when present", ch)
		}
		for _, e := range list {
			entry, ok := e.(object)
			if !ok {
				return fmt.Errorf("%s entries must be objects", ch)
			}
			if err := st.entry(ch, entry, i); err != nil {
				return fmt.Errorf("%s: %w", ch, err)
			}
		}
	}
	if s, has := m["state"]; has {
		fields, ok := s.(object)
		if !ok || len(fields) == 0 {
			return fmt.Errorf("state must be a non-empty object when present")
		}
		if err := fieldsOK(fields, append([]string{"?"}, stateFields...)); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	return nil
}

func (st *traceState) entry(ch string, e object, i int) error {
	switch ch {
	case "send":
		if _, wrapped := e["on"]; wrapped {
			msg, ok := e["msg"].(object)
			if !hasOnly(e, "on", "msg") || !wsURL(e["on"]) || !ok {
				return fmt.Errorf("an entry is a message or {on, msg}")
			}
			e = msg
		}
		if t := str(e["type"]); t == "" || t == "ping" {
			return fmt.Errorf("a sent message has a string type other than ping")
		}
	case "command":
		if !st.players[str(e["player"])] {
			return fmt.Errorf("unknown player %v", e["player"])
		}
		switch e["do"] {
		case "play", "pause":
			return fieldsOK(e, []string{"player", "do"})
		case "seek":
			if !required(e, "positionMs", intAtLeast(0)) {
				return fmt.Errorf("seek needs positionMs")
			}
			return fieldsOK(e, []string{"player", "do", "positionMs"})
		case "setRate":
			if _, ok := numberOf(e["rate"]); !ok {
				return fmt.Errorf("setRate needs rate")
			}
			return fieldsOK(e, []string{"player", "do", "rate"})
		default:
			return fmt.Errorf("unknown command %v", e["do"])
		}
	case "event":
		want, ok := eventMembers[str(e["type"])]
		if !ok {
			return fmt.Errorf("unknown event type %v", e["type"])
		}
		return fieldsOK(e, append([]string{"type"}, want...))
	case "resolver":
		if len(e) != 1 {
			return fmt.Errorf("an entry has one member")
		}
		for k, v := range e {
			if !slices.Contains([]string{"resolve", "resolveCancelled", "probe", "probeCancelled"}, k) || str(v) == "" {
				return fmt.Errorf("unknown entry %s", k)
			}
		}
	case "net":
		for _, k := range []string{"get", "open"} {
			if _, has := e[k]; has {
				if str(e[k]) == "" {
					return fmt.Errorf("%s takes a URL", k)
				}
				return fieldsOK(e, []string{k})
			}
		}
		if str(e["close"]) == "" || !required(e, "code", nullOr(anyInt)) {
			return fmt.Errorf("an entry is {get}, {open} or {close, code}")
		}
		return fieldsOK(e, []string{"close", "code"})
	case "done":
		id := str(e["id"])
		at, ok := st.calls[id]
		if !ok || at > i {
			return fmt.Errorf("done %q names no call made by this step", id)
		}
		if st.done[id] {
			return fmt.Errorf("call %q is done twice", id)
		}
		st.done[id] = true
		_, value := e["value"]
		_, failed := e["error"]
		if value && failed || failed && str(e["error"]) == "" {
			return fmt.Errorf("done %q has a value or an error code, not both", id)
		}
		_, re := e["re"]
		_, message := e["message"]
		if (re || message) && !failed || !optional(e, "re", nullOr(text)) || !optional(e, "message", text) {
			return fmt.Errorf("done %q: re (a string or null) and message (a string) go with an error", id)
		}
		return fieldsOK(e, []string{"id", "?", "value", "error", "re", "message"})
	}
	return nil
}

// fieldsOK fails unless m holds every name before "?" in names and nothing outside names.
func fieldsOK(m object, names []string) error {
	need, _, _ := cut(names)
	for _, k := range need {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("missing %s", k)
		}
	}
	for k := range m {
		if !slices.Contains(names, k) || k == "?" {
			return fmt.Errorf("unknown member %s", k)
		}
	}
	return nil
}

func hasOnly(m object, names ...string) bool { return fieldsOK(m, names) == nil }

// cut splits names at "?" into the required and the optional ones.
func cut(names []string) (required, optional []string, found bool) {
	if i := slices.Index(names, "?"); i >= 0 {
		return names[:i], names[i+1:], true
	}
	return names, nil, false
}

func keys(m object) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// validList reports whether doc is a valid public list (SDK.md §3.2).
func validList(doc []byte) bool {
	_, err := ParseRelayList(doc)
	return err == nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
