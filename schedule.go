package main

import (
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"
)

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// Validate checks a schedule against the thermostat it's for.
func (sc *Schedule) Validate(t *Thermostat) error {
	sc.Name = strings.TrimSpace(sc.Name)
	if sc.Name == "" {
		sc.Name = "Change"
	}
	if len(sc.Name) > 64 {
		return fmt.Errorf("name must be at most 64 characters")
	}
	if sc.Date != "" {
		if _, err := time.ParseInLocation("2006-01-02", sc.Date, time.Local); err != nil {
			return fmt.Errorf("date must be YYYY-MM-DD")
		}
		sc.Days = 0
	} else if sc.Days <= 0 || sc.Days > 127 {
		return fmt.Errorf("pick at least one day")
	}
	if !hhmm.MatchString(sc.At) {
		return fmt.Errorf("time must be HH:MM (24-hour)")
	}
	z := t.zone(sc.ZoneID)
	if z == nil {
		return fmt.Errorf("unknown zone %q", sc.ZoneID)
	}
	sc.ZoneID = z.ID
	sc.Mode = strings.ToUpper(sc.Mode)
	if sc.Mode != "" && !slices.Contains(z.Modes, sc.Mode) {
		return fmt.Errorf("mode must be one of %s", strings.Join(z.Modes, ", "))
	}
	if sc.Hold != "" && sc.Hold != "hold" && sc.Hold != "schedule" {
		return fmt.Errorf("hold must be hold, schedule or empty")
	}
	if sc.Mode == "" && sc.Heat == nil && sc.Cool == nil && sc.Hold == "" {
		return fmt.Errorf("choose a mode, a setpoint or hold/schedule to change")
	}
	for _, p := range []**float64{&sc.Heat, &sc.Cool} {
		if *p != nil {
			v := t.roundTemp(**p)
			*p = &v
		}
	}
	if sc.Heat != nil || sc.Cool != nil {
		// Checked against the setpoint limits; the other setpoint can still
		// move when it runs, to keep the deadband
		probe := Zone{}
		if _, _, err := t.setpoints(&probe, sc.Heat, sc.Cool); err != nil {
			return err
		}
	}
	return nil
}

// lastTime is the latest time at or before now the schedule was set for;
// false when there's none in the past week (or a one-off is still to come).
func (sc *Schedule) lastTime(now time.Time) (time.Time, bool) {
	clock, err := time.Parse("15:04", sc.At)
	if err != nil {
		return time.Time{}, false
	}
	at := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, clock.Hour(), clock.Minute(), 0, 0, now.Location())
	}
	if sc.Date != "" {
		d, err := time.ParseInLocation("2006-01-02", sc.Date, now.Location())
		if err != nil {
			return time.Time{}, false
		}
		t := at(d.Year(), d.Month(), d.Day())
		return t, !t.After(now)
	}
	for back := 0; back <= 7; back++ {
		t := at(now.Year(), now.Month(), now.Day()-back)
		if !t.After(now) && sc.Days&(1<<int(t.Weekday())) != 0 {
			return t, true
		}
	}
	return time.Time{}, false
}

// Owed is the run the schedule still owes at now: its latest time, if it
// hasn't run since, wasn't added or edited after it, and is less than late
// ago (always allowing the minute itself).
func (sc *Schedule) Owed(now time.Time, late time.Duration) (time.Time, bool) {
	if !sc.Enabled {
		return time.Time{}, false
	}
	at, ok := sc.lastTime(now)
	if !ok || now.Sub(at) >= max(late, time.Minute) {
		return time.Time{}, false
	}
	if sc.LastRun != nil && !sc.LastRun.Before(at) || sc.ChangedAt.After(at) {
		return time.Time{}, false
	}
	return at, true
}

// commands is what a schedule sends, in order.
func (sc *Schedule) commands() []Command {
	var out []Command
	if sc.Mode != "" {
		out = append(out, Command{Action: "mode", Zone: sc.ZoneID, Value: sc.Mode})
	}
	if sc.Heat != nil || sc.Cool != nil {
		out = append(out, Command{Action: "setpoints", Zone: sc.ZoneID, Heat: sc.Heat, Cool: sc.Cool})
	}
	if sc.Hold != "" {
		out = append(out, Command{Action: sc.Hold, Zone: sc.ZoneID})
	}
	return out
}

// scheduler runs schedules from the Pi. A schedule that couldn't run on
// time, because the Pi was off or a change failed, still runs up to late
// afterwards: a failure is tried again after 1, 2, 4... minutes (at most
// 15), and missed ones run in the order they were due, so the thermostat
// ends up as the last of them meant. A late run is skipped if the thermostat
// was changed by hand, or by a later schedule, since it was due.
type scheduler struct {
	late   time.Duration
	send   func(by, thermostatID string, c Command) error
	settle func() // reads the thermostat back between changes
	mark   func(id int64, at time.Time) error
	byHand func(thermostatID string, since time.Time) (bool, error)
	note   func(CommandEntry)
	retry  map[int64]retryState // by schedule id
}

type retryState struct {
	due   time.Time // the run being retried
	fails int
	next  time.Time
}

func newScheduler(h *hvac) *scheduler {
	late := 360
	if cfg.Schedules.LateMinutes != nil {
		late = *cfg.Schedules.LateMinutes
	}
	return &scheduler{
		late:   time.Duration(late) * time.Minute,
		send:   h.Send,
		settle: h.refresh,
		mark:   db.MarkScheduleRun,
		byHand: db.ChangedByHandSince,
		note:   db.RecordCommand,
		retry:  map[int64]retryState{},
	}
}

// runScheduler checks the schedules every 20 seconds, in the Pi's local
// time. The first check, 20 seconds after starting, makes up missed runs.
func runScheduler(h *hvac, stop <-chan struct{}) {
	s := newScheduler(h)
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	purged := time.Time{}
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			if now.Sub(purged) > 24*time.Hour {
				db.Purge(cfg.Database.RetentionDays)
				purged = now
			}
			scheds, err := db.Schedules("")
			if err != nil {
				log.Printf("⚠️  could not read schedules: %v", err)
				continue
			}
			s.tick(now, scheds)
		}
	}
}

func (s *scheduler) tick(now time.Time, scheds []Schedule) {
	type job struct {
		sc  Schedule
		due time.Time
	}
	var jobs []job
	owed := map[int64]bool{}
	for _, sc := range scheds {
		due, ok := sc.Owed(now, s.late)
		if !ok {
			continue
		}
		owed[sc.ID] = true
		if r, ok := s.retry[sc.ID]; ok && r.due.Equal(due) && now.Before(r.next) {
			continue
		}
		jobs = append(jobs, job{sc, due})
	}
	for id := range s.retry {
		if !owed[id] {
			delete(s.retry, id) // ran, too late now, or deleted
		}
	}
	slices.SortStableFunc(jobs, func(a, b job) int { return a.due.Compare(b.due) })

	waiting := map[string]bool{} // thermostats with a failed run: later ones wait, to keep the order
	for _, j := range jobs {
		sc := j.sc
		if waiting[sc.ThermostatID] {
			continue
		}
		lateBy := now.Sub(j.due).Truncate(time.Minute)
		if lateBy > 0 {
			if why := s.skipReason(sc, j.due, scheds); why != "" {
				log.Printf("⏭️  skipped schedule %q due %s: %s", sc.Name, j.due.Format("Mon 15:04"), why)
				if err := s.mark(sc.ID, now); err != nil {
					log.Printf("⚠️  could not mark schedule %d: %v", sc.ID, err)
				}
				s.note(CommandEntry{User: "schedule: " + sc.Name, ThermostatID: sc.ThermostatID, Action: "schedule",
					Detail: fmt.Sprintf("skipped the %s run: %s", j.due.Format("Mon 15:04"), why), OK: true})
				continue
			}
		}
		if err := s.run(sc, now, lateBy); err != nil {
			r := s.retry[sc.ID]
			if !r.due.Equal(j.due) {
				r = retryState{due: j.due}
			}
			r.fails++
			r.next = now.Add(min(time.Minute<<(r.fails-1), 15*time.Minute))
			s.retry[sc.ID] = r
			waiting[sc.ThermostatID] = true
			if r.next.Sub(j.due) < max(s.late, time.Minute) {
				log.Printf("⏰ schedule %q failed (%v); trying again at %s", sc.Name, err, r.next.Format("15:04"))
			} else {
				log.Printf("⏰ schedule %q failed (%v); giving up, it's too late now", sc.Name, err)
			}
			continue
		}
		delete(s.retry, sc.ID)
	}
}

// skipReason is why a late run shouldn't happen now; "" to run it.
func (s *scheduler) skipReason(sc Schedule, due time.Time, scheds []Schedule) string {
	for _, o := range scheds {
		if o.ID != sc.ID && o.ThermostatID == sc.ThermostatID && o.ZoneID == sc.ZoneID &&
			o.LastRun != nil && o.LastRun.After(due) {
			return fmt.Sprintf("the later schedule %q has run since", o.Name)
		}
	}
	changed, err := s.byHand(sc.ThermostatID, due)
	if err != nil {
		log.Printf("⚠️  could not check the change log: %v", err)
	}
	if changed {
		return "the thermostat was changed by hand since"
	}
	return ""
}

func (s *scheduler) run(sc Schedule, now time.Time, lateBy time.Duration) error {
	by := "schedule: " + sc.Name
	if lateBy > 0 {
		by += ", " + strings.TrimSuffix(lateBy.String(), "0s") + " late"
	}
	for _, c := range sc.commands() {
		// Each change waits for the last to show, so the next is planned
		// against the thermostat's new state
		if err := s.send(by, sc.ThermostatID, c); err != nil {
			return err
		}
		s.settle()
	}
	if err := s.mark(sc.ID, now); err != nil {
		return fmt.Errorf("sent, but could not mark it as run: %w", err)
	}
	log.Printf("⏰ ran schedule %q%s", sc.Name, strings.TrimPrefix(by, "schedule: "+sc.Name))
	return nil
}

// describeSchedule summarises a schedule for the command log.
func describeSchedule(sc Schedule) string {
	var days []string
	for d := time.Sunday; d <= time.Saturday; d++ {
		if sc.Days&(1<<int(d)) != 0 {
			days = append(days, d.String()[:3])
		}
	}
	when := strings.Join(days, " ")
	if sc.Date != "" {
		when = "once on " + sc.Date
	}
	var what []string
	if sc.Mode != "" {
		what = append(what, humanize(strings.ToLower(sc.Mode)))
	}
	if sc.Heat != nil {
		what = append(what, fmt.Sprintf("heat %g°", *sc.Heat))
	}
	if sc.Cool != nil {
		what = append(what, fmt.Sprintf("cool %g°", *sc.Cool))
	}
	switch sc.Hold {
	case "hold":
		what = append(what, "hold")
	case "schedule":
		what = append(what, "follow schedule")
	}
	s := fmt.Sprintf("%s: %s at %s, %s", sc.Name, when, sc.At, strings.Join(what, ", "))
	if !sc.Enabled {
		s += " (off)"
	}
	return s
}

// ---- Run time ----

// DayRuntime is how long the system heated and cooled on one local day.
type DayRuntime struct {
	Date    string  `json:"date"`    // YYYY-MM-DD
	Heating float64 `json:"heating"` // minutes
	Cooling float64 `json:"cooling"`
}

// runtimes adds up heating and cooling time per local day from readings
// (oldest first). Each reading's state is taken to last until the next one,
// but no longer than maxGap, so an outage isn't counted as running.
func runtimes(rs []Reading, from time.Time, days int, maxGap time.Duration) []DayRuntime {
	out := make([]DayRuntime, days)
	index := map[string]int{}
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location())
	for i := range out {
		out[i].Date = start.AddDate(0, 0, i).Format("2006-01-02")
		index[out[i].Date] = i
	}
	for i, r := range rs {
		if r.RunState != "heating" && r.RunState != "cooling" {
			continue
		}
		end := r.At.Add(maxGap)
		if i+1 < len(rs) && rs[i+1].At.Before(end) {
			end = rs[i+1].At
		}
		// Split across midnight
		for t := r.At; t.Before(end); {
			local := t.In(from.Location())
			next := time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, from.Location())
			if next.After(end) {
				next = end
			}
			if d, ok := index[local.Format("2006-01-02")]; ok {
				if r.RunState == "heating" {
					out[d].Heating += next.Sub(t).Minutes()
				} else {
					out[d].Cooling += next.Sub(t).Minutes()
				}
			}
			t = next
		}
	}
	return out
}
