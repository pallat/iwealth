package store

import (
	"context"
	"database/sql"
	"time"

	_ "modernc.org/sqlite"

	"github.com/pallat/iwealth/internal/config"
	"github.com/pallat/iwealth/internal/dip"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS nav (fund TEXT NOT NULL, date TEXT NOT NULL, nav REAL NOT NULL, PRIMARY KEY (fund, date));
CREATE TABLE IF NOT EXISTS alert_state (fund TEXT PRIMARY KEY, level REAL NOT NULL);
CREATE TABLE IF NOT EXISTS tax_paid (id INTEGER PRIMARY KEY AUTOINCREMENT, fund TEXT NOT NULL, type TEXT NOT NULL, amount REAL NOT NULL, paid_on TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS deduction_entry (year INTEGER NOT NULL, key TEXT NOT NULL, amount REAL NOT NULL, PRIMARY KEY (year, key));
CREATE TABLE IF NOT EXISTS year_end_reminder (day TEXT PRIMARY KEY);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; also keeps :memory: on one connection
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const dayFmt = "2006-01-02"

func (s *Store) UpsertNAV(ctx context.Context, fund string, date time.Time, nav float64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO nav(fund,date,nav) VALUES(?,?,?)
		ON CONFLICT(fund,date) DO UPDATE SET nav=excluded.nav`, fund, date.Format(dayFmt), nav)
	return err
}

func (s *Store) HasNAV(ctx context.Context, fund string, date time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nav WHERE fund=? AND date=?`, fund, date.Format(dayFmt)).Scan(&n)
	return n > 0, err
}

func (s *Store) History(ctx context.Context, fund string) ([]dip.Point, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT date, nav FROM nav WHERE fund=? ORDER BY date`, fund)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dip.Point
	for rows.Next() {
		var ds string
		var p dip.Point
		if err := rows.Scan(&ds, &p.NAV); err != nil {
			return nil, err
		}
		if p.Date, err = time.Parse(dayFmt, ds); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) LastLevel(ctx context.Context, fund string) (float64, error) {
	var l float64
	err := s.db.QueryRowContext(ctx, `SELECT level FROM alert_state WHERE fund=?`, fund).Scan(&l)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return l, err
}

func (s *Store) SetLevel(ctx context.Context, fund string, level float64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO alert_state(fund,level) VALUES(?,?)
		ON CONFLICT(fund) DO UPDATE SET level=excluded.level`, fund, level)
	return err
}

func (s *Store) AddPaid(ctx context.Context, fund string, t config.FundType, amount float64, on time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tax_paid(fund,type,amount,paid_on) VALUES(?,?,?,?)`,
		fund, string(t), amount, on.Format(dayFmt))
	return err
}

func (s *Store) PaidByType(ctx context.Context, year int) (map[config.FundType]float64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT type, SUM(amount) FROM tax_paid
		WHERE paid_on >= ? AND paid_on < ? GROUP BY type`,
		time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format(dayFmt),
		time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC).Format(dayFmt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[config.FundType]float64{}
	for rows.Next() {
		var t string
		var sum float64
		if err := rows.Scan(&t, &sum); err != nil {
			return nil, err
		}
		out[config.FundType(t)] = sum
	}
	return out, rows.Err()
}

// SetDeduction records the year-to-date total for a manually reported deduction
// key, replacing any earlier value. amount 0 removes the entry.
func (s *Store) SetDeduction(ctx context.Context, year int, key config.FundType, amount float64) error {
	if amount == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM deduction_entry WHERE year=? AND key=?`, year, string(key))
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO deduction_entry(year,key,amount) VALUES(?,?,?)
		ON CONFLICT(year,key) DO UPDATE SET amount=excluded.amount`, year, string(key), amount)
	return err
}

// Deductions returns the manually reported totals for a year.
func (s *Store) Deductions(ctx context.Context, year int) (map[config.FundType]float64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, amount FROM deduction_entry WHERE year=?`, year)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[config.FundType]float64{}
	for rows.Next() {
		var k string
		var a float64
		if err := rows.Scan(&k, &a); err != nil {
			return nil, err
		}
		out[config.FundType(k)] = a
	}
	return out, rows.Err()
}

func (s *Store) ReminderSent(ctx context.Context, day time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM year_end_reminder WHERE day=?`, day.Format(dayFmt)).Scan(&n)
	return n > 0, err
}

func (s *Store) MarkReminderSent(ctx context.Context, day time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO year_end_reminder(day) VALUES(?)`, day.Format(dayFmt))
	return err
}
