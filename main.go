package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

var (
	cfg    *Config
	thermo *hvac
)

func main() {
	configPath := flag.String("config", "config.yaml", "config file")
	check := flag.Bool("check", false, "sign in, print what the thermostats report, and exit")
	dump := flag.Bool("dump", false, "sign in, print the raw device JSON from the cloud, and exit")
	history := flag.Bool("history", false, "sign in, print each thermostat's daily and monthly run-time history JSON, and exit")
	flag.Parse()

	var err error
	cfg, err = LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	client := newNexiaClient(cfg.Account)
	if *history {
		runHistoryCheck(client)
		return
	}
	if *check || *dump {
		runCheck(client, *dump)
		return
	}

	thermo = newHVAC(client, time.Duration(cfg.PollSeconds)*time.Second)
	if cfg.Database.Enabled {
		db, err = openStore(cfg.Database)
		if err != nil {
			log.Fatalf("Failed to open database: %v", err)
		}
		thermo.record = db.RecordReadings
		log.Printf("🗄️  Readings and schedules in MariaDB database %s", cfg.Database.Name)
	} else {
		log.Printf("Database disabled: no history, and schedules are off")
	}

	stop := make(chan struct{})
	go thermo.Run(stop)
	if db != nil {
		go runScheduler(thermo, stop)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Bind, cfg.Server.Port)
	log.Printf("🌡️  MyThermostat starting on http://%s (reading every %ds)", addr, cfg.PollSeconds)
	if cfg.Server.AllowDirect {
		log.Printf("⚠️  allow_direct is on: requests that bypass the hub are treated as admin")
	}
	log.Fatal(http.ListenAndServe(addr, newMux()))
}

// Admins and users may change the thermostat; which of them see it at all is
// up to the service's allowed_roles in the hub.
func newMux() *http.ServeMux {
	member := func(h http.HandlerFunc) http.HandlerFunc { return requireRole(h, "admin", "user") }
	files := http.StripPrefix("/static/", http.FileServer(http.Dir("static")))
	// no-cache makes the browser check for a newer app.js on every load (a
	// cheap 304 when unchanged), so an update never runs with the old script
	static := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /hvac/health", handleHealth)

	mux.HandleFunc("GET /", member(handleIndex))
	mux.HandleFunc("GET /static/", member(static))
	mux.HandleFunc("GET /api/me", member(handleMe))
	mux.HandleFunc("GET /api/thermostats", member(handleThermostats))
	mux.HandleFunc("POST /api/thermostats/{id}/command", member(handleCommand))
	mux.HandleFunc("GET /api/thermostats/{id}/history", member(handleHistory))
	mux.HandleFunc("GET /api/thermostats/{id}/runtime", member(handleRuntime))
	mux.HandleFunc("GET /api/thermostats/{id}/commands", member(handleCommands))
	mux.HandleFunc("GET /api/thermostats/{id}/schedules", member(handleListSchedules))
	mux.HandleFunc("POST /api/thermostats/{id}/schedules", member(handleAddSchedule))
	mux.HandleFunc("PUT /api/schedules/{sid}", member(handleUpdateSchedule))
	mux.HandleFunc("DELETE /api/schedules/{sid}", member(handleDeleteSchedule))
	return mux
}

type ctxKey string

// requireRole trusts the X-Hub-* headers set by MyHomeServicesHub. That is only
// safe because this service binds to 127.0.0.1 by default, so only the hub can
// reach it.
func requireRole(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, role := r.Header.Get("X-Hub-User"), r.Header.Get("X-Hub-Role")
		if r.Header.Get("X-Hub-Authenticated") != "true" {
			if !cfg.Server.AllowDirect {
				writeError(w, http.StatusUnauthorized, "open this page through MyHomeServicesHub")
				return
			}
			user, role = "direct", "admin"
		}
		allowed := false
		for _, r := range roles {
			allowed = allowed || r == role
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "your role ("+role+") cannot do this")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey("user"), user)
		ctx = context.WithValue(ctx, ctxKey("role"), role)
		next(w, r.WithContext(ctx))
	}
}

func currentUser(r *http.Request) string {
	return r.Context().Value(ctxKey("user")).(string)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	content, err := os.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "Failed to load page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"user": currentUser(r),
		"role": r.Context().Value(ctxKey("role")).(string),
	})
}

func handleThermostats(w http.ResponseWriter, r *http.Request) {
	s := thermo.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": s, "database": db != nil})
}

func findThermostat(w http.ResponseWriter, id string) (Thermostat, bool) {
	t, ok := thermo.thermostat(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown thermostat "+id)
	}
	return t, ok
}

func handleCommand(w http.ResponseWriter, r *http.Request) {
	var c Command
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	t, ok := findThermostat(w, r.PathValue("id"))
	if !ok {
		return
	}
	if _, err := t.plan(thermo.client.mobile, c); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := thermo.Send(currentUser(r), t.ID, c); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func requireDB(w http.ResponseWriter) bool {
	if db == nil {
		writeError(w, http.StatusServiceUnavailable, "this needs the database (database.enabled in config.yaml)")
		return false
	}
	return true
}

// runtimeDays is how many days the run time chart shows.
const runtimeDays = 14

func handleHistory(w http.ResponseWriter, r *http.Request) {
	t, ok := findThermostat(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	z := t.zone(r.URL.Query().Get("zone"))
	if z == nil {
		writeError(w, http.StatusNotFound, "unknown zone")
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 24*31 {
		hours = 24
	}
	now := time.Now()
	from := now.AddDate(0, 0, -(runtimeDays - 1))
	day0 := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.Local)
	since := now.Add(-time.Duration(hours) * time.Hour)
	first := day0
	if since.Before(first) {
		first = since
	}
	all, err := db.Readings(t.ID, z.ID, first)
	if err != nil {
		log.Printf("History query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read history")
		return
	}
	recent := []Reading{}
	for _, rd := range all {
		if !rd.At.Before(since) {
			recent = append(recent, rd)
		}
	}
	maxGap := 2 * time.Duration(cfg.PollSeconds) * time.Second
	writeJSON(w, http.StatusOK, map[string]any{
		"readings": recent,
		"runtime":  runtimes(all, day0, runtimeDays, maxGap),
	})
}

// handleRuntime passes on the thermostat's own run-time history from the
// cloud. Unlike the history above it needs no database.
func handleRuntime(w http.ResponseWriter, r *http.Request) {
	t, ok := findThermostat(w, r.PathValue("id"))
	if !ok {
		return
	}
	period := r.URL.Query().Get("period")
	if period != "monthly" {
		period = "daily"
	}
	data, err := thermo.RuntimeHistory(t.ID, period)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func handleCommands(w http.ResponseWriter, r *http.Request) {
	t, ok := findThermostat(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	cmds, err := db.RecentCommands(t.ID, 50)
	if err != nil {
		log.Printf("Command log query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read the command log")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": cmds})
}

func handleListSchedules(w http.ResponseWriter, r *http.Request) {
	t, ok := findThermostat(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	scheds, err := db.Schedules(t.ID)
	if err != nil {
		log.Printf("Schedule query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read schedules")
		return
	}
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{"schedules": scheds,
		"timezone": now.Format("MST"), "today": now.Format("2006-01-02")})
}

func decodeSchedule(w http.ResponseWriter, r *http.Request, t *Thermostat) (*Schedule, bool) {
	var sc Schedule
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&sc); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return nil, false
	}
	if err := sc.Validate(t); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return &sc, true
}

func handleAddSchedule(w http.ResponseWriter, r *http.Request) {
	t, ok := findThermostat(w, r.PathValue("id"))
	if !ok || !requireDB(w) {
		return
	}
	sc, ok := decodeSchedule(w, r, &t)
	if !ok {
		return
	}
	sc.ThermostatID, sc.CreatedBy = t.ID, currentUser(r)
	id, err := db.AddSchedule(*sc)
	if err != nil {
		log.Printf("Failed to add schedule: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save the schedule")
		return
	}
	sc.ID = id
	db.RecordCommand(CommandEntry{User: sc.CreatedBy, ThermostatID: t.ID, Action: "schedule_add", Detail: describeSchedule(*sc), OK: true})
	writeJSON(w, http.StatusOK, sc)
}

// scheduleFromPath loads the schedule named in the URL.
func scheduleFromPath(w http.ResponseWriter, r *http.Request) (Schedule, Thermostat, bool) {
	id, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad schedule id")
		return Schedule{}, Thermostat{}, false
	}
	sc, err := db.Schedule(id)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "no such schedule")
		return sc, Thermostat{}, false
	}
	if err != nil {
		log.Printf("Schedule query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read schedules")
		return sc, Thermostat{}, false
	}
	t, ok := findThermostat(w, sc.ThermostatID)
	return sc, t, ok
}

func handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	if !requireDB(w) {
		return
	}
	old, t, ok := scheduleFromPath(w, r)
	if !ok {
		return
	}
	sc, ok := decodeSchedule(w, r, &t)
	if !ok {
		return
	}
	sc.ID, sc.ThermostatID = old.ID, old.ThermostatID
	if err := db.UpdateSchedule(*sc); err != nil {
		log.Printf("Failed to update schedule: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save the schedule")
		return
	}
	db.RecordCommand(CommandEntry{User: currentUser(r), ThermostatID: t.ID, Action: "schedule_edit", Detail: describeSchedule(*sc), OK: true})
	writeJSON(w, http.StatusOK, sc)
}

func handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if !requireDB(w) {
		return
	}
	sc, t, ok := scheduleFromPath(w, r)
	if !ok {
		return
	}
	if err := db.DeleteSchedule(sc.ID); err != nil {
		log.Printf("Failed to delete schedule: %v", err)
		writeError(w, http.StatusInternalServerError, "could not delete the schedule")
		return
	}
	db.RecordCommand(CommandEntry{User: currentUser(r), ThermostatID: t.ID, Action: "schedule_delete", Detail: describeSchedule(sc), OK: true})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// runCheck signs in and prints what the cloud reports, to set up and to
// see what a thermostat offers. With raw, it prints the device JSON as is.
func runCheck(c *nexiaClient, raw bool) {
	fmt.Printf("Signing in to %s as %s...\n", brandRoots[cfg.Account.Brand], cfg.Account.Username)
	devs, _, err := c.house()
	if err != nil {
		log.Fatal(err)
	}
	if raw {
		out, _ := json.MarshalIndent(devs, "", "  ")
		fmt.Println(string(out))
		fmt.Fprintln(os.Stderr, "(This is your home's device data: share it only with people you trust.)")
		return
	}
	ts := parseThermostats(devs)
	if len(ts) == 0 {
		fmt.Printf("Signed in, but found no thermostats with zones among %d device(s). Run with -dump to see them.\n", len(devs))
		return
	}
	for _, t := range ts {
		fmt.Printf("\n%s (id %s) model %q firmware %q, °%s\n", t.Name, t.ID, t.Model, t.Firmware, t.Unit)
		fmt.Printf("  status %q (%s)", t.Status, t.RunState)
		if t.Humidity != nil {
			fmt.Printf(", humidity %g%%", *t.Humidity)
		}
		if t.Outdoor != nil {
			fmt.Printf(", outdoor %g°", *t.Outdoor)
		}
		fmt.Printf("\n  setpoint limits %g°–%g°, deadband %g°\n", t.HeatMin, t.CoolMax, t.Deadband)
		for _, z := range t.Zones {
			fmt.Printf("  zone %q (id %s): %s°, mode %s %v, heat %s°, cool %s°, %s, run mode %q\n",
				z.Name, z.ID, f(z.Temp), z.Mode, z.Modes, f(z.Heat), f(z.Cool), z.Status, z.RunMode)
			printSettings("    ", z.Settings)
		}
		printSettings("  ", t.Settings)
	}
}

func runHistoryCheck(c *nexiaClient) {
	devs, _, err := c.house()
	if err != nil {
		log.Fatal(err)
	}
	ts := parseThermostats(devs)
	if len(ts) == 0 {
		log.Fatal("no thermostats found; run with -check")
	}
	for _, t := range ts {
		for _, period := range []string{"daily", "monthly"} {
			fmt.Printf("\n=== %s (id %s) %s ===\n", t.Name, t.ID, period)
			m, err := c.runtimeHistory(t.ID, period)
			if err != nil {
				fmt.Println(err)
				continue
			}
			out, _ := json.MarshalIndent(m, "", "  ")
			fmt.Println(string(out))
			fmt.Println("(read from " + c.historyBase + ")")
		}
	}
}

func printSettings(indent string, ss []Setting) {
	for _, s := range ss {
		fmt.Printf("%ssetting %s (%q) = %q, options:", indent, s.Type, s.Title, s.Value)
		for _, o := range s.Options {
			fmt.Printf(" %s=%q", o.Value, o.Label)
		}
		fmt.Println()
	}
}

func f(p *float64) string {
	if p == nil {
		return "?"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
