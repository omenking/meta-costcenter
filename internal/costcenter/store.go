package costcenter

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite"
)

// Usage-only, local-first Muse observability. Only allowlisted usage fields reach SQLite;
// transcript fields are never stored.

const schema = `
CREATE TABLE IF NOT EXISTS sources (
  path TEXT PRIMARY KEY, identity TEXT, offset INTEGER DEFAULT 0,
  anchor TEXT, scanned_at TEXT, errors INTEGER DEFAULT 0,
  pending INTEGER DEFAULT 0, status TEXT, parser_version INTEGER DEFAULT 1);
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, session TEXT, parent TEXT, project TEXT,
  recorded_at TEXT, model TEXT, billing_mode TEXT, input INTEGER,
  output INTEGER, cached INTEGER, source TEXT, sequence TEXT,
  usage_status TEXT, key_label TEXT, turn TEXT, images INTEGER);
CREATE INDEX IF NOT EXISTS events_time ON events(recorded_at);
CREATE INDEX IF NOT EXISTS events_session ON events(session);
CREATE TABLE IF NOT EXISTS metadata (source TEXT PRIMARY KEY, project TEXT, model TEXT);
CREATE TABLE IF NOT EXISTS summaries (
  id TEXT PRIMARY KEY, session TEXT, recorded_at TEXT, model TEXT,
  input INTEGER, output INTEGER, cached INTEGER, source TEXT);
CREATE TABLE IF NOT EXISTS observations (
  id INTEGER PRIMARY KEY, observed_at TEXT, period_start TEXT,
  period_end TEXT, amount TEXT, currency TEXT, kind TEXT, scope TEXT);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT);
-- Rates entered on this machine for models without public pricing. Never exported to the repo.
CREATE TABLE IF NOT EXISTS rates (
  model TEXT PRIMARY KEY, input_per_million TEXT, cached_input_per_million TEXT,
  output_per_million TEXT, updated_at TEXT, per_image TEXT);
CREATE TABLE IF NOT EXISTS alerts (
  id INTEGER PRIMARY KEY, period TEXT, project TEXT, billing_mode TEXT,
  limit_usd TEXT, warn_levels TEXT DEFAULT '[80]', enabled INTEGER DEFAULT 1, created_at TEXT,
  revoke_keys INTEGER DEFAULT 0, key_match TEXT);
-- What the Muse SessionStart hook found about each session's credential. A resumed session can
-- have several rows, because it may be resumed with a different credential.
CREATE TABLE IF NOT EXISTS session_billing (
  session TEXT, detected_at TEXT, billing_mode TEXT, source TEXT,
  key_label TEXT, hook_source TEXT, detail TEXT,
  PRIMARY KEY (session, detected_at));
-- Where each prompt is in its session log. The text is read from the log when a session page asks
-- for it and is never stored. turn is the run the prompt started or was added to.
CREATE TABLE IF NOT EXISTS prompts (
  intent TEXT PRIMARY KEY, session TEXT, turn TEXT, recorded_at TEXT, source TEXT, offset INTEGER);
CREATE INDEX IF NOT EXISTS prompts_session ON prompts(session);
-- The reasoning effort Muse was configured with when each prompt was submitted, from the
-- UserPromptSubmit hook. efforts is the per-model map, since the model is only known afterwards.
CREATE TABLE IF NOT EXISTS turn_settings (
  turn TEXT PRIMARY KEY, session TEXT, recorded_at TEXT, model TEXT, effort TEXT, efforts TEXT, source TEXT);
-- Names and labels given to top-level sessions in the dashboard, so that the cost of a piece of
-- work can be followed across sessions. Subagents take their top-level session's label.
CREATE TABLE IF NOT EXISTS session_labels (
  session TEXT PRIMARY KEY, name TEXT, label TEXT, updated_at TEXT);`

// Store is the usage database. Its *sql.DB is safe for concurrent use by the dashboard.
type Store struct {
	DB   *sql.DB
	Path string
}

func OpenStore(path string) (*Store, error) { return openStore(path, 10000) }

// openStore waits up to busyMS for a lock held by another process, such as a running dashboard.
func openStore(path string, busyMS int) (*Store, error) {
	path = expandHome(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if _, err := os.Stat(filepath.Dir(path)); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, busyMS))
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db, Path: path}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	for _, migrate := range []func() error{s.migrateWarnLevels, s.migrateKeyRevocation, s.migrateMonthlyWarning,
		s.migrateKeyLabel, s.migrateTurn, s.migrateImages, s.migrateImageRates} {
		if err := migrate(); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) columns(table string) (map[string]bool, error) {
	rows, err := s.DB.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

// inTx runs fn in a transaction, committing only if it succeeds.
func (s *Store) inTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// migrateWarnLevels: alerts first had one warning percentage; they now keep a JSON list of up to four.
func (s *Store) migrateWarnLevels() error {
	columns, err := s.columns("alerts")
	if err != nil || !columns["warn_percent"] {
		return err
	}
	return s.inTx(func(tx *sql.Tx) error {
		if !columns["warn_levels"] {
			if _, err := tx.Exec("ALTER TABLE alerts ADD COLUMN warn_levels TEXT DEFAULT '[80]'"); err != nil {
				return err
			}
		}
		// Python's json.dumps([70]) is "[70]", which is what json_array produces for one number.
		if _, err := tx.Exec("UPDATE alerts SET warn_levels = json_array(warn_percent)"); err != nil {
			return err
		}
		_, err := tx.Exec("ALTER TABLE alerts DROP COLUMN warn_percent")
		return err
	})
}

// migrateKeyRevocation: alerts can ask Meta Keyguard to revoke matching API keys once exceeded.
func (s *Store) migrateKeyRevocation() error {
	columns, err := s.columns("alerts")
	if err != nil {
		return err
	}
	return s.inTx(func(tx *sql.Tx) error {
		if !columns["revoke_keys"] {
			if _, err := tx.Exec("ALTER TABLE alerts ADD COLUMN revoke_keys INTEGER DEFAULT 0"); err != nil {
				return err
			}
		}
		if !columns["key_match"] {
			if _, err := tx.Exec("ALTER TABLE alerts ADD COLUMN key_match TEXT"); err != nil {
				return err
			}
		}
		return nil
	})
}

// migrateKeyLabel: events record the Meta Keyguard key that paid for them, when the hook saw one.
func (s *Store) migrateKeyLabel() error {
	columns, err := s.columns("events")
	if err != nil || columns["key_label"] {
		return err
	}
	_, err = s.DB.Exec("ALTER TABLE events ADD COLUMN key_label TEXT")
	return err
}

// migrateTurn: events record the Muse turn (run) they belong to. Older events get it when their logs
// are read again, which a parser version change triggers.
func (s *Store) migrateTurn() error {
	columns, err := s.columns("events")
	if err != nil || columns["turn"] {
		return err
	}
	_, err = s.DB.Exec("ALTER TABLE events ADD COLUMN turn TEXT")
	return err
}

// migrateImages: usage recorded with `meta-cost record` can count generated images, which image
// models charge for instead of tokens.
func (s *Store) migrateImages() error {
	columns, err := s.columns("events")
	if err != nil || columns["images"] {
		return err
	}
	_, err = s.DB.Exec("ALTER TABLE events ADD COLUMN images INTEGER")
	return err
}

// migrateImageRates: local rates can price images as well as tokens.
func (s *Store) migrateImageRates() error {
	columns, err := s.columns("rates")
	if err != nil || columns["per_image"] {
		return err
	}
	_, err = s.DB.Exec("ALTER TABLE rates ADD COLUMN per_image TEXT")
	return err
}

// migrateMonthlyWarning: the CLI used to store one monthly threshold in settings; it is now an ordinary alert.
func (s *Store) migrateMonthlyWarning() error {
	var monthly string
	if err := s.DB.QueryRow("SELECT value FROM settings WHERE key='monthly_usd'").Scan(&monthly); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	var levelsText sql.NullString
	s.DB.QueryRow("SELECT value FROM settings WHERE key='alert_levels'").Scan(&levelsText)
	var levels []float64
	if levelsText.Valid {
		json.Unmarshal([]byte(levelsText.String), &levels)
	}
	seen := map[int]bool{}
	var below []int
	for _, level := range levels {
		if level > 0 && level < 100 && level == float64(int(level)) && !seen[int(level)] {
			seen[int(level)] = true
			below = append(below, int(level))
		}
	}
	sort.Ints(below)
	if len(below) > maxWarnings {
		below = below[:maxWarnings]
	}
	if len(below) == 0 {
		below = []int{80}
	}
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO alerts (period, project, billing_mode, limit_usd, warn_levels, enabled, created_at) "+
			"VALUES ('month', NULL, NULL, ?, ?, 1, ?)", monthly, pyDumps(below), nowISO()); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM settings WHERE key IN ('monthly_usd', 'alert_levels')")
		return err
	})
}

func expandHome(path string) string {
	if path == "~" || len(path) > 1 && path[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[1:])
	}
	return path
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
