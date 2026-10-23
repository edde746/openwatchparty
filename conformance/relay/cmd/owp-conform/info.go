package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// relayIDRe is the relay ID rule (§5.2), which /v1/info's id follows.
var relayIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// protocolLimits are the `limits` fixed in v1 (SPEC §13.1); every relay reports exactly these.
var protocolLimits = map[string]float64{
	"maxMessageBytes": 8192, "maxMediaBytes": 4096, "maxExtBytes": 1024, "maxNameLength": 32,
	"maxClientLength": 64, "minRate": 0.5, "maxRate": 2.0,
}

// policyLimits are the other `limits`, at the public relays' values (§13.2). Each relay chooses its own.
var policyLimits = map[string]float64{
	"maxParticipants": 32, "maxConnections": 10000, "maxConnectionsPerIp": 10, "maxRooms": 10000, "maxRoomsPerIp": 3,
	"ipv6Factor56": 4, "ipv6Factor48": 16, "messageBurst": 30, "messagesPerSecond": 10, "joinBurst": 20,
	"joinsPerSecond": 1, "joinBurstTotal": 2000, "joinsPerSecondTotal": 200,
}

// rateLimits are the policy limits that are refill rates: any positive number. The others are positive ints.
var rateLimits = map[string]bool{"messagesPerSecond": true, "joinsPerSecond": true, "joinsPerSecondTotal": true}

// policyMax are the upper bounds §13.2 sets on policy limits: at most 32 participants, so every relay message stays
// within the 32768 bytes clients accept.
var policyMax = map[string]float64{"maxParticipants": 32}

// policyDefault is `policy` at the public relays' values, unscaled (§13.3).
var policyDefault = map[string]float64{
	"stallGraceMs": 500, "stallReleaseMs": 400, "waitMs": 15000, "mediaWaitMs": 30000, "exclusionMs": 60000,
	"startDelayMinMs": 750, "startDelayMaxMs": 2000, "startDelayRttFactor": 1.5, "defaultRttMs": 500,
	"seekCoalesceMs": 200, "pauseToleranceMs": 2000, "hostPromotionMs": 30000, "resumeMs": 300000,
	"maxRoomAgeMs": 86400000,
}

// unscaled are the policy values test mode leaves alone (§13.3).
var unscaled = map[string]bool{"startDelayRttFactor": true, "defaultRttMs": true, "pauseToleranceMs": true}

// scaledPolicy is a public policy value as a relay in test mode with factor s reports it: scaled durations are
// rounded down to whole milliseconds (§13.3).
func scaledPolicy(key string, s float64) float64 {
	if unscaled[key] {
		return policyDefault[key]
	}
	return math.Floor(policyDefault[key]*s + 1e-9)
}

// loadInfo reads the limits and policy the relay reports. The scenarios time and size everything by these values
// (§5.3: "the durations in effect"); a value the relay doesn't report falls back to the public one at the suite's
// --timer-scale, and the info scenario fails.
func (e *env) loadInfo() {
	info, _, err := getJSON(e.httpBase + "/v1/info")
	if err != nil {
		fmt.Printf("/v1/info: %v; using the public limits and policy at timer scale %g\n\n", err, e.scale)
		return
	}
	e.limits, e.policy = obj(info, "limits"), obj(info, "policy")
}

// pol is a policy value in effect on the relay under test, in ms for durations.
func (x *sc) pol(key string) float64 {
	if v, ok := x.policy[key].(float64); ok {
		return v
	}
	return scaledPolicy(key, x.scale)
}

// polD is a policy duration.
func (x *sc) polD(key string) time.Duration {
	return time.Duration(x.pol(key) * float64(time.Millisecond))
}

// lim is a limit in effect on the relay under test.
func (x *sc) lim(key string) float64 {
	if v, ok := x.limits[key].(float64); ok {
		return v
	}
	if v, ok := protocolLimits[key]; ok {
		return v
	}
	return policyLimits[key]
}

// startDelay is the delay from release to atMs for the highest reported rttMs (§12.6).
func (x *sc) startDelay(maxRtt float64) float64 {
	return min(max(math.Floor(x.pol("startDelayRttFactor")*maxRtt), x.pol("startDelayMinMs")), x.pol("startDelayMaxMs"))
}

// scInfo: /v1/info reports the protocol, the relay's ID, every limit within its bounds and every policy value; with
// --relay-bin, the configured ID and the public values at the suite's timer scale (§5.3, §13).
func scInfo(x *sc) error {
	info, hdr, err := getJSON(x.httpBase + "/v1/info")
	if err != nil {
		return err
	}
	if err := errs(header(hdr, "/v1/info", "Access-Control-Allow-Origin", "*"),
		headerPrefix(hdr, "/v1/info", "Content-Type", "application/json")); err != nil {
		return err
	}
	if v, ok := info["protocol"].(float64); !ok || v != 1 {
		return fmt.Errorf("/v1/info protocol = %v, want 1", info["protocol"])
	}
	id := str(info, "id")
	if !relayIDRe.MatchString(id) {
		return fmt.Errorf("/v1/info id = %v, want a relay ID (§5.2)", info["id"])
	}
	lim, pol := obj(info, "limits"), obj(info, "policy")
	if lim == nil || pol == nil {
		return fmt.Errorf("/v1/info limits = %v, policy = %v, want objects", info["limits"], info["policy"])
	}
	for _, key := range sortedKeys(protocolLimits) {
		if v, ok := lim[key].(float64); !ok || v != protocolLimits[key] {
			return fmt.Errorf("/v1/info limits.%s = %v, want %v (fixed in v1)", key, lim[key], protocolLimits[key])
		}
	}
	for _, key := range sortedKeys(policyLimits) {
		v, ok := lim[key].(float64)
		if rateLimits[key] && (!ok || v <= 0) {
			return fmt.Errorf("/v1/info limits.%s = %v, want a positive number", key, lim[key])
		}
		if !rateLimits[key] && (!ok || v <= 0 || v != math.Trunc(v)) {
			return fmt.Errorf("/v1/info limits.%s = %v, want a positive int", key, lim[key])
		}
		if most, ok := policyMax[key]; ok && v > most {
			return fmt.Errorf("/v1/info limits.%s = %v, want at most %v", key, v, most)
		}
	}
	for _, key := range sortedKeys(policyDefault) {
		v, ok := pol[key].(float64)
		if !ok || v < 0 || (key != "startDelayRttFactor" && v != math.Trunc(v)) {
			return fmt.Errorf("/v1/info policy.%s = %v, want a non-negative int", key, pol[key])
		}
	}
	if x.relay == nil {
		return nil
	}
	// The suite started the relay: it knows the ID and that the relay runs the public values in test mode.
	if id != ownID {
		return fmt.Errorf("/v1/info id = %q, want --id %q", id, ownID)
	}
	for _, key := range sortedKeys(policyLimits) {
		if lim[key] != policyLimits[key] {
			return fmt.Errorf("/v1/info limits.%s = %v, want the public %v", key, lim[key], policyLimits[key])
		}
	}
	for _, key := range sortedKeys(policyDefault) {
		if want := scaledPolicy(key, x.scale); pol[key] != want {
			return fmt.Errorf("/v1/info policy.%s = %v, want the public value at timer scale %g: %v", key, pol[key],
				x.scale, want)
		}
	}
	return nil
}

// scHealth: /health answers 200 text/plain `ok`, readable from any origin.
func scHealth(x *sc) error {
	hc := &http.Client{Timeout: reply}
	h, err := hc.Get(x.httpBase + "/health")
	if err != nil {
		return err
	}
	defer h.Body.Close()
	body, err := io.ReadAll(h.Body)
	if err != nil {
		return fmt.Errorf("/health: %v", err)
	}
	if h.StatusCode != 200 || string(body) != "ok" {
		return fmt.Errorf("/health = %d %q, want 200 \"ok\"", h.StatusCode, body)
	}
	return errs(header(h.Header, "/health", "Access-Control-Allow-Origin", "*"),
		headerPrefix(h.Header, "/health", "Content-Type", "text/plain"))
}

func header(h http.Header, what, key, want string) error {
	if got := h.Get(key); got != want {
		return fmt.Errorf("%s %s = %q, want %q", what, key, got, want)
	}
	return nil
}

// headerPrefix allows parameters after the media type (`text/plain; charset=utf-8`).
func headerPrefix(h http.Header, what, key, want string) error {
	got := h.Get(key)
	if mt, _, _ := strings.Cut(got, ";"); strings.TrimSpace(mt) != want {
		return fmt.Errorf("%s %s = %q, want %s", what, key, got, want)
	}
	return nil
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
