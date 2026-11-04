package relay

import (
    "os"
    "path/filepath"
    "strings"
    "testing"
    "time"
)

func waitForNoRooms(t *testing.T, tr *testRelay) {
    t.Helper()
    deadline := time.Now().Add(3*time.Second)
    for tr.srv.roomCount() != 0 {
        if time.Now().After(deadline) { t.Fatal("room did not close") }
        time.Sleep(10*time.Millisecond)
    }
}

func TestPermanentRoomCodesReuseAndRestart(t *testing.T) {
    cfg := testConfig()
    cfg.PermanentRooms = true
    cfg.StateDir = t.TempDir()
    tr := startRelay(t, cfg)
    host := tr.dial()
    host.create("Host", "Test/1.0")
    code := host.code
    host.send(msg{"type":"leave"})
    waitForNoRooms(t,tr)

    guest := tr.dial()
    guest.join(code,"Returning guest")
    if guest.code != code || hostOf(guest.last) != guest.peerID {
        t.Fatalf("reopened code did not assign a new host: %v",guest.last)
    }
    guest.send(msg{"type":"leave"})
    waitForNoRooms(t,tr)
    tr.stop()

    saved,err := os.ReadFile(filepath.Join(cfg.StateDir,roomsFile))
    if err != nil {t.Fatal(err)}
    if !strings.Contains(string(saved), code) {t.Fatal("reserved code missing from saved state")}

    next := startRelay(t,cfg)
    back := next.dial()
    back.join(code,"Host after restart")
    if back.code != code || hostOf(back.last) != back.peerID {
        t.Fatalf("reserved code not reusable after restart: %v",back.last)
    }
}

func TestReservedCodeRestoredWithoutSession(t *testing.T) {
    cfg := testConfig()
    cfg.PermanentRooms = true
    cfg.StateDir = t.TempDir()
    saved := []byte(`{"version":1,"rooms":[],"reserved":["K7MQ2X"]}`)
    if err := os.WriteFile(filepath.Join(cfg.StateDir, roomsFile),saved,0600); err != nil {t.Fatal(err)}
    tr := startRelay(t,cfg)
    c := tr.dial()
    c.join("K7M-Q2X","Returning host")
    if c.code!="K7MQ2X" || hostOf(c.last)!=c.peerID { t.Fatalf("bad reopened room: %v",c.last) }
}

func TestPermanentRoomsNeedStateDir(t *testing.T) {
    cfg := testConfig()
    cfg.PermanentRooms = true
    if _,err:=New(cfg);err==nil {t.Fatal("expected missing storage error")}
}

func TestPermanentRoomsUnknownCodesStillFail(t *testing.T) {
    cfg := testConfig()
    cfg.PermanentRooms = true
    cfg.StateDir = t.TempDir()
    tr := startRelay(t,cfg)
    c := tr.dial()
    c.send(msg{"type":"join","code":"K7MQ2X","name":"Guest","client":"Test/1.0"})
    c.expectError(errRoomNotFound)
}
