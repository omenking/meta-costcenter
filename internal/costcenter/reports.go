package costcenter

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Rate is a model's price per million tokens, and per generated image for image models. Public
// rates come from pricing.json; local rates are entered on this machine and override them.
type Rate struct {
	Model           string
	EffectiveFrom   string
	SnapshotVersion string
	SourceURL       string
	Local           bool
	Input           Dec
	CachedInput     Dec
	Output          Dec
	PerImage        Dec
}

// RateValues is a rate's three prices, as the dashboard shows them.
type RateValues struct {
	Input       Dec `json:"input_per_million"`
	CachedInput Dec `json:"cached_input_per_million"`
	Output      Dec `json:"output_per_million"`
	PerImage    Dec `json:"per_image"`
}

func (r Rate) Values() RateValues { return RateValues{r.Input, r.CachedInput, r.Output, r.PerImage} }

var rateFields = []string{"input_per_million", "cached_input_per_million", "output_per_million"}

// LoadPrices reads and validates a versioned pricing file.
func LoadPrices(data []byte) ([]Rate, error) {
	var file struct {
		Rates *[]map[string]any `json:"rates"`
	}
	decoder := json.NewDecoder(bytesReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&file); err != nil {
		return nil, err
	}
	if file.Rates == nil {
		return nil, fmt.Errorf("pricing file has no rates")
	}
	var rates []Rate
	for _, r := range *file.Rates {
		for _, key := range []string{"model", "effective_from", "source_url", "snapshot_version", "retrieved_at"} {
			if !truthy(r[key]) {
				return nil, fmt.Errorf("Pricing rate requires %s", key)
			}
		}
		effective := pyStr(r["effective_from"])
		if _, err := parseDate(effective); err != nil {
			return nil, err
		}
		if r["currency"] != "USD" {
			return nil, fmt.Errorf("Pricing currency must be USD")
		}
		values := make([]Dec, 3)
		for i, key := range rateFields {
			d, err := ParseDecimal(pyStr(r[key]))
			if err != nil {
				return nil, fmt.Errorf("Rates must be numbers")
			}
			if !d.Finite || d.Value.Sign() < 0 {
				return nil, fmt.Errorf("Rates must be finite and nonnegative")
			}
			values[i] = d.Value
		}
		// Image models also have a price per image; token-only models leave it out.
		var perImage Dec
		if value, ok := r["per_image"]; ok {
			d, err := ParseDecimal(pyStr(value))
			if err != nil || !d.Finite || d.Value.Sign() < 0 {
				return nil, fmt.Errorf("Rates must be finite and nonnegative")
			}
			perImage = d.Value
		}
		source, _ := r["source_url"].(string)
		rates = append(rates, Rate{Model: pyStr(r["model"]), EffectiveFrom: effective, SnapshotVersion: pyStr(r["snapshot_version"]),
			SourceURL: source, Local: truthy(r["local"]), Input: values[0], CachedInput: values[1], Output: values[2], PerImage: perImage})
	}
	return rates, nil
}

// WithLocalRates is the public rates plus the rates entered on this machine.
func (s *Store) WithLocalRates(rates []Rate) ([]Rate, error) {
	rows, err := s.DB.Query("SELECT model, input_per_million, cached_input_per_million, output_per_million, COALESCE(per_image, '0') FROM rates ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Rate
	for _, r := range rates {
		if !r.Local {
			result = append(result, r)
		}
	}
	for rows.Next() {
		var model, input, cached, output, perImage sql.NullString
		if err := rows.Scan(&model, &input, &cached, &output, &perImage); err != nil {
			return nil, err
		}
		result = append(result, Rate{Model: model.String, EffectiveFrom: "0001-01-01", SnapshotVersion: "local", Local: true,
			Input: mustDec(input.String), CachedInput: mustDec(cached.String), Output: mustDec(output.String), PerImage: mustDec(perImage.String)})
	}
	return result, rows.Err()
}

// rateFor is the applicable rate, and whether it is assumed (applied before its verified date).
func rateFor(model, day string, rates []Rate) (*Rate, bool) {
	var candidates []Rate
	for _, r := range rates {
		if r.Model == model {
			candidates = append(candidates, r)
		}
	}
	for _, r := range candidates {
		if r.Local {
			return &r, false
		}
	}
	var best *Rate
	for i, r := range candidates {
		if r.EffectiveFrom <= day && (best == nil || r.EffectiveFrom > best.EffectiveFrom) {
			best = &candidates[i]
		}
	}
	if best != nil {
		return best, false
	}
	for i, r := range candidates {
		if best == nil || r.EffectiveFrom < best.EffectiveFrom {
			best = &candidates[i]
		}
	}
	return best, best != nil
}

// Row is one model call, as stored.
type Row struct {
	ID          string  `json:"id"`
	Session     string  `json:"session"`
	Parent      *string `json:"parent"`
	Project     string  `json:"project"`
	RecordedAt  string  `json:"recorded_at"`
	Model       string  `json:"model"`
	BillingMode string  `json:"billing_mode"`
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	Cached      *int64  `json:"cached"`
	Turn        string  `json:"turn,omitempty"`
	Images      *int64  `json:"images,omitempty"`
	Source      string  `json:"-"`
	Sequence    string  `json:"sequence"`
	UsageStatus string  `json:"usage_status"`
	KeyLabel    *string `json:"key_label"`
	Label       string  `json:"label,omitempty"`
}

// field is a row's value for a grouping or filter field.
func (r Row) field(name string) string {
	switch name {
	case "model":
		return r.Model
	case "project":
		return r.Project
	case "session":
		return r.Session
	case "billing_mode":
		return r.BillingMode
	case "label":
		if r.Label == "" {
			return unlabelled
		}
		return r.Label
	}
	return ""
}

// estimate is the API-equivalent USD value of a row, its pricing snapshot, and whether its rate is
// assumed. ok is false when the cost is unknown.
func estimate(row Row, rates []Rate) (value Dec, snapshot string, assumed, ok bool) {
	day := row.RecordedAt
	if len(day) > 10 {
		day = day[:10]
	}
	rate, assumed := rateFor(row.Model, day, rates)
	if rate == nil || row.Cached == nil {
		return Dec{}, "", false, false
	}
	cached := *row.Cached
	value = rate.Input.MulInt(row.Input - cached).Add(rate.CachedInput.MulInt(cached)).Add(rate.Output.MulInt(row.Output)).DivInt(1_000_000)
	if row.Images != nil {
		value = value.Add(rate.PerImage.MulInt(*row.Images))
	}
	return value, rate.SnapshotVersion, assumed, true
}

// Filters narrow the usage a report covers. Empty fields do not filter.
type Filters struct {
	Period, From, To, Project, Model, Session, BillingMode, Label, Sort string
}

var periods = map[string]bool{"today": true, "week": true, "month": true, "all": true}

// bounds is the local date range of the filters; start is nil for all time.
func bounds(f Filters) (*time.Time, time.Time, error) {
	t := today()
	period := f.Period
	if period == "" {
		period = "today"
	}
	var start *time.Time
	switch period {
	case "today":
		start = &t
	case "week":
		s := t.AddDate(0, 0, -6)
		start = &s
	case "month":
		s := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.Local)
		start = &s
	case "all":
	default:
		return nil, t, fmt.Errorf("Period must be today, week, month, or all, not %s", pyRepr(period))
	}
	if f.From != "" {
		s, err := parseDate(f.From)
		if err != nil {
			return nil, t, err
		}
		start = &s
	}
	end := t
	if f.To != "" {
		e, err := parseDate(f.To)
		if err != nil {
			return nil, t, err
		}
		end = e
	}
	if start != nil && start.After(end) {
		return nil, t, fmt.Errorf("Start date must be on or before end date")
	}
	return start, end, nil
}

// NotFound is a drill-down target (session, project, alert, or rate) that does not exist.
type NotFound struct{ Message string }

func (e *NotFound) Error() string { return e.Message }

// parentMap maps each session to its parent session ("" for a top-level session).
type parentMap map[string]string

func (s *Store) parents() (parentMap, error) {
	rows, err := s.DB.Query("SELECT session, MAX(parent) FROM events GROUP BY session")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parents := parentMap{}
	for rows.Next() {
		var session, parent sql.NullString
		if err := rows.Scan(&session, &parent); err != nil {
			return nil, err
		}
		parents[session.String] = parent.String
	}
	return parents, rows.Err()
}

// family is the session and all of its descendants.
func family(parents parentMap, session string) map[string]bool {
	members := map[string]bool{session: true}
	for {
		grew := false
		for s, p := range parents {
			if p != "" && members[p] && !members[s] {
				members[s] = true
				grew = true
			}
		}
		if !grew {
			return members
		}
	}
}

// roots maps each session to its top-level ancestor; unknown parents make a session a root.
func roots(parents parentMap) map[string]string {
	result := map[string]string{}
	for session := range parents {
		node, seen := session, map[string]bool{session: true}
		for {
			p := parents[node]
			if _, known := parents[p]; p == "" || !known || seen[p] {
				break
			}
			node = p
			seen[node] = true
		}
		result[session] = node
	}
	return result
}

// inheritedProjects: subagent logs carry no workspace metadata, so they take the project of the
// nearest ancestor that has one.
func (s *Store) inheritedProjects(parents parentMap) (map[string]string, error) {
	rows, err := s.DB.Query("SELECT session, MAX(project) FROM events WHERE project != 'unassigned' GROUP BY session")
	if err != nil {
		return nil, err
	}
	known := map[string]string{}
	for rows.Next() {
		var session, project sql.NullString
		if err := rows.Scan(&session, &project); err != nil {
			rows.Close()
			return nil, err
		}
		known[session.String] = project.String
	}
	rows.Close()
	result := map[string]string{}
	for session := range parents {
		node, seen, ok := session, map[string]bool{}, true
		for {
			if _, found := known[node]; found || seen[node] {
				break
			}
			seen[node] = true
			p := parents[node]
			if p == "" {
				ok = false
				break
			}
			node = p
		}
		if project, found := known[node]; ok && found {
			result[session] = project
		}
	}
	return result, nil
}

func (s *Store) allRows() ([]Row, error) {
	rows, err := s.DB.Query("SELECT id, session, parent, project, recorded_at, model, billing_mode, input, output, cached, " +
		"source, sequence, usage_status, key_label, turn, images FROM events ORDER BY recorded_at, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Row
	for rows.Next() {
		var r Row
		var id, session, parent, project, recorded, model, billing, source, sequence, status, keyLabel, turn sql.NullString
		var input, output, cached, images sql.NullInt64
		if err := rows.Scan(&id, &session, &parent, &project, &recorded, &model, &billing, &input, &output, &cached,
			&source, &sequence, &status, &keyLabel, &turn, &images); err != nil {
			return nil, err
		}
		r.ID, r.Session, r.Project, r.RecordedAt = id.String, session.String, project.String, recorded.String
		r.Model, r.BillingMode, r.Source, r.Sequence, r.UsageStatus = model.String, billing.String, source.String, sequence.String, status.String
		r.Input, r.Output, r.Turn = input.Int64, output.Int64, turn.String
		if parent.Valid {
			p := parent.String
			r.Parent = &p
		}
		if cached.Valid {
			c := cached.Int64
			r.Cached = &c
		}
		if images.Valid {
			n := images.Int64
			r.Images = &n
		}
		if keyLabel.Valid {
			k := keyLabel.String
			r.KeyLabel = &k
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// Query is the usage matching the filters, by local calendar date (including historical DST).
func (s *Store) Query(f Filters) ([]Row, error) {
	start, end, err := bounds(f)
	if err != nil {
		return nil, err
	}
	all, err := s.allRows()
	if err != nil {
		return nil, err
	}
	parents, err := s.parents()
	if err != nil {
		return nil, err
	}
	projects, err := s.inheritedProjects(parents)
	if err != nil {
		return nil, err
	}
	labels, err := s.sessionLabels()
	if err != nil {
		return nil, err
	}
	rootOf := roots(parents)
	var members map[string]bool
	if f.Session != "" {
		members = family(parents, f.Session)
	}
	startDay, endDay := "", dateString(end)
	if start != nil {
		startDay = dateString(*start)
	}
	result := []Row{}
	for _, row := range all {
		if row.Project == "unassigned" {
			if p, ok := projects[row.Session]; ok {
				row.Project = p
			}
		}
		row.Label = labels[rootOf[row.Session]].Label
		day := localDay(row.RecordedAt)
		if (startDay != "" && day < startDay) || day > endDay {
			continue
		}
		if f.Session != "" && !members[row.Session] {
			continue
		}
		if (f.Project != "" && row.Project != f.Project) || (f.Model != "" && row.Model != f.Model) ||
			(f.BillingMode != "" && row.BillingMode != f.BillingMode) || (f.Label != "" && row.field("label") != f.Label) {
			continue
		}
		result = append(result, row)
	}
	return result, nil
}

// Totals are token counts and estimated costs for a set of rows.
type Totals struct {
	Events                    int   `json:"events"`
	Input                     int64 `json:"input"`
	Output                    int64 `json:"output"`
	Cached                    int64 `json:"cached"`
	CacheUnknownEvents        int   `json:"cache_unknown_events"`
	CostUnknownEvents         int   `json:"cost_unknown_events"`
	CostAssumedEvents         int   `json:"cost_assumed_events"`
	PricedTokens              int64 `json:"priced_tokens"`
	Images                    int64 `json:"images"`
	PaygUSD                   Dec   `json:"payg_usd"`
	SubscriptionEquivalentUSD Dec   `json:"subscription_equivalent_usd"`
	UnknownModeEquivalentUSD  Dec   `json:"unknown_mode_equivalent_usd"`
}

func totals(rows []Row, rates []Rate) Totals {
	t := Totals{Events: len(rows)}
	for _, row := range rows {
		t.Input += row.Input
		t.Output += row.Output
		if row.Images != nil {
			t.Images += *row.Images
		}
		if row.Cached != nil {
			t.Cached += *row.Cached
		} else {
			t.CacheUnknownEvents++
		}
		value, _, assumed, ok := estimate(row, rates)
		if assumed {
			t.CostAssumedEvents++
		}
		if !ok {
			t.CostUnknownEvents++
			continue
		}
		t.PricedTokens += row.Input + row.Output
		switch row.BillingMode {
		case "payg":
			t.PaygUSD = t.PaygUSD.Add(value)
		case "subscription":
			t.SubscriptionEquivalentUSD = t.SubscriptionEquivalentUSD.Add(value)
		default:
			t.UnknownModeEquivalentUSD = t.UnknownModeEquivalentUSD.Add(value)
		}
	}
	return t
}

func (t Totals) Equivalent() Dec {
	return t.PaygUSD.Add(t.SubscriptionEquivalentUSD).Add(t.UnknownModeEquivalentUSD)
}

func (t Totals) tokens() int64 { return t.Input + t.Output }

// ranked is the distinct values of a field, most tokens first.
func ranked(rows []Row, field string) []string {
	var order []string
	weight := map[string]int64{}
	for _, r := range rows {
		v := r.field(field)
		if _, seen := weight[v]; !seen {
			order = append(order, v)
		}
		weight[v] += r.Input + r.Output
	}
	sort.SliceStable(order, func(i, j int) bool { return weight[order[i]] > weight[order[j]] })
	return order
}

// bucket groups rows by key, keeping the order in which keys first appear.
func bucket(rows []Row, key func(Row) string) ([]string, map[string][]Row) {
	var order []string
	buckets := map[string][]Row{}
	for _, r := range rows {
		k := key(r)
		if _, seen := buckets[k]; !seen {
			order = append(order, k)
		}
		buckets[k] = append(buckets[k], r)
	}
	return order, buckets
}

type Group struct {
	Label string `json:"label"`
	Totals
}

func grouped(rows []Row, field string, rates []Rate) []Group {
	order, buckets := bucket(rows, func(r Row) string { return r.field(field) })
	result := []Group{}
	for _, label := range order {
		result = append(result, Group{label, totals(buckets[label], rates)})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].tokens() > result[j].tokens() })
	return result
}

type TimelinePoint struct {
	Bucket string `json:"bucket"`
	Totals
}

// bucketKey is the local time bucket a moment falls in: a day, an hour, or a 15-minute block.
func bucketKey(t time.Time, unit string) string {
	switch unit {
	case "hour":
		return t.Format("2006-01-02T15:00")
	case "15m":
		return t.Add(-time.Duration(t.Minute()%15) * time.Minute).Format("2006-01-02T15:04")
	}
	return t.Format("2006-01-02")
}

func timeline(rows []Row, rates []Rate, unit string) []TimelinePoint {
	order, buckets := bucket(rows, func(r Row) string { return bucketKey(localTime(r.RecordedAt), unit) })
	sort.Strings(order)
	result := []TimelinePoint{}
	for _, key := range order {
		result = append(result, TimelinePoint{key, totals(buckets[key], rates)})
	}
	return result
}

// Chart is the usage chart for a date range at one resolution. Every bucket in the range is
// present, including empty ones and those still to come today, so that the bars keep their place
// on the axis while usage arrives.
type Chart struct {
	Unit string `json:"unit"`
	// Units are the resolutions the range allows; finer ones would draw too many bars.
	Units   []string        `json:"units"`
	Current string          `json:"current"` // the bucket holding now, or "" outside the range
	Points  []TimelinePoint `json:"points"`
}

var chartUnits = []struct {
	name    string
	step    time.Duration
	maxDays int
}{{"15m", 15 * time.Minute, 2}, {"hour", time.Hour, 14}, {"day", 0, 0}}

// chart buckets rows by the requested unit when the range allows it, and otherwise by the finest
// unit that suits the range: 15-minute blocks for one day, hours for up to a week, days beyond.
func chart(rows []Row, rates []Rate, f Filters, requested string) (Chart, error) {
	start, end, err := bounds(f)
	if err != nil {
		return Chart{}, err
	}
	first := end
	if start != nil {
		first = *start
	} else if len(rows) > 0 {
		t := localTime(rows[0].RecordedAt)
		first = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
	}
	stop := end.AddDate(0, 0, 1)
	days := 0
	for d := first; d.Before(stop); d = d.AddDate(0, 0, 1) {
		days++
	}
	c := Chart{}
	for _, u := range chartUnits {
		if u.maxDays == 0 || days <= u.maxDays {
			c.Units = append(c.Units, u.name)
		}
	}
	c.Unit = "day"
	if days <= 1 {
		c.Unit = "15m"
	} else if days <= 7 {
		c.Unit = "hour"
	}
	for _, u := range c.Units {
		if u == requested {
			c.Unit = u
		}
	}
	filled := map[string]TimelinePoint{}
	for _, p := range timeline(rows, rates, c.Unit) {
		filled[p.Bucket] = p
	}
	next := func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	for _, u := range chartUnits {
		if u.name == c.Unit && u.step > 0 {
			step := u.step
			next = func(t time.Time) time.Time { return t.Add(step) }
		}
	}
	empty := totals(nil, rates)
	for t := first; t.Before(stop); t = next(t) {
		key := bucketKey(t, c.Unit)
		// The repeated hour when clocks go back is one bucket.
		if n := len(c.Points); n > 0 && c.Points[n-1].Bucket == key {
			continue
		}
		point, ok := filled[key]
		if !ok {
			point = TimelinePoint{key, empty}
		}
		c.Points = append(c.Points, point)
	}
	if moment := now().Local(); !moment.Before(first) && moment.Before(stop) {
		c.Current = bucketKey(moment, c.Unit)
	}
	return c, nil
}

type SessionSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Label        string   `json:"label"`
	Project      string   `json:"project"`
	Started      string   `json:"started"`
	LastActivity string   `json:"last_activity"`
	Models       []string `json:"models"`
	BillingModes []string `json:"billing_modes"`
	Subagents    int      `json:"subagents"`
	Totals
}

// Sessions is one summary per top-level session; subagent usage rolls up into its root.
func (s *Store) Sessions(rows []Row, rates []Rate) ([]SessionSummary, error) {
	parents, err := s.parents()
	if err != nil {
		return nil, err
	}
	labels, err := s.sessionLabels()
	if err != nil {
		return nil, err
	}
	rootOf := roots(parents)
	order, families := bucket(rows, func(r Row) string {
		if root, ok := rootOf[r.Session]; ok {
			return root
		}
		return r.Session
	})
	result := []SessionSummary{}
	for _, root := range order {
		items := families[root]
		var own []Row
		sessions := map[string]bool{}
		for _, r := range items {
			if r.Session == root {
				own = append(own, r)
			}
			sessions[r.Session] = true
		}
		if len(own) == 0 {
			own = items
		}
		delete(sessions, root)
		result = append(result, SessionSummary{ID: root, Name: labels[root].Name, Label: labels[root].Label, Project: ranked(own, "project")[0], Started: items[0].RecordedAt,
			LastActivity: items[len(items)-1].RecordedAt, Models: ranked(items, "model"), BillingModes: ranked(items, "billing_mode"),
			Subagents: len(sessions), Totals: totals(items, rates)})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].LastActivity > result[j].LastActivity })
	return result, nil
}

func (s *Store) SessionsFor(f Filters, rates []Rate) ([]SessionSummary, error) {
	rows, err := s.Query(f)
	if err != nil {
		return nil, err
	}
	return s.Sessions(rows, rates)
}

type ProjectSummary struct {
	Label string `json:"label"`
	Totals
	Sessions     int    `json:"sessions"`
	LastActivity string `json:"last_activity"`
}

func (s *Store) Projects(rows []Row, rates []Rate) ([]ProjectSummary, error) {
	parents, err := s.parents()
	if err != nil {
		return nil, err
	}
	rootOf := roots(parents)
	order, buckets := bucket(rows, func(r Row) string { return r.Project })
	result := []ProjectSummary{}
	for _, label := range order {
		items := buckets[label]
		sessions := map[string]bool{}
		for _, r := range items {
			if root, ok := rootOf[r.Session]; ok {
				sessions[root] = true
			} else {
				sessions[r.Session] = true
			}
		}
		result = append(result, ProjectSummary{Label: label, Totals: totals(items, rates), Sessions: len(sessions),
			LastActivity: items[len(items)-1].RecordedAt})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].tokens() > result[j].tokens() })
	return result, nil
}

// periodOnly: detail pages are scoped by their URL; only the date range carries over.
func periodOnly(f Filters) Filters { return Filters{Period: f.Period, From: f.From, To: f.To} }

// EstimatedRow is a row with its estimate, for drill-down tables. The source path is left out.
type EstimatedRow struct {
	Row
	APIEquivalentUSD *Dec    `json:"api_equivalent_usd"`
	PricingSnapshot  *string `json:"pricing_snapshot"`
	RateAssumed      bool    `json:"rate_assumed"`
}

func withEstimates(rows []Row, rates []Rate) []EstimatedRow {
	result := []EstimatedRow{}
	for _, r := range rows {
		e := EstimatedRow{Row: r}
		if value, snapshot, assumed, ok := estimate(r, rates); ok {
			e.APIEquivalentUSD, e.PricingSnapshot, e.RateAssumed = &value, &snapshot, assumed
		}
		result = append(result, e)
	}
	return result
}

// ExportRow is a row as `export` and the dashboard's download write it. Exports omit source paths.
type ExportRow struct {
	Row
	EstimatedAPIEquivalentUSD *string `json:"estimated_api_equivalent_usd"`
	PricingSnapshot           *string `json:"pricing_snapshot"`
	RateAssumed               bool    `json:"rate_assumed"`
}

func exportRows(rows []Row, rates []Rate) []ExportRow {
	result := []ExportRow{}
	for _, r := range rows {
		e := ExportRow{Row: r}
		if value, snapshot, assumed, ok := estimate(r, rates); ok {
			text := value.String()
			e.EstimatedAPIEquivalentUSD, e.PricingSnapshot, e.RateAssumed = &text, &snapshot, assumed
		}
		result = append(result, e)
	}
	return result
}

var exportFields = []string{"id", "session", "parent", "project", "recorded_at", "model", "billing_mode", "input", "output",
	"cached", "sequence", "usage_status", "estimated_api_equivalent_usd", "pricing_snapshot", "rate_assumed", "key_label", "images", "label"}

// csvRecord writes an export row as Python's csv module did: None as empty, booleans as True/False.
func (e ExportRow) csvRecord() []string {
	opt := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	cached := ""
	if e.Cached != nil {
		cached = fmt.Sprint(*e.Cached)
	}
	images := ""
	if e.Images != nil {
		images = fmt.Sprint(*e.Images)
	}
	assumed := "False"
	if e.RateAssumed {
		assumed = "True"
	}
	return []string{e.ID, e.Session, opt(e.Parent), e.Project, e.RecordedAt, e.Model, e.BillingMode, fmt.Sprint(e.Input),
		fmt.Sprint(e.Output), cached, e.Sequence, e.UsageStatus, opt(e.EstimatedAPIEquivalentUSD), opt(e.PricingSnapshot), assumed,
		opt(e.KeyLabel), images, e.Label}
}

type TreeNode struct {
	ID      string  `json:"id"`
	Project *string `json:"project"`
	Totals
	Children []TreeNode `json:"children"`
}

type SessionDetail struct {
	ID     string  `json:"id"`
	Parent *string `json:"parent"`
	Root   string  `json:"root"`
	// Name and Label belong to the root session; a subagent shows its root's.
	Name         string   `json:"name"`
	Label        string   `json:"label"`
	Project      *string  `json:"project"`
	Started      *string  `json:"started"`
	LastActivity *string  `json:"last_activity"`
	BillingModes []string `json:"billing_modes"`
	// BillingDetections are what the Muse hook found for this session, or for the nearest ancestor
	// when a subagent has none of its own.
	BillingDetections []DetectionRow  `json:"billing_detections"`
	Totals            Totals          `json:"totals"`
	Own               Totals          `json:"own"`
	Timeline          []TimelinePoint `json:"timeline"`
	TimelineUnit      string          `json:"timeline_unit"`
	Tree              *TreeNode       `json:"tree"`
	Turns             []Turn          `json:"turns"`
	Models            []Group         `json:"models"`
	Events            []EstimatedRow  `json:"events"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Store) SessionDetail(session string, f Filters, rates []Rate) (*SessionDetail, error) {
	parents, err := s.parents()
	if err != nil {
		return nil, err
	}
	if _, ok := parents[session]; !ok {
		return nil, &NotFound{"Unknown session: " + session}
	}
	scoped := periodOnly(f)
	scoped.Session = session
	rows, err := s.Query(scoped)
	if err != nil {
		return nil, err
	}
	members := family(parents, session)
	sorted := make([]string, 0, len(members))
	for m := range members {
		sorted = append(sorted, m)
	}
	sort.Strings(sorted)
	_, bySession := bucket(rows, func(r Row) string { return r.Session })
	var node func(sid string, seen map[string]bool) *TreeNode
	node = func(sid string, seen map[string]bool) *TreeNode {
		next := map[string]bool{sid: true}
		for k := range seen {
			next[k] = true
		}
		children := []TreeNode{}
		for _, c := range sorted {
			if parents[c] == sid && !next[c] {
				if n := node(c, next); n != nil {
					children = append(children, *n)
				}
			}
		}
		own := bySession[sid]
		if len(own) == 0 && len(children) == 0 && sid != session {
			return nil
		}
		var project *string
		if len(own) > 0 {
			p := ranked(own, "project")[0]
			project = &p
		}
		return &TreeNode{ID: sid, Project: project, Totals: totals(own, rates), Children: children}
	}
	unit := "day"
	var started, last *string
	if len(rows) > 0 {
		first, final := rows[0].RecordedAt, rows[len(rows)-1].RecordedAt
		started, last = &first, &final
		if localTime(final).Sub(localTime(first)) < 48*time.Hour {
			unit = "hour"
		}
	}
	var project sql.NullString
	err = s.DB.QueryRow("SELECT project FROM events WHERE session=? GROUP BY project ORDER BY COUNT(*) DESC LIMIT 1", session).Scan(&project)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	var projectPtr *string
	if project.Valid {
		projectPtr = &project.String
	}
	detections, err := s.SessionDetections(session)
	for node, seen := parents[session], map[string]bool{session: true}; err == nil && len(detections) == 0 && node != "" && !seen[node]; node = parents[node] {
		seen[node] = true
		detections, err = s.SessionDetections(node)
	}
	if err != nil {
		return nil, err
	}
	turns, err := s.Turns(session, rows, rates)
	if err != nil {
		return nil, err
	}
	labels, err := s.sessionLabels()
	if err != nil {
		return nil, err
	}
	root := roots(parents)[session]
	return &SessionDetail{ID: session, Turns: turns, Parent: optional(parents[session]), Root: root, Name: labels[root].Name, Label: labels[root].Label, Project: projectPtr,
		Started: started, LastActivity: last, BillingModes: ranked(rows, "billing_mode"), BillingDetections: detections,
		Totals: totals(rows, rates),
		Own:    totals(bySession[session], rates), Timeline: timeline(rows, rates, unit), TimelineUnit: unit,
		Tree: node(session, map[string]bool{}), Models: grouped(rows, "model", rates), Events: withEstimates(rows, rates)}, nil
}

type ProjectDetail struct {
	Project  string           `json:"project"`
	Totals   Totals           `json:"totals"`
	Timeline []TimelinePoint  `json:"timeline"`
	Chart    Chart            `json:"chart"`
	Models   []Group          `json:"models"`
	Sessions []SessionSummary `json:"sessions"`
}

// ProjectDetail reports one project; resolution is the chart unit asked for, or "" for the default.
func (s *Store) ProjectDetail(project string, f Filters, rates []Rate, resolution string) (*ProjectDetail, error) {
	var one int
	if err := s.DB.QueryRow("SELECT 1 FROM events WHERE project=? LIMIT 1", project).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			return nil, &NotFound{"Unknown project: " + project}
		}
		return nil, err
	}
	scoped := periodOnly(f)
	scoped.Project = project
	rows, err := s.Query(scoped)
	if err != nil {
		return nil, err
	}
	sessions, err := s.Sessions(rows, rates)
	if err != nil {
		return nil, err
	}
	c, err := chart(rows, rates, scoped, resolution)
	if err != nil {
		return nil, err
	}
	return &ProjectDetail{Project: project, Totals: totals(rows, rates), Timeline: timeline(rows, rates, "day"), Chart: c,
		Models: grouped(rows, "model", rates), Sessions: sessions}, nil
}

type Reconciliation struct {
	Session        string `json:"session"`
	Model          string `json:"model"`
	Status         string `json:"status"`
	SummaryInput   int64  `json:"summary_input"`
	ObservedInput  int64  `json:"observed_input"`
	SummaryOutput  int64  `json:"summary_output"`
	ObservedOutput int64  `json:"observed_output"`
}

// Reconciliation compares run summaries with the observed events of the whole session family.
// Summaries are a comparison only and never count toward totals.
func (s *Store) Reconciliation() ([]Reconciliation, error) {
	rows, err := s.DB.Query("SELECT session, recorded_at, model, input, output FROM summaries ORDER BY recorded_at")
	if err != nil {
		return nil, err
	}
	type summary struct {
		session, recorded, model string
		input, output            int64
	}
	var summaries []summary
	for rows.Next() {
		var x summary
		var session, recorded, model sql.NullString
		var input, output sql.NullInt64
		if err := rows.Scan(&session, &recorded, &model, &input, &output); err != nil {
			rows.Close()
			return nil, err
		}
		x.session, x.recorded, x.model, x.input, x.output = session.String, recorded.String, model.String, input.Int64, output.Int64
		summaries = append(summaries, x)
	}
	rows.Close()
	result := []Reconciliation{}
	for _, x := range summaries {
		observed, err := s.Query(Filters{Period: "all", Session: x.session})
		if err != nil {
			return nil, err
		}
		var input, output int64
		for _, r := range observed {
			if r.Model == x.model && r.RecordedAt <= x.recorded {
				input += r.Input
				output += r.Output
			}
		}
		status := "mismatch / scope unverified"
		if input == x.input && output == x.output {
			status = "match"
		}
		result = append(result, Reconciliation{x.session, x.model, status, x.input, input, x.output, output})
	}
	return result, nil
}

func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }
