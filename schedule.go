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

// Due reports whether the schedule should run in the minute that contains now.
func (sc *Schedule) Due(now time.Time) bool {
	if !sc.Enabled || now.Format("15:04") != sc.At {
		return false
	}
	if sc.Date != "" {
		if now.Format("2006-01-02") != sc.Date {
			return false
		}
	} else if sc.Days&(1<<int(now.Weekday())) == 0 {
		return false
	}
	minute := now.Truncate(time.Minute)
	return sc.LastRun == nil || sc.LastRun.Before(minute)
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

// runScheduler checks the schedules every 20 seconds, in the Pi's local time.
func runScheduler(h *hvac, stop <-chan struct{}) {
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
			for _, sc := range scheds {
				if sc.Due(now) {
					runSchedule(h, sc, now)
				}
			}
		}
	}
}

func runSchedule(h *hvac, sc Schedule, now time.Time) {
	// Marked first so a failure isn't retried every tick for the rest of the minute
	if err := db.MarkScheduleRun(sc.ID, now); err != nil {
		log.Printf("⚠️  could not mark schedule %d as run: %v", sc.ID, err)
		return
	}
	by := "schedule: " + sc.Name
	for _, c := range sc.commands() {
		// Each change waits for the last to show, so the next is planned
		// against the thermostat's new state
		if err := h.Send(by, sc.ThermostatID, c); err != nil {
			return
		}
		h.refresh()
	}
	log.Printf("⏰ ran schedule %q", sc.Name)
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
