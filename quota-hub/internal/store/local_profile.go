package store

import "quota-hub/internal/model"

// CommitLocalProfile is metadata-only: it changes no usage observation, clocks,
// cooldown, root error, attempt or history. Unchanged local files cause no write.
func (s *Store) CommitLocalProfile(id string, profile model.Profile) error {
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
	if profile.Identity == "" || profile.Invalidated {
		return ErrInvalid
	}
	old := e.st
	if old.Profile != nil && (old.Profile.Invalidated || (old.Profile.Identity != "" && old.Profile.Identity != profile.Identity)) {
		return ErrIdentityChanged
	}
	p := &model.Profile{Identity: profile.Identity, AccountEmail: model.Email(value(profile.AccountEmail)), Plan: model.ProfileText(value(profile.Plan), 100)}
	if old.Profile != nil && !old.ProfileHidden && old.Profile.Identity == p.Identity && value(old.Profile.AccountEmail) == value(p.AccountEmail) && value(old.Profile.Plan) == value(p.Plan) {
		return nil
	}
	ns := *old
	ns.Profile, ns.ProfileHidden = p, false
	if err := s.writeState(&ns); err != nil {
		return err
	}
	s.publish(e, &ns)
	return nil
}
