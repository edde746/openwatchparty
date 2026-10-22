package main

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// SPEC §5: room codes. /v1/info itself is in info.go.

// reloadWait bounds how long the relay may take to pick up a changed deny-list file, or to free a closed
// connection's place. The spec gives no bound; this one is generous.
const reloadWait = 3 * time.Second

// scCodes: codes are 6 symbols; the first is drawn at random from the relay's creation prefixes; codes are
// case-insensitive; a string that isn't a code of this relay's rooms finds no room.
func scCodes(x *sc) error {
	own := x.creationPrefixes()
	n := 6
	if x.relay != nil {
		n = 60 // 4 creation prefixes: each one missing from 60 random codes has odds of 4 × 0.75^60 ≈ 1e-7
	}
	seen := map[byte]bool{}
	var kept *Client
	for i := range n {
		c, j, err := x.create(fmt.Sprintf("C%d", i))
		if err != nil {
			return err
		}
		if !codeRe.MatchString(c.Code) || !strings.ContainsRune(own, rune(c.Code[0])) ||
			str(obj(j, "room"), "code") != c.Code {
			return fmt.Errorf("code %q (room.code %q): want 6 symbols starting with one of %q", c.Code,
				str(obj(j, "room"), "code"), own)
		}
		seen[c.Code[0]] = true
		if kept == nil {
			kept = c
			continue
		}
		_ = c.send(Msg{"type": "end"})
		if _, err := c.waitFor(reply, "closed ended", isType("closed")); err != nil {
			return err
		}
		c.close()
	}
	if x.relay != nil && len(seen) != len(own) {
		return fmt.Errorf("%d codes started with %q of the creation prefixes %q; want each at random", n, setString(seen), own)
	}
	// Case-insensitive join and resume (§5.1).
	b, j, err := x.join(lowerCode(kept.Code), "B")
	if err != nil {
		return fmt.Errorf("join with the code in lower case: %w", err)
	}
	if str(j, "code") != kept.Code {
		return fmt.Errorf("joined.code = %q after a lower-case join, want %q", str(j, "code"), kept.Code)
	}
	b.drop()
	if _, err := kept.waitFor(reply, "B disconnected", connectedIs(b.PeerID, false)); err != nil {
		return err
	}
	b2, err := x.dial("B2")
	if err != nil {
		return err
	}
	m := resumeMsg(b)
	m["code"] = lowerCode(b.Code)
	if _, err := enter(b2, m); err != nil {
		return fmt.Errorf("resume with the code in lower case: %w", err)
	}
	// Strings that name no room here.
	other := strings.Map(func(r rune) rune {
		if strings.ContainsRune(own, r) {
			return -1
		}
		return r
	}, alphabet)
	lost := []string{"ABC", "IIIIII", kept.Code + "2", strings.Repeat(string(own[0]), 7)}
	if other != "" {
		lost = append(lost, string(other[0])+kept.Code[1:])
	}
	for _, code := range lost {
		_, j, err := x.join(code, "Lost")
		if err := refusedWith(j, err, "room_not_found", fmt.Sprintf("join %q", code)); err != nil {
			return err
		}
	}
	return nil
}

// scTypedCodes: join and resume find the room by the code as a user typed it. The relay removes the separators §5.1
// lists (spaces, tabs, line breaks, no-break and narrow spaces, hyphens, dashes and the minus sign) and upper-cases
// ASCII a–z; every other character stays. A string that then isn't the room's code, because it keeps an unlisted
// character or has too few or too many symbols, gets room_not_found for join and resume_failed for resume (§5.1,
// §7.2, §7.3).
func scTypedCodes(x *sc) error {
	a, _, err := x.create("A")
	if err != nil {
		return err
	}
	code, lower := a.Code, strings.ToLower(a.Code)
	alone := isRoom(func(m Msg) bool { return len(participants(m)) == 1 })
	for _, typed := range []string{
		lower[:3] + "-" + lower[3:],
		code[:3] + " " + code[3:],
		"\t" + code[:3] + "\u2010" + code[3:] + "\r\n",
		code[:3] + "\u2212" + code[3:],
		strings.Join(strings.Split(lower, ""), "\u00A0"),
		code[:2] + "\u2007" + code[2:4] + "\u202F" + code[4:],
		code[:3] + "\u2011\u2012\u2013\u2014" + code[3:],
	} {
		c, j, err := x.join(typed, "B")
		if err != nil {
			return fmt.Errorf("join with %+q: %w", typed, err)
		}
		if str(j, "code") != code {
			return fmt.Errorf("joined.code = %q after a join with %+q, want %q", str(j, "code"), typed, code)
		}
		_ = c.send(Msg{"type": "leave"}) // the room stays at two, whatever the relay's maxParticipants
		if _, err := a.waitFor(reply, "B left", alone); err != nil {
			return err
		}
	}
	b, _, err := x.join(code, "B")
	if err != nil {
		return err
	}
	b.drop()
	if _, err := a.waitFor(reply, "B disconnected", connectedIs(b.PeerID, false)); err != nil {
		return err
	}
	b2, err := x.dial("B2")
	if err != nil {
		return err
	}
	m := resumeMsg(b)
	m["code"] = " " + lower[:3] + "\u2013" + lower[3:]
	j, err := enter(b2, m)
	if err != nil {
		return fmt.Errorf("resume with %+q: %w", m["code"], err)
	}
	if str(j, "code") != code || b2.PeerID != b.PeerID {
		return fmt.Errorf("resume with %+q: joined.code %q, peer %q; want %q, %q", m["code"], str(j, "code"), b2.PeerID,
			code, b.PeerID)
	}
	fullWidth := strings.Map(func(r rune) rune { return r + 0xFEE0 }, code) // full-width digits and letters
	for _, other := range []string{
		code[:3] + "\u2015" + code[3:], // horizontal bar, not a listed dash
		code[:3] + "_" + code[3:],
		code[:3] + "." + code[3:],
		fullWidth,
		code[:3] + "-" + code[3:5], // 5 symbols
		code + "-" + code[:1],      // 7 symbols
	} {
		c, j, err := x.join(other, "Lost")
		if err := refusedWithRe(j, err, "room_not_found", "join", fmt.Sprintf("join with %+q", other)); err != nil {
			return err
		}
		m := resumeMsg(b2)
		m["code"] = other
		j, err = enter(c, m)
		if err := refusedWithRe(j, err, "resume_failed", "resume", fmt.Sprintf("resume with %+q", other)); err != nil {
			return err
		}
	}
	return nil
}

func setString(m map[byte]bool) string {
	b := make([]byte, 0, len(m))
	for k := range m {
		b = append(b, k)
	}
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	return string(b)
}

// creationPrefixes is what the relay may create codes with: --prefixes when the suite started the relay, else any
// symbol (§5.2).
func (x *sc) creationPrefixes() string {
	if x.relay != nil {
		return ownPrefixes
	}
	return alphabet
}

// freeCode is a random code that starts with first. On a relay holding n rooms, it names one of them with odds of at
// most n in 31^5.
func freeCode(first byte) string {
	b := []byte{first, 0, 0, 0, 0, 0}
	for i := 1; i < len(b); i++ {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}

// foreignCode is a random code whose first symbol is not one of the relay's creation prefixes, or "" when the suite
// doesn't know of such a symbol.
func (x *sc) foreignCode() string {
	own := x.creationPrefixes()
	for i := range len(alphabet) {
		if !strings.ContainsRune(own, rune(alphabet[i])) {
			return freeCode(alphabet[i])
		}
	}
	return ""
}
