package costcenter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type webFixture struct {
	*fixture
	dashboard *Dashboard
	url       string
}

// newWebFixture serves the dashboard over the fixture's logs, with one event already collected.
func newWebFixture(t *testing.T, config func(*fixture) Config) *webFixture {
	t.Helper()
	f := newFixture(t)
	f.write(defaultEvent(), "")
	var c Config
	if config != nil {
		c = config(f)
	}
	d, err := NewDashboard(f.store, []string{f.logs}, c, f.rates, map[string]string{"period": "all"},
		100*time.Millisecond, os.DirFS("../../web"), 0)
	if err != nil {
		t.Fatal(err)
	}
	go d.Serve()
	t.Cleanup(d.Close)
	w := &webFixture{fixture: f, dashboard: d, url: fmt.Sprintf("http://127.0.0.1:%d", d.Port())}
	for i := 0; i < 100 && d.State().LastSuccess == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if d.State().LastSuccess == nil {
		t.Fatal("the first scan did not finish")
	}
	return w
}

func (w *webFixture) request(method, path, body string, headers map[string]string) (int, []byte, http.Header) {
	w.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, w.url+path, reader)
	if err != nil {
		w.t.Fatal(err)
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

func (w *webFixture) get(path string, headers map[string]string) (int, []byte, http.Header) {
	return w.request("GET", path, "", headers)
}

func (w *webFixture) getJSON(path string, target any) {
	w.t.Helper()
	code, data, _ := w.get(path, nil)
	if code != 200 {
		w.t.Fatalf("GET %s: %d %s", path, code, data)
	}
	if err := json.Unmarshal(data, target); err != nil {
		w.t.Fatal(err)
	}
}

// send makes a JSON write request and returns its status and decoded body.
func (w *webFixture) send(method, path, body string, headers map[string]string) (int, map[string]any) {
	w.t.Helper()
	all := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		all[k] = v
	}
	code, data, _ := w.request(method, path, body, all)
	var decoded map[string]any
	json.Unmarshal(data, &decoded)
	return code, decoded
}

func (w *webFixture) dashboardJSON(path string) map[string]any {
	var data map[string]any
	w.getJSON(path, &data)
	return data
}

func totalsOf(data map[string]any) map[string]any { return data["totals"].(map[string]any) }

func TestReactRoutesAndBootstrap(t *testing.T) {
	w := newWebFixture(t, nil)
	_, shell, _ := w.get("/", nil)
	if !bytes.Contains(shell, []byte(`id="root"`)) {
		t.Fatal("no React root")
	}
	for _, path := range []string{"/breakdown", "/activity", "/sessions", "/sessions/parent", "/projects", "/projects/my%20project"} {
		if _, data, _ := w.get(path, nil); !bytes.Equal(data, shell) {
			t.Errorf("%s is not the app shell", path)
		}
	}
	for _, path := range []string{"/breakdown/x", "/sessions/a/b", "/nope"} {
		if code, _, _ := w.get(path, nil); code != 404 {
			t.Errorf("%s: %d", path, code)
		}
	}
	var config struct {
		Filters    map[string]any `json:"filters"`
		InstanceID string         `json:"instance_id"`
	}
	w.getJSON("/api/config", &config)
	if config.Filters["period"] != "all" || config.Filters["project"] != nil || config.InstanceID != w.dashboard.InstanceID {
		t.Fatalf("%+v", config)
	}
	if code, _, _ := w.get("/app.js", nil); code != 200 {
		t.Fatalf("app.js: %d", code)
	}
	if code, _, headers := w.get("/logo.png", nil); code != 200 || headers.Get("Content-Type") != "image/png" {
		t.Fatalf("logo: %d %s", code, headers.Get("Content-Type"))
	}
}

func TestExplicitFiltersCanClearLaunchDefaults(t *testing.T) {
	w := newWebFixture(t, nil)
	w.dashboard.Filters = map[string]string{"period": "all", "project": "missing"}
	if events := totalsOf(w.dashboardJSON("/api/dashboard"))["events"]; events != 0.0 {
		t.Fatalf("launch filter ignored: %v events", events)
	}
	data := w.dashboardJSON("/api/dashboard?period=all")
	if totalsOf(data)["events"] != 1.0 || data["filters"].(map[string]any)["project"] != nil {
		t.Fatalf("%v %v", totalsOf(data), data["filters"])
	}
}

func TestWebFiltersRefreshAndExports(t *testing.T) {
	w := newWebFixture(t, nil)
	code, page, headers := w.get("/", nil)
	if code != 200 || !bytes.Contains(page, []byte("Usage overview")) {
		t.Fatalf("%d", code)
	}
	if !strings.Contains(headers.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("CSP %q", headers.Get("Content-Security-Policy"))
	}
	if input := totalsOf(w.dashboardJSON("/api/dashboard"))["input"]; input != 100.0 {
		t.Fatalf("input %v", input)
	}
	if events := totalsOf(w.dashboardJSON("/api/dashboard?model=missing"))["events"]; events != 0.0 {
		t.Fatalf("events %v", events)
	}
	w.write(event("appended", "parent", 50, 20, "test-model"), "")
	w.request("POST", "/api/refresh", "", nil)
	var data map[string]any
	for i := 0; i < 100; i++ {
		if data = w.dashboardJSON("/api/dashboard?session=parent"); totalsOf(data)["input"] == 150.0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if totalsOf(data)["input"] != 150.0 || data["filters"].(map[string]any)["session"] != "parent" {
		t.Fatalf("%v %v", totalsOf(data), data["filters"])
	}
	var exported []map[string]any
	w.getJSON("/api/export", &exported)
	if _, ok := exported[0]["source"]; ok {
		t.Fatal("export exposes source paths")
	}
	if _, body, _ := w.get("/api/dashboard", nil); bytes.Contains(body, []byte("SECRET_")) {
		t.Fatal("dashboard exposes prompt text")
	}
	if _, csv, _ := w.get("/api/export?format=csv", nil); !bytes.Contains(csv, []byte("input")) {
		t.Fatal("CSV export has no header")
	}
}

func TestDrillDownEndpoints(t *testing.T) {
	w := newWebFixture(t, nil)
	w.write(event("child-event", "child", 50, 20, "test-model"), filepath.Join(w.logs, "parent/subagent/child/session.jsonl"))
	w.request("POST", "/api/refresh", "", nil)
	var listed []map[string]any
	for i := 0; i < 100; i++ {
		if w.getJSON("/api/sessions?period=all", &listed); len(listed) > 0 && listed[0]["input"] == 150.0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(listed) != 1 || listed[0]["id"] != "parent" || listed[0]["input"] != 150.0 || listed[0]["subagents"] != 1.0 {
		t.Fatalf("%v", listed)
	}
	detail := w.dashboardJSON("/api/sessions/parent?period=all")
	if detail["tree"].(map[string]any)["children"].([]any)[0].(map[string]any)["id"] != "child" {
		t.Fatalf("tree %v", detail["tree"])
	}
	if _, body, _ := w.get("/api/sessions/parent?period=all", nil); bytes.Contains(body, []byte("SECRET_")) {
		t.Fatal("session detail exposes prompt text")
	}
	var projects []map[string]any
	w.getJSON("/api/projects?period=all", &projects)
	project := w.dashboardJSON("/api/projects/" + url.PathEscape(projects[0]["label"].(string)) + "?period=all")
	if totalsOf(project)["input"] != 150.0 {
		t.Fatalf("project %v", totalsOf(project))
	}
	dashboard := w.dashboardJSON("/api/dashboard?period=all")
	if dashboard["session_count"] != 1.0 || dashboard["top_sessions"].([]any)[0].(map[string]any)["id"] != "parent" {
		t.Fatalf("%v %v", dashboard["session_count"], dashboard["top_sessions"])
	}
	for path, want := range map[string]int{"/api/sessions/missing": 404, "/api/projects/missing": 404,
		"/api/sessions?period=bad": 400, "/api/sessions/parent?from=bad": 400, "/api/dashboard/x": 404} {
		if code, _, _ := w.get(path, nil); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
}

func TestPricingAndAlertAPI(t *testing.T) {
	w := newWebFixture(t, nil)
	for _, page := range []string{"/pricing", "/alerts"} {
		if code, _, _ := w.get(page, nil); code != 200 {
			t.Fatalf("%s: %d", page, code)
		}
	}
	var models []map[string]any
	w.getJSON("/api/pricing", &models)
	if len(models) != 1 || models[0]["model"] != "test-model" || models[0]["source"] != "public" {
		t.Fatalf("%v", models)
	}
	expect := func(want int, method, path, body string, headers map[string]string) map[string]any {
		t.Helper()
		code, data := w.send(method, path, body, headers)
		if code != want {
			t.Fatalf("%s %s: %d, want %d (%v)", method, path, code, want, data)
		}
		return data
	}
	expect(200, "PUT", "/api/pricing/test-model", `{"input_per_million":"0","cached_input_per_million":"0","output_per_million":"0"}`, nil)
	if w.getJSON("/api/pricing", &models); models[0]["source"] != "local" {
		t.Fatalf("%v", models)
	}
	if usd := totalsOf(w.dashboardJSON("/api/dashboard?period=all"))["unknown_mode_equivalent_usd"]; usd != "0" {
		t.Fatalf("usd %v", usd)
	}
	expect(200, "DELETE", "/api/pricing/test-model", `{}`, nil)
	expect(404, "DELETE", "/api/pricing/test-model", `{}`, nil)
	// A high local rate makes the fixture's few tokens pass a one-cent limit.
	expect(200, "PUT", "/api/pricing/test-model", `{"input_per_million":"100000","cached_input_per_million":"10000","output_per_million":"200000"}`, nil)
	expect(400, "POST", "/api/alerts", `{"period":"day","limit_usd":"0.001"}`, nil)
	created := expect(201, "POST", "/api/alerts", `{"period":"day","limit_usd":"0.01"}`, nil)
	id := int64(created["id"].(float64))
	alerts := w.dashboardJSON("/api/dashboard?period=all")["alerts"].([]any)
	if len(alerts) != 1 || alerts[0].(map[string]any)["id"] != float64(id) || alerts[0].(map[string]any)["level"] != "exceeded" {
		t.Fatalf("%v", alerts)
	}
	expect(200, "PUT", fmt.Sprintf("/api/alerts/%d", id), `{"period":"day","limit_usd":"5","enabled":false}`, nil)
	var listed []map[string]any
	if w.getJSON("/api/alerts", &listed); listed[0]["level"] != "paused" {
		t.Fatalf("%v", listed)
	}
	expect(400, "POST", "/api/alerts", `{"period":"day","limit_usd":"-1"}`, nil)
	expect(404, "PUT", "/api/alerts/999", `{"period":"day","limit_usd":"1"}`, nil)
	expect(404, "PUT", "/api/alerts/abc", `{"period":"day","limit_usd":"1"}`, nil)
	expect(404, "POST", "/api/pricing", `{}`, nil)
	// Cross-site writes: a form-encoded body or a foreign Origin is refused.
	expect(415, "POST", "/api/alerts", `{"period":"day","limit_usd":"1"}`, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	expect(403, "POST", "/api/alerts", `{"period":"day","limit_usd":"1"}`, map[string]string{"Origin": "https://evil.example"})
	expect(200, "DELETE", fmt.Sprintf("/api/alerts/%d", id), `{}`, nil)
	if w.getJSON("/api/alerts", &listed); len(listed) != 0 {
		t.Fatalf("%v", listed)
	}
}

func TestOccupiedPortPreservesOriginalError(t *testing.T) {
	w := newWebFixture(t, nil)
	_, err := NewDashboard(w.store, []string{w.logs}, Config{}, w.rates, nil, time.Second, os.DirFS("../../web"), w.dashboard.Port())
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("%v", err)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestShowFallsBackToAFreePort(t *testing.T) {
	w := newWebFixture(t, nil)
	occupied := w.dashboard.Port()
	store, err := OpenStore(filepath.Join(w.root, "fallback.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var out lockedBuffer
	stop := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- RunDashboard(store, []string{w.logs}, Config{}, w.rates, map[string]string{"period": "all"},
			100*time.Millisecond, os.DirFS("../../web"), occupied, false, &out, stop)
	}()
	var target string
	for i := 0; i < 100 && target == ""; i++ {
		for _, line := range strings.Split(out.String(), "\n") {
			if rest, ok := strings.CutPrefix(line, "Muse Cost dashboard: "); ok {
				target = rest
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("Port %d is in use", occupied)) {
		t.Fatalf("output %q", out.String())
	}
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(parsed.Host)
	if port == "" || port == "0" || port == fmt.Sprint(occupied) || parsed.RawQuery != "" {
		t.Fatalf("chose %q", target)
	}
	// Failed initialization must leave the existing service untouched.
	if code, _, _ := w.get("/", nil); code != 200 {
		t.Fatalf("%d", code)
	}
}

func TestInvalidFiltersAndForeignRequests(t *testing.T) {
	w := newWebFixture(t, nil)
	for _, path := range []string{"/api/dashboard?period=bad", "/api/dashboard?from=bad", "/api/dashboard?billing_mode=bad"} {
		if code, _, _ := w.get(path, nil); code != 400 {
			t.Errorf("%s: %d", path, code)
		}
	}
	for _, headers := range []map[string]string{{"Host": "evil.example"}, {"Origin": "https://evil.example"}} {
		if code, _, _ := w.get("/api/dashboard", headers); code != 403 {
			t.Errorf("%v: %d", headers, code)
		}
	}
}

func TestSessionLabels(t *testing.T) {
	w := newWebFixture(t, nil)
	w.write(event("child-event", "child", 50, 20, "test-model"), filepath.Join(w.logs, "parent/subagent/child/session.jsonl"))
	w.request("POST", "/api/refresh", "", nil)
	for i := 0; i < 100; i++ {
		if code, _, _ := w.get("/api/sessions/child?period=all", nil); code == 200 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	expect := func(want int, method, path, body string) {
		t.Helper()
		if code, data := w.send(method, path, body, nil); code != want {
			t.Fatalf("%s %s: %d, want %d (%v)", method, path, code, want, data)
		}
	}
	expect(200, "PUT", "/api/sessions/parent", `{"name":"  Fix   invoice export ","label":"invoicing"}`)
	expect(400, "PUT", "/api/sessions/child", `{"label":"invoicing"}`)
	expect(400, "PUT", "/api/sessions/parent", `{"label":"Unlabelled"}`)
	expect(400, "PUT", "/api/sessions/parent", `{"label":7}`)
	expect(404, "PUT", "/api/sessions/missing", `{"label":"invoicing"}`)

	var listed []map[string]any
	w.getJSON("/api/sessions?period=all", &listed)
	if len(listed) != 1 || listed[0]["name"] != "Fix invoice export" || listed[0]["label"] != "invoicing" {
		t.Fatalf("%v", listed)
	}
	// The subagent's usage counts towards its top-level session's label.
	child := w.dashboardJSON("/api/sessions/child?period=all")
	if child["label"] != "invoicing" || child["name"] != "Fix invoice export" {
		t.Fatalf("child %v %v", child["name"], child["label"])
	}
	dashboard := w.dashboardJSON("/api/dashboard?period=all&label=invoicing")
	groups := dashboard["groups"].(map[string]any)["label"].([]any)
	if totalsOf(dashboard)["input"] != 150.0 || len(groups) != 1 || groups[0].(map[string]any)["label"] != "invoicing" {
		t.Fatalf("%v %v", totalsOf(dashboard), groups)
	}
	if labels := dashboard["options"].(map[string]any)["label"].([]any); len(labels) != 1 || labels[0] != "invoicing" {
		t.Fatalf("options %v", labels)
	}
	if events := totalsOf(w.dashboardJSON("/api/dashboard?period=all&label=unlabelled"))["events"]; events != 0.0 {
		t.Fatalf("unlabelled events %v", events)
	}
	// Clearing both removes the label, and the usage is unlabelled again.
	expect(200, "PUT", "/api/sessions/parent", `{"name":"","label":""}`)
	if events := totalsOf(w.dashboardJSON("/api/dashboard?period=all&label=unlabelled"))["events"]; events != 2.0 {
		t.Fatalf("unlabelled events %v", events)
	}
}
