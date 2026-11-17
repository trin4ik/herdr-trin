package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

type Record struct {
	Event
	USD        *float64 `json:"usd"`
	Price      Price    `json:"price"`
	ImportedAt string   `json:"importedAt"`
}
type state struct {
	Offsets        map[string]int64 `json:"offsets"`
	ETag           string           `json:"etag"`
	PriceCheckedAt string           `json:"priceCheckedAt"`
	PriceError     *string          `json:"priceError"`
}
type Counts struct {
	Lines      int `json:"lines"`
	Imported   int `json:"imported"`
	Duplicates int `json:"duplicates"`
	BadLines   int `json:"badLines"`
	Warnings   int `json:"warnings"`
}
type Importer struct {
	Dir       string
	ModelsURL string
	Offline   bool
	Now       func() time.Time
	lock      *os.File
	prices    snapshot
	index     priceIndex
	ids       map[string]map[string]bool
}

func Open(dir string) (*Importer, error) {
	for _, sub := range []string{"", "events", "ledger", "prices"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "importer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another importer is running: %w", err)
	}
	return &Importer{Dir: dir, ModelsURL: "https://models.dev/api.json", Now: time.Now, lock: f, ids: make(map[string]map[string]bool)}, nil
}
func (i *Importer) Close() error { return i.lock.Close() }

func atomicWrite(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func saveJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'))
}
func appendLedger(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	end, err := completeEnd(f)
	if err != nil {
		return err
	}
	if err = f.Truncate(end); err != nil {
		return err
	}
	if _, err = f.WriteAt(b, end); err != nil {
		return err
	}
	return f.Sync()
}

// completeEnd drops a trailing partial line left by a crash during append.
func completeEnd(f *os.File) (int64, error) {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return 0, err
	}
	last := make([]byte, 1)
	if _, err = f.ReadAt(last, st.Size()-1); err != nil || last[0] == '\n' {
		return st.Size(), err
	}
	all := make([]byte, st.Size())
	if _, err = f.ReadAt(all, 0); err != nil {
		return 0, err
	}
	return int64(bytes.LastIndexByte(all, '\n') + 1), nil
}

func (i *Importer) dayIDs(d string) (map[string]bool, error) {
	if ids, ok := i.ids[d]; ok {
		return ids, nil
	}
	ids := make(map[string]bool)
	_, err := readLines(filepath.Join(i.Dir, "ledger", d+".jsonl"), 0, func(b []byte) error {
		var r Record
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		ids[r.ID] = true
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	i.ids[d] = ids
	return ids, nil
}

func (i *Importer) Cycle() (summary Summary, err error) {
	now := i.Now().UTC()
	s := state{Offsets: make(map[string]int64)}
	summary = Summary{V: 1, GeneratedAt: now.Format(time.RFC3339), OK: true, Windows: make(map[string]Report)}
	defer func() {
		if err != nil {
			text := err.Error()
			summary.OK, summary.Error = false, &text
			clear(i.ids)
		}
		summary.Prices = PriceSummary{i.index.Version, i.index.FetchedAt, s.PriceCheckedAt, s.PriceError}
		if writeErr := saveJSON(filepath.Join(i.Dir, "summary.json"), summary); err == nil {
			err = writeErr
		}
		// The cache survives cycles, but never grows indefinitely in service mode.
		days := make([]string, 0, len(i.ids))
		for d := range i.ids {
			days = append(days, d)
		}
		sort.Strings(days)
		for len(days) > 4 {
			delete(i.ids, days[0])
			days = days[1:]
		}
	}()
	b, readErr := os.ReadFile(filepath.Join(i.Dir, "state.json"))
	if readErr == nil {
		if err = json.Unmarshal(b, &s); err != nil {
			return
		}
	} else if !os.IsNotExist(readErr) {
		err = readErr
		return
	}
	if s.Offsets == nil {
		s.Offsets = make(map[string]int64)
	}
	if !i.Offline {
		s.PriceError = nil
		if fetchErr := i.fetch(&s, summary.GeneratedAt); fetchErr != nil {
			text := fetchErr.Error()
			if len(text) > 300 {
				text = text[:300]
			}
			s.PriceError = &text
		}
	}
	if loadErr := i.loadPrices(); loadErr != nil {
		text := loadErr.Error()
		s.PriceError = &text
	}
	var files []string
	files, err = filepath.Glob(filepath.Join(i.Dir, "events", "*.jsonl"))
	if err != nil {
		return
	}
	buffers := make(map[string]*bytes.Buffer)
	for _, path := range files {
		name := filepath.Base(path)
		var st os.FileInfo
		st, err = os.Stat(path)
		if err != nil {
			return
		}
		offset := s.Offsets[name]
		if st.Size() < offset {
			offset = 0
			summary.LastCycle.Warnings++
		}
		s.Offsets[name], err = readLines(path, offset, func(line []byte) error {
			summary.LastCycle.Lines++
			e, valid := parseEvent(line)
			if !valid {
				summary.LastCycle.BadLines++
				return nil
			}
			d := day(e.Time)
			ids, err := i.dayIDs(d)
			if err != nil {
				return err
			}
			if ids[e.ID] {
				summary.LastCycle.Duplicates++
				return nil
			}
			usd, price := i.price(e)
			row, err := json.Marshal(Record{e, usd, price, summary.GeneratedAt})
			if err != nil {
				return err
			}
			if buffers[d] == nil {
				buffers[d] = new(bytes.Buffer)
			}
			buffers[d].Write(append(row, '\n'))
			ids[e.ID] = true
			summary.LastCycle.Imported++
			return nil
		})
		if err != nil {
			return
		}
	}
	for d, buf := range buffers {
		if err = appendLedger(filepath.Join(i.Dir, "ledger", d+".jsonl"), buf.Bytes()); err != nil {
			return
		}
	}
	if err = saveJSON(filepath.Join(i.Dir, "state.json"), s); err != nil {
		return
	}
	for label, duration := range map[string]time.Duration{"5h": 5 * time.Hour, "24h": 24 * time.Hour} {
		summary.Windows[label], err = ReadReport(i.Dir, now.Add(-duration), now)
		if err != nil {
			return
		}
	}
	summary.LastEventAt, err = lastEvent(i.Dir)
	return
}
