package httpapi

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"quota-hub/internal/model"
	"quota-hub/internal/usage"
)

const maxUsageSize = 4 << 20

type usageFile struct {
	mu      sync.Mutex
	path    string
	mtime   time.Time
	size    int64
	cached  bool
	summary *usage.Summary
	lastLog time.Time
}

func newUsageFile(paths []string) usageFile {
	var path string
	if len(paths) > 0 {
		path = paths[0]
	}
	return usageFile{path: path}
}

// attach only changes the deep-copied HTTP snapshot, never persisted provider data.
func (f *usageFile) attach(snapshot *model.Snapshot, now time.Time) {
	if f.path == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.load() {
		if f.lastLog.IsZero() || now.Sub(f.lastLog) >= time.Minute {
			log.Print("quota-hub: local usage summary unavailable")
			f.lastLog = now
		}
		return
	}
	generated, _ := time.Parse(time.RFC3339, f.summary.GeneratedAt)
	for si := range snapshot.Subscriptions {
		sub := &snapshot.Subscriptions[si]
		for qi := range sub.Quotas {
			q := &sub.Quotas[qi]
			if q.Window.ResetAt == nil {
				continue
			}
			for _, current := range f.summary.Current {
				delta := current.Reset.Sub(*q.Window.ResetAt)
				if current.Subscription != sub.ID || current.Quota != q.ID || current.Reset.IsZero() ||
					delta < -120*time.Second || delta > 120*time.Second {
					continue
				}
				q.Usage = &model.Usage{
					Calls: current.Calls, CostUSD: current.USD, Unpriced: current.Unpriced, Partial: current.Partial,
					InputTokens: current.Tokens.Input, OutputTokens: current.Tokens.Output,
					ReasoningTokens: current.Tokens.Reasoning, CacheReadTokens: current.Tokens.CacheRead,
					CacheWriteTokens: current.Tokens.CacheWrite, Since: current.Start, GeneratedAt: generated,
				}
				break
			}
		}
	}
}

func (f *usageFile) load() bool {
	info, err := os.Stat(f.path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxUsageSize {
		f.summary, f.cached = nil, false
		return false
	}
	if f.cached && info.ModTime().Equal(f.mtime) && info.Size() == f.size {
		return f.summary != nil
	}
	f.mtime, f.size, f.cached, f.summary = info.ModTime(), info.Size(), true, nil
	file, err := os.Open(f.path)
	if err != nil {
		f.cached = false // Permission/read failures may recover without a new mtime.
		return false
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxUsageSize+1))
	if err != nil || len(body) > maxUsageSize {
		f.cached = false
		return false
	}
	var summary usage.Summary
	if json.Unmarshal(body, &summary) != nil || summary.Current == nil {
		return false
	}
	if generated, err := time.Parse(time.RFC3339, summary.GeneratedAt); err != nil || generated.IsZero() {
		return false
	}
	for _, w := range summary.Current {
		if w.Calls < 0 || w.USD < 0 || w.Unpriced < 0 ||
			w.Tokens.Input < 0 || w.Tokens.Output < 0 || w.Tokens.Reasoning < 0 ||
			w.Tokens.CacheRead < 0 || w.Tokens.CacheWrite < 0 {
			return false
		}
	}
	f.summary = &summary
	return true
}
