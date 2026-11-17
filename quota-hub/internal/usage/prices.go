package usage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
type snapshot map[string]struct {
	Models map[string]struct {
		Cost cost `json:"cost"`
	} `json:"models"`
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
	m, ok := i.prices[*e.Provider].Models[*e.Model]
	if !ok {
		p.Status = "unknown_model"
		return nil, p
	}
	r := m.Cost.rate
	context := float64(e.Tokens.Input) + float64(e.Tokens.CacheRead) + float64(e.Tokens.CacheWrite)
	for _, tier := range m.Cost.Tiers {
		if tier.Tier.Type == "context" && context > float64(tier.Tier.Size) && tier.Tier.Size > p.Tier {
			r, p.Tier = tier.rate, tier.Tier.Size
		}
	}
	if m.Cost.Over != nil && context > 200000 && p.Tier < 200000 {
		r, p.Tier = *m.Cost.Over, 200000
	}
	t := e.Tokens
	usd := (float64(t.Input)*r.Input + (float64(t.Output)+float64(t.Reasoning))*r.Output + float64(t.CacheRead)*r.Read + float64(t.CacheWrite)*r.Write) / 1e6
	return &usd, p
}
