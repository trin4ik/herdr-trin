package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	quotaModel "quota-hub/internal/model"
)

type QuotaSummary struct {
	URL       string  `json:"url"`
	CheckedAt string  `json:"checkedAt"`
	Error     *string `json:"error"`
}

type QuotaWindow struct {
	V               int        `json:"v"`
	Subscription    string     `json:"subscription"`
	Quota           string     `json:"quota"`
	Provider        string     `json:"provider"`
	Model           *string    `json:"model"`
	Label           *string    `json:"label"`
	DurationSeconds *int64     `json:"durationSeconds"`
	Start           *time.Time `json:"start"`
	Reset           time.Time  `json:"reset"`
	SeenAt          time.Time  `json:"seenAt"`
}

type WindowReport struct {
	Subscription string     `json:"subscription"`
	Quota        string     `json:"quota"`
	Provider     string     `json:"provider"`
	Model        *string    `json:"model"`
	Label        *string    `json:"label"`
	Start        *time.Time `json:"start"`
	Reset        time.Time  `json:"reset"`
	Partial      bool       `json:"partial"`
	Totals
}

type CurrentWindow struct {
	WindowReport
	UsedPercent *float64 `json:"usedPercent"`
}

func windowKey(subscription, quota string) string {
	// JSON encoding avoids assumptions about identifier separators.
	b, _ := json.Marshal([]string{subscription, quota})
	return string(b)
}

func fetchQuotas(url string) (*quotaModel.Snapshot, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		// Do not follow redirects to an unexpected endpoint or inherit URL auth.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil || req.URL.User != nil {
		return nil, fmt.Errorf("invalid quota URL")
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota HTTP %d", response.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("quota read failed")
	}
	if len(b) > 1<<20 {
		return nil, fmt.Errorf("quota response exceeds 1 MiB")
	}
	var snapshot quotaModel.Snapshot
	if json.Unmarshal(b, &snapshot) != nil || snapshot.SchemaVersion != quotaModel.SchemaVersion {
		return nil, fmt.Errorf("invalid quota snapshot")
	}
	return &snapshot, nil
}

func readQuotaWindows(dir string) ([]QuotaWindow, error) {
	windows := []QuotaWindow{}
	_, err := readLines(filepath.Join(dir, "windows.jsonl"), 0, func(b []byte) error {
		var window QuotaWindow
		if err := json.Unmarshal(b, &window); err != nil {
			return err
		}
		windows = append(windows, window)
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	return windows, err
}

func (i *Importer) updateQuotas(s *state, summary *Summary, now time.Time) error {
	summary.Quotas = &QuotaSummary{URL: i.QuotasURL, CheckedAt: now.Format(time.RFC3339)}
	if s.QuotaURL != i.QuotasURL {
		s.QuotaSnapshot = nil
		s.QuotaURL = i.QuotasURL
	}
	snapshot, fetchErr := fetchQuotas(i.QuotasURL)
	if fetchErr != nil {
		text := fetchErr.Error()
		summary.Quotas.Error = &text
	} else {
		s.QuotaSnapshot = snapshot
		// Recover appends completed before a crash saving state.json.
		windows, err := readQuotaWindows(i.Dir)
		if err != nil {
			return err
		}
		if s.QuotaWindows == nil {
			s.QuotaWindows = make(map[string]QuotaWindow)
		}
		for _, window := range windows {
			s.QuotaWindows[windowKey(window.Subscription, window.Quota)] = window
		}
		for _, subscription := range snapshot.Subscriptions {
			if subscription.OpenCodeProvider == nil || *subscription.OpenCodeProvider == "" {
				continue
			}
			for _, quota := range subscription.Quotas {
				if quota.Window.ResetAt == nil || quota.Window.ResetAt.IsZero() {
					continue
				}
				key := windowKey(subscription.ID, quota.ID)
				previous, exists := s.QuotaWindows[key]
				reset := quota.Window.ResetAt.UTC()
				if exists && reset.Sub(previous.Reset) >= -120*time.Second && reset.Sub(previous.Reset) <= 120*time.Second {
					continue
				}
				window := QuotaWindow{
					V: 1, Subscription: subscription.ID, Quota: quota.ID,
					Provider: *subscription.OpenCodeProvider, Model: quota.Model, Label: quota.Label,
					DurationSeconds: quota.Window.DurationSeconds, Reset: reset, SeenAt: now,
				}
				if window.DurationSeconds != nil {
					start := reset.Add(-time.Duration(*window.DurationSeconds) * time.Second)
					window.Start = &start
				} else if exists {
					start := previous.Reset
					window.Start = &start
				}
				b, err := json.Marshal(window)
				if err != nil {
					return err
				}
				if err := appendLedger(filepath.Join(i.Dir, "windows.jsonl"), append(b, '\n')); err != nil {
					return err
				}
				s.QuotaWindows[key] = window
			}
		}
	}
	if s.QuotaSnapshot == nil {
		return nil
	}
	providers := make(map[string]bool)
	percent := make(map[string]*float64)
	for _, subscription := range s.QuotaSnapshot.Subscriptions {
		if subscription.OpenCodeProvider != nil && *subscription.OpenCodeProvider != "" {
			providers[*subscription.OpenCodeProvider] = true
		}
		for _, quota := range subscription.Quotas {
			percent[windowKey(subscription.ID, quota.ID)] = quota.UsedPercent
		}
	}
	keys := make([]string, 0, len(s.QuotaWindows))
	for key := range s.QuotaWindows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	summary.Current = []CurrentWindow{}
	for _, key := range keys {
		report, err := reportQuotaWindow(i.Dir, s.QuotaWindows[key])
		if err != nil {
			return err
		}
		summary.Current = append(summary.Current, CurrentWindow{report, percent[key]})
	}
	unmatched, err := readReport(i.Dir, now.Add(-24*time.Hour), now, func(record Record) bool {
		return record.Provider == nil || !providers[*record.Provider]
	})
	if err != nil {
		return err
	}
	summary.Unmatched = &unmatched
	return nil
}

var modelSeparators = regexp.MustCompile(`[^a-z0-9]`)

func reportQuotaWindow(dir string, window QuotaWindow) (WindowReport, error) {
	start := window.SeenAt
	if window.Start != nil {
		start = *window.Start
	}
	report, err := readReport(dir, start, window.Reset, func(record Record) bool {
		if record.Provider == nil || *record.Provider != window.Provider {
			return false
		}
		if window.Model == nil {
			return true
		}
		slug := strings.Trim(modelSeparators.ReplaceAllString(strings.ToLower(*window.Model), "-"), "-")
		return record.Model != nil && strings.Contains("-"+strings.ToLower(*record.Model)+"-", "-"+slug+"-")
	})
	return WindowReport{
		Subscription: window.Subscription, Quota: window.Quota, Provider: window.Provider,
		Model: window.Model, Label: window.Label, Start: window.Start, Reset: window.Reset,
		Partial: window.Start == nil, Totals: report.Totals,
	}, err
}

// ReadWindows reports every recorded instance without opening an importer.
func ReadWindows(dir string) ([]WindowReport, error) {
	windows, err := readQuotaWindows(dir)
	if err != nil {
		return nil, err
	}
	reports := make([]WindowReport, 0, len(windows))
	for _, window := range windows {
		report, err := reportQuotaWindow(dir, window)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}
