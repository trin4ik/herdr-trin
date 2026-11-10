// Package store persists quota state privately and serves race-safe public copies.
//
// Layout under the root directory (all 0700 dirs / 0600 files, no symlinks followed):
//
//	LOCK                  flock(2) single-writer lock held while the Store is open
//	state/<id>.json       atomic per-ID source of truth: schedule, error, last-good
//	                      (including PRIVATE raw), optional hybrid scheduling metadata
//	                      (version 2 only when present) and pending journal records
//	history/<id>.jsonl    normalized successes only (public data, never raw)
//	attempts/<id>.jsonl   one line per local attempt (public data, never raw)
//
// Commit first writes the state file (schedule + last-good + pending journal
// records with deterministic IDs), then drains the pending records to the JSONL
// files, then rewrites the state without them. Recovery re-drains and dedupes by
// comparing against the last complete journal line, so a crash at any point
// neither loses nor duplicates records.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"quota-hub/internal/model"
)

const (
	stateVersion = 1
	// stateVersionHybrid is written only when hybrid metadata exists. A binary that
	// predates it fails closed on the file instead of silently dropping a rate-limit
	// deadline; this reader accepts both versions.
	stateVersionHybrid  = 2
	stateVersionProfile = 3 // Independent remote-profile cooldown and credential generation.
	maxStateSize        = 16 << 20
	maxRawSize          = 4 << 20
	maxLineSize         = 1 << 20
	maxPending          = 256
	maxFailures         = 1 << 20

	CodeOlderObservation = "older_observation"
	CodeCollectionError  = "collection_error"
)

// Fixed errors: they never carry paths, raw data or upstream text.
var (
	ErrLocked            = errors.New("store: directory is locked by another process")
	ErrCorrupt           = errors.New("store: corrupt persisted data")
	ErrPersist           = errors.New("store: cannot persist state")
	ErrJournal           = errors.New("store: cannot append journal")
	ErrInvalid           = errors.New("store: invalid input")
	ErrUnknownID         = errors.New("store: unknown subscription id")
	ErrClosed            = errors.New("store: closed")
	ErrUnsafeRoot        = errors.New("store: unsafe root directory")
	ErrOlderObservation  = errors.New("store: observation older than last-good")
	ErrIdentityChanged   = errors.New("store: source account identity changed")
	ErrDefinitionChanged = errors.New("store: subscription provider/source changed; use a new id")
)

type good struct {
	ObservedAt time.Time       `json:"observed_at"`
	Plan       *string         `json:"plan"`
	Quotas     []model.Quota   `json:"quotas"`
	Metrics    []model.Metric  `json:"metrics"`
	Key        string          `json:"key"`
	Raw        json.RawMessage `json:"raw,omitempty"`             // PRIVATE
	Identity   string          `json:"source_identity,omitempty"` // PRIVATE fingerprint
	Source     string          `json:"source,omitempty"`          // actual acquisition source override
}

type historyRecord struct {
	ID         string         `json:"id"`
	SubID      string         `json:"subscription_id"`
	Provider   string         `json:"provider"`
	Product    string         `json:"product"`
	Source     string         `json:"source"`
	Seq        uint64         `json:"seq"`
	RecordedAt time.Time      `json:"recorded_at"`
	ObservedAt time.Time      `json:"observed_at"`
	Plan       *string        `json:"plan"`
	Quotas     []model.Quota  `json:"quotas"`
	Metrics    []model.Metric `json:"metrics"`
}

type attemptRecord struct {
	ID            string           `json:"id"`
	SubID         string           `json:"subscription_id"`
	Provider      string           `json:"provider"`
	Source        string           `json:"source"`
	Seq           uint64           `json:"seq"`
	AttemptAt     time.Time        `json:"attempt_at"`
	NextAttemptAt time.Time        `json:"next_attempt_at"`
	Outcome       string           `json:"outcome"` // ok | error | older_observation
	ObservedAt    *time.Time       `json:"observed_at"`
	Error         *model.ErrorInfo `json:"error"`
}

type state struct {
	Version         int                 `json:"version"`
	ID              string              `json:"id"`
	DefinitionHash  string              `json:"definition_hash,omitempty"`
	Seq             uint64              `json:"seq"`
	Failures        int                 `json:"failures"`
	LastAttemptAt   *time.Time          `json:"last_attempt_at,omitempty"`
	NextAttemptAt   *time.Time          `json:"next_attempt_at,omitempty"`
	Error           *model.ErrorInfo    `json:"error,omitempty"`
	Good            *good               `json:"good,omitempty"`
	Hybrid          *model.HybridState  `json:"hybrid,omitempty"`
	Profile         *model.Profile      `json:"profile,omitempty"`
	ProfileHidden   bool                `json:"profile_hidden,omitempty"`
	RemoteProfile   *model.ProfileState `json:"remote_profile,omitempty"`
	PendingHistory  []historyRecord     `json:"pending_history,omitempty"`
	PendingAttempts []attemptRecord     `json:"pending_attempts,omitempty"`
}

type entry struct {
	def model.Definition
	st  *state // replaced wholesale on commit, never mutated in place
}

// Store is safe for concurrent use. Commit calls are serialized.
type Store struct {
	root string
	lock *os.File

	commitMu sync.Mutex
	mu       sync.RWMutex
	closed   bool
	defs     []model.Definition
	byID     map[string]*entry
	clock    func() time.Time
}

// ValidID reports whether id is usable as a file name: [a-z0-9][a-z0-9_-]{0,63}.
func ValidID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// Open locks dir exclusively, recovers pending journal records and loads state
// for defs. Only IDs in defs are exposed; state of other IDs stays untouched.
func Open(dir string, defs []model.Definition) (*Store, error) {
	seen := make(map[string]bool, len(defs))
	for _, d := range defs {
		if !ValidID(d.ID) || seen[d.ID] || d.StaleAfter < 0 {
			return nil, fmt.Errorf("%w: definition", ErrInvalid)
		}
		seen[d.ID] = true
	}
	if dir == "" {
		return nil, fmt.Errorf("%w: empty directory", ErrInvalid)
	}
	root := filepath.Clean(dir)
	if err := ensureDir(root, true); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, "LOCK"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: lock file", ErrUnsafeRoot)
	}
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		lock.Close()
		return nil, ErrUnsafeRoot
	}
	if err := lock.Chmod(0o600); err != nil {
		lock.Close()
		return nil, ErrUnsafeRoot
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, ErrLocked
	}
	s := &Store{
		root:  root,
		lock:  lock,
		defs:  append([]model.Definition(nil), defs...),
		byID:  make(map[string]*entry, len(defs)),
		clock: time.Now,
	}
	if err := s.load(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	for _, sub := range []string{"state", "history", "attempts"} {
		if err := ensureDir(filepath.Join(s.root, sub), false); err != nil {
			return err
		}
	}
	for _, d := range s.defs {
		st, err := s.readState(d.ID)
		if err != nil {
			return err
		}
		fingerprint := definitionKey(d)
		if st.DefinitionHash != "" && st.DefinitionHash != fingerprint {
			return ErrDefinitionChanged
		}
		st.DefinitionHash = fingerprint
		for _, p := range []string{s.historyPath(d.ID), s.attemptsPath(d.ID)} {
			if err := repairTail(p); err != nil {
				return err
			}
		}
		if len(st.PendingHistory)+len(st.PendingAttempts) > 0 {
			if st, err = s.drain(st); err != nil {
				return err
			}
		}
		s.byID[d.ID] = &entry{def: d, st: st}
	}
	return nil
}

// Close releases the directory lock. It is idempotent.
func (s *Store) Close() error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	return s.lock.Close()
}

// Ready reports that the store is open and fully recovered.
func (s *Store) Ready() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.closed
}

// Get returns a deep copy of one subscription as of the store clock.
func (s *Store) Get(id string) (model.Subscription, bool) {
	return s.GetAt(id, s.clock())
}

// GetAt is Get with an explicit time used for stale computation.
func (s *Store) GetAt(id string, now time.Time) (model.Subscription, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	if !ok {
		return model.Subscription{}, false
	}
	return view(e, now), true
}

// Failures returns the consecutive failed attempts recorded for id.
func (s *Store) Failures(id string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.byID[id]; ok {
		return e.st.Failures
	}
	return 0
}

// Hybrid returns a deep copy of the persisted hybrid scheduling metadata. The
// bool is false for an unknown id or when no hybrid metadata was ever stored.
func (s *Store) Hybrid(id string) (model.HybridState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	if !ok || e.st.Hybrid == nil {
		return model.HybridState{}, false
	}
	return *cloneHybrid(e.st.Hybrid), true
}

// Snapshot returns deep-copied public state for all defined subscriptions.
func (s *Store) Snapshot(now time.Time) model.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := model.Snapshot{
		SchemaVersion: model.SchemaVersion,
		GeneratedAt:   now.UTC(),
		Subscriptions: make([]model.Subscription, 0, len(s.defs)),
	}
	for _, d := range s.defs {
		snap.Subscriptions = append(snap.Subscriptions, view(s.byID[d.ID], now))
	}
	return snap
}

func view(e *entry, now time.Time) model.Subscription {
	st := e.st
	sub := model.Subscription{
		ID:            e.def.ID,
		Provider:      e.def.Provider,
		Product:       e.def.Product,
		Source:        e.def.Source,
		LastAttemptAt: cloneTime(st.LastAttemptAt),
		NextAttemptAt: cloneTime(st.NextAttemptAt),
		Error:         cloneErr(st.Error),
		Quotas:        []model.Quota{},
		Metrics:       []model.Metric{},
	}
	if h := st.Hybrid; h != nil {
		sub.LastPassiveAt = cloneTime(h.LastPassiveAt)
		sub.NextPollAt = cloneTime(h.NextRemoteAttemptAt)
		sub.LastPollError = cloneErr(h.RemoteError)
	}
	g := st.Good
	if g == nil {
		sub.Stale = true
		sub.Status = "unknown"
		if st.Error != nil {
			sub.Status = "error"
		}
		return sub
	}
	t := g.ObservedAt
	sub.LastSuccessAt = &t
	if g.Source != "" {
		sub.Source = g.Source
	}
	if g.Plan != nil {
		p := *g.Plan
		sub.Plan = &p
	}
	if p := st.Profile; p != nil {
		if p.Invalidated || p.Identity == "" || (st.RemoteProfile != nil && p.Identity != st.RemoteProfile.Identity) {
			sub.Plan = nil
		} else {
			sub.Plan = model.ProfileText(value(p.Plan), 100)
			if e.def.PublishAccountEmail {
				sub.AccountEmail = model.Email(value(p.AccountEmail))
			}
		}
	}
	if st.ProfileHidden {
		sub.AccountEmail, sub.Plan = nil, nil
	}
	sub.Quotas = cloneQuotas(g.Quotas)
	sub.Metrics = cloneMetrics(g.Metrics)
	stale := st.Error != nil
	if e.def.StaleAfter > 0 {
		at := g.ObservedAt.Add(e.def.StaleAfter)
		sub.StaleAfter = &at
		if !now.Before(at) {
			stale = true
		}
	}
	sub.Stale = stale
	sub.Status = "ok"
	if stale {
		sub.Status = "stale"
	}
	return sub
}

// Commit durably records one attempt. The state file (including NextAttemptAt)
// is on disk before the update becomes visible to readers. Errors are fixed
// values; ErrOlderObservation is returned (after the attempt is recorded) when a
// result is older than the stored last-good, which is kept.
func (s *Store) Commit(u model.Update) error {
	if !ValidID(u.ID) {
		return ErrInvalid
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	s.mu.RLock()
	closed, e := s.closed, s.byID[u.ID]
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if e == nil {
		return ErrUnknownID
	}
	if u.AttemptAt.IsZero() || u.NextAttemptAt.IsZero() || (u.Result == nil) == (u.Failure == nil) {
		return ErrInvalid
	}
	old := e.st
	if len(old.PendingHistory) >= maxPending || len(old.PendingAttempts) >= maxPending {
		return ErrJournal
	}

	at, next := u.AttemptAt.UTC(), u.NextAttemptAt.UTC()
	ns := *old
	ns.Version = stateVersion
	ns.ID = u.ID
	ns.Seq = old.Seq + 1
	ns.LastAttemptAt = &at
	ns.NextAttemptAt = &next
	ns.PendingHistory = append([]historyRecord(nil), old.PendingHistory...)
	ns.PendingAttempts = append([]attemptRecord(nil), old.PendingAttempts...)
	currentAttempt := old.LastAttemptAt == nil || !u.AttemptAt.Before(*old.LastAttemptAt)
	if currentAttempt && u.Result != nil && u.Result.ProfileIdentity != "" && (old.Good == nil || !u.Result.ObservedAt.Before(old.Good.ObservedAt)) {
		selectRemoteProfileIdentity(&ns, u.Result.ProfileIdentity, true)
	} else if currentAttempt && u.Failure != nil && u.Failure.ProfileIdentityChecked {
		selectRemoteProfileIdentity(&ns, u.Failure.ProfileIdentity, true)
	}

	att := attemptRecord{
		ID: recordID(u.ID, ns.Seq, ""), SubID: u.ID, Seq: ns.Seq, AttemptAt: at, NextAttemptAt: next,
		Provider: e.def.Provider, Source: e.def.Source,
	}
	if u.Hybrid != nil {
		ns.Hybrid = sanitizeHybrid(*u.Hybrid)
	}
	older := false
	identityChanged := false
	if u.Result != nil {
		g, err := normalizeResult(*u.Result)
		if err != nil {
			return err
		}
		obs := g.ObservedAt
		att.ObservedAt = &obs
		actual := e.def.Source
		if g.Source != "" {
			actual = g.Source
		}
		att.Source = actual
		if old.Good != nil && old.Good.Identity != "" && g.Identity != old.Good.Identity {
			identityChanged = true
			ns.Error = &model.ErrorInfo{Code: "source_identity_changed"}
			ns.Failures = bump(old.Failures)
			att.Outcome = "error"
			ns.Profile = &model.Profile{Invalidated: true}
		} else if old.Good != nil && g.ObservedAt.Before(old.Good.ObservedAt) {
			older = true
			ns.Error = &model.ErrorInfo{Code: CodeOlderObservation}
			ns.Failures = bump(old.Failures)
			att.Outcome = CodeOlderObservation
		} else {
			ns.Good = g
			if p := u.Result.Profile; p != nil && (p.Identity != "" || p.Invalidated) {
				if e.def.Provider == "codex" && old.Profile != nil && !old.Profile.Invalidated && p.Identity != old.Profile.Identity && sameCodexAccount(old.Profile.Identity, p.Identity) {
					// A late response from the previous native token cannot replace
					// the generation selected by VerifyProfileIdentity.
				} else if old.Profile != nil && (old.Profile.Invalidated || (old.Profile.Identity != "" && p.Identity != old.Profile.Identity)) {
					ns.Profile = &model.Profile{Invalidated: true}
				} else {
					ns.Profile = &model.Profile{Identity: p.Identity, AccountEmail: model.Email(value(p.AccountEmail)), Plan: model.ProfileText(value(p.Plan), 100), Invalidated: p.Invalidated}
					ns.ProfileHidden = false
				}
			}
			ns.Error = nil
			ns.Failures = 0
			att.Outcome = "ok"
			if old.Good == nil || old.Good.Key != g.Key {
				ns.PendingHistory = append(ns.PendingHistory, historyRecord{
					ID: recordID(u.ID, ns.Seq, g.Key), SubID: u.ID, Seq: ns.Seq,
					Provider: e.def.Provider, Product: e.def.Product, Source: actual,
					RecordedAt: at, ObservedAt: g.ObservedAt, Plan: g.Plan,
					Quotas: g.Quotas, Metrics: g.Metrics,
				})
			}
		}
	} else {
		ns.Error = sanitizeFailure(*u.Failure)
		// These failures cannot establish that cached metadata still belongs to
		// the selected account. Fail closed without deleting last-good usage.
		if ns.Error.Code == "source_identity_changed" {
			ns.Profile = &model.Profile{Invalidated: true}
		}
		ns.Failures = bump(old.Failures)
		att.Outcome = "error"
	}
	att.Error = cloneErr(ns.Error)
	ns.PendingAttempts = append(ns.PendingAttempts, att)

	for _, r := range ns.PendingHistory {
		if b, err := json.Marshal(r); err != nil || len(b) >= maxLineSize {
			return ErrInvalid
		}
	}
	if b, err := json.Marshal(att); err != nil || len(b) >= maxLineSize {
		return ErrInvalid
	}

	if err := s.writeState(&ns); err != nil {
		return err
	}
	s.publish(e, &ns)
	cleared, err := s.drain(&ns)
	if err != nil {
		return err
	}
	s.publish(e, cleared)
	if older {
		return ErrOlderObservation
	}
	if identityChanged {
		return ErrIdentityChanged
	}
	return nil
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// VerifyProfileIdentity is metadata-only: no usage/error/schedule/history changes.
// An unavailable local source hides metadata until re-confirmed; a different
// account permanently blocks metadata for this ID, even if an old response arrives.
func (s *Store) VerifyProfileIdentity(id, identity string, valid bool) error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	s.mu.RLock()
	e, closed := s.byID[id], s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if e == nil {
		return ErrUnknownID
	}
	ns := *e.st
	if ns.Profile != nil && ns.Profile.Invalidated {
		return nil
	}
	expected := ""
	actual := identity
	if ns.Profile != nil && ns.Profile.Identity != "" {
		expected = ns.Profile.Identity
	} else if ns.Good != nil {
		expected = ns.Good.Identity
		// Legacy state has only the quota account fingerprint, not the native
		// credential generation suffix used for personal metadata.
		actual, _, _ = strings.Cut(identity, ".")
	}
	changed := valid && expected != "" && expected != actual
	if changed {
		if e.def.Provider == "codex" && sameCodexAccount(expected, actual) {
			// Codex can re-confirm identity from the authenticated usage response.
			// Hide the old profile immediately and select the new generation.
			ns.Profile = &model.Profile{Identity: actual}
		} else {
			ns.Profile = &model.Profile{Invalidated: true}
		}
	}
	hidden := !valid || changed
	if !changed && ns.ProfileHidden == hidden {
		return nil
	}
	ns.ProfileHidden = hidden
	if err := s.writeState(&ns); err != nil {
		return err
	}
	s.publish(e, &ns)
	return nil
}

func sameCodexAccount(a, b string) bool {
	accountA, _, hasA := strings.Cut(a, ".")
	accountB, _, hasB := strings.Cut(b, ".")
	return hasA && hasB && len(accountA) == 64 && accountA == accountB
}

// SetHybrid durably replaces the hybrid metadata and the next local check time
// in one atomic state write. It is metadata only: it records no attempt, success
// or history, and never touches last-good, error, failures or freshness.
func (s *Store) SetHybrid(id string, hs model.HybridState, nextCheck time.Time) error {
	if !ValidID(id) || nextCheck.IsZero() {
		return ErrInvalid
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	s.mu.RLock()
	closed, e := s.closed, s.byID[id]
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if e == nil {
		return ErrUnknownID
	}
	ns := *e.st
	ns.ID = id
	ns.Hybrid = sanitizeHybrid(hs)
	next := nextCheck.UTC()
	ns.NextAttemptAt = &next
	if err := s.writeState(&ns); err != nil {
		return err
	}
	s.publish(e, &ns)
	cleared, err := s.drain(&ns)
	if err != nil {
		return err
	}
	s.publish(e, cleared)
	return nil
}

// RescheduleSuccess applies a new cadence to a healthy ordinary collector.
// It cannot clear failure/cooldown state and never invents a data observation.
func (s *Store) RescheduleSuccess(id string, next time.Time) error {
	if !ValidID(id) || next.IsZero() {
		return ErrInvalid
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	s.mu.RLock()
	closed, e := s.closed, s.byID[id]
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if e == nil {
		return ErrUnknownID
	}
	if e.st.Error != nil || e.st.Good == nil || e.st.Hybrid != nil || next.Before(e.st.Good.ObservedAt) {
		return ErrInvalid
	}
	ns := *e.st
	v := next.UTC()
	ns.NextAttemptAt = &v
	if err := s.writeState(&ns); err != nil {
		return err
	}
	s.publish(e, &ns)
	cleared, err := s.drain(&ns)
	if err != nil {
		return err
	}
	s.publish(e, cleared)
	return nil
}

func (s *Store) publish(e *entry, st *state) {
	s.mu.Lock()
	e.st = st
	s.mu.Unlock()
}

// drain appends pending records to the journals (deduping against the last
// complete line), then persists the state without them.
func (s *Store) drain(st *state) (*state, error) {
	if len(st.PendingHistory) == 0 && len(st.PendingAttempts) == 0 {
		return st, nil
	}
	var hids, aids []string
	var hl, al [][]byte
	for _, r := range st.PendingHistory {
		b, err := json.Marshal(r)
		if err != nil {
			return st, ErrCorrupt
		}
		hids, hl = append(hids, r.ID), append(hl, b)
	}
	for _, r := range st.PendingAttempts {
		b, err := json.Marshal(r)
		if err != nil {
			return st, ErrCorrupt
		}
		aids, al = append(aids, r.ID), append(al, b)
	}
	if err := appendJSONL(s.historyPath(st.ID), hids, hl); err != nil {
		return st, err
	}
	if err := appendJSONL(s.attemptsPath(st.ID), aids, al); err != nil {
		return st, err
	}
	cl := *st
	cl.PendingHistory, cl.PendingAttempts = nil, nil
	if err := s.writeState(&cl); err != nil {
		return st, err
	}
	return &cl, nil
}

func (s *Store) statePath(id string) string    { return filepath.Join(s.root, "state", id+".json") }
func (s *Store) historyPath(id string) string  { return filepath.Join(s.root, "history", id+".jsonl") }
func (s *Store) attemptsPath(id string) string { return filepath.Join(s.root, "attempts", id+".jsonl") }

func (s *Store) writeState(st *state) error {
	st.Version = stateVersion
	if st.Hybrid != nil {
		st.Version = stateVersionHybrid
	}
	if st.RemoteProfile != nil {
		st.Version = stateVersionProfile
	}
	data, err := json.Marshal(st)
	if err != nil || len(data) > maxStateSize {
		return ErrInvalid
	}
	if err := writeFileAtomic(s.statePath(st.ID), data); err != nil {
		return ErrPersist
	}
	return nil
}

func (s *Store) readState(id string) (*state, error) {
	data, err := readBounded(s.statePath(id), maxStateSize)
	if errors.Is(err, os.ErrNotExist) {
		return &state{Version: stateVersion, ID: id}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: state %s", ErrCorrupt, id)
	}
	var st state
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&st); err != nil || st.ID != id ||
		!(st.Version == stateVersion || (st.Version == stateVersionHybrid && st.Hybrid != nil) || (st.Version == stateVersionProfile && st.RemoteProfile != nil)) {
		return nil, fmt.Errorf("%w: state %s", ErrCorrupt, id)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%w: state %s", ErrCorrupt, id)
	}
	if st.Hybrid != nil && st.Hybrid.RemoteFailures < 0 {
		return nil, fmt.Errorf("%w: state %s", ErrCorrupt, id)
	}
	if st.Failures < 0 || len(st.PendingHistory) > maxPending || len(st.PendingAttempts) > maxPending {
		return nil, fmt.Errorf("%w: state %s", ErrCorrupt, id)
	}
	if st.RemoteProfile != nil && st.RemoteProfile.Failures < 0 {
		return nil, fmt.Errorf("%w: state %s", ErrCorrupt, id)
	}
	if st.Good != nil {
		if st.Good.ObservedAt.IsZero() || st.Good.Quotas == nil || st.Good.Metrics == nil {
			return nil, fmt.Errorf("%w: state %s", ErrCorrupt, id)
		}
	}
	return &st, nil
}

// ---- validation / normalization ----

func recordID(id string, seq uint64, key string) string {
	if key == "" {
		return fmt.Sprintf("%s-%d", id, seq)
	}
	return fmt.Sprintf("%s-%d-%s", id, seq, key)
}

func definitionKey(d model.Definition) string {
	sum := sha256.Sum256([]byte(d.Provider + "\x00" + d.Product + "\x00" + d.Source))
	return hex.EncodeToString(sum[:])
}

func bump(n int) int {
	if n < maxFailures {
		return n + 1
	}
	return n
}

func finite(p *float64) bool { return p == nil || (!math.IsNaN(*p) && !math.IsInf(*p, 0)) }

func normalizeResult(r model.Result) (*good, error) {
	if r.ObservedAt.IsZero() {
		return nil, ErrInvalid
	}
	g := &good{
		ObservedAt: r.ObservedAt.UTC(),
		Identity:   r.Identity,
		Source:     safeSource(r.Source),
		Quotas:     cloneQuotas(r.Quotas),
		Metrics:    cloneMetrics(r.Metrics),
	}
	if r.Plan != nil {
		p := *r.Plan
		g.Plan = &p
	}
	for i := range g.Quotas {
		q := &g.Quotas[i]
		if !finite(q.Used) || !finite(q.Limit) || !finite(q.Remaining) || !finite(q.UsedPercent) || !finite(q.RemainingPercent) {
			return nil, ErrInvalid
		}
		utcTime(&q.Window.StartedAt)
		utcTime(&q.Window.ResetAt)
	}
	for _, m := range g.Metrics {
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			return nil, ErrInvalid
		}
	}
	if len(r.Raw) > 0 {
		if len(r.Raw) > maxRawSize || !json.Valid(r.Raw) {
			return nil, ErrInvalid
		}
		g.Raw = append(json.RawMessage(nil), r.Raw...)
	}
	key, err := json.Marshal(struct {
		O time.Time
		P *string
		Q []model.Quota
		M []model.Metric
		S string `json:",omitempty"`
	}{g.ObservedAt, g.Plan, g.Quotas, g.Metrics, g.Source})
	if err != nil {
		return nil, ErrInvalid
	}
	sum := sha256.Sum256(key)
	g.Key = hex.EncodeToString(sum[:16])
	return g, nil
}

func sanitizeFailure(f model.ErrorInfo) *model.ErrorInfo {
	out := &model.ErrorInfo{Code: f.Code}
	if !safeCode(f.Code) {
		out.Code = CodeCollectionError
	}
	if f.HTTPStatus >= 100 && f.HTTPStatus <= 599 {
		out.HTTPStatus = f.HTTPStatus
	}
	if f.RetryAt != nil && !f.RetryAt.IsZero() {
		t := f.RetryAt.UTC()
		out.RetryAt = &t
	}
	return out
}

func sanitizeHybrid(h model.HybridState) *model.HybridState {
	out := &model.HybridState{
		LastPassiveAt:       nonZeroUTC(h.LastPassiveAt),
		LastRemoteAttemptAt: nonZeroUTC(h.LastRemoteAttemptAt),
		NextRemoteAttemptAt: nonZeroUTC(h.NextRemoteAttemptAt),
		RemoteFailures:      h.RemoteFailures,
	}
	if out.RemoteFailures < 0 {
		out.RemoteFailures = 0
	}
	if out.RemoteFailures > maxFailures {
		out.RemoteFailures = maxFailures
	}
	if h.RemoteError != nil {
		out.RemoteError = sanitizeFailure(*h.RemoteError)
	}
	return out
}

func nonZeroUTC(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	c := t.UTC()
	return &c
}

func cloneHybrid(h *model.HybridState) *model.HybridState {
	if h == nil {
		return nil
	}
	return &model.HybridState{
		LastPassiveAt:       cloneTime(h.LastPassiveAt),
		LastRemoteAttemptAt: cloneTime(h.LastRemoteAttemptAt),
		NextRemoteAttemptAt: cloneTime(h.NextRemoteAttemptAt),
		RemoteFailures:      h.RemoteFailures,
		RemoteError:         cloneErr(h.RemoteError),
	}
}

// safeSource accepts only short fixed identifiers from trusted collectors;
// anything else falls back to the definition's source.
func safeSource(c string) string {
	if len(c) == 0 || len(c) > 64 {
		return ""
	}
	for i := 0; i < len(c); i++ {
		b := c[i]
		if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-' || b == '.') {
			return ""
		}
	}
	return c
}

func safeCode(c string) bool {
	if len(c) == 0 || len(c) > 64 {
		return false
	}
	for i := 0; i < len(c); i++ {
		b := c[i]
		if !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_') {
			return false
		}
	}
	return true
}

// ---- deep copies ----

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func utcTime(p **time.Time) {
	if *p != nil {
		t := (*p).UTC()
		*p = &t
	}
}

func cloneF(p *float64) *float64 {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

func cloneStr(p *string) *string {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

func cloneErr(e *model.ErrorInfo) *model.ErrorInfo {
	if e == nil {
		return nil
	}
	return &model.ErrorInfo{Code: e.Code, HTTPStatus: e.HTTPStatus, RetryAt: cloneTime(e.RetryAt)}
}

func cloneQuotas(in []model.Quota) []model.Quota {
	out := make([]model.Quota, len(in))
	for i, q := range in {
		out[i] = model.Quota{
			ID: q.ID, Scope: cloneStr(q.Scope), Metric: q.Metric, Unit: q.Unit,
			Used: cloneF(q.Used), Limit: cloneF(q.Limit), Remaining: cloneF(q.Remaining),
			UsedPercent: cloneF(q.UsedPercent), RemainingPercent: cloneF(q.RemainingPercent),
			Authority: q.Authority,
			Label:      cloneStr(q.Label),
			ObservedAt: cloneTime(q.ObservedAt),
			Window: model.Window{
				Kind: q.Window.Kind, Period: cloneStr(q.Window.Period),
				StartedAt: cloneTime(q.Window.StartedAt), ResetAt: cloneTime(q.Window.ResetAt),
			},
		}
		if q.Limited != nil {
			limited := *q.Limited
			out[i].Limited = &limited
		}
		if len(q.Counters) > 0 {
			counters := make(map[string]float64, len(q.Counters))
			for key, value := range q.Counters {
				counters[key] = value
			}
			out[i].Counters = counters
		}
		if q.Window.DurationSeconds != nil {
			d := *q.Window.DurationSeconds
			out[i].Window.DurationSeconds = &d
		}
	}
	return out
}

func cloneMetrics(in []model.Metric) []model.Metric {
	out := make([]model.Metric, len(in))
	copy(out, in)
	return out
}

// ---- filesystem helpers ----

func ensureDir(path string, root bool) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if root {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("%w: create", ErrUnsafeRoot)
			}
		}
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: create", ErrUnsafeRoot)
		}
		fi, err = os.Lstat(path)
	}
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return ErrUnsafeRoot
	}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return ErrUnsafeRoot
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return ErrUnsafeRoot
		}
	}
	return nil
}

func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrCorrupt
	}
	return data, nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// tail inspects the end of a JSONL file. It returns the file size, the size of
// an incomplete trailing fragment (no final newline) and the last complete line.
func tail(f *os.File) (size, fragment int64, last []byte, err error) {
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return 0, 0, nil, ErrCorrupt
	}
	size = fi.Size()
	if size == 0 {
		return 0, 0, nil, nil
	}
	win := int64(maxLineSize + 2)
	if size < win {
		win = size
	}
	buf := make([]byte, win)
	if _, err := f.ReadAt(buf, size-win); err != nil {
		return 0, 0, nil, err
	}
	whole := win == size
	if buf[len(buf)-1] != '\n' {
		i := bytes.LastIndexByte(buf, '\n')
		if i < 0 && !whole {
			return 0, 0, nil, ErrCorrupt
		}
		fragment = int64(len(buf) - (i + 1))
		if fragment > maxLineSize {
			return 0, 0, nil, ErrCorrupt
		}
		buf = buf[:i+1]
		if len(buf) == 0 {
			return size, fragment, nil, nil
		}
	}
	buf = buf[:len(buf)-1]
	i := bytes.LastIndexByte(buf, '\n')
	if i < 0 && !whole {
		return 0, 0, nil, ErrCorrupt
	}
	last = buf[i+1:]
	if len(last) > maxLineSize {
		return 0, 0, nil, ErrCorrupt
	}
	return size, fragment, last, nil
}

func lastID(line []byte) (string, error) {
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(line, &r); err != nil || r.ID == "" {
		return "", ErrCorrupt
	}
	return r.ID, nil
}

// repairTail fixes only our own incomplete trailing line: a fragment that is
// valid JSON gets its newline, any other fragment is truncated. A corrupt
// complete last line fails closed. Caller must hold the directory lock.
func repairTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrJournal
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return ErrJournal
	}
	size, frag, last, err := tail(f)
	if err != nil {
		return ErrCorrupt
	}
	if frag > 0 {
		buf := make([]byte, frag)
		if _, err := f.ReadAt(buf, size-frag); err != nil {
			return ErrCorrupt
		}
		if _, err := lastID(buf); err == nil && json.Valid(buf) {
			if _, err := f.WriteAt([]byte{'\n'}, size); err != nil {
				return ErrJournal
			}
		} else if err := f.Truncate(size - frag); err != nil {
			return ErrJournal
		}
		if err := f.Sync(); err != nil {
			return ErrJournal
		}
		if size, frag, last, err = tail(f); err != nil || frag != 0 {
			return ErrCorrupt
		}
	}
	if last != nil {
		if _, err := lastID(last); err != nil {
			return ErrCorrupt
		}
	}
	return nil
}

// appendJSONL appends lines whose IDs follow the last complete journal line,
// skipping everything up to and including a pending record already present.
func appendJSONL(path string, ids []string, lines [][]byte) error {
	if err := repairTail(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return ErrJournal
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return ErrJournal
	}
	size, _, last, err := tail(f)
	if err != nil {
		return ErrCorrupt
	}
	start := 0
	if last != nil {
		id, err := lastID(last)
		if err != nil {
			return ErrCorrupt
		}
		for i := len(ids) - 1; i >= 0; i-- {
			if ids[i] == id {
				start = i + 1
				break
			}
		}
	}
	if start >= len(lines) {
		return nil
	}
	var buf bytes.Buffer
	for _, l := range lines[start:] {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		return ErrJournal
	}
	if err := f.Sync(); err != nil {
		return ErrJournal
	}
	if size == 0 {
		syncDir(filepath.Dir(path))
	}
	return nil
}
