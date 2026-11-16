package main

// Hoppie-compatible endpoint for sectors (docs/cpdlc.md §14.1, §14.2).

import (
	"context"
	"log"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	logonCache    = 5 * time.Minute
	badLogonCache = time.Minute // a refused (code, callsign) is not pinged again for this long
	pingBurst     = 5           // uncached Hoppie pings allowed per pingWindow, so junk codes can't flood Hoppie
	pingWindow    = 10 * time.Second
)

type Config struct {
	HoppieURL   string          `json:"hoppie_url"`
	PollSeconds int             `json:"poll_seconds"`  // > 0 only for tests and the local simulator
	StaleSecs   int             `json:"stale_seconds"` // 0: 180 (T_resp)
	Stations    []StationConfig `json:"stations"`
}

type StationConfig struct {
	Station  string   `json:"station"`
	Facility string   `json:"facility"` // CURRENT ATC UNIT name, e.g. "ROMA CTR"; empty: the station
	PDC      bool     `json:"pdc"`      // sectors answer PDC requests (an airport, e.g. LIRF)
	TopDown  string   `json:"top_down"` // PDC station: the CTR station answering its PDCs while it is empty
	Sectors  []string `json:"sectors"`  // callsign patterns, e.g. "LIRR_*CTR"
}

type Proxy struct {
	up       *upstream
	patterns []pattern
	stations map[string]*Station
	spacing  time.Duration // between sends as one station
	now      func() time.Time

	statePath string
	saved     []byte // last state written

	mu       sync.Mutex
	logons   map[logonKey]logonCheck
	pingFrom time.Time // start of the current pingWindow
	pings    int
}

type logonKey struct{ logon, callsign string }

type logonCheck struct {
	valid bool
	until time.Time
}

type pattern struct {
	glob    string
	station *Station
}

func newProxy(cfg Config, logon string) *Proxy {
	p := &Proxy{
		up:       newUpstream(cfg.HoppieURL, logon),
		stations: map[string]*Station{},
		spacing:  2 * time.Second,
		now:      time.Now,
		logons:   map[logonKey]logonCheck{},
	}
	for _, s := range cfg.Stations {
		name := strings.ToUpper(s.Station)
		st := newStation(name, cfg.PollSeconds)
		st.facility = s.Facility
		st.pdc = s.PDC
		if cfg.StaleSecs > 0 {
			st.staleAfter = time.Duration(cfg.StaleSecs) * time.Second
		}
		p.stations[name] = st
		for _, g := range s.Sectors {
			st.patterns = append(st.patterns, strings.ToUpper(g))
			p.patterns = append(p.patterns, pattern{strings.ToUpper(g), st})
		}
	}
	airports := map[string]bool{}
	for _, s := range cfg.Stations {
		st := p.stations[strings.ToUpper(s.Station)]
		st.pdcAirports = airports
		if s.PDC {
			airports[st.name] = true
		}
	}
	for _, s := range cfg.Stations {
		if s.TopDown == "" {
			continue
		}
		st := p.stations[strings.ToUpper(s.Station)]
		// Only airport -> CTR: a chain would break the airport-then-CTR lock order.
		if td := p.stations[strings.ToUpper(s.TopDown)]; td != nil && st.pdc && !td.pdc {
			st.topDown = td
		} else {
			log.Printf("%s: top_down %q is not a CTR station of this proxy, ignored", st.name, s.TopDown)
		}
	}
	return p
}

func (p *Proxy) stationFor(callsign string) *Station {
	for _, pt := range p.patterns {
		if ok, _ := path.Match(pt.glob, callsign); ok {
			return pt.station
		}
	}
	return nil
}

// checkLogon pings Hoppie with the sector's own code, cached per (code, callsign) (§14.2).
// It returns the error reply for the sector, or "" when the code is good.
// A network failure is not a bad code.
func (p *Proxy) checkLogon(logon, callsign string) string {
	key, now := logonKey{logon, callsign}, p.now()
	p.mu.Lock()
	if c, cached := p.logons[key]; cached && now.Before(c.until) {
		p.mu.Unlock()
		if c.valid {
			return ""
		}
		return "error {illegal logon code}"
	}
	for k, c := range p.logons {
		if !now.Before(c.until) {
			delete(p.logons, k)
		}
	}
	if now.Sub(p.pingFrom) >= pingWindow {
		p.pingFrom, p.pings = now, 0
	}
	if p.pings >= pingBurst {
		p.mu.Unlock()
		log.Printf("ping for %s refused: more than %d logon checks in %v", callsign, pingBurst, pingWindow)
		return "error {proxy busy, retry}"
	}
	p.pings++
	p.mu.Unlock()

	reply, err := p.up.post(logon, callsign, "SERVER", "ping", "")
	if err != nil {
		log.Printf("ping for %s failed: %v", callsign, err)
		return "error {hoppie unreachable}"
	}
	valid := strings.HasPrefix(reply, "ok")
	ttl := logonCache
	if !valid {
		ttl = badLogonCache
	}
	p.mu.Lock()
	p.logons[key] = logonCheck{valid, p.now().Add(ttl)}
	p.mu.Unlock()
	if !valid {
		return "error {illegal logon code}"
	}
	return ""
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	assumed, withAssumed := r.Form["assumed"]
	if withAssumed {
		assumed = assumed[:1]
	}
	reply := p.handle(
		assumed,
		r.Form.Get("logon"),
		strings.ToUpper(strings.TrimSpace(r.Form.Get("from"))),
		strings.ToUpper(strings.TrimSpace(r.Form.Get("to"))),
		strings.ToLower(r.Form.Get("type")),
		r.Form.Get("packet"),
	)
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(reply))
}

// assumed is set when a poll carries the sector's flights (docs/cpdlc.md §14.3).
func (p *Proxy) handle(assumed []string, logon, from, to, kind, pkt string) string {
	st := p.stationFor(from)
	if st == nil {
		return "error {callsign not authorized on this proxy}"
	}
	if logon == "" {
		return "error {illegal logon code}"
	}
	if refused := p.checkLogon(logon, from); refused != "" {
		return refused
	}
	if !st.touch(from, logon) {
		return "error {callsign already in use}"
	}
	switch kind {
	case "poll":
		if len(assumed) == 1 {
			st.setAssumedPacket(from, assumed[0])
		}
		if st.upstreamDown() {
			return "error {hoppie unreachable}" // the inbox waits for the link to come back
		}
		return formatPoll(st.takeInbox(from))
	case "ping":
		return "ok"
	case "assumed":
		st.setAssumedPacket(from, pkt)
		return "ok"
	case "cpdlc", "telex":
		return p.send(st, from, to, kind, pkt)
	default:
		return "error {unknown type " + kind + "}"
	}
}

// send forwards an uplink as the station and returns Hoppie's own answer.
func (p *Proxy) send(st *Station, from, to, kind, pkt string) string {
	if to == "" {
		return "error {no recipient}"
	}
	as := st
	if kind == "telex" {
		// A PDC goes out as the airport station the pilot asked, even from its top-down CTR.
		for _, airport := range p.stations {
			if airport != st && airport.topDown != st {
				continue
			}
			if handled, refused := airport.claimPdc(st, from, to); handled {
				if refused != "" {
					return refused
				}
				as = airport
				break
			}
		}
	}
	reply, err := p.sendAs(as, to, kind, st.rewriteUplink(from, to, kind, pkt))
	if err != nil {
		log.Printf("%s: send for %s to %s failed: %v", st.name, from, to, err)
		return "error {hoppie unreachable}"
	}
	return reply
}

// sendAs posts as the station, spaced from its other sends.
func (p *Proxy) sendAs(st *Station, to, kind, pkt string) (string, error) {
	st.sendMu.Lock()
	defer st.sendMu.Unlock()
	if wait := p.spacing - time.Since(st.lastSend); wait > 0 {
		time.Sleep(wait)
	}
	st.lastSend = time.Now()
	return p.up.post(p.up.logon, st.name, to, kind, pkt)
}

// flushAsync runs flush off the tick loop: its 2 s spacing per uplink must not stall polls,
// prune or saves. One flush per station at a time.
func (p *Proxy) flushAsync(st *Station) {
	if !st.flushing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer st.flushing.Store(false)
		p.flush(st)
	}()
}

// flush sends the station's own uplinks (notices, automatic answers, logoffs).
func (p *Proxy) flush(st *Station) {
	for _, u := range st.takeOutbox() {
		if reply, err := p.sendAs(st, u.to, u.kind, u.packet); err != nil || !strings.HasPrefix(reply, "ok") {
			log.Printf("%s: proxy uplink %s to %s failed: %v %s", st.name, u.packet, u.to, err, reply)
		}
	}
}

func (p *Proxy) poll(st *Station) {
	reply, err := p.up.post(p.up.logon, st.name, "SERVER", "poll", "")
	if err != nil {
		log.Printf("%s: poll failed: %v", st.name, err)
		st.upstream(false)
		return
	}
	messages, err := parsePoll(reply)
	if err != nil {
		// `callsign already in use`: someone polls this station outside the proxy.
		log.Printf("%s: poll refused: %v", st.name, err)
		st.upstream(false)
		return
	}
	gap, answered := st.upstream(true), 0
	stale, oldLogons := gap > st.staleAfter, gap > logonMaxAge
	for _, m := range messages {
		if stale && st.answerStale(m) {
			answered++
			continue
		}
		st.route(m, oldLogons)
	}
	if answered > 0 {
		log.Printf("%s: first poll after an outage, %d stale requests answered TEMPORARILY UNAVAILABLE", st.name, answered)
	}
}

func (p *Proxy) run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, st := range p.stations {
			if st.duePoll() {
				p.poll(st)
			}
			st.prune()
			p.flushAsync(st)
		}
		if err := p.save(); err != nil {
			log.Printf("state %s: %v", p.statePath, err)
		}
	}
}
