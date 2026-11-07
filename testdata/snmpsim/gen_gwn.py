#!/usr/bin/env python3
"""Generates gwn7721.snmprec: a simulated Grandstream GWN7721 for integration tests.

Run: python3 testdata/snmpsim/gen_gwn.py > testdata/snmpsim/gwn7721.snmprec

Modelled on what a real GWN7721 (firmware 1.0.7) answers: sysObjectID is
Grandstream's enterprise OID, ifName is "2.5GE1" ... "SFP+10", IF-MIB has
64-bit counters, BRIDGE-MIB has only the port map, and there is no forwarding
table at all (neither BRIDGE-MIB nor Q-BRIDGE-MIB) and no LLDP-MIB. All values
are made up; the community of the agent is its file name (gwn7721).
"""

# ifIndex: (ifName, operUp, speed Mbit/s, in rate, out rate)
PORTS = {i: (f"2.5GE{i}", i in (1, 2), 2500 if i == 1 else 1000, 20_000 * i, 200_000 * i) for i in range(1, 9)}
PORTS[9] = ("SFP+9", True, 10000, 500_000, 500_000)
PORTS[10] = ("SFP+10", False, 0, 0, 0)

rows = []


def add(oid, tag, value):
    rows.append((tuple(int(x) for x in oid.split(".")), f"{oid}|{tag}|{value}"))


add("1.3.6.1.2.1.1.1.0", "4", "GWN7721")
add("1.3.6.1.2.1.1.2.0", "6", "1.3.6.1.4.1.42397")
add("1.3.6.1.2.1.1.3.0", "67:numeric", "rate=100,initial=100000")
add("1.3.6.1.2.1.1.5.0", "4", "gwn-test")

for idx, (name, up, speed, rin, rout) in PORTS.items():
    add(f"1.3.6.1.2.1.2.2.1.2.{idx}", "4", name)
    add(f"1.3.6.1.2.1.2.2.1.3.{idx}", "2", 6)
    add(f"1.3.6.1.2.1.2.2.1.8.{idx}", "2", 1 if up else 2)
    add(f"1.3.6.1.2.1.31.1.1.1.1.{idx}", "4", name)
    add(f"1.3.6.1.2.1.31.1.1.1.6.{idx}", "70:numeric", f"rate={rin},initial=1000000")
    add(f"1.3.6.1.2.1.31.1.1.1.10.{idx}", "70:numeric", f"rate={rout},initial=1000000")
    add(f"1.3.6.1.2.1.31.1.1.1.15.{idx}", "66", speed if up else 0)
    add(f"1.3.6.1.2.1.31.1.1.1.18.{idx}", "4", "")
    add(f"1.3.6.1.2.1.17.1.4.1.2.{idx}", "2", idx)
    add(f"1.3.6.1.2.1.17.7.1.4.5.1.1.{idx}", "66", 1)  # dot1qPvid: the only Q-BRIDGE table

# Grandstream system scalars (model, firmware)
add("1.3.6.1.4.1.42397.1.3.1.1.0", "4", "GWN7721")
add("1.3.6.1.4.1.42397.1.3.1.4.0", "4", "1.0.7.1")

for _, line in sorted(rows):
    print(line)
