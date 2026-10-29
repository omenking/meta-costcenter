package costcenter

import (
	"database/sql"
	"strings"
	"unicode/utf8"
)

// unlabelled is the label of sessions that have none, for grouping and filtering.
const unlabelled = "unlabelled"

// SessionLabel is the name and label given to a top-level session.
type SessionLabel struct{ Name, Label string }

// sessionLabels maps each labelled top-level session to its name and label.
func (s *Store) sessionLabels() (map[string]SessionLabel, error) {
	rows, err := s.DB.Query("SELECT session, name, label FROM session_labels")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]SessionLabel{}
	for rows.Next() {
		var session, name, label sql.NullString
		if err := rows.Scan(&session, &name, &label); err != nil {
			return nil, err
		}
		result[session.String] = SessionLabel{name.String, label.String}
	}
	return result, rows.Err()
}

// labelNames is every label in use, for the filter and the label field's suggestions.
func (s *Store) labelNames() ([]any, error) {
	rows, err := s.DB.Query("SELECT DISTINCT label FROM session_labels WHERE label != '' ORDER BY label")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []any{}
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		result = append(result, label)
	}
	return result, rows.Err()
}

// SetSessionLabel names and labels a top-level session; an empty name and label clear both.
func (s *Store) SetSessionLabel(session string, body any) error {
	values, ok := body.(map[string]any)
	if !ok {
		return invalid("Send a name and a label")
	}
	text := func(key string, limit int) (string, error) {
		raw, present := values[key]
		if !present || raw == nil {
			return "", nil
		}
		value, ok := raw.(string)
		if !ok {
			return "", invalid("The %s must be text", key)
		}
		value = strings.Join(strings.Fields(value), " ")
		if utf8.RuneCountInString(value) > limit {
			return "", invalid("The %s must be at most %d characters", key, limit)
		}
		return value, nil
	}
	name, err := text("name", 120)
	if err != nil {
		return err
	}
	label, err := text("label", 60)
	if err != nil {
		return err
	}
	if strings.EqualFold(label, unlabelled) {
		return invalid("%s is reserved for sessions without a label", pyRepr(unlabelled))
	}
	parents, err := s.parents()
	if err != nil {
		return err
	}
	if _, ok := parents[session]; !ok {
		return &NotFound{"Unknown session: " + session}
	}
	if root := roots(parents)[session]; root != session {
		return invalid("Subagents take the label of their top-level session %s", root)
	}
	if name == "" && label == "" {
		_, err = s.DB.Exec("DELETE FROM session_labels WHERE session=?", session)
		return err
	}
	_, err = s.DB.Exec("INSERT OR REPLACE INTO session_labels (session, name, label, updated_at) VALUES (?, ?, ?, ?)",
		session, name, label, nowISO())
	return err
}
