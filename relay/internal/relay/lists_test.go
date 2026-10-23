package relay

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestParseDenyList(t *testing.T) {
	deny := parseDenyList([]byte("# comment\n\n  Bad App \nOTHER\r\n"))
	if len(deny) != 2 || !deny["bad app"] || !deny["other"] {
		t.Fatalf("deny list: %v", deny)
	}
}

func TestRoomCodes(t *testing.T) {
	for range 1000 {
		code := newRoomCode("K7")
		if !validCode(code) || (code[0] != 'K' && code[0] != '7') {
			t.Fatalf("code %q outside alphabet or prefixes", code)
		}
	}
	if _, err := normalizePrefixes("k7m"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "K0", "KK", "é"} {
		if _, err := normalizePrefixes(bad); err == nil {
			t.Errorf("prefixes %q accepted", bad)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	// Each separator SPEC.md §5.1 lists goes, wherever it stands.
	for _, sep := range " \t\n\r\u00A0\u2007\u202F-\u2010\u2011\u2012\u2013\u2014\u2212" {
		s := string(sep)
		if got := normalizeCode(s + "K7M" + s + s + "Q2X" + s); got != "K7MQ2X" {
			t.Errorf("U+%04X: %q, want K7MQ2X", sep, got)
		}
	}
	for in, want := range map[string]string{
		"k7m-q2x": "K7MQ2X", // ASCII a-z upper-cased
		"aZz":     "AZZ",
		// Other characters stay: unlisted dashes, spaces and punctuation,
		// full-width letters, and letters outside ASCII, whatever their case.
		"K7M\u2015Q2X": "K7M\u2015Q2X", "K7M_Q2X": "K7M_Q2X", "K7M.Q2X": "K7M.Q2X",
		"K7M\u00ADQ2X": "K7M\u00ADQ2X", "K7M\uFF0DQ2X": "K7M\uFF0DQ2X", "K7M\u2009Q2X": "K7M\u2009Q2X",
		"\uFF4B7mq2x": "\uFF4B7MQ2X", "\u017F\u0131é": "\u017F\u0131é",
	} {
		if got := normalizeCode(in); got != want {
			t.Errorf("%+q: %+q, want %+q", in, got, want)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	bad := map[string]func(*Config){
		"id":           func(c *Config) { c.ID = "Bad ID" },
		"timer scale":  func(c *Config) { c.TimerScale = 0 },
		"participants": func(c *Config) { c.Limits.MaxParticipants = 0 },
		"33 per room":  func(c *Config) { c.Limits.MaxParticipants = 33 },
		"message rate": func(c *Config) { c.Limits.MessagesPerSecond = 0 },
		"prefixes":     func(c *Config) { c.Prefixes = "0" },
	}
	for name, change := range bad {
		cfg := testConfig()
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	cfg := testConfig()
	cfg.Limits.MaxParticipants = 32
	if _, err := New(cfg); err != nil {
		t.Errorf("32 participants refused: %v", err)
	}
}

func getJSON(t *testing.T, url string) (map[string]any, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %v", url, resp.StatusCode, err)
	}
	return body, resp.Header
}

func TestInfoAndHealth(t *testing.T) {
	cfg := testConfig()
	cfg.ID = "eu"
	cfg.TimerScale = 0.1
	cfg.Limits.MaxParticipants = 5
	tr := startRelay(t, cfg)
	info, h := getJSON(t, tr.ts.URL+"/v1/info")
	if h.Get("Access-Control-Allow-Origin") != "*" || num(info, "protocol") != 1 || info["id"] != "eu" ||
		!slices.Equal(slices.Sorted(maps.Keys(info)), []string{"id", "limits", "policy", "protocol"}) {
		t.Fatalf("info: %v %v", info, h)
	}
	limits, policy := info["limits"].(msg), info["policy"].(msg)
	if num(limits, "maxParticipants") != 5 || num(limits, "maxMessageBytes") != 8192 ||
		num(limits, "maxConnectionsPerIp") != 10 || limits["joinsPerSecond"] != 1.0 {
		t.Fatalf("limits: %v", limits)
	}
	if num(policy, "waitMs") != 1500 || num(policy, "pauseToleranceMs") != 2000 || num(policy, "defaultRttMs") != 500 ||
		policy["startDelayRttFactor"] != 1.5 || num(policy, "resumeMs") != 30000 ||
		num(policy, "maxRoomAgeMs") != 8640000 || len(policy) != 14 {
		t.Fatalf("policy at --timer-scale 0.1: %v", policy)
	}

	resp, err := http.Get(tr.ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("health: %v", resp)
	}
}
