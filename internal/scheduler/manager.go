// Package scheduler runs one poll loop per switch and books traffic.
package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lan-dot-party/lanleecher/internal/accounting"
	"github.com/lan-dot-party/lanleecher/internal/collector"
	"github.com/lan-dot-party/lanleecher/internal/config"
	"github.com/lan-dot-party/lanleecher/internal/resolver"
	"github.com/lan-dot-party/lanleecher/internal/secret"
	"github.com/lan-dot-party/lanleecher/internal/snmp"
	"github.com/lan-dot-party/lanleecher/internal/store"
)

// Manager owns the pollers.
type Manager struct {
	db       *store.DB
	key      *secret.Key
	cfg      *config.Config
	log      *slog.Logger
	resolver *resolver.Service

	// OnPoll is called after every successful counter poll (board refresh).
	OnPoll func()

	mu      sync.Mutex
	ctx     context.Context
	pollers map[int64]*poller
}

// New creates a manager. Call Start to begin polling.
func New(db *store.DB, key *secret.Key, cfg *config.Config, res *resolver.Service, log *slog.Logger) *Manager {
	return &Manager{db: db, key: key, cfg: cfg, resolver: res, log: log.With("component", "scheduler"),
		pollers: map[int64]*poller{}}
}

// Start launches pollers for all enabled switches.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	return m.Reload(ctx)
}

// Reload starts, stops or restarts pollers to match the database.
func (m *Manager) Reload(ctx context.Context) error {
	switches, err := m.db.ListSwitches(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return nil
	}
	want := map[int64]*store.Switch{}
	for _, s := range switches {
		if s.Enabled {
			want[s.ID] = s
		}
	}
	for id, p := range m.pollers {
		s, ok := want[id]
		if !ok || fingerprint(s) != p.fingerprint {
			p.stop()
			delete(m.pollers, id)
		}
	}
	for id, s := range want {
		if _, running := m.pollers[id]; running {
			continue
		}
		target, err := m.Target(s)
		if err != nil {
			m.log.Error("switch credentials", "switch", s.Name, "err", err)
			if serr := m.db.SetSwitchStatus(ctx, s.ID, false, err.Error(), store.Now()); serr != nil {
				m.log.Error("save switch status", "switch", s.Name, "err", serr)
			}
			continue
		}
		p := newPoller(m, s, target)
		pctx, cancel := context.WithCancel(m.ctx)
		p.cancel = cancel
		m.pollers[id] = p
		go p.run(pctx)
	}
	return nil
}

// PollNow triggers an immediate poll of a switch (after a settings change).
func (m *Manager) PollNow(id int64) {
	m.mu.Lock()
	p := m.pollers[id]
	m.mu.Unlock()
	if p != nil {
		select {
		case p.kick <- struct{}{}:
		default:
		}
	}
}

// Target builds the SNMP target of a switch, decrypting its credentials.
func (m *Manager) Target(s *store.Switch) (snmp.Target, error) {
	plain, err := m.key.Decrypt(s.CredentialsEnc)
	if err != nil {
		return snmp.Target{}, err
	}
	creds, err := snmp.UnmarshalCredentials(plain)
	if err != nil {
		return snmp.Target{}, fmt.Errorf("decode credentials: %w", err)
	}
	return snmp.Target{Host: s.Host, Port: s.Port, Version: s.SNMPVersion, Creds: creds,
		Timeout: m.cfg.Polling.Timeout, Retries: m.cfg.Polling.Retries}, nil
}

// TestResult is shown by the "Verbindung testen" button.
type TestResult struct {
	System     collector.System
	Interfaces []*collector.Interface
	Duration   time.Duration
}

// Test connects to a target and lists its interfaces without storing anything.
func (m *Manager) Test(ctx context.Context, t snmp.Target) (*TestResult, error) {
	start := time.Now()
	c := &collector.SNMP{Target: t}
	sys, err := c.System(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := c.Poll(ctx)
	if err != nil {
		return nil, err
	}
	res := &TestResult{System: sys, Duration: time.Since(start)}
	res.Interfaces = append(res.Interfaces, sortedInterfaces(snap)...)
	return res, nil
}

// Rate is the live throughput of a port from the last poll.
type Rate struct {
	DownBps float64
	UpBps   float64
	Role    accounting.Role
	At      time.Time
}

// PortRates returns the live rates of a switch's ports by port ID.
func (m *Manager) PortRates(switchID int64) map[int64]Rate {
	m.mu.Lock()
	p := m.pollers[switchID]
	m.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.rates()
}

// Throughput sums the live rates of all non-uplink ports.
func (m *Manager) Throughput() (downBps, upBps float64) {
	m.mu.Lock()
	ps := make([]*poller, 0, len(m.pollers))
	for _, p := range m.pollers {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	for _, p := range ps {
		for _, r := range p.rates() {
			if r.Role == accounting.RoleUplink || time.Since(r.At) > 3*p.interval {
				continue
			}
			downBps += r.DownBps
			upBps += r.UpBps
		}
	}
	return downBps, upBps
}

// Status is the live state of a poller.
type Status struct {
	Running      bool
	LastDuration time.Duration
	Interval     time.Duration
	// NoFDB is set when the last FDB reads succeeded but returned no entries:
	// the switch's traffic cannot be attributed to clients.
	NoFDB bool
}

// Status returns the poller state of a switch.
func (m *Manager) Status(switchID int64) Status {
	m.mu.Lock()
	p := m.pollers[switchID]
	m.mu.Unlock()
	if p == nil {
		return Status{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return Status{Running: true, LastDuration: p.lastDuration, Interval: p.interval, NoFDB: p.emptyFDB >= noFDBAfter}
}

func fingerprint(s *store.Switch) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%d|%s|%v|", s.Host, s.Port, s.SNMPVersion, s.PollInterval, s.FDBAdapter, s.ARPSource)
	h.Write(s.CredentialsEnc)
	return hex.EncodeToString(h.Sum(nil))
}
