package main

// One Hoppie station shared by the sectors of a FIR (docs/cpdlc.md §14.3–§14.5).

import (
	"log"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	notInUse        = "CPDLC NOT IN USE. VOICE ONLY UNTIL NOTIFIED"
	unableNotInUse  = "UNABLE@_@CPDLC NOT IN USE. VOICE ONLY"
	unavailable     = "CPDLC TEMPORARILY UNAVAILABLE. REVERT TO VOICE"
	pdcNotAvailable = "PDC NOT AVAILABLE"
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
	pollInterval  = 40 * time.Second  // fixed, under Hoppie's 45–75 s since the proxy polls for several callsigns
	pdcExpiry     = 30 * time.Minute  // a PDC request is forgotten after this long
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
	to, kind, packet string
}

// pdcRequest is a PREDEP CLEARANCE given to every sector of a PDC station; by is the sector
// whose telex answered it first (§14.10).
type pdcRequest struct {
	at time.Time
	by string
}

type Station struct {
	name        string
	facility    string   // CURRENT ATC UNIT text of the proxy's own logon answer
	pdc         bool     // its sectors answer PDC requests; otherwise the proxy answers PDC NOT AVAILABLE
	patterns    []string // sector callsigns of this FIR
	pollSeconds int      // > 0: fixed interval (tests, local simulator); 0: pollInterval
	staleAfter  time.Duration
	now         func() time.Time

	mu      sync.Mutex
	sectors map[string]*sector
	owner   map[string]string // aircraft -> sector that assumed it
	nextOf  map[string]string // aircraft -> coordinated next sector
	counter map[string]int    // aircraft -> last MIN sent to it
	mins    map[string]map[string]origin
	conns   map[string]*conn
	// shortcut: not saved, a proxy restart loses who sent a pending PDC; persist it if that ever matters.
	pdcs       map[string]*pdcRequest
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
		pdcs:        map[string]*pdcRequest{},
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
		case c == nil || tracker == "" || c.handedOver: // after HANDOVER the pilot logs off itself
		case st.inside(tracker):
			c.insideAt = st.now()
		case !c.parked && st.served(a, c):
		default: // left the FIR: exit now, not park first and exit on the next report
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
	if isPdcRequest(m) {
		st.routePdc(m)
		return
	}
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

func isPdcRequest(m Message) bool {
	return m.Type == "telex" && strings.Contains(strings.ToUpper(m.Packet), "PREDEP")
}

// REQUEST PREDEP CLEARANCE <CS> <TYPE> TO <DEST> AT <DEP> STAND <n> ATIS <x>
var pdcFormat = regexp.MustCompile(`^REQUEST PREDEP CLEARANCE \S+ \S+ TO [A-Z]{4} AT ([A-Z]{4}) STAND \S+ ATIS [A-Z]$`)

// pdcDeparture is the AT airport of a well-formed PDC request, or "".
func pdcDeparture(text string) string {
	if g := pdcFormat.FindStringSubmatch(strings.Join(strings.Fields(strings.ToUpper(text)), " ")); g != nil {
		return g[1]
	}
	return ""
}

// routePdc gives a well-formed PDC request for this station's own airport to every live sector
// of a PDC station; they agree among themselves who answers, and claimPdc lets only the first
// telex through. Anything else gets PDC NOT AVAILABLE. mu is held.
func (st *Station) routePdc(m Message) {
	delivered := false
	if st.pdc && pdcDeparture(m.Packet) == st.name {
		for _, s := range st.sectors {
			if st.live(s) {
				s.inbox = append(s.inbox, m)
				delivered = true
			}
		}
	}
	if !delivered {
		st.telex(m.From, pdcNotAvailable)
		log.Printf("%s: PDC request from %s, %s", st.name, m.From, pdcNotAvailable)
		return
	}
	st.pdcs[m.From] = &pdcRequest{at: st.now()} // a repeated request can be answered again
}

// claimPdc lets one sector answer a pending PDC request: a telex from another sector to the
// same aircraft is refused, and the others are told who sent it.
func (st *Station) claimPdc(callsign, aircraft string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	r := st.pdcs[aircraft]
	switch {
	case r == nil || r.by == callsign:
		return ""
	case r.by != "":
		return "error {pdc already sent by " + r.by + "}"
	}
	r.by = callsign
	for cs, s := range st.sectors {
		if cs != callsign && st.live(s) {
			s.inbox = append(s.inbox, Message{From: aircraft, Type: "vcp", Packet: "PDC SENT BY " + callsign})
		}
	}
	log.Printf("%s: PDC for %s sent by %s", st.name, aircraft, callsign)
	return ""
}

// answerStale answers a pilot request queued during an outage instead of delivering it.
func (st *Station) answerStale(m Message) bool {
	if isPdcRequest(m) {
		st.mu.Lock()
		defer st.mu.Unlock()
		st.telex(m.From, pdcNotAvailable)
		return true
	}
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
	st.outbox = append(st.outbox, outgoing{to, "cpdlc", packet{min, mrn, "NE", text}.String()})
}

// telex queues a proxy free-text telex to an aircraft; mu is held.
func (st *Station) telex(to, text string) {
	st.outbox = append(st.outbox, outgoing{to, "telex", text})
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
	interval := pollInterval
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
	for a, r := range st.pdcs {
		if now.Sub(r.at) > pdcExpiry {
			delete(st.pdcs, a)
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
