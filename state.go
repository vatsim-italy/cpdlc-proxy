package main

// Proxy state on disk, so a restart resumes where it was (docs/cpdlc.md §14.6).
// Inboxes, sector liveness and owner/next tables are left out: sectors resend them on every poll.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"time"
)

type savedState struct {
	Stations map[string]savedStation `json:"stations"`
}

type savedStation struct {
	LastPoll time.Time                         `json:"last_poll"` // last successful Hoppie poll
	Conns    map[string]savedConn              `json:"conns"`
	Counter  map[string]int                    `json:"counter"`
	Mins     map[string]map[string]savedOrigin `json:"mins"`
}

type savedConn struct {
	Authorized bool            `json:"authorized"`
	Parked     bool            `json:"parked"`
	HandedOver bool            `json:"handed_over"`
	ParkedAt   time.Time       `json:"parked_at"`
	InsideAt   time.Time       `json:"inside_at"`
	Touched    time.Time       `json:"touched"`
	Known      map[string]bool `json:"known"`
}

type savedOrigin struct {
	Sector string    `json:"sector"`
	Local  string    `json:"local"`
	At     time.Time `json:"at"`
}

func (st *Station) snapshot() savedStation {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := savedStation{LastPoll: st.pollOK, Conns: map[string]savedConn{}, Counter: map[string]int{}, Mins: map[string]map[string]savedOrigin{}}
	for a, c := range st.conns {
		s.Conns[a] = savedConn{c.authorized, c.parked, c.handedOver, c.parkedAt, c.insideAt, c.touched, c.known}
	}
	for a, n := range st.counter {
		s.Counter[a] = n
	}
	for a, sent := range st.mins {
		m := map[string]savedOrigin{}
		for min, o := range sent {
			m[min] = savedOrigin{o.sector, o.local, o.at}
		}
		s.Mins[a] = m
	}
	return s
}

func (st *Station) restore(s savedStation) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.pollOK = s.LastPoll
	for a, c := range s.Conns {
		if c.Known == nil {
			c.Known = map[string]bool{}
		}
		// ownerLostAt is not kept: the park grace starts again once sectors can poll. The 30 min
		// parked exit restarts too, so a long proxy outage logs nobody off before sectors report.
		st.conns[a] = &conn{authorized: c.Authorized, parked: c.Parked, handedOver: c.HandedOver, parkedAt: c.ParkedAt,
			insideAt: st.now(), touched: c.Touched, known: c.Known}
	}
	for a, n := range s.Counter {
		st.counter[a] = n
	}
	for a, sent := range s.Mins {
		m := map[string]origin{}
		for min, o := range sent {
			m[min] = origin{o.Sector, o.Local, o.At}
		}
		st.mins[a] = m
	}
}

// save writes the state atomically when it changed since the last write.
func (p *Proxy) save() error {
	if p.statePath == "" {
		return nil
	}
	s := savedState{Stations: map[string]savedStation{}}
	for name, st := range p.stations {
		s.Stations[name] = st.snapshot()
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if bytes.Equal(data, p.saved) {
		return nil
	}
	tmp := p.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.statePath); err != nil {
		return err
	}
	p.saved = data
	return nil
}

// load restores the state file; a missing file is an empty state. It returns the connections restored.
func (p *Proxy) load() (int, error) {
	if p.statePath == "" {
		return 0, nil
	}
	data, err := os.ReadFile(p.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var s savedState
	if err := json.Unmarshal(data, &s); err != nil {
		return 0, err
	}
	n := 0
	for name, saved := range s.Stations {
		if st := p.stations[name]; st != nil {
			st.restore(saved)
			n += len(saved.Conns)
		}
	}
	p.saved = data
	return n, nil
}
