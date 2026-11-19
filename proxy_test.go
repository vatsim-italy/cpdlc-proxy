package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHoppie is tools/hoppie-sim/server.py in Go, with a set of valid logon codes.
type fakeHoppie struct {
	mu     sync.Mutex
	valid  map[string]bool
	queues map[string][]Message
	sent   []Message // every cpdlc/telex, From = sender
	sentTo []string
	polls  int
	pings  int
	down   bool
}

func (f *fakeHoppie) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	from, to := strings.ToUpper(r.Form.Get("from")), strings.ToUpper(r.Form.Get("to"))
	kind, pkt := r.Form.Get("type"), r.Form.Get("packet")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if kind == "ping" {
		f.pings++
	}
	if !f.valid[r.Form.Get("logon")] {
		_, _ = w.Write([]byte("error {illegal logon code}"))
		return
	}
	switch kind {
	case "poll":
		f.polls++
		_, _ = w.Write([]byte(formatPoll(f.queues[from])))
		delete(f.queues, from)
	case "cpdlc", "telex":
		f.queues[to] = append(f.queues[to], Message{from, kind, pkt})
		f.sent = append(f.sent, Message{from, kind, pkt})
		f.sentTo = append(f.sentTo, to)
		_, _ = w.Write([]byte("ok"))
	default:
		_, _ = w.Write([]byte("ok"))
	}
}

// pilot sends a downlink straight to Hoppie, as EasyCPDLC would.
func (f *fakeHoppie) pilot(from, to, pkt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues[to] = append(f.queues[to], Message{from, "cpdlc", pkt})
}

func (f *fakeHoppie) lastSent() (Message, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1], f.sentTo[len(f.sentTo)-1]
}

type rig struct {
	t      *testing.T
	hoppie *fakeHoppie
	proxy  *Proxy
	url    string
	cfg    Config
	now    time.Time // frozen clock of proxy and station
}

func newRig(t *testing.T) *rig {
	hoppie := &fakeHoppie{
		valid:  map[string]bool{"PROXY": true, "CODE1": true, "CODE7": true, "CODE8": true},
		queues: map[string][]Message{},
	}
	hs := httptest.NewServer(hoppie)
	t.Cleanup(hs.Close)
	r := &rig{t: t, hoppie: hoppie, now: time.Now()}
	r.cfg.HoppieURL = hs.URL
	r.cfg.Stations = []StationConfig{{Station: "LIRR", Facility: "ROMA CTR", Sectors: []string{"LIRR_*_CTR"}}}
	r.start(filepath.Join(t.TempDir(), "state.json"))
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { r.proxy.ServeHTTP(w, req) }))
	t.Cleanup(ps.Close)
	r.url = ps.URL
	return r
}

// start builds a fresh proxy on the state file, as a restarted process would.
func (r *rig) start(statePath string) (restored int, err error) {
	r.proxy = newProxy(r.cfg, "PROXY")
	r.proxy.spacing = 0
	r.proxy.statePath = statePath
	r.proxy.now = func() time.Time { return r.now }
	for _, st := range r.proxy.stations {
		st.now = r.proxy.now
		st.pollOK = r.now // as if just polled: routing tests start warm (TestFirstPollBacklogIsStale)
	}
	return r.proxy.load()
}

func (r *rig) restart() {
	r.t.Helper()
	if err := r.proxy.save(); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.start(r.proxy.statePath); err != nil {
		r.t.Fatal(err)
	}
}

// sector makes one request to the proxy, as HoppieClient does.
func (r *rig) sector(logon, from, to, kind, pkt string) string {
	r.t.Helper()
	resp, err := http.PostForm(r.url, url.Values{
		"logon": {logon}, "from": {from}, "to": {to}, "type": {kind}, "packet": {pkt},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func (r *rig) pollHoppie() {
	st := r.proxy.stations["LIRR"]
	r.proxy.poll(st)
	r.proxy.flush(st)
}

func (r *rig) inbox(logon, from string) []Message {
	r.t.Helper()
	messages, err := parsePoll(r.sector(logon, from, "SERVER", "poll", ""))
	if err != nil {
		r.t.Fatal(err)
	}
	return messages
}

func expect(t *testing.T, got []Message, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want packets %v", got, want)
	}
	for i := range want {
		if got[i].Packet != want[i] {
			t.Fatalf("message %d: got %q, want %q", i, got[i].Packet, want[i])
		}
	}
}

func TestParsePoll(t *testing.T) {
	got, err := parsePoll("ok {AZA1 cpdlc {/data2/3//Y/REQUEST DIRECT TO A/C}} {RYR2 telex {HELLO}}")
	if err != nil || len(got) != 2 || got[0].From != "AZA1" || got[0].Packet != "/data2/3//Y/REQUEST DIRECT TO A/C" ||
		got[1].Type != "telex" || got[1].Packet != "HELLO" {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := parsePoll("error {callsign already in use}"); err == nil {
		t.Fatal("error reply accepted")
	}
	if got, _ := parsePoll("ok"); len(got) != 0 {
		t.Fatalf("empty poll gave %v", got)
	}
	p, ok := parsePacket("/data2/12/3/WU/CLIMB TO @FL370@ A/B")
	if !ok || p.String() != "/data2/12/3/WU/CLIMB TO @FL370@ A/B" || p.text != "CLIMB TO @FL370@ A/B" {
		t.Fatalf("packet %v", p)
	}
}

func TestAuthorization(t *testing.T) {
	r := newRig(t)
	if got := r.sector("CODE1", "LIMM_W_CTR", "SERVER", "poll", ""); !strings.Contains(got, "not authorized") {
		t.Fatalf("other FIR: %q", got)
	}
	if got := r.sector("BAD", "LIRR_N_CTR", "SERVER", "poll", ""); got != "error {illegal logon code}" {
		t.Fatalf("bad code: %q", got)
	}
	if got := r.sector("CODE1", "LIRR_N_CTR", "SERVER", "poll", ""); got != "ok" {
		t.Fatalf("good code: %q", got)
	}
	if got := r.sector("CODE7", "LIRR_N_CTR", "SERVER", "poll", ""); got != "error {callsign already in use}" {
		t.Fatalf("second code on one callsign: %q", got)
	}
}

func TestNoHoppiePollWithoutSectors(t *testing.T) {
	r := newRig(t)
	st := r.proxy.stations["LIRR"]
	if st.duePoll() {
		t.Fatal("polled with no sector connected")
	}
	r.sector("CODE1", "LIRR_N_CTR", "SERVER", "poll", "")
	if !st.duePoll() || st.duePoll() {
		t.Fatal("want one poll now, then wait")
	}
}

func TestRoutesToOwnerThenNext(t *testing.T) {
	r := newRig(t)
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1 RYR2|BAW3")
	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "|AZA1")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/1//Y/REQUEST FL370")
	r.hoppie.pilot("BAW3", "LIRR", "/data2/1//Y/REQUEST LOGON")
	r.pollHoppie()
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"), "/data2/1//Y/REQUEST FL370", "/data2/1//Y/REQUEST LOGON")
	expect(t, r.inbox("CODE8", "LIRR_NE8_CTR"))

	// NE1 releases AZA1: its next sector NE8 gets it.
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "RYR2")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/2//Y/REQUEST FL390")
	r.pollHoppie()
	expect(t, r.inbox("CODE8", "LIRR_NE8_CTR"), "/data2/2//Y/REQUEST FL390")
}

func TestMinRewriteAndReplyToSender(t *testing.T) {
	r := newRig(t)
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1")
	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "")

	// Both sectors use local MIN 1; Hoppie must see two different MINs from LIRR.
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1//WU/CLIMB TO @FL370@")
	first, to := r.hoppie.lastSent()
	if first.From != "LIRR" || to != "AZA1" || first.Packet != "/data2/1//WU/CLIMB TO @FL370@" {
		t.Fatalf("first uplink %v to %s", first, to)
	}
	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "AZA1") // internal transfer
	r.sector("CODE8", "LIRR_NE8_CTR", "AZA1", "cpdlc", "/data2/1//WU/DESCEND TO @FL250@")
	if second, _ := r.hoppie.lastSent(); second.Packet != "/data2/2//WU/DESCEND TO @FL250@" {
		t.Fatalf("second uplink %v", second)
	}

	// The pilot answers both: each reply reaches the sector that sent it, with its own MIN.
	r.hoppie.pilot("AZA1", "LIRR", "/data2/5/1/N/WILCO")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/6/2/N/UNABLE")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/7//Y/REQUEST FL390")
	r.pollHoppie()
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"), "/data2/5/1/N/WILCO")
	expect(t, r.inbox("CODE8", "LIRR_NE8_CTR"), "/data2/6/1/N/UNABLE", "/data2/7//Y/REQUEST FL390")
}

func TestUnownedLogonAcceptedByProxy(t *testing.T) {
	r := newRig(t)
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.hoppie.pilot("DLH4", "LIRR", "/data2/7//Y/REQUEST LOGON")
	r.pollHoppie()
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"))
	r.expectSent("DLH4", "/data2/1/7/NE/LOGON ACCEPTED", "/data2/2//NE/CURRENT ATC UNIT@_@LIRR@_@ROMA CTR",
		"/data2/3//NE/"+notInUse)
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "DLH4")
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"), "CONNECTED NOTAUTH")
}

// A FIR left unpolled for over logonMaxAge drops unowned logons from that batch, then accepts fresh ones.
func TestOldUnownedLogonDropped(t *testing.T) {
	r := newRig(t)
	st, advance := r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.pollHoppie()
	advance(logonMaxAge + time.Minute)
	r.hoppie.pilot("DLH4", "LIRR", "/data2/7//Y/REQUEST LOGON")
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.pollHoppie()
	r.hoppie.mu.Lock()
	sent := len(r.hoppie.sent)
	r.hoppie.mu.Unlock()
	if sent != 0 || st.conns["DLH4"] != nil {
		t.Fatalf("old logon answered: %d uplinks, conn %v", sent, st.conns["DLH4"])
	}
	advance(time.Minute)
	r.hoppie.pilot("DLH4", "LIRR", "/data2/8//Y/REQUEST LOGON")
	r.pollHoppie()
	r.expectSent("DLH4", "/data2/1/8/NE/LOGON ACCEPTED", "/data2/2//NE/CURRENT ATC UNIT@_@LIRR@_@ROMA CTR",
		"/data2/3//NE/"+notInUse)
}

// clock gives the station and a way to move the frozen time.
func (r *rig) clock() (st *Station, advance func(time.Duration)) {
	return r.proxy.stations["LIRR"], func(d time.Duration) { r.now = r.now.Add(d) }
}

func (r *rig) tick() {
	st := r.proxy.stations["LIRR"]
	st.prune()
	r.proxy.flush(st)
}

// expectSent checks the last uplinks Hoppie got, oldest first.
func (r *rig) expectSent(to string, want ...string) {
	r.t.Helper()
	r.hoppie.mu.Lock()
	defer r.hoppie.mu.Unlock()
	n := len(r.hoppie.sent)
	if n < len(want) {
		r.t.Fatalf("sent %v, want %v", r.hoppie.sent, want)
	}
	for i, w := range want {
		m, dest := r.hoppie.sent[n-len(want)+i], r.hoppie.sentTo[n-len(want)+i]
		if m.From != "LIRR" || dest != to || m.Packet != w {
			r.t.Fatalf("uplink %d: %v to %s, want %q to %s", i, m, dest, w, to)
		}
	}
}

func (r *rig) sentCount() int {
	r.hoppie.mu.Lock()
	defer r.hoppie.mu.Unlock()
	return len(r.hoppie.sent)
}

// NE1 -> NE7 (no CPDLC) -> NE8.
func TestParkedAcrossNonCpdlcSector(t *testing.T) {
	r := newRig(t)
	_, advance := r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1/1/NE/LOGON ACCEPTED")
	r.tick()
	advance(29 * time.Second)
	r.tick()
	if r.sentCount() != 1 {
		t.Fatal("parked inside the grace")
	}
	advance(2 * time.Second)
	r.tick()
	r.expectSent("AZA1", "/data2/2//NE/"+notInUse)

	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/5//Y/REQUEST FL390")
	r.pollHoppie()
	r.expectSent("AZA1", "/data2/3/5/NE/"+unableNotInUse)
	expect(t, r.inbox("CODE8", "LIRR_NE8_CTR"))

	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "AZA1")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/6//Y/REQUEST FL370")
	r.pollHoppie()
	got := r.inbox("CODE8", "LIRR_NE8_CTR")
	expect(t, got, "CONNECTED NOTAUTH", "/data2/6//Y/REQUEST FL370")
	if got[0].From != "AZA1" || got[0].Type != "vcp" {
		t.Fatalf("adoption %v", got[0])
	}
	advance(time.Minute)
	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "AZA1")
	r.tick()
	r.expectSent("AZA1", "/data2/3/5/NE/"+unableNotInUse) // nothing new
}

func TestInternalHandoffInsideGrace(t *testing.T) {
	r := newRig(t)
	_, advance := r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1/1/NE/LOGON ACCEPTED")
	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "")
	r.tick()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.tick()
	advance(20 * time.Second)
	r.tick()
	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "AZA1")
	expect(t, r.inbox("CODE8", "LIRR_NE8_CTR"), "CONNECTED AUTH")
	advance(20 * time.Second)
	r.inbox("CODE8", "LIRR_NE8_CTR")
	r.tick()
	if r.sentCount() != 1 {
		t.Fatal("NOT IN USE sent during an internal handoff")
	}
	// NE1 claims it back: it no longer knows the connection, so it is told again.
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1")
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"), "CONNECTED AUTH")
}

// NE1 hands AZA1 over to LIMM: the proxy waits for the pilot's LOGOFF and never parks it.
func TestNoParkAfterHandover(t *testing.T) {
	r := newRig(t)
	_, advance := r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1/1/NE/LOGON ACCEPTED")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/2//WU/CONTACT @LIMM 135.455@_@MILANO CTR")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/3//NE/HANDOVER @LIMM")
	r.tick()
	sent := r.sentCount()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "") // tracked by LIMM now
	r.tick()
	advance(time.Minute)
	r.tick()
	if r.sentCount() != sent {
		t.Fatal("parked after HANDOVER")
	}
	r.hoppie.pilot("AZA1", "LIRR", "/data2/4//N/LOGOFF")
	r.pollHoppie()
	if st := r.proxy.stations["LIRR"]; st.conns["AZA1"] != nil {
		t.Fatal("connection kept after the pilot's LOGOFF")
	}
}

func parkedRig(t *testing.T) (*rig, func(time.Duration)) {
	r := newRig(t)
	_, advance := r.clock()
	r.sector("CODE7", "LIRR_NE7_CTR", "", "assumed", "")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/1//Y/REQUEST LOGON")
	r.pollHoppie()
	r.expectSent("AZA1", "/data2/3//NE/"+notInUse)
	return r, advance
}

func TestExitFromParked(t *testing.T) {
	r, _ := parkedRig(t)
	r.sector("CODE7", "LIRR_NE7_CTR", "", "assumed", "||AZA1=")
	r.sector("CODE7", "LIRR_NE7_CTR", "", "assumed", "||AZA1=LIRR_NE7_CTR")
	r.tick()
	if r.sentCount() != 3 {
		t.Fatal("exit with the tracker inside the FIR")
	}
	r.sector("CODE7", "LIRR_NE7_CTR", "", "assumed", "||AZA1=LIMM_W_CTR")
	r.tick()
	r.expectSent("AZA1", "/data2/4//NE/ATC SERVICE TERMINATED", "/data2/5//NE/LOGOFF")
	if r.proxy.stations["LIRR"].conns["AZA1"] != nil {
		t.Fatal("connection kept after exit")
	}
}

func TestExitAfterParkedTimeout(t *testing.T) {
	r, advance := parkedRig(t)
	advance(20 * time.Minute)
	r.sector("CODE7", "LIRR_NE7_CTR", "", "assumed", "||AZA1=LIRR_NE7_CTR")
	advance(15 * time.Minute)
	r.tick()
	if r.sentCount() != 3 {
		t.Fatal("exit despite a tracker inside the FIR")
	}
	advance(15 * time.Minute)
	r.tick()
	r.expectSent("AZA1", "/data2/4//NE/ATC SERVICE TERMINATED", "/data2/5//NE/LOGOFF")
}

func TestNextSectorNotUsedForConnected(t *testing.T) {
	r, _ := parkedRig(t)
	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "|AZA1")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/2//Y/REQUEST FL390")
	r.pollHoppie()
	expect(t, r.inbox("CODE8", "LIRR_NE8_CTR"))
	r.expectSent("AZA1", "/data2/4/2/NE/"+unableNotInUse)
}

func TestPollCarriesAssumed(t *testing.T) {
	r := newRig(t)
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/1//Y/REQUEST LOGON") // the proxy accepts it
	r.pollHoppie()
	resp, err := http.PostForm(r.url, url.Values{
		"logon": {"CODE1"}, "from": {"LIRR_NE1_CTR"}, "to": {"SERVER"}, "type": {"poll"}, "assumed": {"AZA1|"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	got, _ := parsePoll(string(body))
	expect(t, got, "CONNECTED NOTAUTH") // the claim is applied before the inbox is read
}

func TestHoppieDownReachesSectors(t *testing.T) {
	r := newRig(t)
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1")
	st := r.proxy.stations["LIRR"]
	for i := 0; i < downAfter; i++ {
		st.upstream(false)
	}
	if got := r.sector("CODE1", "LIRR_NE1_CTR", "SERVER", "poll", ""); got != "error {hoppie unreachable}" {
		t.Fatalf("got %q", got)
	}
	r.hoppie.pilot("AZA1", "LIRR", "/data2/1//Y/REQUEST FL370")
	r.pollHoppie() // back
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"), "/data2/1//Y/REQUEST FL370")
}

func TestLogonCodeRevoked(t *testing.T) {
	r := newRig(t)
	_, advance := r.clock()
	if got := r.sector("CODE1", "LIRR_N_CTR", "SERVER", "poll", ""); got != "ok" {
		t.Fatalf("good code: %q", got)
	}
	r.hoppie.mu.Lock()
	delete(r.hoppie.valid, "CODE1")
	r.hoppie.mu.Unlock()
	advance(4 * time.Minute)
	if got := r.sector("CODE1", "LIRR_N_CTR", "SERVER", "poll", ""); got != "ok" {
		t.Fatalf("inside the cache: %q", got)
	}
	advance(2 * time.Minute)
	if got := r.sector("CODE1", "LIRR_N_CTR", "SERVER", "poll", ""); got != "error {illegal logon code}" {
		t.Fatalf("after the cache: %q", got)
	}
}

func TestBadLogonsDoNotFloodHoppie(t *testing.T) {
	r := newRig(t)
	_, advance := r.clock()
	pings := func() int {
		r.hoppie.mu.Lock()
		defer r.hoppie.mu.Unlock()
		return r.hoppie.pings
	}
	for i := 0; i < 3; i++ {
		if got := r.sector("BAD", "LIRR_N_CTR", "SERVER", "poll", ""); got != "error {illegal logon code}" {
			t.Fatalf("bad code: %q", got)
		}
	}
	if n := pings(); n != 1 {
		t.Fatalf("a refused code was pinged %d times, want 1", n)
	}
	for i := 0; i < pingBurst; i++ {
		r.sector("JUNK"+strconv.Itoa(i), "LIRR_N_CTR", "SERVER", "poll", "")
	}
	if got := r.sector("CODE1", "LIRR_N_CTR", "SERVER", "poll", ""); got != "error {proxy busy, retry}" {
		t.Fatalf("over the ping limit: %q", got)
	}
	if n := pings(); n != pingBurst {
		t.Fatalf("%d pings in one window, want %d", n, pingBurst)
	}
	advance(badLogonCache) // also past pingWindow
	if got := r.sector("CODE1", "LIRR_N_CTR", "SERVER", "poll", ""); got != "ok" {
		t.Fatalf("next window: %q", got)
	}
	if n := len(r.proxy.logons); n != 1 {
		t.Fatalf("%d cached checks, want only CODE1 once the junk expired", n)
	}
}

func TestRestartKeepsConnections(t *testing.T) {
	r := newRig(t)
	_, advance := r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1/1/NE/LOGON ACCEPTED")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/5//WU/CLIMB TO @FL370@")
	r.expectSent("AZA1", "/data2/1/1/NE/LOGON ACCEPTED", "/data2/2//WU/CLIMB TO @FL370@")
	r.hoppie.pilot("DLH4", "LIRR", "/data2/1//Y/REQUEST LOGON") // no sector: parked
	r.pollHoppie()
	r.expectSent("DLH4", "/data2/3//NE/"+notInUse)

	r.restart()
	st := r.proxy.stations["LIRR"]
	if c := st.conns["AZA1"]; c == nil || !c.authorized || c.parked || !c.known["LIRR_NE1_CTR"] {
		t.Fatalf("AZA1 after restart: %+v", c)
	}
	if c := st.conns["DLH4"]; c == nil || !c.parked {
		t.Fatalf("DLH4 after restart: %+v", c)
	}

	// NE8 takes AZA1 over; NE1 still gets the reply to its own uplink.
	advance(5 * time.Second)
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.sector("CODE8", "LIRR_NE8_CTR", "", "assumed", "AZA1")
	expect(t, r.inbox("CODE8", "LIRR_NE8_CTR"), "CONNECTED AUTH")
	r.sector("CODE8", "LIRR_NE8_CTR", "AZA1", "cpdlc", "/data2/1//WU/DESCEND TO @FL250@")
	r.expectSent("AZA1", "/data2/3//WU/DESCEND TO @FL250@") // continues, no collision with MIN 1 or 2
	r.hoppie.pilot("AZA1", "LIRR", "/data2/9/2/N/WILCO")
	r.pollHoppie()
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"), "/data2/9/5/N/WILCO")
	r.tick()
	if c := st.conns["DLH4"]; c == nil || !c.parked {
		t.Fatal("parked flight lost on the first tick")
	}
}

func TestCorruptStateStartsEmpty(t *testing.T) {
	r := newRig(t)
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"stations": {"LIRR": {"conns": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.start(path); err == nil {
		t.Fatal("corrupt state accepted")
	}
	if len(r.proxy.stations["LIRR"].conns) != 0 {
		t.Fatal("corrupt state partly applied")
	}
	if n, err := r.start(filepath.Join(t.TempDir(), "missing.json")); n != 0 || err != nil {
		t.Fatalf("missing state: %d %v", n, err)
	}
}

// staleRig has AZA1 with NE1, one uplink (station MIN 1) open and a successful poll behind it.
func staleRig(t *testing.T) (*rig, func(time.Duration)) {
	r := newRig(t)
	_, advance := r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1//WU/CLIMB TO @FL370@")
	r.pollHoppie()
	return r, advance
}

func (r *rig) expectStaleHandled() {
	r.t.Helper()
	r.hoppie.pilot("AZA1", "LIRR", "/data2/4//Y/REQUEST FL390")
	r.hoppie.pilot("AZA1", "LIRR", "/data2/5/1/N/WILCO")
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1")
	r.pollHoppie()
	expect(r.t, r.inbox("CODE1", "LIRR_NE1_CTR"), "/data2/5/1/N/WILCO")
	r.expectSent("AZA1", "/data2/2/4/NE/"+unavailable)

	r.hoppie.pilot("AZA1", "LIRR", "/data2/6//Y/REQUEST FL370")
	r.pollHoppie()
	expect(r.t, r.inbox("CODE1", "LIRR_NE1_CTR"), "/data2/6//Y/REQUEST FL370")
}

func TestProxyDownStaleRequests(t *testing.T) {
	r, advance := staleRig(t)
	r.restart()
	advance(10 * time.Minute)
	r.expectStaleHandled()
}

func TestHoppieDownStaleRequests(t *testing.T) {
	r, advance := staleRig(t)
	r.hoppie.mu.Lock()
	r.hoppie.down = true
	r.hoppie.mu.Unlock()
	for i := 0; i < 4; i++ {
		advance(50 * time.Second)
		r.pollHoppie()
	}
	r.hoppie.mu.Lock()
	r.hoppie.down = false
	r.hoppie.mu.Unlock()
	r.expectStaleHandled()
}

func TestShortOutageDeliversRequests(t *testing.T) {
	r, advance := staleRig(t)
	r.hoppie.mu.Lock()
	r.hoppie.down = true
	r.hoppie.mu.Unlock()
	for i := 0; i < 3; i++ {
		advance(50 * time.Second)
		r.pollHoppie()
	}
	r.hoppie.mu.Lock()
	r.hoppie.down = false
	r.hoppie.mu.Unlock()
	advance(20 * time.Second)
	r.hoppie.pilot("AZA1", "LIRR", "/data2/4//Y/REQUEST FL390")
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "AZA1")
	r.pollHoppie()
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"), "/data2/4//Y/REQUEST FL390")
}

// A flight the sector already told NOT IN USE (below the floor) is parked without a second one.
func TestParkNoRepeatedNotInUse(t *testing.T) {
	r := newRig(t)
	_, advance := r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1/1/NE/LOGON ACCEPTED")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/2//NE/CPDLC NOT IN USE. VOICE ONLY UNTIL NOTIFIED")
	r.tick()
	advance(31 * time.Second)
	r.tick()
	if r.sentCount() != 2 {
		t.Fatal("NOT IN USE repeated on park")
	}
	r.hoppie.pilot("AZA1", "LIRR", "/data2/5//Y/REQUEST FL390")
	r.pollHoppie()
	r.expectSent("AZA1", "/data2/3/5/NE/"+unableNotInUse) // parked all the same
}

// Released to a tracker outside the FIR inside the grace: exit at once, never park first.
func TestExitWithoutParking(t *testing.T) {
	r := newRig(t)
	r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1/1/NE/LOGON ACCEPTED")
	r.tick()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "||AZA1=LIMM_W_CTR")
	r.tick()
	r.expectSent("AZA1", "/data2/2//NE/ATC SERVICE TERMINATED", "/data2/3//NE/LOGOFF")
	if r.sentCount() != 3 {
		t.Fatal("parked before the exit")
	}
}

// After HANDOVER to the next FIR the tracker is outside: wait for the pilot's LOGOFF.
func TestNoExitAfterHandover(t *testing.T) {
	r := newRig(t)
	r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1/1/NE/LOGON ACCEPTED")
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/2//NE/HANDOVER @LIMM")
	r.sector("CODE1", "LIRR_NE1_CTR", "", "assumed", "||AZA1=LIMM_W_CTR")
	r.tick()
	if r.sentCount() != 2 {
		t.Fatal("proxy uplinked after HANDOVER")
	}
}

// pdcRig adds LIRF, a PDC station, with DEL and TWR connected.
func pdcRig(t *testing.T) *rig {
	r := newRig(t)
	r.cfg.Stations = append(r.cfg.Stations, StationConfig{Station: "LIRF", PDC: true, Sectors: []string{"LIRF_DEL", "LIRF_TWR"}})
	if _, err := r.start(""); err != nil {
		t.Fatal(err)
	}
	r.sector("CODE7", "LIRF_DEL", "SERVER", "ping", "")
	r.sector("CODE8", "LIRF_TWR", "SERVER", "ping", "")
	return r
}

func (r *rig) pollStation(name string) {
	st := r.proxy.stations[name]
	r.proxy.poll(st)
	r.proxy.flush(st)
}

// pdcAnswer is the text of a proxy PDC refusal, sent as CPDLC with RA NE.
func pdcAnswer(t *testing.T, m Message) string {
	t.Helper()
	p, ok := parsePacket(m.Packet)
	if !ok || p.ra != "NE" {
		t.Fatalf("not a CPDLC NE uplink: %v", m)
	}
	return p.text
}

func (f *fakeHoppie) telex(from, to, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues[to] = append(f.queues[to], Message{from, "telex", text})
}

const predep = "REQUEST PREDEP CLEARANCE AZA1 A320 TO LIMC AT LIRF STAND 101 ATIS A"

func TestPdcNotAvailableForAnotherAirportOrBadFormat(t *testing.T) {
	r := pdcRig(t)
	for text, want := range map[string]string{
		"REQUEST PREDEP CLEARANCE AZA1 A320 TO LIRF AT LIBD STAND 107 ATIS B": pdcRejected,       // not LIRF's departure
		"REQUEST PREDEP CLEARANCE AZA1 A320 AT LIRF":                          pdcErrorInMessage, // malformed: no TO
	} {
		r.hoppie.telex("AZA1", "LIRF", text)
		r.pollStation("LIRF")
		expect(t, r.inbox("CODE7", "LIRF_DEL"))
		if m, to := r.hoppie.lastSent(); to != "AZA1" || m.Type != "cpdlc" || pdcAnswer(t, m) != want {
			t.Fatalf("%q: sent %v to %s", text, m, to)
		}
	}
}

func TestPdcDeparture(t *testing.T) {
	for text, want := range map[string]string{
		"request predep clearance  EJU56VC A320 TO EDDL AT LIMC STAND 107 ATIS B ": "LIMC",
		"REQUEST PREDEP CLEARANCE EJU56VC A320 TO EDDL AT LIMC":                    "LIMC",
		"REQUEST PREDEP CLEARANCE EJU56VC A320 TO EDDL ATIS B":                     "",
		"REQUEST PREDEP CLEARANCE EJU56VC A320 TO EDDL":                            "",
	} {
		if got, ok := pdcDeparture(text); !ok || got != want {
			t.Fatalf("%q: got %q %v, want %q", text, got, ok, want)
		}
	}
	if _, ok := pdcDeparture("REQUEST PREDEP CLEARANCE EJU56VC A320 TO EDDL STAND"); ok {
		t.Fatal("STAND without a value accepted")
	}
}

func TestPdcWithoutAtDelivered(t *testing.T) {
	r := pdcRig(t)
	r.hoppie.telex("AZA1", "LIRF", "REQUEST PREDEP CLEARANCE AZA1 A320 TO LIMC")
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE7", "LIRF_DEL"), "REQUEST PREDEP CLEARANCE AZA1 A320 TO LIMC")
}

func TestPdcNotAvailableOnCtrStation(t *testing.T) {
	r := newRig(t)
	r.sector("CODE1", "LIRR_NE1_CTR", "SERVER", "ping", "")
	r.hoppie.telex("AZA1", "LIRR", predep)
	r.pollHoppie()
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"))
	m, to := r.hoppie.lastSent()
	if m.From != "LIRR" || m.Type != "cpdlc" || to != "AZA1" || pdcAnswer(t, m) != pdcRejected {
		t.Fatalf("sent %v to %s", m, to)
	}
}

func TestPdcToEverySectorFirstAnswerWins(t *testing.T) {
	r := pdcRig(t)
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE7", "LIRF_DEL"), predep)
	expect(t, r.inbox("CODE8", "LIRF_TWR"), predep)

	if got := r.sector("CODE8", "LIRF_TWR", "AZA1", "telex", "PDC TWR"); got != "ok" {
		t.Fatalf("first PDC: %s", got)
	}
	if got := r.sector("CODE7", "LIRF_DEL", "AZA1", "telex", "PDC DEL"); got != "error {pdc already sent by LIRF_TWR}" {
		t.Fatalf("second PDC: %s", got)
	}
	if got := r.sector("CODE8", "LIRF_TWR", "AZA1", "telex", "PDC TWR AGAIN"); got != "ok" {
		t.Fatalf("resend by the claimer: %s", got)
	}
	expect(t, r.inbox("CODE7", "LIRF_DEL"), "PDC SENT BY LIRF_TWR")
	expect(t, r.inbox("CODE8", "LIRF_TWR"))

	// The pilot asks again: the request is open to every sector once more.
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.pollStation("LIRF")
	if got := r.sector("CODE7", "LIRF_DEL", "AZA1", "telex", "PDC DEL"); got != "ok" {
		t.Fatalf("PDC after a new request: %s", got)
	}
}

func TestPdcStaleAfterGapNotAvailable(t *testing.T) {
	r := pdcRig(t)
	r.pollStation("LIRF")
	r.now = r.now.Add(10 * time.Minute)
	r.sector("CODE7", "LIRF_DEL", "SERVER", "ping", "")
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE7", "LIRF_DEL"))
	m, to := r.hoppie.lastSent()
	if m.From != "LIRF" || m.Type != "cpdlc" || to != "AZA1" || pdcAnswer(t, m) != pdcRejected {
		t.Fatalf("sent %v to %s", m, to)
	}
}

func TestPollEvery40Seconds(t *testing.T) {
	r := newRig(t)
	st, advance := r.clock()
	r.sector("CODE1", "LIRR_NE1_CTR", "SERVER", "ping", "")
	if !st.duePoll() {
		t.Fatal("first poll not due")
	}
	advance(39 * time.Second)
	if st.duePoll() {
		t.Fatal("poll due before 40 s")
	}
	advance(time.Second)
	r.sector("CODE1", "LIRR_NE1_CTR", "SERVER", "ping", "")
	if !st.duePoll() {
		t.Fatal("poll not due at 40 s")
	}
}

// topDownRig: LIRF answers PDCs, LIRR's CTR sectors cover it while it is empty; NE1 and NE7 online.
func topDownRig(t *testing.T) *rig {
	r := newRig(t)
	r.cfg.Stations = append(r.cfg.Stations, StationConfig{Station: "LIRF", PDC: true, TopDown: "LIRR", Sectors: []string{"LIRF_DEL"}})
	if _, err := r.start(""); err != nil {
		t.Fatal(err)
	}
	r.sector("CODE1", "LIRR_NE1_CTR", "SERVER", "ping", "")
	r.sector("CODE7", "LIRR_NE7_CTR", "SERVER", "ping", "")
	return r
}

func TestPdcTopDownToCtrWhileAirportEmpty(t *testing.T) {
	r := topDownRig(t)
	if !r.proxy.stations["LIRF"].duePoll() {
		t.Fatal("LIRF not polled with only its top-down CTR online")
	}
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"), predep)
	expect(t, r.inbox("CODE7", "LIRR_NE7_CTR"), predep)

	if got := r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "telex", "PDC NE1"); got != "ok" {
		t.Fatalf("CTR PDC: %s", got)
	}
	if m, to := r.hoppie.lastSent(); m.From != "LIRF" || to != "AZA1" {
		t.Fatalf("CTR PDC sent as %s to %s, want LIRF", m.From, to)
	}
	if got := r.sector("CODE7", "LIRR_NE7_CTR", "AZA1", "telex", "PDC NE7"); got != "error {pdc already sent by LIRR_NE1_CTR}" {
		t.Fatalf("second CTR PDC: %s", got)
	}
	expect(t, r.inbox("CODE7", "LIRR_NE7_CTR"), "PDC SENT BY LIRR_NE1_CTR")
	// A CTR telex with no PDC pending goes out as LIRR, as before.
	r.sector("CODE1", "LIRR_NE1_CTR", "BAW3", "telex", "HELLO")
	if m, _ := r.hoppie.lastSent(); m.From != "LIRR" {
		t.Fatalf("plain CTR telex sent as %s", m.From)
	}

	// LIRF_DEL comes online: new requests go to it only.
	r.sector("CODE8", "LIRF_DEL", "SERVER", "ping", "")
	r.hoppie.telex("AZA2", "LIRF", "REQUEST PREDEP CLEARANCE AZA2 A320 TO LIMC")
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE8", "LIRF_DEL"), "REQUEST PREDEP CLEARANCE AZA2 A320 TO LIMC")
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"))
	// The CTR cannot take a request given to LIRF's own sectors: its telex is a plain LIRR one.
	r.sector("CODE1", "LIRR_NE1_CTR", "AZA2", "telex", "HELLO")
	if m, _ := r.hoppie.lastSent(); m.From != "LIRR" {
		t.Fatalf("CTR telex to AZA2 sent as %s", m.From)
	}
	if got := r.sector("CODE8", "LIRF_DEL", "AZA2", "telex", "PDC DEL"); got != "ok" {
		t.Fatalf("DEL PDC: %s", got)
	}
}

func TestPdcRelayFromWrongStation(t *testing.T) {
	r := topDownRig(t)
	for text, want := range map[string]string{
		"REQUEST PREDEP CLEARANCE AZA1 A320 TO LIMC AT LIRF STAND 101 ATIS A": "PDC NOT AVAIL. RELAY TO @LIRF@",
		"REQUEST PREDEP CLEARANCE AZA1 A320 TO LIMC":                          "PDC NOT AVAIL. RELAY TO @LIRF@", // LIRR covers LIRF
		"REQUEST PREDEP CLEARANCE AZA1 A320 TO LIRF AT LIBD":                  pdcRejected,                      // not a PDC airport here
	} {
		r.hoppie.telex("AZA1", "LIRR", text)
		r.pollHoppie()
		expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"))
		if m, to := r.hoppie.lastSent(); m.From != "LIRR" || m.Type != "cpdlc" || to != "AZA1" || pdcAnswer(t, m) != want {
			t.Fatalf("%q: sent %v to %s, want %q", text, m, to, want)
		}
	}
}

// A station's first poll since the proxy started may bring hours-old messages (Hoppie has no
// timestamps): PDC requests are rejected, logons are dropped.
func TestFirstPollBacklogIsStale(t *testing.T) {
	r := pdcRig(t)
	r.proxy.stations["LIRF"].pollOK = time.Time{}
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.hoppie.pilot("AZA2", "LIRF", "/data2/1//Y/REQUEST LOGON")
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE7", "LIRF_DEL"))
	if m, to := r.hoppie.lastSent(); to != "AZA1" || pdcAnswer(t, m) != pdcRejected {
		t.Fatalf("backlog PDC: sent %v to %s", m, to)
	}
	if r.sentCount() != 1 || r.proxy.stations["LIRF"].conns["AZA2"] != nil {
		t.Fatal("backlog logon accepted")
	}
	// Later polls are fresh again.
	r.hoppie.telex("AZA3", "LIRF", "REQUEST PREDEP CLEARANCE AZA3 A320 TO LIMC")
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE7", "LIRF_DEL"), "REQUEST PREDEP CLEARANCE AZA3 A320 TO LIMC")
}

// sectorPdc sends a cpdlc uplink marked as an answer to a PDC request, as HoppieClient does.
func (r *rig) sectorPdc(logon, from, to, pkt string) string {
	r.t.Helper()
	resp, err := http.PostForm(r.url, url.Values{
		"logon": {logon}, "from": {from}, "to": {to}, "type": {"cpdlc"}, "packet": {pkt}, "pdc": {"1"},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func (f *fakeHoppie) sentTexts(to, prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for i, m := range f.sent {
		if p, ok := parsePacket(m.Packet); ok && f.sentTo[i] == to && strings.HasPrefix(p.text, prefix) {
			n++
		}
	}
	return n
}

func TestPdcAutoAckOncePerDelivery(t *testing.T) {
	r := pdcRig(t)
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.pollStation("LIRF")
	if n := r.hoppie.sentTexts("AZA1", pdcReceived); n != 1 {
		t.Fatalf("%d acks for one request to two sectors", n)
	}
	m, _ := r.hoppie.lastSent()
	if p, _ := parsePacket(m.Packet); m.From != "LIRF" || p.ra != "NE" || p.text != pdcReceived {
		t.Fatalf("ack %v", m)
	}
	// Refused requests get no RCD RECEIVED.
	r.hoppie.telex("AZA2", "LIRF", "REQUEST PREDEP CLEARANCE AZA2 A320 TO LIRF AT LIMC")
	r.pollStation("LIRF")
	if n := r.hoppie.sentTexts("AZA2", pdcReceived); n != 0 {
		t.Fatalf("ack to a refused request")
	}
	// A repeat nobody answered yet: ALREADY RECEIVED, not delivered again.
	r.inbox("CODE7", "LIRF_DEL")
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE7", "LIRF_DEL"))
	if m, to := r.hoppie.lastSent(); to != "AZA1" || pdcAnswer(t, m) != pdcAlready {
		t.Fatalf("repeat: sent %v to %s", m, to)
	}
	// Once answered, a repeat is a new request.
	if got := r.sectorPdc("CODE7", "LIRF_DEL", "AZA1", "/data2/1//R/AZA1 CLRD"); got != "ok" {
		t.Fatalf("PDC: %s", got)
	}
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE7", "LIRF_DEL"), predep)
}

func TestPdcMarkedCpdlcClaimsAndRogerReturns(t *testing.T) {
	r := pdcRig(t)
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.pollStation("LIRF")
	r.inbox("CODE7", "LIRF_DEL")
	r.inbox("CODE8", "LIRF_TWR")

	if got := r.sectorPdc("CODE7", "LIRF_DEL", "AZA1", "/data2/5//R/@AZA1@ CLRD TO @LIMC@"); got != "ok" {
		t.Fatalf("PDC: %s", got)
	}
	m, to := r.hoppie.lastSent()
	sent, _ := parsePacket(m.Packet)
	if m.From != "LIRF" || to != "AZA1" || sent.ra != "R" || sent.min == "5" {
		t.Fatalf("PDC sent %v to %s", m, to)
	}
	now := r.now.UTC()
	if header := "CLD " + now.Format("1504") + "Z " + now.Format("060102") + " LIRF PDC " + sent.min + " @AZA1@ CLRD TO @LIMC@"; sent.text != header {
		t.Fatalf("PDC text %q, want %q", sent.text, header)
	}
	if got := r.sectorPdc("CODE8", "LIRF_TWR", "AZA1", "/data2/9//R/@AZA1@ CLRD"); got != "error {pdc already sent by LIRF_DEL}" {
		t.Fatalf("second PDC: %s", got)
	}
	expect(t, r.inbox("CODE8", "LIRF_TWR"), "PDC SENT BY LIRF_DEL")

	r.hoppie.pilot("AZA1", "LIRF", "/data2/3/"+sent.min+"/N/ROGER")
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE7", "LIRF_DEL"), "/data2/3/5/N/ROGER")
	expect(t, r.inbox("CODE8", "LIRF_TWR"))
	if m, to := r.hoppie.lastSent(); to != "AZA1" || m.From != "LIRF" || pdcAnswer(t, m) != pdcConfirmed {
		t.Fatalf("CDA: sent %v to %s", m, to)
	}
	// Once only.
	r.hoppie.pilot("AZA1", "LIRF", "/data2/4/"+sent.min+"/N/ROGER")
	r.pollStation("LIRF")
	if n := r.hoppie.sentTexts("AZA1", pdcConfirmed); n != 1 {
		t.Fatalf("%d CDAs", n)
	}
}

func TestPdcMarkedWithoutRequestRefused(t *testing.T) {
	r := pdcRig(t)
	if got := r.sectorPdc("CODE7", "LIRF_DEL", "AZA1", "/data2/1//R/@AZA1@ CLRD"); got != "error {no pdc request pending}" {
		t.Fatalf("PDC without request: %s", got)
	}
	r.hoppie.mu.Lock()
	defer r.hoppie.mu.Unlock()
	if len(r.hoppie.sent) != 0 {
		t.Fatalf("sent %v", r.hoppie.sent)
	}
}

// Regression: an unmarked uplink from the top-down CTR (its automatic LOGON ACCEPTED) to an
// aircraft with a PDC pending neither takes the PDC nor goes out as the airport.
func TestPdcTopDownUnmarkedCpdlcLeavesPdcAlone(t *testing.T) {
	r := topDownRig(t)
	r.hoppie.telex("AZA1", "LIRF", predep)
	r.pollStation("LIRF")
	r.inbox("CODE1", "LIRR_NE1_CTR")
	r.inbox("CODE7", "LIRR_NE7_CTR")

	if got := r.sector("CODE1", "LIRR_NE1_CTR", "AZA1", "cpdlc", "/data2/1/4/NE/LOGON ACCEPTED"); got != "ok" {
		t.Fatalf("logon accepted: %s", got)
	}
	if m, _ := r.hoppie.lastSent(); m.From != "LIRR" {
		t.Fatalf("LOGON ACCEPTED sent as %s", m.From)
	}
	expect(t, r.inbox("CODE7", "LIRR_NE7_CTR"))

	if got := r.sectorPdc("CODE7", "LIRR_NE7_CTR", "AZA1", "/data2/8//R/@AZA1@ CLRD"); got != "ok" {
		t.Fatalf("NE7 PDC: %s", got)
	}
	m, _ := r.hoppie.lastSent()
	sent, _ := parsePacket(m.Packet)
	if m.From != "LIRF" {
		t.Fatalf("CTR PDC sent as %s", m.From)
	}
	expect(t, r.inbox("CODE1", "LIRR_NE1_CTR"), "PDC SENT BY LIRR_NE7_CTR")

	// The pilot's ROGER reaches LIRF and goes back to the CTR sector that sent the PDC.
	r.hoppie.pilot("AZA1", "LIRF", "/data2/6/"+sent.min+"/N/ROGER")
	r.pollStation("LIRF")
	expect(t, r.inbox("CODE7", "LIRR_NE7_CTR"), "/data2/6/8/N/ROGER")
}
