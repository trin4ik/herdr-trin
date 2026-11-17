package usage

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var clock = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func importer(t *testing.T) *Importer {
	t.Helper()
	i, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	i.Offline, i.Now = true, func() time.Time { return clock }
	t.Cleanup(func() { i.Close() })
	return i
}
func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func event(id string, at time.Time) string {
	p, m := "p", "m"
	b, _ := json.Marshal(Event{V: 1, ID: id, Time: at.UnixMilli(), Source: "step", Status: "ended", Session: "s", Provider: &p, Model: &m, Tokens: Tokens{Input: 10, Output: 4, Reasoning: 2, CacheRead: 3, CacheWrite: 2}})
	return string(b) + "\n"
}
func cycle(t *testing.T, i *Importer) Summary {
	t.Helper()
	s, err := i.Cycle()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func rows(t *testing.T, i *Importer, d string) []Record {
	t.Helper()
	var out []Record
	_, err := readLines(filepath.Join(i.Dir, "ledger", d+".jsonl"), 0, func(b []byte) error {
		var r Record
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestImportOffsetsAndReplay(t *testing.T) {
	i := importer(t)
	a := event("a", clock.Add(-time.Hour))
	write(t, filepath.Join(i.Dir, "events", "a.jsonl"), a+a)
	write(t, filepath.Join(i.Dir, "events", "b.jsonl"), a+event("b", clock.Add(-2*time.Hour)))
	s := cycle(t, i)
	if s.LastCycle != (Counts{Lines: 4, Imported: 2, Duplicates: 2}) {
		t.Fatalf("%+v", s.LastCycle)
	}
	if s.Windows["5h"].Calls != 2 || s.Windows["5h"].Unpriced != 2 || s.LastEventAt == nil {
		t.Fatalf("%+v", s)
	}
	b, err := os.ReadFile(filepath.Join(i.Dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state state
	if err = json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	if state.Offsets["a.jsonl"] != int64(2*len(a)) {
		t.Fatalf("%+v", state)
	}
	if s = cycle(t, i); s.LastCycle.Lines != 0 {
		t.Fatalf("%+v", s.LastCycle)
	}
	// Simulate a crash after ledger fsync and before saving offsets; restart cache too.
	state.Offsets = map[string]int64{}
	if err = saveJSON(filepath.Join(i.Dir, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	clear(i.ids)
	if s = cycle(t, i); s.LastCycle.Duplicates != 4 || s.LastCycle.Imported != 0 {
		t.Fatalf("%+v", s.LastCycle)
	}
	if r := rows(t, i, "2026-11-17"); len(r) != 2 || r[0].ImportedAt != clock.Format(time.RFC3339) {
		t.Fatalf("%+v", r)
	}
}

func TestPartialEventAndJournalRepair(t *testing.T) {
	i := importer(t)
	path := filepath.Join(i.Dir, "events", "a.jsonl")
	a, b := event("a", clock), event("b", clock)
	write(t, path, a+strings.TrimSuffix(b, "\n"))
	if s := cycle(t, i); s.LastCycle.Imported != 1 {
		t.Fatalf("%+v", s)
	}
	ledger := filepath.Join(i.Dir, "ledger", "2026-11-17.jsonl")
	f, err := os.OpenFile(ledger, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"v":1,"id":"partial"`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	write(t, path, a+b)
	if s := cycle(t, i); s.LastCycle.Imported != 1 {
		t.Fatalf("%+v", s)
	}
	if r := rows(t, i, "2026-11-17"); len(r) != 2 || r[1].ID != "b" {
		t.Fatalf("%+v", r)
	}
	// Truncation of an input file resets its offset and is visible as a warning.
	write(t, path, a)
	if s := cycle(t, i); s.LastCycle.Warnings != 1 || s.LastCycle.Duplicates != 1 {
		t.Fatalf("%+v", s.LastCycle)
	}
}

func TestBadEvents(t *testing.T) {
	i := importer(t)
	valid := event("ok", clock)
	invalid := []string{"not json\n", strings.Replace(valid, `"v":1`, `"v":2`, 1), strings.Replace(valid, `"id":"ok"`, `"id":""`, 1), strings.Replace(valid, `"time":1794916800000`, `"time":null`, 1), strings.Replace(valid, `"input":10`, `"input":-1`, 1), strings.Replace(valid, `"output":4`, `"output":null`, 1), strings.Replace(valid, `"reasoning":2,`, "", 1)}
	for n, line := range invalid {
		if _, ok := parseEvent([]byte(line)); ok {
			t.Fatalf("accepted case %d: %s", n, line)
		}
	}
	write(t, filepath.Join(i.Dir, "events", "a.jsonl"), strings.Join(invalid, "")+valid)
	s := cycle(t, i)
	if s.LastCycle.BadLines != len(invalid) || s.LastCycle.Imported != 1 {
		t.Fatalf("%+v", s.LastCycle)
	}
}

const priceJSON = `{"p":{"models":{"m":{"cost":{"input":1,"output":2,"cache_read":3,"cache_write":4,"tiers":[{"tier":{"type":"context","size":100},"input":5,"output":6},{"tier":{"type":"context","size":300000},"input":9,"output":10},{"tier":{"type":"other","size":400000},"input":999}],"context_over_200k":{"input":7,"output":8}}}}}}`

func TestPrices(t *testing.T) {
	i := importer(t)
	i.index.Version = strings.Repeat("a", 64)
	var err error
	i.prices, err = decodePrices([]byte(priceJSON))
	if err != nil {
		t.Fatal(err)
	}
	p, m := "p", "m"
	for _, tt := range []struct {
		input int64
		tier  int64
		want  float64
	}{
		{95, 0, (95 + 12 + 9 + 8) / 1e6}, // context ==100: strict comparison
		{96, 100, (96*5 + 6*6) / 1e6},
		{199995, 100, (199995*5 + 6*6) / 1e6},
		{199996, 200000, (199996*7 + 6*8) / 1e6},
		{300001, 300000, (300001*9 + 6*10) / 1e6},
	} {
		e := Event{Provider: &p, Model: &m, Tokens: Tokens{Input: tt.input, Output: 4, Reasoning: 2, CacheRead: 3, CacheWrite: 2}}
		usd, price := i.price(e)
		if usd == nil || math.Abs(*usd-tt.want) > 1e-12 || price.Tier != tt.tier || price.Status != "ok" || price.Version != strings.Repeat("a", 12) {
			t.Fatalf("%+v -> %v %+v", tt, usd, price)
		}
	}
	for _, tt := range []struct {
		provider, model *string
		prices          snapshot
		status          string
	}{
		{nil, nil, i.prices, "no_model"}, {&p, &m, nil, "no_prices"}, {&m, &m, i.prices, "unknown_model"},
	} {
		i.prices = tt.prices
		usd, price := i.price(Event{Provider: tt.provider, Model: tt.model})
		if usd != nil || price.Status != tt.status {
			t.Fatalf("%v %+v", usd, price)
		}
	}
}

func TestModePrices(t *testing.T) {
	i := importer(t)
	var err error
	i.prices, err = decodePrices([]byte(`{"p":{"models":{
		"m":{"cost":{"input":1,"output":2,"cache_read":3,"cache_write":4,"tiers":[{"tier":{"type":"context","size":100},"input":5,"output":6}],"context_over_200k":{"input":7,"output":8}},
		     "experimental":{"modes":{"fast":{"cost":{"input":10,"output":20}},"pro":{},"real":{"cost":{"input":1}},"big":{"cost":{"input":10,"context_over_200k":{"input":70}}}}}},
		"m-real":{"cost":{"input":100}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	p := "p"
	for _, tt := range []struct {
		model  string
		tokens Tokens
		tier   int64
		want   float64
	}{
		{"m-fast", Tokens{Input: 10, Output: 4}, 0, (10*10 + 4*20) / 1e6},                               // mode replaces base
		{"m-fast", Tokens{Input: 1000, Output: 4}, 100, (1000*5 + 4*6) / 1e6},                           // base tiers stay
		{"m-fast", Tokens{CacheRead: 5, CacheWrite: 5}, 0, 0},                                           // missing mode rates are 0, as in OpenCode
		{"m-pro", Tokens{Input: 10, Output: 4, CacheRead: 3, CacheWrite: 2}, 0, (10 + 8 + 9 + 8) / 1e6}, // mode without cost = base price
		{"m-big", Tokens{Input: 300000}, 200000, 300000 * 70 / 1e6},                                     // mode tier replaces the same-size base tier
		{"m-real", Tokens{Input: 1}, 0, 100 / 1e6},                                                      // exact id wins
	} {
		m := tt.model
		usd, price := i.price(Event{Provider: &p, Model: &m, Tokens: tt.tokens})
		if usd == nil || math.Abs(*usd-tt.want) > 1e-12 || price.Tier != tt.tier {
			t.Fatalf("%+v -> %v %+v", tt, usd, price)
		}
	}
	m := "m-slow"
	if usd, price := i.price(Event{Provider: &p, Model: &m}); usd != nil || price.Status != "unknown_model" {
		t.Fatalf("%v %+v", usd, price)
	}
}

func TestFetchPrices(t *testing.T) {
	i := importer(t)
	i.Offline = false
	mode := "200"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode != "200" && r.Header.Get("If-None-Match") != `"v1"` {
			t.Errorf("etag %q", r.Header.Get("If-None-Match"))
		}
		w.Header().Set("ETag", `"v1"`)
		switch mode {
		case "304":
			w.WriteHeader(304)
		case "error":
			w.WriteHeader(503)
		case "invalid":
			_, _ = w.Write([]byte(`[]`))
		case "alt":
			_, _ = w.Write([]byte(`{"other":{"models":{}}}`))
		default:
			_, _ = w.Write([]byte(priceJSON))
		}
	}))
	defer srv.Close()
	i.ModelsURL = srv.URL
	write(t, filepath.Join(i.Dir, "events", "a.jsonl"), event("a", clock.Add(-time.Hour)))
	s := cycle(t, i)
	if s.Prices.Error != nil || s.Prices.Version == "" || s.Windows["5h"].USD == 0 {
		t.Fatalf("%+v", s)
	}
	indexPath := filepath.Join(i.Dir, "prices", "index.jsonl")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"304", "same", "error", "invalid"} {
		mode = v
		i.Now = func() time.Time { return clock.Add(time.Minute) }
		s = cycle(t, i)
		after, err := os.ReadFile(indexPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) || s.Prices.Version == "" {
			t.Fatalf("index changed on %s", mode)
		}
		if (s.Prices.Error != nil) != (mode == "error" || mode == "invalid") {
			t.Fatalf("%s: %+v", mode, s.Prices)
		}
	}
	files, _ := filepath.Glob(filepath.Join(i.Dir, "prices", "*.json"))
	if len(files) != 1 {
		t.Fatalf("%v", files)
	}
	// A fresh importer loads the last snapshot even with a failed request.
	clear(i.ids)
	i.prices = nil
	i.index = priceIndex{}
	mode = "error"
	s = cycle(t, i)
	if s.Prices.Version == "" || s.Prices.Error == nil {
		t.Fatalf("%+v", s.Prices)
	}
	// Catalog changes and then returns to the first version: the first becomes current again.
	first := s.Prices.Version
	for _, v := range []string{"alt", "200"} {
		mode = v
		s = cycle(t, i)
	}
	index, _ := os.ReadFile(indexPath)
	if s.Prices.Version != first || strings.Count(string(index), "\n") != 3 {
		t.Fatalf("%s\n%+v", index, s.Prices)
	}
}

func TestReportAndLock(t *testing.T) {
	i := importer(t)
	if second, err := Open(i.Dir); err == nil {
		second.Close()
		t.Fatal("second lock acquired")
	} else if !strings.Contains(err.Error(), "another importer is running") {
		t.Fatal(err)
	}
	write(t, filepath.Join(i.Dir, "events", "a.jsonl"), event("old", clock.Add(-25*time.Hour))+event("start", clock.Add(-time.Hour))+event("end", clock))
	cycle(t, i)
	// A malformed record outside the window's days must not be read.
	write(t, filepath.Join(i.Dir, "ledger", "2026-01-01.jsonl"), "bad\n")
	r, err := ReadReport(i.Dir, clock.Add(-time.Hour), clock)
	if err != nil {
		t.Fatal(err)
	}
	if r.Calls != 1 || r.Unpriced != 1 || r.Tokens.Input != 10 || len(r.ByModel) != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestBaseDirAndFailureSummary(t *testing.T) {
	t.Setenv("HOME", "/srv/example")
	for _, tt := range []struct{ xdg, want string }{{"/synthetic/state", "/synthetic/state/quota-usage"}, {"relative", "/srv/example/.local/state/quota-usage"}, {"", "/srv/example/.local/state/quota-usage"}} {
		t.Setenv("XDG_STATE_HOME", tt.xdg)
		if got := BaseDir(); got != tt.want {
			t.Fatalf("%q != %q", got, tt.want)
		}
	}
	i := importer(t)
	write(t, filepath.Join(i.Dir, "state.json"), "broken")
	s, err := i.Cycle()
	if err == nil || s.OK || s.Error == nil {
		t.Fatalf("%+v %v", s, err)
	}
	b, err := os.ReadFile(filepath.Join(i.Dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ok":false`) {
		t.Fatal(string(b))
	}
}

func TestDayCacheLimit(t *testing.T) {
	i := importer(t)
	var input strings.Builder
	for n := 0; n < 8; n++ {
		input.WriteString(event("a", clock.AddDate(0, 0, -n)))
	}
	write(t, filepath.Join(i.Dir, "events", "a.jsonl"), input.String())
	cycle(t, i)
	if len(i.ids) > 4 {
		t.Fatalf("cached %d days", len(i.ids))
	}
}
