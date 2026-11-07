package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/lan-dot-party/lanleecher/internal/accounting"
	"github.com/lan-dot-party/lanleecher/internal/collector"
	"github.com/lan-dot-party/lanleecher/internal/fdb"
	"github.com/lan-dot-party/lanleecher/internal/macaddr"
	"github.com/lan-dot-party/lanleecher/internal/resolver"
	"github.com/lan-dot-party/lanleecher/internal/snmp"
	"github.com/lan-dot-party/lanleecher/internal/store"
)

type poller struct {
	m           *Manager
	sw          *store.Switch
	target      snmp.Target
	fingerprint string
	interval    time.Duration
	fdbInterval time.Duration
	log         *slog.Logger
	cancel      context.CancelFunc
	kick        chan struct{}

	mu           sync.Mutex
	macs         map[int][]macaddr.MAC // ifIndex -> MACs from the last FDB read
	live         map[int64]Rate
	lastDuration time.Duration
	lastFDB      time.Time
	lastUnknown  int // FDB entries on unknown interfaces in the last read
	emptyFDB     int // successful FDB reads in a row that returned no entries
}

// noFDBAfter is the number of empty FDB reads in a row after which a switch
// is taken to have no forwarding table at all (Grandstream GWN switches, for
// example, implement neither BRIDGE-MIB nor Q-BRIDGE-MIB forwarding tables).
// One empty read is normal right after a switch has booted.
const noFDBAfter = 2

// noFDB reports whether the switch seems to have no forwarding table.
func (p *poller) noFDB() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.emptyFDB >= noFDBAfter
}

func newPoller(m *Manager, s *store.Switch, t snmp.Target) *poller {
	interval := m.cfg.Polling.Interval
	if s.PollInterval > 0 {
		interval = time.Duration(s.PollInterval) * time.Second
	}
	fdbInterval := m.cfg.Polling.FDBInterval
	if fdbInterval < interval {
		fdbInterval = interval
	}
	return &poller{m: m, sw: s, target: t, fingerprint: fingerprint(s), interval: interval, fdbInterval: fdbInterval,
		log: m.log.With("switch", s.Name), kick: make(chan struct{}, 1), live: map[int64]Rate{}}
}

func (p *poller) stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *poller) run(ctx context.Context) {
	p.log.Info("poller started", "host", p.sw.Host, "interval", p.interval, "fdb_interval", p.fdbInterval)
	defer p.log.Info("poller stopped")

	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		p.cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.kick:
		}
	}
}

// cycle polls counters and, when due, the forwarding table. Counters go
// first so that new ports exist before MACs are assigned to them.
func (p *poller) cycle(ctx context.Context) {
	start := time.Now()
	pollCtx, cancel := context.WithTimeout(ctx, p.interval)
	defer cancel()
	err := p.pollCounters(pollCtx)
	if err == nil && time.Since(p.lastFDB) >= p.fdbInterval-time.Second {
		if ferr := p.pollFDB(pollCtx); ferr != nil {
			p.log.Warn("fdb poll failed", "err", ferr)
		} else {
			p.lastFDB = time.Now()
		}
	}
	if ctx.Err() != nil {
		return
	}
	status := ""
	if err != nil {
		status = err.Error()
		p.log.Warn("counter poll failed", "err", err)
	}
	if serr := p.m.db.SetSwitchStatus(context.WithoutCancel(ctx), p.sw.ID, err == nil, status, store.Now()); serr != nil {
		p.log.Error("save switch status", "err", serr)
	}
	p.mu.Lock()
	p.lastDuration = time.Since(start)
	p.mu.Unlock()
	if err == nil && p.m.OnPoll != nil {
		p.m.OnPoll()
	}
}

func (p *poller) pollCounters(ctx context.Context) error {
	g, err := snmp.Dial(ctx, p.target, 0)
	if err != nil {
		return err
	}
	defer snmp.Close(g)
	snap, err := collector.PollWith(g)
	if err != nil {
		return err
	}
	if p.sw.SysDescr == "" {
		p.refreshSystem(ctx, g)
	}

	states, err := p.m.db.LoadCounterStates(ctx, p.sw.ID)
	if err != nil {
		return err
	}
	event, err := p.m.db.ActiveEvent(ctx)
	if errors.Is(err, store.ErrNotFound) {
		event = nil
	} else if err != nil {
		return err
	}
	p.mu.Lock()
	macs := p.macs
	p.mu.Unlock()

	obs := make([]store.PortObservation, 0, len(snap.Interfaces))
	for _, i := range snap.Interfaces {
		role := accounting.RoleClient
		if !i.Physical() || i.IsLAG() {
			// VLAN interfaces carry routed traffic, port-channels are trunks.
			role = accounting.RoleUplink
		}
		obs = append(obs, store.PortObservation{IfIndex: i.IfIndex, IfName: i.Name, IfAlias: i.Alias, IfDescr: i.Descr,
			IfType: i.Type, Physical: i.Physical(), OperUp: i.OperUp, SpeedMbps: i.SpeedMbps, HCSupported: i.HasHC,
			DefaultRole: role})
	}

	live := map[int64]Rate{}
	var booked uint64
	err = p.m.db.InTx(ctx, func(tx *store.Tx) error {
		ports, err := tx.UpsertPorts(ctx, p.sw.ID, obs, store.Now())
		if err != nil {
			return err
		}
		for _, i := range sortedInterfaces(snap) {
			port := ports[i.IfIndex]
			if port == nil || !i.HasHC || !i.OperUp {
				// Down ports keep their last state; the next successful
				// reading continues from there.
				continue
			}
			cur := store.CounterState{PortID: port.ID, In: i.HCIn, Out: i.HCOut, SysUptime: snap.SysUptime,
				Discontinuity: i.Discontinuity, PolledAt: snap.At}
			var resIn, resOut accounting.Result
			if prev, ok := states[port.ID]; ok {
				prevIn := accounting.Reading{Value: prev.In, SysUptime: prev.SysUptime, Discontinuity: prev.Discontinuity, At: prev.PolledAt}
				prevOut := prevIn
				prevOut.Value = prev.Out
				resIn = accounting.Delta(&prevIn, accounting.Reading{Value: i.HCIn, SysUptime: snap.SysUptime,
					Discontinuity: i.Discontinuity, At: snap.At}, i.SpeedMbps)
				resOut = accounting.Delta(&prevOut, accounting.Reading{Value: i.HCOut, SysUptime: snap.SysUptime,
					Discontinuity: i.Discontinuity, At: snap.At}, i.SpeedMbps)
			} else {
				resIn = accounting.Delta(nil, accounting.Reading{}, 0)
				resOut = resIn
			}
			p.logUnusual(port, "in", resIn)
			p.logUnusual(port, "out", resOut)
			if err := tx.SaveCounterState(ctx, cur); err != nil {
				return err
			}

			down, up := accounting.ClientDirection(resIn.Delta, resOut.Delta)
			if el := resIn.Elapsed.Seconds(); el > 0 && resIn.Kind != accounting.KindReset {
				live[port.ID] = Rate{DownBps: float64(down) * 8 / el, UpBps: float64(up) * 8 / el, Role: port.Role, At: snap.At}
			}
			if event == nil || (down == 0 && up == 0) {
				continue
			}
			if err := p.book(ctx, tx, event.ID, port, macs[i.IfIndex], down, up, snap.At); err != nil {
				return err
			}
			booked += down + up
		}
		return nil
	})
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.live = live
	p.mu.Unlock()
	p.log.Debug("counters polled", "interfaces", len(snap.Interfaces), "booked_bytes", booked)
	return nil
}

// book credits one port delta: always on the port, and on the attribution target.
func (p *poller) book(ctx context.Context, tx *store.Tx, eventID int64, port *store.Port, macs []macaddr.MAC,
	down, up uint64, at time.Time) error {
	if err := tx.AddPortTraffic(ctx, eventID, port.ID, down, up, at); err != nil {
		return err
	}
	target := accounting.Attribute(port.Role, macs)
	switch target.Kind {
	case accounting.TargetMAC:
		return tx.AddSubjectTraffic(ctx, eventID, store.SubjectMAC, target.MAC.String(), port.ID, down, up, at)
	case accounting.TargetPortGroup:
		gid, err := tx.EnsurePortGroup(ctx, eventID, port.ID)
		if err != nil {
			return err
		}
		for _, m := range target.Members {
			if err := tx.TouchPortGroupMAC(ctx, gid, m.String(), at); err != nil {
				return err
			}
		}
		return tx.AddSubjectTraffic(ctx, eventID, store.SubjectPortGroup, strconv.FormatInt(gid, 10), port.ID, down, up, at)
	case accounting.TargetUnassigned:
		return tx.AddSubjectTraffic(ctx, eventID, store.SubjectUnassigned, strconv.FormatInt(port.ID, 10), port.ID, down, up, at)
	}
	return nil
}

func (p *poller) logUnusual(port *store.Port, dir string, r accounting.Result) {
	switch r.Kind {
	case accounting.KindImplausible:
		p.log.Warn("implausible delta dropped", "port", port.ShortName(), "direction", dir, "octets", r.Raw,
			"elapsed", r.Elapsed, "speed_mbps", port.SpeedMbps)
	case accounting.KindReset:
		p.log.Info("counter reset detected", "port", port.ShortName(), "direction", dir, "credited", r.Delta)
	case accounting.KindWrap:
		p.log.Info("counter wrap", "port", port.ShortName(), "direction", dir, "credited", r.Delta)
	}
}

func (p *poller) refreshSystem(ctx context.Context, g *gosnmp.GoSNMP) {
	res, err := g.Get([]string{"1.3.6.1.2.1.1.1.0", "1.3.6.1.2.1.1.5.0"})
	if err != nil || len(res.Variables) != 2 {
		return
	}
	descr, name := snmp.String(res.Variables[0]), snmp.String(res.Variables[1])
	if err := p.m.db.SetSwitchSystem(ctx, p.sw.ID, descr, name); err == nil {
		p.sw.SysDescr, p.sw.SysName = descr, name
	}
}

type session struct {
	g      *gosnmp.GoSNMP
	target snmp.Target
}

func (s *session) Client() *gosnmp.GoSNMP { return s.g }

func (s *session) VLAN(ctx context.Context, vlan int) (*gosnmp.GoSNMP, error) {
	return snmp.Dial(ctx, s.target, vlan)
}

func (p *poller) pollFDB(ctx context.Context) error {
	src, err := fdb.ForAdapter(p.sw.FDBAdapter)
	if err != nil {
		return err
	}
	g, err := snmp.Dial(ctx, p.target, 0)
	if err != nil {
		return err
	}
	defer snmp.Close(g)
	entries, err := src.Read(ctx, &session{g: g, target: p.target})
	if err != nil {
		return err
	}
	ports, err := p.m.db.ListPorts(ctx, p.sw.ID)
	if err != nil {
		return err
	}
	byIf := map[int]*store.Port{}
	names := fdb.IfNames{}
	for _, port := range ports {
		byIf[port.IfIndex] = port
		names[port.IfIndex] = []string{port.IfName, port.IfDescr}
	}
	lldp, err := fdb.LLDPNeighbors(g, names)
	if err != nil {
		p.log.Debug("lldp not available", "err", err)
	}

	macs := map[int][]macaddr.MAC{}
	var sightings []store.Sighting
	unknown, unknownIf := 0, 0
	for _, e := range entries {
		port := byIf[e.IfIndex]
		if port == nil {
			unknown++
			unknownIf = e.IfIndex
			continue
		}
		macs[e.IfIndex] = append(macs[e.IfIndex], e.MAC)
		sightings = append(sightings, store.Sighting{MAC: e.MAC.String(), PortID: port.ID, VLAN: e.VLAN,
			Uplink: port.Role == accounting.RoleUplink})
	}
	// Warn only when the number changes, not on every read.
	if unknown != p.lastUnknown {
		if unknown > 0 {
			p.log.Warn("fdb entries on unknown interfaces ignored; try another FDB adapter",
				"entries", unknown, "of", len(entries), "example_if_index", unknownIf, "adapter", src.Name())
		}
		p.lastUnknown = unknown
	}
	neighbors := map[int64]string{}
	for ifIndex, name := range lldp {
		if port := byIf[ifIndex]; port != nil {
			neighbors[port.ID] = name
		}
	}
	err = p.m.db.InTx(ctx, func(tx *store.Tx) error {
		return tx.RecordSightings(ctx, p.sw.ID, sightings, neighbors, p.m.cfg.Polling.UplinkMACThreshold, store.Now())
	})
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.macs = macs
	if len(entries) == 0 {
		p.emptyFDB++
	} else {
		p.emptyFDB = 0
	}
	warnNoFDB := p.emptyFDB == noFDBAfter
	p.mu.Unlock()
	p.log.Debug("fdb polled", "entries", len(entries), "lldp_neighbors", len(neighbors))
	if warnNoFDB {
		p.log.Warn("the switch reports no forwarding table (MAC to port); traffic on its ports is counted but not attributed to clients",
			"adapter", src.Name())
	}

	if p.sw.ARPSource && p.m.resolver != nil && p.m.resolver.Enabled("snmp-arp") {
		pairs, err := resolver.ReadSNMPARP(g)
		if err != nil {
			p.log.Warn("arp read failed", "err", err)
		} else {
			p.m.resolver.ObserveARP(ctx, pairs, "snmp-arp")
		}
	}
	return nil
}

func (p *poller) rates() map[int64]Rate {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[int64]Rate, len(p.live))
	for k, v := range p.live {
		out[k] = v
	}
	return out
}

func sortedInterfaces(s *collector.Snapshot) []*collector.Interface {
	out := make([]*collector.Interface, 0, len(s.Interfaces))
	for _, i := range s.Interfaces {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].IfIndex < out[b].IfIndex })
	return out
}
