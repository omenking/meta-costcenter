package costcenter

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A loopback-only dashboard, sharing the CLI's collector and accounting queries.

var filterNames = []string{"period", "from", "to", "project", "model", "session", "billing_mode", "label"}

var pagePattern = regexp.MustCompile(`^(?:/|/(?:breakdown|activity|sessions|projects|pricing|alerts)|/(?:sessions|projects)/[^/]+)$`)
var apiPattern = regexp.MustCompile(`^/api/(dashboard|export|sessions|projects|pricing|alerts|keyguard)(?:/([^/]+))?$`)

const maxBody = 16 * 1024

const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// ScanState is what the dashboard shows about collection.
type ScanState struct {
	Scanning    bool     `json:"scanning"`
	LastSuccess *string  `json:"last_success"`
	Error       *string  `json:"error"`
	Warnings    []string `json:"warnings"`
	Added       *int     `json:"added,omitempty"`
}

// Dashboard serves the browser dashboard and rescans the logs in the background.
type Dashboard struct {
	Store      *Store
	Roots      []string
	Config     Config
	Rates      []Rate
	Filters    map[string]string // launch filters, used when a request names none
	Interval   time.Duration
	Web        fs.FS
	InstanceID string

	listener net.Listener
	server   *http.Server
	mu       sync.Mutex
	state    ScanState
	refresh  chan struct{}
	stop     chan struct{}
	done     chan struct{}
}

// NewDashboard binds 127.0.0.1:port and starts collecting; Serve then handles requests.
func NewDashboard(store *Store, roots []string, config Config, rates []Rate, filters map[string]string,
	interval time.Duration, web fs.FS, port int) (*Dashboard, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	id := make([]byte, 12)
	rand.Read(id)
	d := &Dashboard{Store: store, Roots: roots, Config: config, Rates: rates, Filters: filters, Interval: interval, Web: web,
		InstanceID: hex.EncodeToString(id), listener: listener, state: ScanState{Scanning: true, Warnings: []string{}},
		refresh: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	d.server = &http.Server{Handler: d, ReadHeaderTimeout: 10 * time.Second}
	go d.collect()
	return d, nil
}

func (d *Dashboard) Port() int { return d.listener.Addr().(*net.TCPAddr).Port }

func (d *Dashboard) Serve() error {
	err := d.server.Serve(d.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (d *Dashboard) Close() {
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	d.server.Close()
	d.listener.Close()
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
	}
}

func (d *Dashboard) collect() {
	defer close(d.done)
	for {
		d.mu.Lock()
		d.state.Scanning = true
		d.mu.Unlock()
		result, err := d.Store.Scan(d.Roots, d.Config.Sessions)
		d.mu.Lock()
		d.state.Scanning = false
		if err != nil {
			message := "Collection failed; showing last imported usage."
			d.state.Error = &message
		} else {
			d.state.LastSuccess, d.state.Error, d.state.Warnings, d.state.Added = &result.ScannedAt, nil, result.Warnings, &result.Added
		}
		d.mu.Unlock()
		select {
		case <-d.stop:
			return
		case <-d.refresh:
		case <-time.After(d.Interval):
		}
	}
}

func (d *Dashboard) Refresh() {
	select {
	case d.refresh <- struct{}{}:
	default:
	}
}

func (d *Dashboard) State() ScanState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

func (d *Dashboard) allowed(r *http.Request) bool {
	port := d.Port()
	if r.Host != fmt.Sprintf("127.0.0.1:%d", port) && r.Host != fmt.Sprintf("localhost:%d", port) {
		return false
	}
	origin := r.Header.Get("Origin")
	return origin == "" || origin == fmt.Sprintf("http://127.0.0.1:%d", port) || origin == fmt.Sprintf("http://localhost:%d", port)
}

func send(w http.ResponseWriter, code int, data any, kind, attachment string) {
	body, ok := data.([]byte)
	if !ok {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		encoder.Encode(data)
		body = bytes.TrimRight(buf.Bytes(), "\n")
		kind = "application/json; charset=utf-8"
	}
	h := w.Header()
	h.Set("Content-Type", kind)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", csp)
	if attachment != "" {
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, attachment))
	}
	w.WriteHeader(code)
	w.Write(body)
}

func sendJSON(w http.ResponseWriter, code int, data any) { send(w, code, data, "", "") }

func sendError(w http.ResponseWriter, code int, message string) {
	sendJSON(w, code, map[string]string{"error": message})
}

func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		d.get(w, r)
	case http.MethodPost, http.MethodPut, http.MethodDelete:
		d.write(w, r)
	default:
		sendError(w, http.StatusNotImplemented, "Unsupported method")
	}
}

var assets = map[string][2]string{
	"/app.js":    {"app.js", "text/javascript; charset=utf-8"},
	"/style.css": {"style.css", "text/css; charset=utf-8"},
	"/logo.png":  {"logo.png", "image/png"},
}

func (d *Dashboard) get(w http.ResponseWriter, r *http.Request) {
	if !d.allowed(r) {
		sendError(w, http.StatusForbidden, "Local dashboard requests only.")
		return
	}
	path := r.URL.EscapedPath()
	if asset, ok := assets[path]; ok {
		data, err := fs.ReadFile(d.Web, asset[0])
		if err != nil {
			sendError(w, http.StatusNotFound, "Not found")
			return
		}
		send(w, http.StatusOK, data, asset[1], "")
		return
	}
	if pagePattern.MatchString(path) {
		// Client-side routes; deep links such as /sessions/<id> must survive a refresh.
		data, _ := fs.ReadFile(d.Web, "index.html")
		send(w, http.StatusOK, data, "text/html; charset=utf-8", "")
		return
	}
	if path == "/api/config" {
		// Every launch filter is named, null when unset, as the Python version sent them.
		launch := map[string]any{"sort": nil}
		for _, name := range filterNames {
			launch[name] = nil
			if v, ok := d.Filters[name]; ok {
				launch[name] = v
			}
		}
		sendJSON(w, http.StatusOK, map[string]any{"filters": launch, "instance_id": d.InstanceID})
		return
	}
	route := apiPattern.FindStringSubmatch(path)
	if route == nil {
		sendError(w, http.StatusNotFound, "Not found")
		return
	}
	endpoint, key, hasKey := route[1], "", route[2] != ""
	if hasKey {
		var err error
		if key, err = url.PathUnescape(route[2]); err != nil {
			sendError(w, http.StatusNotFound, "Not found")
			return
		}
	}
	raw, _ := url.ParseQuery(r.URL.RawQuery)
	last := func(name string) (string, bool) {
		values, ok := raw[name]
		if !ok || len(values) == 0 {
			return "", false
		}
		return values[len(values)-1], true
	}
	explicit := false
	for _, name := range filterNames {
		if _, ok := raw[name]; ok {
			explicit = true
		}
	}
	echo := map[string]any{}
	values := map[string]string{}
	for _, name := range filterNames {
		if v, ok := last(name); ok {
			echo[name], values[name] = v, v
		} else if v, ok := d.Filters[name]; ok && !explicit && v != "" {
			echo[name], values[name] = v, v
		} else {
			echo[name] = nil
		}
	}
	if values["period"] == "" {
		values["period"], echo["period"] = "today", "today"
	}
	filters := Filters{Period: values["period"], From: values["from"], To: values["to"], Project: values["project"],
		Model: values["model"], Session: values["session"], BillingMode: values["billing_mode"], Label: values["label"]}
	invalidFilter := func() {
		sendError(w, http.StatusBadRequest, "Invalid filter. Check period, date range, and billing mode.")
	}
	if _, _, err := bounds(filters); err != nil {
		invalidFilter()
		return
	}
	switch filters.BillingMode {
	case "", "payg", "subscription", "unknown":
	default:
		invalidFilter()
		return
	}
	// The chart resolution is a display choice rather than a filter; an unknown one gets the default.
	resolution, _ := last("resolution")
	// Local rates are read on every request so edits on the Pricing page apply at once.
	rates, err := d.Store.WithLocalRates(d.Rates)
	if err != nil {
		d.fail(w, err)
		return
	}
	var result any
	switch {
	case endpoint == "pricing" && !hasKey:
		result, err = d.Store.PricingTable(rates)
	case endpoint == "alerts" && !hasKey:
		var alerts []AlertStatus
		alerts, err = d.Store.AlertStatuses(rates, today())
		result = AnnotateAlerts(alerts, d.Config)
	case endpoint == "keyguard" && !hasKey:
		var match *string
		if v, ok := last("match"); ok {
			match = &v
		}
		summary, err := KeyguardSummary(d.Config, match)
		if err != nil {
			sendError(w, http.StatusBadRequest, err.Error())
			return
		}
		sendJSON(w, http.StatusOK, summary)
		return
	case endpoint == "sessions" && hasKey:
		result, err = d.Store.SessionDetail(key, filters, rates)
	case endpoint == "sessions":
		result, err = d.Store.SessionsFor(filters, rates)
	case endpoint == "projects" && hasKey:
		result, err = d.Store.ProjectDetail(key, filters, rates, resolution)
	case endpoint == "projects":
		var rows []Row
		if rows, err = d.Store.Query(filters); err == nil {
			result, err = d.Store.Projects(rows, rates)
		}
	case hasKey:
		sendError(w, http.StatusNotFound, "Not found")
		return
	case endpoint == "export":
		var rows []Row
		if rows, err = d.Store.Query(filters); err != nil {
			break
		}
		exported := exportRows(rows, rates)
		if format, ok := raw["format"]; ok && len(format) == 1 && format[0] == "csv" {
			var buf bytes.Buffer
			writer := csv.NewWriter(&buf)
			writer.UseCRLF = true
			if len(exported) > 0 {
				writer.Write(exportFields)
			} else {
				writer.Write([]string{"id", "session", "model", "input", "output", "cached"})
			}
			for _, e := range exported {
				writer.Write(e.csvRecord())
			}
			writer.Flush()
			send(w, http.StatusOK, buf.Bytes(), "text/csv; charset=utf-8", "muse-usage.csv")
			return
		}
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		encoder.Encode(exported)
		send(w, http.StatusOK, bytes.TrimRight(buf.Bytes(), "\n"), "application/json; charset=utf-8", "muse-usage.json")
		return
	default:
		result, err = d.report(filters, echo, rates, resolution)
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	sendJSON(w, http.StatusOK, result)
}

func (d *Dashboard) fail(w http.ResponseWriter, err error) {
	var notFound *NotFound
	var validation *ValidationError
	switch {
	case errors.As(err, &notFound):
		sendError(w, http.StatusNotFound, notFound.Message)
	case errors.As(err, &validation):
		sendError(w, http.StatusBadRequest, validation.Message)
	default:
		fmt.Fprintf(os.Stderr, "meta-cost: dashboard: %v\n", err)
		sendError(w, http.StatusServiceUnavailable, "Usage database temporarily unavailable; retrying shortly.")
	}
}

type dashboardDay struct {
	Day string `json:"day"`
	TimelinePoint
}

func (d *Dashboard) report(f Filters, echo map[string]any, rates []Rate, resolution string) (map[string]any, error) {
	s := d.Store
	rows, err := s.Query(f)
	if err != nil {
		return nil, err
	}
	monthFilters := f
	monthFilters.Period, monthFilters.From, monthFilters.To = "month", "", ""
	monthly, err := s.Query(monthFilters)
	if err != nil {
		return nil, err
	}
	groups := map[string][]Group{}
	for _, field := range []string{"model", "project", "session", "billing_mode", "label"} {
		groups[field] = grouped(rows, field, rates)
	}
	days := []dashboardDay{}
	for _, point := range timeline(rows, rates, "day") {
		days = append(days, dashboardDay{point.Bucket, point})
	}
	usageChart, err := chart(rows, rates, f, resolution)
	if err != nil {
		return nil, err
	}
	sessionRows, err := s.Sessions(rows, rates)
	if err != nil {
		return nil, err
	}
	projectRows, err := s.Projects(rows, rates)
	if err != nil {
		return nil, err
	}
	var lastScan *string
	var scanned sql.NullString
	var errorCount, pending sql.NullInt64
	if err := s.DB.QueryRow("SELECT MAX(scanned_at), SUM(errors), SUM(pending) FROM sources").Scan(&scanned, &errorCount, &pending); err != nil {
		return nil, err
	}
	if scanned.Valid {
		lastScan = &scanned.String
	}
	tail := rows
	if len(tail) > 100 {
		tail = tail[len(tail)-100:]
	}
	provenance := withEstimates(tail, rates)
	for i, j := 0, len(provenance)-1; i < j; i, j = i+1, j-1 {
		provenance[i], provenance[j] = provenance[j], provenance[i]
	}
	options := map[string][]any{}
	for _, field := range []string{"model", "project", "session"} {
		list, err := s.distinct(field)
		if err != nil {
			return nil, err
		}
		options[field] = list
	}
	if options["label"], err = s.labelNames(); err != nil {
		return nil, err
	}
	state := d.State()
	warnings := append([]string{}, state.Warnings...)
	if errorCount.Int64 > 0 {
		warnings = append(warnings, fmt.Sprintf("%d malformed or unsupported records. Run meta-cost doctor for diagnostics.", errorCount.Int64))
	}
	var summaries int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM summaries").Scan(&summaries); err != nil {
		return nil, err
	}
	if summaries > 0 {
		warnings = append(warnings, fmt.Sprintf("%d run summary records are comparison-only and excluded from event totals.", summaries))
	}
	alerts, err := s.AlertStatuses(rates, today())
	if err != nil {
		return nil, err
	}
	var sources int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM sources").Scan(&sources); err != nil {
		return nil, err
	}
	topSessions := append([]SessionSummary{}, sessionRows...)
	sort.SliceStable(topSessions, func(i, j int) bool { return topSessions[i].Equivalent().Cmp(topSessions[j].Equivalent()) > 0 })
	if len(topSessions) > 5 {
		topSessions = topSessions[:5]
	}
	topProjects := append([]ProjectSummary{}, projectRows...)
	sort.SliceStable(topProjects, func(i, j int) bool { return topProjects[i].Equivalent().Cmp(topProjects[j].Equivalent()) > 0 })
	if len(topProjects) > 5 {
		topProjects = topProjects[:5]
	}
	status := map[string]any{"scanning": state.Scanning, "last_success": state.LastSuccess, "error": state.Error,
		"warnings": warnings, "last_scan": lastScan, "pending_lines": pending.Int64, "interval": d.Interval.Seconds()}
	if state.Added != nil {
		status["added"] = *state.Added
	}
	return map[string]any{"filters": echo, "totals": totals(rows, rates), "month": totals(monthly, rates), "groups": groups,
		"timeline": days, "chart": usageChart, "events": provenance, "session_count": len(sessionRows), "top_sessions": topSessions,
		"top_projects": topProjects, "options": options, "alerts": AnnotateAlerts(alerts, d.Config), "status": status,
		"source_count": sources}, nil
}

func (s *Store) distinct(field string) ([]any, error) {
	rows, err := s.DB.Query(fmt.Sprintf("SELECT DISTINCT %s FROM events ORDER BY %s", field, field))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []any{}
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if v.Valid {
			result = append(result, v.String)
		} else {
			result = append(result, nil)
		}
	}
	return result, rows.Err()
}

// errUnsupportedMedia: a JSON content type cannot be sent cross-site without a CORS preflight,
// which is never approved, so requiring it blocks cross-site writes.
var errUnsupportedMedia = errors.New("unsupported media type")

func body(r *http.Request) (any, error) {
	if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
		return nil, errUnsupportedMedia
	}
	if r.ContentLength > maxBody {
		return nil, invalid("Request body too large")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBody {
		return nil, invalid("Request body too large")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	value, err := decodeJSON(data)
	if err != nil {
		return nil, invalid("Request body must be valid JSON")
	}
	return value, nil
}

func alertID(key string) (int64, error) {
	if key == "" || strings.Trim(key, "0123456789") != "" {
		return 0, &NotFound{"Unknown alert: " + key}
	}
	id, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return 0, &NotFound{"Unknown alert: " + key}
	}
	return id, nil
}

func (d *Dashboard) write(w http.ResponseWriter, r *http.Request) {
	if !d.allowed(r) {
		sendError(w, http.StatusForbidden, "Local dashboard requests only.")
		return
	}
	path := r.URL.EscapedPath()
	if r.Method == http.MethodPost && path == "/api/refresh" {
		d.Refresh()
		sendJSON(w, http.StatusAccepted, map[string]bool{"scanning": true})
		return
	}
	route := apiPattern.FindStringSubmatch(path)
	if route == nil {
		sendError(w, http.StatusNotFound, "Not found")
		return
	}
	endpoint, hasKey := route[1], route[2] != ""
	key, err := url.PathUnescape(route[2])
	if err != nil {
		sendError(w, http.StatusNotFound, "Not found")
		return
	}
	var handler func() (int, any, error)
	switch {
	case r.Method == http.MethodPost && endpoint == "alerts" && !hasKey:
		handler = func() (int, any, error) {
			b, err := body(r)
			if err != nil {
				return 0, nil, err
			}
			id, err := d.Store.CreateAlert(b)
			return http.StatusCreated, map[string]int64{"id": id}, err
		}
	case r.Method == http.MethodPut && endpoint == "alerts" && hasKey:
		handler = func() (int, any, error) {
			id, err := alertID(key)
			if err != nil {
				return 0, nil, err
			}
			b, err := body(r)
			if err != nil {
				return 0, nil, err
			}
			return http.StatusOK, nil, d.Store.UpdateAlert(id, b)
		}
	case r.Method == http.MethodDelete && endpoint == "alerts" && hasKey:
		handler = func() (int, any, error) {
			id, err := alertID(key)
			if err != nil {
				return 0, nil, err
			}
			return http.StatusOK, nil, d.Store.DeleteAlert(id)
		}
	case r.Method == http.MethodPut && endpoint == "sessions" && hasKey:
		handler = func() (int, any, error) {
			b, err := body(r)
			if err != nil {
				return 0, nil, err
			}
			return http.StatusOK, nil, d.Store.SetSessionLabel(key, b)
		}
	case r.Method == http.MethodPut && endpoint == "pricing" && hasKey:
		handler = func() (int, any, error) {
			b, err := body(r)
			if err != nil {
				return 0, nil, err
			}
			return http.StatusOK, nil, d.Store.SetLocalRate(key, b)
		}
	case r.Method == http.MethodDelete && endpoint == "pricing" && hasKey:
		handler = func() (int, any, error) { return http.StatusOK, nil, d.Store.DeleteLocalRate(key) }
	default:
		sendError(w, http.StatusNotFound, "Not found")
		return
	}
	code, result, err := handler()
	if errors.Is(err, errUnsupportedMedia) {
		sendError(w, http.StatusUnsupportedMediaType, "Send JSON with Content-Type: application/json.")
		return
	}
	if err != nil {
		d.fail(w, err)
		return
	}
	if result == nil {
		result = map[string]bool{"ok": true}
	}
	sendJSON(w, code, result)
}

// openBrowser opens the dashboard in the Windows browser from WSL, or the desktop browser elsewhere.
func openBrowser(target string) bool {
	run := func(name string, args ...string) bool {
		if _, err := exec.LookPath(name); err != nil {
			return false
		}
		cmd := exec.Command(name, args...)
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			return false
		}
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			return err == nil
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			return false
		}
	}
	return run("wslview", target) || run("powershell.exe", "-NoProfile", "-Command", "Start-Process", target) ||
		run("xdg-open", target) || run("open", target)
}

// RunDashboard serves the dashboard until interrupted, falling back to a free port when the
// requested one is taken.
func RunDashboard(store *Store, roots []string, config Config, rates []Rate, filters map[string]string,
	interval time.Duration, web fs.FS, port int, browser bool, out io.Writer, stop <-chan struct{}) error {
	d, err := NewDashboard(store, roots, config, rates, filters, interval, web, port)
	if err != nil {
		if !errors.Is(err, syscall.EADDRINUSE) || port == 0 {
			return err
		}
		fmt.Fprintf(out, "Port %d is in use; selecting a free local port.\n", port)
		if d, err = NewDashboard(store, roots, config, rates, filters, interval, web, 0); err != nil {
			return err
		}
	}
	defer d.Close()
	target := fmt.Sprintf("http://127.0.0.1:%d/", d.Port())
	if browser {
		fmt.Fprintf(out, "Muse Cost dashboard: %s\nOpening your browser…\n", target)
	} else {
		fmt.Fprintf(out, "Muse Cost dashboard: %s\n", target)
	}
	fmt.Fprintln(out, "Keep this command running for live updates. Press Ctrl+C to stop.")
	if browser {
		go func() {
			if !openBrowser(target) {
				fmt.Fprintf(out, "Could not open a browser automatically. Open %s\n", target)
			}
		}()
	}
	served := make(chan error, 1)
	go func() { served <- d.Serve() }()
	select {
	case err := <-served:
		return err
	case <-stop:
		return nil
	}
}
