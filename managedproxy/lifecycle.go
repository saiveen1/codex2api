package managedproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Refresh fetches an enabled subscription (all enabled subscriptions for an empty
// ID) and atomically applies the candidate. One failed fetch aborts the whole batch.
func (m *Manager) Refresh(ctx context.Context, id string) error {
	ctx, cancel := m.operationContext(ctx)
	defer cancel()
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	err := m.refreshLocked(ctx, id, true, nil)
	hook := m.hook
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

// Ensure restores persisted feeds after startup, only when subscriptions are
// enabled. It never fetches feeds on the startup path; missing feeds require Refresh.
// It also starts the optional periodic scheduler (0 is manual, minimum 5 minutes).
func (m *Manager) Ensure(ctx context.Context) error {
	ctx, cancel := m.operationContext(ctx)
	defer cancel()
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	if m.closed {
		m.mu.Unlock()
		return errors.New("managed proxy: manager is closed")
	}
	if !m.scheduler {
		m.scheduler = true
		go m.schedule()
	}
	var err error
	if m.proc == nil || !m.proc.healthy || !m.proc.alive() {
		err = m.refreshLocked(ctx, "", false, nil)
	}
	hook := m.hook
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

// DeleteSubscription reconfigures from cached immutable feeds without refetching.
// Historical nodes remain present but inactive, retaining their stable URLs forever.
func (m *Manager) DeleteSubscription(ctx context.Context, id string) error {
	ctx, cancel := m.operationContext(ctx)
	defer cancel()
	if err := m.mu.lock(ctx); err != nil {
		return err
	}
	if m.closed {
		m.mu.Unlock()
		return errors.New("managed proxy: manager is closed")
	}
	s := cloneState(m.state)
	found := false
	for i, sub := range s.Subscriptions {
		if sub.ID == id {
			s.Subscriptions = append(s.Subscriptions[:i], s.Subscriptions[i+1:]...)
			delete(s.Feeds, id)
			found = true
			break
		}
	}
	if !found {
		m.mu.Unlock()
		return errors.New("managed proxy: subscription not found")
	}
	var err error
	if err = ctx.Err(); err == nil {
		if m.proc == nil || !m.proc.alive() {
			for i := range s.Nodes {
				if s.Nodes[i].SubscriptionID == id {
					s.Nodes[i].Active = false
				}
			}
			err = m.commitStopped(s)
		} else {
			err = m.refreshLocked(ctx, "", false, &s)
		}
	}
	hook := m.hook
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (m *Manager) refreshLocked(ctx context.Context, id string, fetch bool, candidate *diskState) (result error) {
	if m.closed {
		return errors.New("managed proxy: manager is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if id != "" {
		found := false
		for _, sub := range m.state.Subscriptions {
			if sub.ID == id {
				found = true
				break
			}
		}
		if !found {
			return errors.New("managed proxy: subscription not found")
		}
	}
	s := cloneState(m.state)
	if candidate != nil {
		s = cloneState(*candidate)
	}
	created := []string{}
	failedID := id
	defer func() {
		if result == nil {
			return
		}
		for _, path := range created {
			_ = os.Remove(path)
		}
		// All errors crossing this boundary are generated locally, not from remote
		// bodies, exec output, net/url.Error, or feed contents.
		message := result.Error()
		if failedID != "" {
			message = fmt.Sprintf("managed proxy: subscription %s: %s", failedID, message)
		}
		m.state.LastError = message
		for i := range m.state.Subscriptions {
			sub := &m.state.Subscriptions[i]
			if sub.ID == failedID || failedID == "" && sub.Enabled && (id == "" || sub.ID == id) {
				sub.LastError = message
			}
		}
		if err := m.persist(m.state); err != nil {
			result = errors.New("managed proxy: operation failed and error state could not be saved")
		} else {
			result = errors.New(message)
		}
	}()
	fetched := map[string]bool{}
	providers := 0
	missingID := ""
	for _, sub := range s.Subscriptions {
		if !sub.Enabled {
			continue
		}
		if fetch && (id == "" || id == sub.ID) {
			failedID = sub.ID
			b, err := m.fetchFeed(ctx, sub.URL, feedLimit)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(append([]byte(sub.ID+"\x00"), b...))
			name := hex.EncodeToString(digest[:]) + ".feed"
			path := filepath.Join(m.root, "feeds", name)
			old, err := readPrivate(path, feedLimit)
			if err == nil {
				if !bytes.Equal(old, b) {
					return errors.New("managed proxy: immutable feed integrity check failed")
				}
			} else if os.IsNotExist(err) {
				if err := atomicFile(path, b, 0600); err != nil {
					return err
				}
				created = append(created, path)
			} else {
				return errors.New("managed proxy: cannot read private feed")
			}
			s.Feeds[sub.ID] = name
			fetched[sub.ID] = true
		}
		if s.Feeds[sub.ID] == "" {
			// Pending subscriptions have no live nodes and must not block another
			// subscription's targeted refresh or deletion.
			missingID = sub.ID
			continue
		}
		if _, err := readPrivate(filepath.Join(m.root, "feeds", s.Feeds[sub.ID]), feedLimit); err != nil {
			failedID = sub.ID
			return errors.New("managed proxy: saved feed is missing, unsafe or unreadable; Refresh it again")
		}
		providers++
	}
	failedID = id
	for i := range s.Nodes {
		s.Nodes[i].Active = false
	}
	for i := range s.Subscriptions {
		if !s.Subscriptions[i].Enabled {
			s.Subscriptions[i].KeptNodes = 0
		}
	}
	s.LastError = ""
	if providers == 0 {
		if missingID != "" && candidate == nil {
			failedID = missingID
			return errors.New("managed proxy: no saved feed; explicitly Refresh a subscription first")
		}
		if err := m.commitStopped(s); err != nil {
			return err
		}
		return nil
	}
	binary, err := m.resolveBinary(ctx, s.Engine)
	if err != nil {
		return err
	}
	stagePort, err := freePort()
	if err != nil {
		return err
	}
	secret, err := randomToken(24)
	if err != nil {
		return err
	}
	stageJSON, err := engineJSON(m.root, s, stagePort, secret, false)
	if err != nil {
		return err
	}
	stage, err := m.launchCore(ctx, binary, stageJSON, stagePort, secret)
	if err != nil {
		return err
	}
	defer stage.stop()
	nodes := map[string]Node{}
	for _, n := range s.Nodes {
		nodes[n.Key] = n
	}
	for i := range s.Subscriptions {
		sub := &s.Subscriptions[i]
		if !sub.Enabled || s.Feeds[sub.ID] == "" {
			sub.KeptNodes = 0
			continue
		}
		failedID = sub.ID
		proxies, err := stage.provider(ctx, sub.ID)
		if err != nil {
			return err
		}
		names, total, err := selectNames(*sub, proxies)
		if err != nil {
			return err
		}
		sub.TotalNodes, sub.KeptNodes = total, len(names)
		if fetched[sub.ID] {
			sub.LastUpdated = time.Now().UTC().Format(time.RFC3339)
			sub.LastError = ""
		}
		for _, name := range names {
			n, err := allocate(&s, sub.ID, name)
			if err != nil {
				return err
			}
			nodes[n.Key] = n
		}
	}
	s.Nodes = sortedNodes(nodes)
	failedID = id
	config, err := engineJSON(m.root, s, s.Engine.ControllerPort, s.Secret, true)
	if err != nil {
		return err
	}
	if err := validateConfig(ctx, binary, m.root, config); err != nil {
		return err
	}
	if err := stage.stop(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rollback, err := m.activate(ctx, binary, config, s)
	if err != nil {
		return err
	}
	if err := m.persist(s); err != nil {
		rollback()
		return err
	}
	oldFeeds := m.state.Feeds
	m.state = s
	m.deleteUnreferenced(oldFeeds, s.Feeds)
	return nil
}

func (m *Manager) commitStopped(s diskState) error {
	old := m.proc
	if old != nil {
		if err := old.stop(); err != nil {
			old.healthy = false
			return err
		}
	}
	m.proc = nil
	if err := m.persist(s); err != nil {
		if old != nil && m.ctx.Err() == nil {
			ctx, cancel := context.WithTimeout(m.ctx, 25*time.Second)
			defer cancel()
			if p, e := m.launchCore(ctx, old.binary, old.config, old.port, old.secret); e == nil && p.selectNodes(ctx, m.state.Nodes) == nil {
				p.ports = old.ports
				p.healthy = old.healthy
				m.proc = p
				m.version = p.version
				m.monitor(p)
			} else if p != nil {
				_ = p.stop()
			}
		}
		return err
	}
	oldFeeds := m.state.Feeds
	m.state = s
	m.deleteUnreferenced(oldFeeds, s.Feeds)
	return nil
}
func (m *Manager) deleteUnreferenced(old, current map[string]string) {
	keep := map[string]bool{}
	for _, name := range current {
		keep[name] = true
	}
	for _, name := range old {
		if !keep[name] {
			_ = os.Remove(filepath.Join(m.root, "feeds", name))
		}
	}
}

func (m *Manager) schedule() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	attempts := map[string]time.Time{}
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.Lock()
			ids := []string{}
			for _, sub := range m.state.Subscriptions {
				if !sub.Enabled || sub.RefreshMinutes < 5 {
					continue
				}
				last, _ := time.Parse(time.RFC3339, sub.LastUpdated)
				if attempts[sub.ID].After(last) {
					last = attempts[sub.ID]
				}
				if now.Sub(last) >= time.Duration(sub.RefreshMinutes)*time.Minute {
					ids = append(ids, sub.ID)
				}
			}
			m.mu.Unlock()
			for _, id := range ids {
				if m.ctx.Err() != nil {
					return
				}
				attempts[id] = time.Now()
				_ = m.Refresh(m.ctx, id)
			}
		}
	}
}

// Close cancels in-flight/scheduled work and stops only the recorded owned child.
// Closing does not erase stable node identities; View immediately marks them inactive.
func (m *Manager) Close() error {
	m.cancel()
	m.mu.Lock()
	m.closed = true
	var err error
	for _, child := range m.children {
		if e := child.stop(); e != nil {
			err = e
		}
	}
	if m.proc != nil && !m.proc.alive() {
		m.proc = nil
	}
	hook := m.hook
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}
