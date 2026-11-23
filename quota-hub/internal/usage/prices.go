package usage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type rate struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Read   float64 `json:"cache_read"`
	Write  float64 `json:"cache_write"`
}
type cost struct {
	rate
	Tiers []struct {
		rate
		Tier struct {
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tier"`
	} `json:"tiers"`
	Over *rate `json:"context_over_200k"`
}
type model struct {
	CanonicalModelID string `json:"canonical_model_id"`
	ReleaseDate      string `json:"release_date"`
	Cost             *cost  `json:"cost"`
	Experimental     struct {
		Modes map[string]struct {
			Cost *cost `json:"cost"`
		} `json:"modes"`
	} `json:"experimental"`
}
type snapshot map[string]struct {
	Models map[string]model `json:"models"`
}
type tier struct {
	size int64
	rate
}

// table lists the base rate and context tiers in OpenCode's order; context_over_200k is a 200000 tier.
func (c *cost) table() (rate, []tier) {
	if c == nil {
		return rate{}, nil
	}
	var tiers []tier
	for _, t := range c.Tiers {
		if t.Tier.Type == "context" {
			tiers = append(tiers, tier{t.Tier.Size, t.rate})
		}
	}
	if c.Over != nil {
		tiers = append(tiers, tier{200000, *c.Over})
	}
	return c.rate, tiers
}

// modeTable prices "<model>-<mode>" like OpenCode's mergeCost: the mode replaces the base rate,
// base tiers stay unless the mode sets a tier of the same size.
func modeTable(base, mode *cost) (rate, []tier) {
	r, tiers := base.table()
	if mode == nil {
		return r, tiers
	}
	var merged []tier
	set := func(t tier) {
		for k := range merged {
			if merged[k].size == t.size {
				merged[k] = t
				return
			}
		}
		merged = append(merged, t)
	}
	for _, t := range tiers {
		set(t)
	}
	r, tiers = mode.table()
	for _, t := range tiers {
		set(t)
	}
	return r, merged
}

func (s snapshot) lookup(provider, id string) (rate, []tier, bool) {
	entry, ok := s[provider]
	if !ok {
		if resolved, ok := s.claudeAgentAlias(provider, id); ok {
			return s.lookup(claudeAgentFirstParty, resolved)
		}
		return s.lookupByModelID(id)
	}
	models := entry.Models
	if m, ok := models[id]; ok {
		r, tiers := m.Cost.table()
		return r, tiers, true
	}
	for base, m := range models {
		for name, mode := range m.Experimental.Modes {
			if base+"-"+name == id {
				r, tiers := modeTable(m.Cost, mode.Cost)
				return r, tiers, true
			}
		}
	}
	return rate{}, nil, false
}

const (
	claudeAgentProvider   = "claude-agent"
	claudeAgentFirstParty = "anthropic"
)

var claudeAgentFamilies = map[string]bool{"opus": true, "sonnet": true, "haiku": true, "fable": true}

// claudeAgentAlias resolves a Claude Code model alias (opus, sonnet, haiku, fable)
// of the OpenCode claude-agent provider to the newest Anthropic model of that family
// in the catalog, which is the model Claude Code runs for the alias. It is an
// estimate: a catalog that lists a model before Claude Code switches to it prices
// the newer model.
func (s snapshot) claudeAgentAlias(provider, id string) (string, bool) {
	if provider != claudeAgentProvider || !claudeAgentFamilies[id] {
		return "", false
	}
	prefix := "claude-" + id + "-"
	best, bestDate := "", ""
	for modelID, m := range s[claudeAgentFirstParty].Models {
		rest, ok := strings.CutPrefix(modelID, prefix)
		if !ok || rest == "" || rest[0] < '0' || rest[0] > '9' || m.Cost == nil || len(m.ReleaseDate) != len("2006-01-02") {
			continue
		}
		if m.ReleaseDate > bestDate || (m.ReleaseDate == bestDate && modelID < best) {
			best, bestDate = modelID, m.ReleaseDate
		}
	}
	return best, best != ""
}

// lookupByModelID uses exact catalog matches to identify the first-party
// provider. Reseller prices and ambiguous canonical IDs are never used.
func (s snapshot) lookupByModelID(id string) (rate, []tier, bool) {
	var canonical string
	for _, entry := range s {
		ref := entry.Models[id].CanonicalModelID
		if ref == "" {
			continue
		}
		if canonical != "" && canonical != ref {
			return rate{}, nil, false
		}
		canonical = ref
	}
	provider, modelID, ok := strings.Cut(canonical, "/")
	// A canonical ID can point to the base model for a differently priced mode.
	// Don't substitute a different model ID when pricing the event.
	if !ok || provider == "" || modelID == "" || (id != modelID && id != canonical) {
		return rate{}, nil, false
	}
	m := s[provider].Models[modelID]
	if m.Cost == nil || (m.CanonicalModelID != "" && m.CanonicalModelID != canonical) {
		return rate{}, nil, false
	}
	r, tiers := m.Cost.table()
	return r, tiers, true
}

type priceIndex struct {
	Version   string `json:"version"`
	FetchedAt string `json:"fetchedAt"`
	ETag      string `json:"etag"`
}
type Price struct {
	Version string `json:"version"`
	Status  string `json:"status"`
	Tier    int64  `json:"tier"`
}

func decodePrices(b []byte) (snapshot, error) {
	var s snapshot
	err := json.Unmarshal(b, &s)
	if err == nil && s == nil {
		err = fmt.Errorf("prices must be a JSON object")
	}
	return s, err
}

func (i *Importer) fetch(s *state, now string) error {
	req, err := http.NewRequest("GET", i.ModelsURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("If-None-Match", s.ETag)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		s.PriceCheckedAt = now
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("models.dev: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if err != nil {
		return err
	}
	if len(b) > 64<<20 {
		return fmt.Errorf("prices exceed 64 MiB")
	}
	if _, err = decodePrices(b); err != nil {
		return err
	}
	version := fmt.Sprintf("%x", sha256.Sum256(b))
	path := filepath.Join(i.Dir, "prices", version+".json")
	if _, err = os.Stat(path); os.IsNotExist(err) {
		err = atomicWrite(path, b)
	}
	if err != nil {
		return err
	}
	last, err := i.lastIndex()
	if err != nil {
		return err
	}
	// A catalog may return to an earlier version: the index still records the switch.
	if last.Version != version {
		row, _ := json.Marshal(priceIndex{version, now, resp.Header.Get("ETag")})
		if err = appendLedger(filepath.Join(i.Dir, "prices", "index.jsonl"), append(row, '\n')); err != nil {
			return err
		}
	}
	s.ETag, s.PriceCheckedAt = resp.Header.Get("ETag"), now
	return nil
}

func (i *Importer) lastIndex() (priceIndex, error) {
	var last priceIndex
	_, err := readLines(filepath.Join(i.Dir, "prices", "index.jsonl"), 0, func(b []byte) error { return json.Unmarshal(b, &last) })
	if os.IsNotExist(err) {
		err = nil
	}
	return last, err
}

func (i *Importer) loadPrices() error {
	last, err := i.lastIndex()
	if err != nil {
		return err
	}
	if last.Version == "" || last.Version == i.index.Version {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(i.Dir, "prices", last.Version+".json"))
	if err != nil {
		return err
	}
	s, err := decodePrices(b)
	if err == nil {
		i.prices, i.index = s, last
	}
	return err
}

func (i *Importer) price(e Event) (*float64, Price) {
	p := Price{Version: i.index.Version, Status: "ok"}
	if len(p.Version) > 12 {
		p.Version = p.Version[:12]
	}
	if e.Provider == nil || e.Model == nil {
		p.Status = "no_model"
		return nil, p
	}
	if i.prices == nil {
		p.Status = "no_prices"
		return nil, p
	}
	r, tiers, ok := i.prices.lookup(*e.Provider, *e.Model)
	if !ok {
		p.Status = "unknown_model"
		return nil, p
	}
	context := float64(e.Tokens.Input) + float64(e.Tokens.CacheRead) + float64(e.Tokens.CacheWrite)
	for _, t := range tiers {
		if context > float64(t.size) && t.size > p.Tier {
			r, p.Tier = t.rate, t.size
		}
	}
	t := e.Tokens
	usd := (float64(t.Input)*r.Input + (float64(t.Output)+float64(t.Reasoning))*r.Output + float64(t.CacheRead)*r.Read + float64(t.CacheWrite)*r.Write) / 1e6
	return &usd, p
}
