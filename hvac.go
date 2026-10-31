package main

import (
	"errors"
	"log"
	"sync"
	"time"
)

// hvac keeps the latest view of the thermostats, read from the cloud every
// poll, and sends changes.
type hvac struct {
	client *nexiaClient
	poll   time.Duration
	record func([]Thermostat, time.Time) // saves a reading; nil = no database

	mu        sync.RWMutex
	stats     []Thermostat
	updated   time.Time // last successful read
	lastError string
	wake      chan struct{}
}

var errNotReady = errors.New("the thermostat hasn't been read from American Standard Home yet")

func newHVAC(c *nexiaClient, poll time.Duration) *hvac {
	return &hvac{client: c, poll: poll, wake: make(chan struct{}, 1)}
}

func (h *hvac) Run(stop <-chan struct{}) {
	h.refresh()
	t := time.NewTicker(h.poll)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-h.wake:
			// Changes take a few seconds to show in the cloud
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Second):
			}
		}
		h.refresh()
	}
}

func (h *hvac) refresh() {
	devs, changed, err := h.client.house()
	now := time.Now()
	h.mu.Lock()
	if err != nil {
		if h.lastError != err.Error() {
			log.Printf("⚠️  could not read the thermostats: %v", err)
		}
		h.lastError = err.Error()
		h.mu.Unlock()
		return
	}
	if h.lastError != "" {
		log.Printf("✅ reading the thermostats again")
	}
	h.lastError = ""
	if changed || h.stats == nil {
		h.stats = parseThermostats(devs)
	}
	h.updated = now
	ts := h.stats
	h.mu.Unlock()
	// Every poll is saved, changed or not: run time is worked out from them
	if h.record != nil {
		h.record(ts, now)
	}
}

// Snapshot is what the page shows.
type Snapshot struct {
	Thermostats []Thermostat `json:"thermostats"`
	Updated     *time.Time   `json:"updated,omitempty"`
	Error       string       `json:"error,omitempty"`
	PollSeconds int          `json:"poll_seconds"`
}

func (h *hvac) Snapshot() Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s := Snapshot{Thermostats: h.stats, Error: h.lastError, PollSeconds: int(h.poll.Seconds())}
	if s.Thermostats == nil {
		s.Thermostats = []Thermostat{}
	}
	if !h.updated.IsZero() {
		u := h.updated
		s.Updated = &u
	}
	return s
}

func (h *hvac) thermostat(id string) (Thermostat, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, t := range h.stats {
		if t.ID == id {
			return t, true
		}
	}
	return Thermostat{}, false
}

// Send makes a change and logs who made it.
func (h *hvac) Send(by, thermostatID string, c Command) error {
	t, ok := h.thermostat(thermostatID)
	if !ok {
		return errNotReady
	}
	req, err := t.plan(h.client.mobile, c)
	if err != nil {
		return err
	}
	if !req.noop {
		err = h.client.post(req.url, req.method, req.payload)
	}
	entry := CommandEntry{User: by, ThermostatID: t.ID, Action: c.Action, Detail: req.detail, OK: err == nil}
	if err != nil {
		entry.Error = err.Error()
		log.Printf("❌ %s: %s by %s failed: %v", t.Name, req.detail, by, err)
	} else {
		log.Printf("▶️  %s: %s by %s", t.Name, req.detail, by)
		select {
		case h.wake <- struct{}{}:
		default:
		}
	}
	db.RecordCommand(entry)
	return err
}
