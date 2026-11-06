package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Reading is one zone at one poll.
type Reading struct {
	At       time.Time `json:"at"`
	ZoneID   string    `json:"zone"`
	Temp     *float64  `json:"temp,omitempty"`
	Heat     *float64  `json:"heat,omitempty"`
	Cool     *float64  `json:"cool,omitempty"`
	Mode     string    `json:"mode"`
	RunState string    `json:"run_state"`
	Humidity *float64  `json:"humidity,omitempty"`
	Outdoor  *float64  `json:"outdoor,omitempty"`
}

// CommandEntry is one change, by a person or a schedule.
type CommandEntry struct {
	At           time.Time `json:"at"`
	User         string    `json:"user"`
	ThermostatID string    `json:"thermostat"`
	Action       string    `json:"action"`
	Detail       string    `json:"detail,omitempty"`
	OK           bool      `json:"ok"`
	Error        string    `json:"error,omitempty"`
}

// Schedule changes a zone at a set local time on chosen weekdays.
type Schedule struct {
	ID           int64      `json:"id"`
	ThermostatID string     `json:"thermostat"`
	ZoneID       string     `json:"zone"`
	Name         string     `json:"name"`
	Days         int        `json:"days"` // bit per time.Weekday: 1 = Sunday ... 64 = Saturday
	Date         string     `json:"date"` // "YYYY-MM-DD" for a one-off, which ignores Days
	At           string     `json:"at"`   // "HH:MM", Pi local time
	Mode         string     `json:"mode"` // "" = leave as is
	Heat         *float64   `json:"heat,omitempty"`
	Cool         *float64   `json:"cool,omitempty"`
	Hold         string     `json:"hold"` // "", "hold" or "schedule"
	Enabled      bool       `json:"enabled"`
	CreatedBy    string     `json:"created_by"`
	LastRun      *time.Time `json:"last_run,omitempty"`
	// ChangedAt is when it was added or last edited: a run it missed before
	// then isn't made up
	ChangedAt time.Time `json:"-"`
}

var errNotFound = errors.New("not found")

// store keeps readings, the command log and schedules in MariaDB. Record
// methods on a nil *store (database disabled) do nothing.
type store struct {
	db *sql.DB
}

var db *store

func openStore(c DatabaseConfig) (*store, error) {
	mc := mysql.NewConfig()
	mc.User = c.User
	mc.Passwd = c.Password
	mc.Net = "tcp"
	mc.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
	mc.DBName = c.Name
	mc.ParseTime = true
	mc.Loc = time.UTC
	mc.Params = map[string]string{"time_zone": "'+00:00'"}
	// Report rows matched rather than changed, so saving a schedule unchanged
	// isn't mistaken for it not existing
	mc.ClientFoundRows = true

	conn, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(4)
	conn.SetConnMaxLifetime(30 * time.Minute)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect to %s as %s: %w", mc.Addr, c.User, err)
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS readings (
			id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			thermostat_id VARCHAR(64) NOT NULL,
			zone_id       VARCHAR(64) NOT NULL,
			read_at       DATETIME NOT NULL,
			temp          DECIMAL(5,1) NULL,
			heat_sp       DECIMAL(5,1) NULL,
			cool_sp       DECIMAL(5,1) NULL,
			mode          VARCHAR(8) NOT NULL DEFAULT '',
			run_state     VARCHAR(8) NOT NULL DEFAULT '',
			humidity      DECIMAL(4,1) NULL,
			outdoor       DECIMAL(5,1) NULL,
			KEY idx_thermostat_read (thermostat_id, read_at)
		) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS command_log (
			id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			created_at    DATETIME NOT NULL,
			user          VARCHAR(96)  NOT NULL,
			thermostat_id VARCHAR(64)  NOT NULL,
			action        VARCHAR(32)  NOT NULL,
			detail        VARCHAR(255) NOT NULL DEFAULT '',
			ok            BOOLEAN NOT NULL,
			error         VARCHAR(512) NOT NULL DEFAULT '',
			KEY idx_thermostat_created (thermostat_id, created_at)
		) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS schedules (
			id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			thermostat_id VARCHAR(64) NOT NULL,
			zone_id       VARCHAR(64) NOT NULL,
			name          VARCHAR(64) NOT NULL,
			days          TINYINT UNSIGNED NOT NULL,
			run_date      CHAR(10) NOT NULL DEFAULT '',
			at_time       CHAR(5) NOT NULL,
			mode          VARCHAR(8) NOT NULL DEFAULT '',
			heat_sp       DECIMAL(5,1) NULL,
			cool_sp       DECIMAL(5,1) NULL,
			hold          VARCHAR(8) NOT NULL DEFAULT '',
			enabled       BOOLEAN NOT NULL DEFAULT TRUE,
			created_by    VARCHAR(64) NOT NULL,
			last_run_at   DATETIME NULL,
			changed_at    DATETIME NULL,
			KEY idx_thermostat (thermostat_id)
		) ENGINE=InnoDB`,
		// Added after the first release
		`ALTER TABLE schedules ADD COLUMN IF NOT EXISTS changed_at DATETIME NULL`,
		// Schedules from before then count as changed now, so none is made up
		`UPDATE schedules SET changed_at = UTC_TIMESTAMP() WHERE changed_at IS NULL`,
	} {
		if _, err := conn.Exec(ddl); err != nil {
			conn.Close()
			return nil, fmt.Errorf("create tables: %w", err)
		}
	}
	return &store{db: conn}, nil
}

// RecordReadings saves one reading per zone.
func (s *store) RecordReadings(ts []Thermostat, at time.Time) {
	if s == nil {
		return
	}
	for _, t := range ts {
		if !t.Online {
			continue
		}
		for _, z := range t.Zones {
			if _, err := s.db.Exec(`INSERT INTO readings (thermostat_id, zone_id, read_at, temp, heat_sp, cool_sp,
					mode, run_state, humidity, outdoor) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				t.ID, z.ID, at.UTC(), z.Temp, z.Heat, z.Cool, z.Mode, t.RunState, t.Humidity, t.Outdoor); err != nil {
				log.Printf("⚠️  could not save reading: %v", err)
				return
			}
		}
	}
}

// Readings returns a zone's readings since a time, oldest first.
func (s *store) Readings(thermostatID, zoneID string, since time.Time) ([]Reading, error) {
	rows, err := s.db.Query(`SELECT read_at, zone_id, temp, heat_sp, cool_sp, mode, run_state, humidity, outdoor
		FROM readings WHERE thermostat_id = ? AND zone_id = ? AND read_at >= ? ORDER BY read_at`,
		thermostatID, zoneID, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reading{}
	for rows.Next() {
		var r Reading
		var temp, heat, cool, hum, outd sql.NullFloat64
		if err := rows.Scan(&r.At, &r.ZoneID, &temp, &heat, &cool, &r.Mode, &r.RunState, &hum, &outd); err != nil {
			return nil, err
		}
		r.Temp, r.Heat, r.Cool, r.Humidity, r.Outdoor = nullFloat(temp), nullFloat(heat), nullFloat(cool), nullFloat(hum), nullFloat(outd)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Purge deletes readings older than the retention.
func (s *store) Purge(days int) {
	res, err := s.db.Exec(`DELETE FROM readings WHERE read_at < ?`, time.Now().UTC().AddDate(0, 0, -days))
	if err != nil {
		log.Printf("⚠️  could not purge old readings: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("🧹 purged %d readings older than %d days", n, days)
	}
}

// RecordCommand saves e. A failure is logged but never fails the request: the
// change has already been sent.
func (s *store) RecordCommand(e CommandEntry) {
	if s == nil {
		return
	}
	if _, err := s.db.Exec(`INSERT INTO command_log (created_at, user, thermostat_id, action, detail, ok, error)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC(), truncate(e.User, 96), truncate(e.ThermostatID, 64), truncate(e.Action, 32),
		truncate(e.Detail, 255), e.OK, truncate(e.Error, 512)); err != nil {
		log.Printf("⚠️  could not write command log: %v", err)
	}
}

func (s *store) RecentCommands(thermostatID string, n int) ([]CommandEntry, error) {
	rows, err := s.db.Query(`SELECT created_at, user, thermostat_id, action, detail, ok, error
		FROM command_log WHERE thermostat_id = ? ORDER BY id DESC LIMIT ?`, thermostatID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CommandEntry{}
	for rows.Next() {
		var e CommandEntry
		if err := rows.Scan(&e.At, &e.User, &e.ThermostatID, &e.Action, &e.Detail, &e.OK, &e.Error); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Schedules returns every schedule, or one thermostat's when thermostatID is set.
func (s *store) Schedules(thermostatID string) ([]Schedule, error) {
	q := `SELECT id, thermostat_id, zone_id, name, days, run_date, at_time, mode, heat_sp, cool_sp, hold,
		enabled, created_by, last_run_at, changed_at FROM schedules`
	args := []any{}
	if thermostatID != "" {
		q += ` WHERE thermostat_id = ?`
		args = append(args, thermostatID)
	}
	rows, err := s.db.Query(q+` ORDER BY at_time, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		var sc Schedule
		var heat, cool sql.NullFloat64
		var last, changed sql.NullTime
		if err := rows.Scan(&sc.ID, &sc.ThermostatID, &sc.ZoneID, &sc.Name, &sc.Days, &sc.Date, &sc.At, &sc.Mode,
			&heat, &cool, &sc.Hold, &sc.Enabled, &sc.CreatedBy, &last, &changed); err != nil {
			return nil, err
		}
		sc.Heat, sc.Cool = nullFloat(heat), nullFloat(cool)
		if last.Valid {
			sc.LastRun = &last.Time
		}
		sc.ChangedAt = changed.Time
		out = append(out, sc)
	}
	return out, rows.Err()
}

func (s *store) Schedule(id int64) (Schedule, error) {
	all, err := s.Schedules("")
	if err != nil {
		return Schedule{}, err
	}
	for _, sc := range all {
		if sc.ID == id {
			return sc, nil
		}
	}
	return Schedule{}, errNotFound
}

func (s *store) AddSchedule(sc Schedule) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO schedules (thermostat_id, zone_id, name, days, run_date, at_time, mode,
			heat_sp, cool_sp, hold, enabled, created_by, changed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, UTC_TIMESTAMP())`,
		sc.ThermostatID, sc.ZoneID, sc.Name, sc.Days, sc.Date, sc.At, sc.Mode, sc.Heat, sc.Cool, sc.Hold, sc.Enabled,
		truncate(sc.CreatedBy, 64))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *store) UpdateSchedule(sc Schedule) error {
	res, err := s.db.Exec(`UPDATE schedules SET zone_id = ?, name = ?, days = ?, run_date = ?, at_time = ?, mode = ?,
		heat_sp = ?, cool_sp = ?, hold = ?, enabled = ?, changed_at = UTC_TIMESTAMP() WHERE id = ?`,
		sc.ZoneID, sc.Name, sc.Days, sc.Date, sc.At, sc.Mode, sc.Heat, sc.Cool, sc.Hold, sc.Enabled, sc.ID)
	return checkAffected(res, err)
}

func (s *store) DeleteSchedule(id int64) error {
	res, err := s.db.Exec(`DELETE FROM schedules WHERE id = ?`, id)
	return checkAffected(res, err)
}

func (s *store) MarkScheduleRun(id int64, at time.Time) error {
	_, err := s.db.Exec(`UPDATE schedules SET last_run_at = ? WHERE id = ?`, at.UTC(), id)
	return err
}

// ChangedByHandSince reports whether someone (not a schedule) changed the
// thermostat through MyThermostat after t.
func (s *store) ChangedByHandSince(thermostatID string, t time.Time) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM command_log WHERE thermostat_id = ? AND created_at > ? AND ok
		AND user NOT LIKE 'schedule:%'`, thermostatID, t.UTC()).Scan(&n)
	return n > 0, err
}

func checkAffected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return errNotFound
	}
	return nil
}

func nullFloat(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
