package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Totals struct {
	Calls    int     `json:"calls"`
	USD      float64 `json:"usd"`
	Unpriced int     `json:"unpriced"`
	Tokens   Tokens  `json:"tokens"`
}
type ModelTotal struct {
	Provider *string `json:"provider"`
	Model    *string `json:"model"`
	Totals
}
type Report struct {
	From string `json:"from"`
	To   string `json:"to"`
	Totals
	ByModel []ModelTotal `json:"byModel"`
}
type PriceSummary struct {
	Version   string  `json:"version"`
	FetchedAt string  `json:"fetchedAt"`
	CheckedAt string  `json:"checkedAt"`
	Error     *string `json:"error"`
}
type Summary struct {
	V           int               `json:"v"`
	GeneratedAt string            `json:"generatedAt"`
	OK          bool              `json:"ok"`
	Error       *string           `json:"error"`
	Prices      PriceSummary      `json:"prices"`
	LastCycle   Counts            `json:"lastCycle"`
	LastEventAt *string           `json:"lastEventAt"`
	Windows     map[string]Report `json:"windows"`
}

func (t *Totals) add(r Record) {
	t.Calls++
	if r.USD == nil {
		t.Unpriced++
	} else {
		t.USD += *r.USD
	}
	t.Tokens.Input += r.Tokens.Input
	t.Tokens.Output += r.Tokens.Output
	t.Tokens.Reasoning += r.Tokens.Reasoning
	t.Tokens.CacheRead += r.Tokens.CacheRead
	t.Tokens.CacheWrite += r.Tokens.CacheWrite
}
func ReadReport(dir string, from, to time.Time) (Report, error) {
	r := Report{From: from.UTC().Format(time.RFC3339Nano), To: to.UTC().Format(time.RFC3339Nano), ByModel: []ModelTotal{}}
	models := make(map[string]*ModelTotal)
	files, err := filepath.Glob(filepath.Join(dir, "ledger", "*.jsonl"))
	if err != nil {
		return r, err
	}
	for _, path := range files {
		d := filepath.Base(path)
		if d < from.UTC().Format("2006-01-02")+".jsonl" || d > to.UTC().Format("2006-01-02")+".jsonl" {
			continue
		}
		_, err = readLines(path, 0, func(b []byte) error {
			var row Record
			if err := json.Unmarshal(b, &row); err != nil {
				return err
			}
			at := time.UnixMilli(row.Time)
			if at.Before(from) || !at.Before(to) {
				return nil
			}
			key, _ := json.Marshal([]*string{row.Provider, row.Model})
			m := models[string(key)]
			if m == nil {
				m = &ModelTotal{Provider: row.Provider, Model: row.Model}
				models[string(key)] = m
			}
			m.add(row)
			r.add(row)
			return nil
		})
		if err != nil {
			return r, err
		}
	}
	for _, m := range models {
		r.ByModel = append(r.ByModel, *m)
	}
	sort.Slice(r.ByModel, func(a, b int) bool {
		if r.ByModel[a].USD != r.ByModel[b].USD {
			return r.ByModel[a].USD > r.ByModel[b].USD
		}
		x, _ := json.Marshal(r.ByModel[a])
		y, _ := json.Marshal(r.ByModel[b])
		return string(x) < string(y)
	})
	return r, nil
}

func lastEvent(dir string) (*string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "ledger", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	for n := len(files) - 1; n >= 0; n-- {
		var latest *int64
		_, err = readLines(files[n], 0, func(b []byte) error {
			var r Record
			if err := json.Unmarshal(b, &r); err != nil {
				return err
			}
			if latest == nil || r.Time > *latest {
				v := r.Time
				latest = &v
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if latest != nil {
			text := time.UnixMilli(*latest).UTC().Format(time.RFC3339Nano)
			return &text, nil
		}
	}
	return nil, nil
}
