package costcenter

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// A turn is one Muse run: a prompt and every model completion it led to, including those of the
// subagents it started. Muse logs which run each completion belongs to, and where each prompt
// is, but not the reasoning effort a run was sent with; the UserPromptSubmit hook records that.

// effortPattern accepts Muse's effort tiers and any similar future name, and nothing else.
var effortPattern = regexp.MustCompile(`^[a-z][a-z_]{0,19}$`)

// EffortSetting is the effort Muse was configured with when a prompt was submitted.
type EffortSetting struct {
	Effort  string            // --reasoning-effort, or the settings' reasoning_effort
	Efforts map[string]string // the settings' model_reasoning_efforts, which /effort changes
	Model   string            // the model Muse was set to, when known
	Source  string            // flag, settings, or default
}

// museArgs is what the hook needs from the Muse process's command line.
func museArgs(pid int) (effort, model string) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return "", ""
	}
	args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	for i, arg := range args {
		name, value, inline := strings.Cut(arg, "=")
		if !inline {
			if i+1 >= len(args) {
				continue
			}
			value = args[i+1]
		}
		switch name {
		case "--reasoning-effort", "--effort":
			effort = value
		case "--model":
			model = value
		}
	}
	return effort, model
}

// museEffort reads the effort the Muse process will send: a command-line flag wins, then the
// model's entry in model_reasoning_efforts, then reasoning_effort, then Muse's default of high.
func museEffort(pid int, env museEnv) EffortSetting {
	flag, model := museArgs(pid)
	if effortPattern.MatchString(flag) {
		return EffortSetting{Effort: flag, Model: model, Source: "flag"}
	}
	setting := EffortSetting{Model: model, Source: "default", Effort: "high"}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(env.authPath()), "settings.json"))
	if err != nil {
		return setting
	}
	var settings struct {
		Model   string            `json:"model"`
		Effort  string            `json:"reasoning_effort"`
		Efforts map[string]string `json:"model_reasoning_efforts"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return setting
	}
	if setting.Model == "" {
		setting.Model = settings.Model
	}
	if effortPattern.MatchString(settings.Effort) {
		setting.Effort, setting.Source = settings.Effort, "settings"
	}
	for model, effort := range settings.Efforts {
		if effortPattern.MatchString(effort) && len(model) <= 200 {
			if setting.Efforts == nil {
				setting.Efforts = map[string]string{}
			}
			setting.Efforts[model], setting.Source = effort, "settings"
		}
	}
	return setting
}

// For is the effort used with model, which is only known once the turn's completions are logged.
func (e EffortSetting) For(model string) string {
	if effort, ok := e.Efforts[model]; ok && e.Source != "flag" {
		return effort
	}
	return e.Effort
}

// promptInput is the part of Muse's UserPromptSubmit payload the hook uses. The payload also
// holds the prompt, which is deliberately not decoded.
type promptInput struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Model     string `json:"model"`
}

func recordPromptSubmit(stdin io.Reader, dbPath string) error {
	var input promptInput
	data, _ := io.ReadAll(io.LimitReader(stdin, 16<<20))
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("unreadable UserPromptSubmit payload")
	}
	if input.SessionID == "" {
		input.SessionID = os.Getenv("MUSE_SESSION_ID")
	}
	if !sessionIDPattern.MatchString(input.SessionID) || !sessionIDPattern.MatchString(input.TurnID) {
		return fmt.Errorf("UserPromptSubmit payload has no usable session_id or turn_id")
	}
	pid, err := museProcess(os.Getppid())
	if err != nil {
		return err
	}
	env, err := processEnv(pid)
	if err != nil {
		return err
	}
	setting := museEffort(pid, env)
	if input.Model != "" && input.Model != "unknown" && len(input.Model) <= 200 {
		setting.Model = input.Model
	}
	store, err := openStore(dbPath, hookBusyMS)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.RecordTurnSetting(input.SessionID, input.TurnID, setting)
}

// HookPromptSubmit is `meta-cost hook prompt-submit`, run by Muse before each prompt is sent. Like
// the SessionStart hook it gives up after hookDeadline and never prints on stdout.
func HookPromptSubmit(stdin io.Reader, dbPath string, stderr io.Writer) {
	done := make(chan error, 1)
	go func() { done <- recordPromptSubmit(stdin, dbPath) }()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintf(stderr, "meta-cost hook: effort not recorded: %v\n", err)
		}
	case <-time.After(hookDeadline):
		fmt.Fprintln(stderr, "meta-cost hook: effort not recorded: gave up waiting for the database")
	}
}

// RecordTurnSetting keeps the first setting seen for a turn; a prompt that steers a running turn
// does not change the effort the turn started with.
func (s *Store) RecordTurnSetting(session, turn string, e EffortSetting) error {
	var efforts any
	if len(e.Efforts) > 0 {
		data, err := json.Marshal(e.Efforts)
		if err != nil {
			return err
		}
		efforts = string(data)
	}
	_, err := s.DB.Exec("INSERT OR IGNORE INTO turn_settings (turn, session, recorded_at, model, effort, efforts, source) VALUES (?,?,?,?,?,?,?)",
		turn, session, nowISO(), nullString(e.Model), e.Effort, efforts, e.Source)
	return err
}

// promptLimit is how much of a prompt the dashboard shows.
const promptLimit = 280

type promptText struct {
	text      string
	truncated bool
}

// promptCache holds prompt previews by log position, so that the page's polling does not reread
// the log. Previews live only in memory.
var promptCache = struct {
	sync.Mutex
	entries map[string]*promptText
}{entries: map[string]*promptText{}}

var promptFields = keep{"payload": keep{"intent_id": nil, "refill_blocks": nil, "model_messages": nil}}

// readPrompt reads the start of a prompt from its record in the session log. It returns nil when
// the log has gone or no longer holds the prompt there.
func readPrompt(source string, offset int64, intent string) *promptText {
	key := fmt.Sprintf("%s\x00%d\x00%s", source, offset, intent)
	promptCache.Lock()
	cached, ok := promptCache.entries[key]
	promptCache.Unlock()
	if ok {
		return cached
	}
	result := loadPrompt(source, offset, intent)
	promptCache.Lock()
	if len(promptCache.entries) > 10000 {
		promptCache.entries = map[string]*promptText{}
	}
	promptCache.entries[key] = result
	promptCache.Unlock()
	return result
}

func loadPrompt(source string, offset int64, intent string) *promptText {
	f, err := os.Open(source)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	line, err := bufio.NewReaderSize(io.LimitReader(f, 64<<20), 1<<16).ReadBytes('\n')
	if err != nil {
		return nil
	}
	value, err := decodeSelective(line, promptFields)
	if err != nil {
		return nil
	}
	record, _ := value.(map[string]any)
	payload, _ := record["payload"].(map[string]any)
	if id, _ := payload["intent_id"].(string); id != intent {
		return nil
	}
	var parts []string
	texts := func(blocks any) {
		list, _ := blocks.([]any)
		for _, block := range list {
			b, _ := block.(map[string]any)
			if text, ok := b["text"].(string); ok && b["kind"] == "text" {
				parts = append(parts, text)
			}
		}
	}
	texts(payload["refill_blocks"])
	if len(parts) == 0 {
		messages, _ := payload["model_messages"].([]any)
		for _, message := range messages {
			m, _ := message.(map[string]any)
			texts(m["content"])
		}
	}
	return preview(strings.Join(parts, " "))
}

// preview collapses whitespace and cuts the text to promptLimit characters.
func preview(text string) *promptText {
	text = strings.Join(strings.FieldsFunc(text, unicode.IsSpace), " ")
	runes := []rune(text)
	if len(runes) <= promptLimit {
		return &promptText{text: text}
	}
	return &promptText{text: strings.TrimRightFunc(string(runes[:promptLimit]), unicode.IsSpace), truncated: true}
}

// Turn is one prompt and what it cost. Its totals include the subagents it started.
type Turn struct {
	ID              string   `json:"id"`
	Started         string   `json:"started"`
	Ended           string   `json:"ended"` // the last completion, the subagents' included
	Prompt          *string  `json:"prompt"`
	PromptTruncated bool     `json:"prompt_truncated"`
	Prompts         int      `json:"prompts"`
	Models          []string `json:"models"`
	Effort          *string  `json:"effort"`
	EffortSource    *string  `json:"effort_source"`
	Completions     int      `json:"completions"`
	Subagents       int      `json:"subagents"`
	// RecordedBy is the tool that reported this turn with `meta-cost record`, which has no prompt.
	RecordedBy *string `json:"recorded_by"`
	// SubagentSessions are the subagents counted in this turn, so the page can list their completions.
	SubagentSessions []string `json:"subagent_sessions"`
	Totals
}

type promptRow struct {
	intent, turn, recordedAt, source string
	offset                           int64
}

// Turns groups a session's usage by turn. Each subagent's usage goes to the turn that was running
// when the subagent made its first completion, and usage logged without a turn, by a version of
// this tool that did not record one, is grouped under an empty ID.
func (s *Store) Turns(session string, rows []Row, rates []Rate) ([]Turn, error) {
	prompts := map[string][]promptRow{}
	result, err := s.DB.Query("SELECT intent, COALESCE(turn, intent), recorded_at, source, offset FROM prompts WHERE session=? ORDER BY recorded_at", session)
	if err != nil {
		return nil, err
	}
	for result.Next() {
		var p promptRow
		var source sql.NullString
		var offset sql.NullInt64
		if err := result.Scan(&p.intent, &p.turn, &p.recordedAt, &source, &offset); err != nil {
			result.Close()
			return nil, err
		}
		p.source, p.offset = source.String, offset.Int64
		prompts[p.turn] = append(prompts[p.turn], p)
	}
	result.Close()
	settings := map[string]EffortSetting{}
	result, err = s.DB.Query("SELECT turn, COALESCE(model, ''), effort, efforts, source FROM turn_settings WHERE session=?", session)
	if err != nil {
		return nil, err
	}
	for result.Next() {
		var turn, model, effort, source string
		var efforts sql.NullString
		if err := result.Scan(&turn, &model, &effort, &efforts, &source); err != nil {
			result.Close()
			return nil, err
		}
		e := EffortSetting{Effort: effort, Model: model, Source: source}
		if efforts.Valid {
			json.Unmarshal([]byte(efforts.String), &e.Efforts)
		}
		settings[turn] = e
	}
	result.Close()

	// The session's own turns, in order, each starting at its prompt or else its first completion.
	order, own := bucket(rows, func(r Row) string {
		if r.Session != session {
			return "\x00"
		}
		return r.Turn
	})
	starts := map[string]string{}
	var ids []string
	for _, id := range order {
		if id == "\x00" {
			continue
		}
		ids = append(ids, id)
		starts[id] = own[id][0].RecordedAt
		if p := prompts[id]; len(p) > 0 && id != "" {
			starts[id] = p[0].recordedAt
		}
	}
	sort.SliceStable(ids, func(i, j int) bool { return starts[ids[i]] < starts[ids[j]] })
	members := map[string][]Row{}
	for _, id := range ids {
		members[id] = own[id]
	}
	// Subagents: the turn running at the subagent's first completion, compared as instants.
	subagents := map[string]map[string]bool{}
	_, children := bucket(own["\x00"], func(r Row) string { return r.Session })
	for child, childRows := range children {
		first := localTime(childRows[0].RecordedAt)
		target := ""
		for _, id := range ids {
			if id != "" && !localTime(starts[id]).After(first) {
				target = id
			}
		}
		if target == "" && len(ids) > 0 {
			target = ids[0]
		}
		if _, ok := members[target]; !ok {
			ids = append(ids, target)
			starts[target] = childRows[0].RecordedAt
		}
		members[target] = append(members[target], childRows...)
		if subagents[target] == nil {
			subagents[target] = map[string]bool{}
		}
		subagents[target][child] = true
	}

	turns := []Turn{}
	for _, id := range ids {
		t := Turn{ID: id, Started: starts[id], Models: ranked(own[id], "model"), Completions: len(own[id]),
			Subagents: len(subagents[id]), SubagentSessions: []string{}, Totals: totals(members[id], rates)}
		for child := range subagents[id] {
			t.SubagentSessions = append(t.SubagentSessions, child)
		}
		sort.Strings(t.SubagentSessions)
		for _, r := range members[id] {
			if t.Ended == "" || localTime(r.RecordedAt).After(localTime(t.Ended)) {
				t.Ended = r.RecordedAt
			}
		}
		if rows := own[id]; len(rows) > 0 && strings.HasPrefix(rows[0].Source, "record:") {
			tool := strings.TrimPrefix(rows[0].Source, "record:")
			t.RecordedBy = &tool
		}
		if id != "" {
			t.Prompts = len(prompts[id])
			for _, p := range prompts[id] {
				if text := readPrompt(p.source, p.offset, p.intent); text != nil {
					t.Prompt, t.PromptTruncated = &text.text, text.truncated
					break
				}
			}
			if e, ok := settings[id]; ok {
				model := e.Model
				if len(t.Models) > 0 {
					model = t.Models[0]
				}
				effort, source := e.For(model), e.Source
				t.Effort, t.EffortSource = &effort, &source
			}
		}
		turns = append(turns, t)
	}
	return turns, nil
}
