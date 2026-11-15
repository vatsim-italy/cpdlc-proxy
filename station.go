package main

// One Hoppie station shared by the sectors of a FIR (docs/cpdlc.md §14.3–§14.5).

import (
	"log"
	"math/rand"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	notInUse       = "CPDLC NOT IN USE. VOICE ONLY UNTIL NOTIFIED@_@DO NOT LOGOFF"
	unableNotInUse = "UNABLE@_@CPDLC NOT IN USE. VOICE ONLY"
	unavailable    = "CPDLC TEMPORARILY UNAVAILABLE. REVERT TO VOICE"
)

const (
	sectorTimeout = 30 * time.Second // no request for this long: the sector is gone
	sectorForget  = 5 * time.Minute  // gone this long: its inbox is dropped
	minExpiry     = time.Hour
	parkGrace     = 30 * time.Second // covers the poll gap of a normal internal handoff
	parkedExit    = 30 * time.Minute // parked without a tracker inside the FIR: it left
	connForget    = 24 * time.Hour
	fastPoll      = 20 * time.Second
	fastWindow    = 180 * time.Second // T_resp: an uplink waiting for a reply keeps polling fast
	staleAfter    = 180 * time.Second // T_resp: requests queued longer than this are stale
	logonMaxAge   = 10 * time.Minute  // a logon possibly queued longer: its pilot has likely left
)

type sector struct {
	logon string
	seen  time.Time
	inbox []Message
}

// origin of a MIN we put on an uplink; sector "" is the proxy itself.
type origin struct {
	sector string
	local  string // the MIN the sector used
	at     time.Time
}

// conn is a CPDLC connection to the station, inferred from the traffic (§14.5).
type conn struct {
	authorized, parked bool
	handedOver         bool // HANDOVER to another station: wait for the pilot's LOGOFF, never park
	parkedAt           time.Time
	ownerLostAt        time.Time // zero while a sector serves it
	insideAt           time.Time // last tracker report inside the FIR
	touched            time.Time
	known              map[string]bool // sectors that have the connection
}

type outgoing struct {
	to, packet string
}

type Station struct {
	name        string
	facility    string   // CURRENT ATC UNIT text of the proxy's own logon answer
	patterns    []string // sector callsigns of this FIR
	pollSeconds int      // > 0: fixed interval (tests, local simulator); 0: Hoppie's 45–75 s
	staleAfter  time.Duration
	now         func() time.Time

	mu         sync.Mutex
	sectors    map[string]*sector
	owner      map[string]string // aircraft -> sector that assumed it
	nextOf     map[string]string // aircraft -> coordinated next sector
	counter    map[string]int    // aircraft -> last MIN sent to it
	mins       map[string]map[string]origin
	conns      map[string]*conn
	outbox     []outgoing // proxy uplinks, sent by Proxy outside mu
	lastUplink time.Time
	polled     time.Time
	nextPoll   time.Time
	failures   int       // Hoppie polls failed in a row
	pollOK     time.Time // last successful Hoppie poll

	sendMu   sync.Mutex
	lastSend time.Time
	flushing atomic.Bool // a flushAsync is running
}

func newStation(name string, pollSeconds int) *Station {
	return &Station{
		name:        name,
		pollSeconds: pollSeconds,
		staleAfter:  staleAfter,
		now:         time.Now,
		sectors:     map[string]*sector{},
		owner:       map[string]string{},
		nextOf:      map[string]string{},
		counter:     map[string]int{},
		mins:        map[string]map[string]origin{},
		conns:       map[string]*conn{},
	}
}

func (st *Station) live(s *sector) bool {
	return s != nil && st.now().Sub(s.seen) < sectorTimeout
}

// touch records a request from a sector; it refuses a second live logon code on one callsign.
func (st *Station) touch(callsign, logon string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.sectors[callsign]
	if s != nil && s.logon != logon && st.live(s) {
		return false
	}
	if s == nil || s.logon != logon {
		s = &sector{logon: logon}
		st.sectors[callsign] = s
		log.Printf("%s: sector %s connected", st.name, callsign)
	}
	s.seen = st.now()
	return true
}

func (st *Station) takeInbox(callsign string) []Message {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.sectors[callsign]
	if s == nil {
		return nil
	}
	out := s.inbox
	s.inbox = nil
	return out
}

func (st *Station) takeOutbox() []outgoing {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := st.outbox
	st.outbox = nil
	return out
}

const downAfter = 3 // failed Hoppie polls before sectors are told

// upstream records a Hoppie poll; gap is the time since the previous successful one (0 if none).
func (st *Station) upstream(ok bool) (gap time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if ok {
		if st.failures >= downAfter {
			log.Printf("%s: hoppie back", st.name)
		}
		st.failures = 0
		now := st.now()
		if !st.pollOK.IsZero() {
			gap = now.Sub(st.pollOK)
		}
		st.pollOK = now
	} else {
		st.failures++
	}
	return gap
}

func (st *Station) upstreamDown() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.failures >= downAfter
}

// setAssumedPacket reads `<assumed>|<next>|<CS=TRACKER ...>`, callsigns space separated;
// the third section (parked flights' EuroScope tracking controller) is optional.
func (st *Station) setAssumedPacket(callsign, pkt string) {
	parts := append(strings.SplitN(strings.ToUpper(pkt), "|", 3), "", "")
	trackers := map[string]string{}
	for _, f := range strings.Fields(parts[2]) {
		cs, tracker, _ := strings.Cut(f, "=")
		trackers[cs] = tracker
	}
	st.setAssumed(callsign, strings.Fields(parts[0]), strings.Fields(parts[1]), trackers)
}

// setAssumed replaces what a sector owns; the latest claim on an aircraft wins.
func (st *Station) setAssumed(callsign string, assumed, next []string, trackers map[string]string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var had []string
	for a, s := range st.owner {
		if s == callsign {
			had = append(had, a)
		}
	}
	st.claim(st.owner, callsign, assumed, "assumed")
	st.claim(st.nextOf, callsign, next, "next")
	for _, a := range had {
		if c := st.conns[a]; c != nil && st.owner[a] != callsign {
			c.known[callsign] = false
		}
	}
	for _, a := range assumed {
		if c := st.conns[a]; c != nil && st.owner[a] == callsign && !c.known[callsign] {
			st.adopt(callsign, a, c)
		}
	}
	for a, tracker := range trackers {
		c := st.conns[a]
		switch {
		case c == nil || !c.parked || tracker == "":
		case st.inside(tracker):
			c.insideAt = st.now()
		default:
			log.Printf("%s: %s tracked by %s, outside the FIR", st.name, a, tracker)
			st.exit(a)
		}
	}
}

func (st *Station) claim(table map[string]string, callsign string, aircraft []string, what string) {
	mine := map[string]bool{}
	for _, a := range aircraft {
		mine[a] = true
		if prev := table[a]; prev != "" && prev != callsign && st.live(st.sectors[prev]) {
			log.Printf("%s: %s %s by both %s and %s, %s wins", st.name, a, what, prev, callsign, callsign)
		}
		table[a] = callsign
	}
	for a, s := range table {
		if s == callsign && !mine[a] {
			delete(table, a)
		}
	}
}

func (st *Station) inside(callsign string) bool {
	for _, g := range st.patterns {
		if ok, _ := path.Match(g, callsign); ok {
			return true
		}
	}
	return false
}

// adopt tells a sector about a connection it does not have, ahead of any message for it.
func (st *Station) adopt(callsign, aircraft string, c *conn) {
	state := "CONNECTED NOTAUTH"
	if c.authorized {
		state = "CONNECTED AUTH"
	}
	s := st.sectors[callsign]
	s.inbox = append([]Message{{From: aircraft, Type: "vcp", Packet: state}}, s.inbox...)
	c.known[callsign] = true
	c.parked = false
	c.ownerLostAt = time.Time{}
	log.Printf("%s: %s adopts %s, %s", st.name, callsign, aircraft, state)
}

// route hands a downlink to a sector, or answers it for the proxy (§14.3, §14.5);
// oldLogons says the batch may hold logons older than logonMaxAge.
func (st *Station) route(m Message, oldLogons bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	p, ok := parsePacket(m.Packet)
	ok = ok && m.Type == "cpdlc"
	c := st.conns[m.From]
	if c != nil {
		c.touched = st.now()
	}
	if ok && p.logonOrOff() {
		delete(st.conns, m.From) // a new logon starts a new connection
		c = nil
	}
	// 1. A reply goes to the sector that sent the uplink, with that sector's own MIN.
	if ok && p.mrn != "" {
		if o, found := st.mins[m.From][p.mrn]; found {
			if s := st.sectors[o.sector]; st.live(s) {
				p.mrn = o.local
				m.Packet = p.String()
				s.inbox = append(s.inbox, m)
				return
			}
		}
	}
	// 2. The sector that assumed the aircraft.
	owner := st.owner[m.From]
	if s := st.sectors[owner]; st.live(s) {
		if c != nil && !c.known[owner] {
			st.adopt(owner, m.From, c)
		}
		s.inbox = append(s.inbox, m)
		return
	}
	// 3. Its coordinated next sector: logons before FIR entry, and flights it accepted.
	next := st.nextOf[m.From]
	if s := st.sectors[next]; st.live(s) && (c == nil || c.known[next]) {
		s.inbox = append(s.inbox, m)
		return
	}
	switch {
	case ok && c == nil && strings.HasPrefix(p.text, "REQUEST LOGON") && oldLogons:
		// shortcut: no timestamps, so every unowned logon after a long gap is dropped; a pilot still there logs on again.
		log.Printf("%s: no sector for %s, logon possibly older than %v dropped", st.name, m.From, logonMaxAge)
		return
	case ok && c == nil && strings.HasPrefix(p.text, "REQUEST LOGON"):
		st.acceptLogon(m.From, p.min)
		return
	case ok && c != nil && c.parked && p.request():
		st.uplink(m.From, p.min, unableNotInUse)
		log.Printf("%s: %s parked, UNABLE to %s", st.name, m.From, p.text)
		return
	}
	log.Printf("%s: no sector for %s, dropped %s %s", st.name, m.From, m.Type, m.Packet)
}

// answerStale answers a pilot request queued during an outage instead of delivering it.
func (st *Station) answerStale(m Message) bool {
	p, ok := parsePacket(m.Packet)
	if m.Type != "cpdlc" || !ok || !p.request() || p.logonOrOff() {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.uplink(m.From, p.min, unavailable)
	return true
}

// acceptLogon answers a logon no sector can take: connected, parked, voice only.
func (st *Station) acceptLogon(aircraft, min string) {
	facility := st.facility
	if facility == "" {
		facility = st.name
	}
	st.uplink(aircraft, min, "LOGON ACCEPTED")
	st.uplink(aircraft, "", "CURRENT ATC UNIT@_@"+st.name+"@_@"+facility)
	st.uplink(aircraft, "", notInUse)
	now := st.now()
	st.conns[aircraft] = &conn{parked: true, parkedAt: now, touched: now, known: map[string]bool{}}
	log.Printf("%s: no sector for %s, logon accepted and parked", st.name, aircraft)
}

func (st *Station) park(aircraft string, c *conn) {
	if c.authorized { // already told NOT IN USE (e.g. below the floor): don't repeat it
		st.uplink(aircraft, "", notInUse)
	}
	c.authorized, c.parked, c.parkedAt = false, true, st.now()
	log.Printf("%s: %s parked", st.name, aircraft)
}

func (st *Station) exit(aircraft string) {
	st.uplink(aircraft, "", "ATC SERVICE TERMINATED")
	st.uplink(aircraft, "", "LOGOFF")
	delete(st.conns, aircraft)
	log.Printf("%s: %s logged off, left the FIR", st.name, aircraft)
}

// uplink queues a proxy message to an aircraft; mu is held.
func (st *Station) uplink(to, mrn, text string) {
	min := st.nextMin(to, origin{at: st.now()})
	st.outbox = append(st.outbox, outgoing{to, packet{min, mrn, "NE", text}.String()})
}

// rewriteUplink gives an uplink a MIN unique for this station and aircraft.
func (st *Station) rewriteUplink(callsign, to, kind, pkt string) string {
	p, ok := parsePacket(pkt)
	if kind != "cpdlc" || !ok {
		return pkt
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.observe(callsign, to, p.text)
	if p.ra != "NE" && p.ra != "N" {
		st.lastUplink = st.now()
	}
	p.min = st.nextMin(to, origin{callsign, p.min, st.now()})
	return p.String()
}

// nextMin takes the aircraft's next station MIN; mu is held.
func (st *Station) nextMin(to string, o origin) string {
	st.counter[to]++
	global := strconv.Itoa(st.counter[to])
	if st.mins[to] == nil {
		st.mins[to] = map[string]origin{}
	}
	st.mins[to][global] = o
	return global
}

// observe follows the connection through a sector's uplink. After HANDOVER the pilot logs off itself.
func (st *Station) observe(callsign, aircraft, text string) {
	c := st.conns[aircraft]
	if strings.HasPrefix(text, "LOGON ACCEPTED") {
		if c == nil {
			c = &conn{known: map[string]bool{}}
			st.conns[aircraft] = c
		}
		c.authorized, c.parked, c.handedOver, c.known[callsign] = true, false, false, true
	}
	if c == nil {
		return
	}
	c.touched = st.now()
	switch {
	case text == "LOGOFF":
		delete(st.conns, aircraft)
	case strings.HasPrefix(text, "CPDLC NOT IN USE"):
		c.authorized = false
	case strings.HasPrefix(text, "CPDLC NOW IN USE"):
		c.authorized = true
	case strings.HasPrefix(text, "HANDOVER"):
		c.handedOver = true
	}
}

// duePoll says whether to poll Hoppie now: never while no sector is connected.
func (st *Station) duePoll() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	anyLive := false
	for _, s := range st.sectors {
		anyLive = anyLive || st.live(s)
	}
	if !anyLive {
		return false
	}
	fast := now.Sub(st.lastUplink) < fastWindow
	if now.Before(st.nextPoll) && !(fast && now.Sub(st.polled) >= fastPoll) {
		return false
	}
	interval := time.Duration(45+rand.Intn(31)) * time.Second
	if st.pollSeconds > 0 {
		interval = time.Duration(st.pollSeconds) * time.Second
	}
	st.polled, st.nextPoll = now, now.Add(interval)
	return true
}

func (st *Station) served(aircraft string, c *conn) bool {
	next := st.nextOf[aircraft]
	return st.live(st.sectors[st.owner[aircraft]]) || (c.known[next] && st.live(st.sectors[next]))
}

// prune is the 1 s tick: forgets stale state, parks unserved connections, logs off parked ones.
func (st *Station) prune() {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	for cs, s := range st.sectors {
		if now.Sub(s.seen) > sectorForget {
			delete(st.sectors, cs)
			log.Printf("%s: sector %s forgotten, %d messages dropped", st.name, cs, len(s.inbox))
		}
	}
	for a, c := range st.conns {
		since := c.parkedAt
		if c.insideAt.After(since) {
			since = c.insideAt
		}
		switch {
		case now.Sub(c.touched) > connForget:
			delete(st.conns, a)
		case c.handedOver:
			// Gone to the next station: connForget drops it if the LOGOFF never comes.
		case c.parked:
			if now.Sub(since) >= parkedExit {
				st.exit(a)
			}
		case st.served(a, c):
			c.ownerLostAt = time.Time{}
		case c.ownerLostAt.IsZero():
			c.ownerLostAt = now
		case now.Sub(c.ownerLostAt) >= parkGrace:
			st.park(a, c)
		}
	}
	for aircraft, sent := range st.mins {
		for min, o := range sent {
			if now.Sub(o.at) > minExpiry {
				delete(sent, min)
			}
		}
		if len(sent) == 0 {
			delete(st.mins, aircraft) // an hour quiet: MINs may start again from 1
			delete(st.counter, aircraft)
		}
	}
}
