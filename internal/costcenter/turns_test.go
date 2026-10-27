package costcenter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// intent is a prompt record like Muse writes; materialized names the turn it went to.
func intent(id, session, text string, at time.Time) map[string]any {
	return map[string]any{"id": "record-" + id, "stream": map[string]any{"id": session}, "sequence": 1,
		"recorded_at": at.UnixMicro(), "payload_type": "runtime.user_intent.accepted",
		"payload": map[string]any{"intent_id": id, "refill_blocks": []any{map[string]any{"kind": "text", "text": text}},
			"model_messages": []any{map[string]any{"content": []any{map[string]any{"kind": "text", "text": text}}}}}}
}

func materialized(id, turn string, at time.Time) map[string]any {
	return map[string]any{"id": "materialized-" + id, "stream": map[string]any{"id": "parent"}, "sequence": 2,
		"recorded_at": at.UnixMicro(), "payload_type": "runtime.user_intent.materialized",
		"payload": map[string]any{"intent_id": id, "outcome": map[string]any{"kind": "top_level_turn_started", "run_id": turn}}}
}

func completion(id, session, turn string, input int64, at time.Time) map[string]any {
	record := event(id, session, input, 0, "test-model")
	record["recorded_at"] = at.UnixMicro()
	record["payload"].(map[string]any)["run_id"] = turn
	return record
}

func TestTurnsGroupPromptsCompletionsAndSubagents(t *testing.T) {
	f := newFixture(t)
	start := time.Now().Add(-time.Hour)
	at := func(minutes int) time.Time { return start.Add(time.Duration(minutes) * time.Minute) }
	long := "Write   the\nreport. " + strings.Repeat("word ", 100) + "SECRET_TAIL"
	f.write(intent("turn-a", "parent", long, at(0)), "")
	f.write(materialized("turn-a", "turn-a", at(0)), "")
	f.write(completion("a1", "parent", "turn-a", 100, at(1)), "")
	// A prompt sent while turn A runs steers it rather than starting a turn.
	f.write(intent("steer", "parent", "also check the tests", at(2)), "")
	f.write(materialized("steer", "turn-a", at(2)), "")
	f.write(completion("a2", "parent", "turn-a", 200, at(3)), "")
	f.write(intent("turn-b", "parent", "1+1", at(10)), "")
	f.write(materialized("turn-b", "turn-b", at(10)), "")
	f.write(completion("b1", "parent", "turn-b", 40, at(11)), "")
	f.write(completion("c1", "child", "child-run", 7, at(12)), filepath.Join(f.logs, "parent/subagent/child/session.jsonl"))
	f.scan(nil)
	if err := f.store.RecordTurnSetting("parent", "turn-b", EffortSetting{Effort: "xhigh", Source: "settings",
		Efforts: map[string]string{"test-model": "medium"}}); err != nil {
		t.Fatal(err)
	}
	detail, err := f.store.SessionDetail("parent", Filters{Period: "all"}, f.rates)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Turns) != 2 {
		t.Fatalf("%d turns", len(detail.Turns))
	}
	a, b := detail.Turns[0], detail.Turns[1]
	if a.ID != "turn-a" || a.Completions != 2 || a.Input != 300 || a.Prompts != 2 || a.Prompt == nil || !a.PromptTruncated ||
		!strings.HasPrefix(*a.Prompt, "Write the report. word") || len([]rune(*a.Prompt)) > promptLimit || a.Effort != nil {
		t.Fatalf("turn a %+v", a)
	}
	// A turn ends at its last completion, a subagent's included.
	if !localTime(a.Ended).Equal(at(3).Truncate(time.Microsecond)) || !localTime(b.Ended).Equal(at(12).Truncate(time.Microsecond)) {
		t.Fatalf("turns end at %s and %s", a.Ended, b.Ended)
	}
	// The subagent started during turn B, so its usage counts there.
	if b.ID != "turn-b" || b.Completions != 1 || b.Subagents != 1 || b.Input != 47 || *b.Prompt != "1+1" ||
		*b.Effort != "medium" || *b.EffortSource != "settings" {
		t.Fatalf("turn b %+v", b)
	}
	// Prompts are read from the log, never stored.
	f.store.Close()
	data, err := os.ReadFile(filepath.Join(f.root, "db.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	wal, _ := os.ReadFile(filepath.Join(f.root, "db.sqlite3-wal"))
	data = append(data, wal...)
	f.open()
	for _, text := range []string{"Write", "also check", "1+1", "SECRET"} {
		if strings.Contains(string(data), text) {
			t.Fatalf("database contains %q", text)
		}
	}
	// Once the log has gone the turn remains, without its prompt.
	promptCache.Lock()
	promptCache.entries = map[string]*promptText{}
	promptCache.Unlock()
	os.Remove(f.path)
	detail, _ = f.store.SessionDetail("parent", Filters{Period: "all"}, f.rates)
	if len(detail.Turns) != 2 || detail.Turns[0].Prompt != nil {
		t.Fatalf("turns without a log %+v", detail.Turns)
	}
}

func TestOlderLogsAreReadAgainForTurns(t *testing.T) {
	f := newFixture(t)
	f.write(completion("a1", "parent", "turn-a", 100, time.Now()), "")
	f.scan(nil)
	f.exec("UPDATE events SET turn=NULL")
	f.exec("UPDATE sources SET parser_version=1, errors=3")
	if added := f.scan(nil).Added; added != 0 {
		t.Fatalf("rereading added %d events", added)
	}
	var turn string
	var errors int
	f.store.DB.QueryRow("SELECT turn FROM events").Scan(&turn)
	f.store.DB.QueryRow("SELECT errors FROM sources").Scan(&errors)
	if turn != "turn-a" || errors != 0 {
		t.Fatalf("turn %q, errors %d", turn, errors)
	}
}

func TestMuseEffortFollowsMusePrecedence(t *testing.T) {
	settings := func(text string) string {
		home := t.TempDir()
		os.MkdirAll(filepath.Join(home, "muse"), 0o700)
		os.WriteFile(filepath.Join(home, "muse", "settings.json"), []byte(text), 0o600)
		return "XDG_CONFIG_HOME=" + home
	}
	both := settings(`{"model":"muse-spark-1.3","reasoning_effort":"xhigh","model_reasoning_efforts":{"muse-spark-1.3":"medium","bad":"DROP TABLE"}}`)
	cases := []struct {
		name, environ string
		cmdline       []string
		model, want   string
		source        string
	}{
		{"flag wins", both, []string{"muse", "--effort", "low"}, "muse-spark-1.3", "low", "flag"},
		{"inline flag", both, []string{"muse", "--reasoning-effort=max"}, "other", "max", "flag"},
		{"per-model setting", both, []string{"muse"}, "muse-spark-1.3", "medium", "settings"},
		{"global setting for other models", both, []string{"muse"}, "muse-wonder-1.0", "xhigh", "settings"},
		{"Muse default", settings(`{}`), []string{"muse"}, "muse-spark-1.3", "high", "default"},
		{"no settings file", "XDG_CONFIG_HOME=" + t.TempDir(), []string{"muse"}, "muse-spark-1.3", "high", "default"},
	}
	for _, c := range cases {
		fakeProc(t, []string{c.environ}, c.cmdline)
		env, err := processEnv(50)
		if err != nil {
			t.Fatal(err)
		}
		e := museEffort(50, env)
		if got := e.For(c.model); got != c.want || e.Source != c.source || e.Efforts["bad"] != "" {
			t.Errorf("%s: %s from %s (%+v)", c.name, got, e.Source, e)
		}
	}
}

func TestPreviewCollapsesAndCuts(t *testing.T) {
	if p := preview("  a\n\tb  "); p.text != "a b" || p.truncated {
		t.Fatalf("%+v", p)
	}
	if p := preview(strings.Repeat("é", promptLimit+5)); len([]rune(p.text)) != promptLimit || !p.truncated {
		t.Fatalf("%d runes", len([]rune(p.text)))
	}
}
