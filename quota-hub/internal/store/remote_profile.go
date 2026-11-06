package store

import "quota-hub/internal/model"

func cloneProfileState(p *model.ProfileState) *model.ProfileState {
	if p == nil {
		return nil
	}
	return &model.ProfileState{Identity: p.Identity, LastAttemptAt: cloneTime(p.LastAttemptAt), NextAttemptAt: cloneTime(p.NextAttemptAt), Failures: p.Failures, Error: cloneErr(p.Error)}
}

// selectRemoteProfileIdentity never carries metadata across a key/credential
// change, even if OpenCode reuses its credential entry ID. Error cooldowns are
// retained across a switch; a previous successful cache TTL is not.
func selectRemoteProfileIdentity(st *state, identity string, checked bool) {
	if !checked {
		return
	}
	if identity == "" {
		st.ProfileHidden = true
		return
	}
	if st.Profile != nil && st.Profile.Invalidated {
		return
	}
	p := cloneProfileState(st.RemoteProfile)
	if p == nil {
		p = &model.ProfileState{Identity: identity}
		if st.Profile != nil && st.Profile.Identity != identity {
			st.Profile = &model.Profile{Identity: identity}
		}
	}
	if p.Identity != identity {
		p.Identity = identity
		if p.Error == nil {
			p.NextAttemptAt = nil
			p.LastAttemptAt = nil
			p.Failures = 0
		}
		st.Profile = &model.Profile{Identity: identity}
	}
	st.RemoteProfile = p
	st.ProfileHidden = false
}

func (s *Store) RemoteProfile(id string) (model.ProfileState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e := s.byID[id]
	if e == nil || e.st.RemoteProfile == nil || e.st.ProfileHidden || (e.st.Profile != nil && e.st.Profile.Invalidated) {
		return model.ProfileState{}, false
	}
	return *cloneProfileState(e.st.RemoteProfile), true
}

// HideRemoteProfile forces revalidation after startup, before HTTP can serve
// restored personal metadata. It performs no network operation or polling.
func (s *Store) HideRemoteProfile(id string) error {
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
	if e.st.RemoteProfile == nil || e.st.ProfileHidden {
		return nil
	}
	ns := *e.st
	ns.ProfileHidden = true
	if err := s.writeState(&ns); err != nil {
		return err
	}
	s.publish(e, &ns)
	return nil
}

// RemoteProfileSchedule includes hidden metadata so restart can restore a
// server Retry-After before any new usage request is allowed.
func (s *Store) RemoteProfileSchedule(id string) (model.ProfileState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e := s.byID[id]
	if e == nil || e.st.RemoteProfile == nil {
		return model.ProfileState{}, false
	}
	return *cloneProfileState(e.st.RemoteProfile), true
}

// CommitProfile atomically stores metadata only. Usage freshness, root errors,
// raw, attempt counts and usage/history journals are deliberately untouched.
func (s *Store) CommitProfile(id string, u model.ProfileUpdate) error {
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
	if u.AttemptAt.IsZero() || u.NextAttemptAt.IsZero() || (u.Profile == nil) == (u.Failure == nil) {
		return ErrInvalid
	}
	old := e.st
	if old.RemoteProfile == nil || u.ExpectedIdentity == "" || old.RemoteProfile.Identity != u.ExpectedIdentity {
		return ErrOlderObservation
	}
	if old.RemoteProfile.LastAttemptAt != nil && u.AttemptAt.Before(*old.RemoteProfile.LastAttemptAt) {
		return ErrOlderObservation
	}
	if old.Profile != nil && old.Profile.Invalidated {
		return ErrIdentityChanged
	}
	ns := *old
	selectRemoteProfileIdentity(&ns, u.CurrentIdentity, u.IdentityChecked)
	p := cloneProfileState(ns.RemoteProfile)
	at, next := u.AttemptAt.UTC(), u.NextAttemptAt.UTC()
	p.LastAttemptAt, p.NextAttemptAt = &at, &next
	if u.Profile != nil {
		if !u.IdentityChecked || u.CurrentIdentity != u.ExpectedIdentity || u.Profile.Identity != u.ExpectedIdentity {
			return ErrInvalid
		}
		ns.Profile = &model.Profile{Identity: u.ExpectedIdentity, AccountEmail: model.Email(value(u.Profile.AccountEmail)), Plan: model.ProfileText(value(u.Profile.Plan), 100)}
		ns.ProfileHidden = false
		p.Failures = 0
		p.Error = nil
	} else {
		p.Failures = bump(p.Failures)
		p.Error = sanitizeFailure(*u.Failure)
	}
	ns.RemoteProfile = p
	if err := s.writeState(&ns); err != nil {
		return err
	}
	s.publish(e, &ns)
	return nil
}
