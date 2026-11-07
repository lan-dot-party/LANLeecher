package scheduler

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/lan-dot-party/lanleecher/internal/accounting"
	"github.com/lan-dot-party/lanleecher/internal/config"
	"github.com/lan-dot-party/lanleecher/internal/secret"
	"github.com/lan-dot-party/lanleecher/internal/snmp"
	"github.com/lan-dot-party/lanleecher/internal/store"
)

// snmpsimBinary finds snmpsim-command-responder: $LANLEECHER_SNMPSIM, the
// repo's .venv, or PATH.
func snmpsimBinary() string {
	if p := os.Getenv("LANLEECHER_SNMPSIM"); p != "" {
		return p
	}
	if p, err := filepath.Abs("../../.venv/bin/snmpsim-command-responder"); err == nil {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, _ := exec.LookPath("snmpsim-command-responder")
	return p
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// startSimulator runs snmpsim with testdata/snmpsim and waits until it answers.
func startSimulator(t *testing.T) snmp.Target {
	bin := snmpsimBinary()
	if bin == "" {
		t.Skip("snmpsim-command-responder not found (pip install snmpsim pysmi, or set LANLEECHER_SNMPSIM)")
	}
	port := freeUDPPort(t)
	cmd := exec.Command(bin, "--data-dir=../../testdata/snmpsim", "--agent-udpv4-endpoint=127.0.0.1:"+strconv.Itoa(port))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	target := snmp.Target{Host: "127.0.0.1", Port: port, Version: "v2c", Creds: snmp.Credentials{Community: "testswitch"},
		Timeout: time.Second, Retries: 0}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		g, err := snmp.Dial(context.Background(), target, 0)
		if err == nil {
			_, err = g.Get([]string{"1.3.6.1.2.1.1.5.0"})
			snmp.Close(g)
			if err == nil {
				return target
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("snmpsim did not start")
	return target
}

func TestPollAndBookAgainstSimulator(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	target := startSimulator(t)
	ctx := context.Background()

	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, log); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	key, _ := secret.New(raw)
	plain, _ := target.Creds.Marshal()
	enc, _ := key.Encrypt(plain)
	sw := &store.Switch{Name: "testswitch", Host: target.Host, Port: target.Port, SNMPVersion: "v2c", CredentialsEnc: enc,
		FDBAdapter: "auto", Enabled: true}
	if err := db.CreateSwitch(ctx, sw); err != nil {
		t.Fatal(err)
	}
	ev, err := db.StartEvent(ctx, "Integration test", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Polling.Timeout = time.Second
	m := New(db, key, cfg, nil, log)
	tgt, err := m.Target(sw)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoller(m, sw, tgt)

	p.cycle(ctx) // first reading + FDB
	time.Sleep(3 * time.Second)
	p.cycle(ctx) // deltas are booked now

	ports, err := db.ListPorts(ctx, sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*store.Port{}
	for _, port := range ports {
		byName[port.IfName] = port
	}
	if r := byName["port-channel 1"].Role; r != accounting.RoleUplink {
		t.Errorf("port-channel role = %s, want uplink", r)
	}
	if byName["Vlan-interface100"].Physical {
		t.Error("Vlan-interface100 classified as physical")
	}
	if !byName["port-channel 1"].UplinkSuspect && byName["port-channel 1"].MACCount != 12 {
		t.Errorf("port-channel mac_count = %d, want 12", byName["port-channel 1"].MACCount)
	}

	rows, err := db.Rankings(ctx, store.RankingQuery{EventID: ev.ID, Order: "down", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("ranking has %d entries, want 2 (one MAC, one port group): %+v", len(rows), rows)
	}
	first, second := rows[0], rows[1]
	if first.Type != "mac" || len(first.MACs) != 1 || first.MACs[0] != "02:00:00:00:00:01" {
		t.Errorf("first entry = %+v, want MAC 02:00:00:00:00:01", first)
	}
	if second.Type != "port_group" || len(second.MACs) != 2 {
		t.Errorf("second entry = %+v, want port group with 2 MACs", second)
	}
	// ifHCOutOctets grows 10x faster: the client must have downloaded more.
	if first.BytesDown < 5*first.BytesUp || first.BytesDown == 0 {
		t.Errorf("direction mapping looks wrong: down=%d up=%d", first.BytesDown, first.BytesUp)
	}
	// ~1 MB/s for ~3 s (generous bounds for timing jitter).
	if first.BytesDown < 1_000_000 || first.BytesDown > 10_000_000 {
		t.Errorf("first entry bytes_down = %d, want roughly 3e6", first.BytesDown)
	}

	// The uplink is counted on the port but never in a ranking.
	traffic, err := db.PortTrafficByEvent(ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	var uplinkTraffic int64
	for _, row := range traffic {
		if row.Port.IfName == "port-channel 1" {
			uplinkTraffic = row.BytesDown + row.BytesUp
		}
	}
	if uplinkTraffic == 0 {
		t.Error("uplink port has no port-level traffic")
	}
	sum, err := db.EventSummary(ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Clients != 3 {
		t.Errorf("clients = %d, want 3", sum.Clients)
	}
	if sum.BytesDown >= first.BytesDown+second.BytesDown+uplinkTraffic {
		t.Error("event summary includes uplink traffic")
	}
}

// TestSwitchWithoutFDB polls a simulated Grandstream GWN7721, which has 64-bit
// counters but no forwarding table: port traffic is counted, nothing is
// attributed to clients, and after two empty FDB reads the poller reports
// that the switch has no forwarding table.
func TestSwitchWithoutFDB(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	target := startSimulator(t)
	target.Creds.Community = "gwn7721"
	ctx := context.Background()

	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, log); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	key, _ := secret.New(raw)
	plain, _ := target.Creds.Marshal()
	enc, _ := key.Encrypt(plain)
	sw := &store.Switch{Name: "gwn", Host: target.Host, Port: target.Port, SNMPVersion: "v2c", CredentialsEnc: enc,
		FDBAdapter: "auto", Enabled: true}
	if err := db.CreateSwitch(ctx, sw); err != nil {
		t.Fatal(err)
	}
	ev, err := db.StartEvent(ctx, "GWN test", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Polling.Timeout = time.Second
	m := New(db, key, cfg, nil, log)
	tgt, err := m.Target(sw)
	if err != nil {
		t.Fatal(err)
	}
	p := newPoller(m, sw, tgt)

	p.cycle(ctx) // first reading + FDB (empty)
	if p.noFDB() {
		t.Fatal("one empty FDB read must not count as \"no forwarding table\" (normal after a reboot)")
	}
	time.Sleep(3 * time.Second)
	p.cycle(ctx) // deltas are booked now
	if err := p.pollFDB(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.noFDB() {
		t.Error("two empty FDB reads: noFDB is false")
	}

	ports, err := db.ListPorts(ctx, sw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 10 {
		t.Fatalf("ports: %d, want 10", len(ports))
	}
	for _, port := range ports {
		if !port.Physical {
			t.Errorf("%s classified as not physical", port.IfName)
		}
	}
	rows, err := db.Rankings(ctx, store.RankingQuery{EventID: ev.ID, Order: "down", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("ranking has %d entries, want none (no forwarding table)", len(rows))
	}
	traffic, err := db.PortTrafficByEvent(ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, row := range traffic {
		total += row.BytesDown + row.BytesUp
	}
	if total == 0 {
		t.Error("no port-level traffic booked")
	}
}
