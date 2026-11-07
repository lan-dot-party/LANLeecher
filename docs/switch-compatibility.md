# Switch compatibility

LANLeecher works with any managed switch that offers, via SNMP v2c or v3:

| Needed | MIB objects | Used for |
| --- | --- | --- |
| 64-bit interface counters | `ifHCInOctets`, `ifHCOutOctets` (IF-MIB) | traffic |
| interface names and status | `ifName`, `ifAlias`, `ifDescr`, `ifType`, `ifOperStatus`, `ifHighSpeed` | port list, plausibility |
| uptime | `sysUpTime` | reboot detection |
| forwarding table | `dot1qTpFdbPort` (Q-BRIDGE-MIB) or `dot1dTpFdbPort` (BRIDGE-MIB) | MAC → port |

Optional, used when present:

| MIB objects | Used for |
| --- | --- |
| `ifCounterDiscontinuityTime` | counter reset detection without reboot |
| `dot1dBasePortIfIndex` | bridge port → ifIndex (otherwise the bridge port is taken as ifIndex) |
| `dot1qTpFdbStatus` / `dot1dTpFdbStatus` | ignore the switch's own and management MACs |
| `lldpRemSysName`, `lldpLocPortId` (LLDP-MIB) | "probably uplink" hint |
| `ipNetToPhysicalPhysAddress`, `ipNetToMediaPhysAddress` | IP addresses (switch marked as ARP source) |

SNMP v1 is not supported: it has no 64-bit counters, and a 32-bit counter wraps after about
34 seconds at 1 Gbit/s.

Use **Test connection** in the switch form: it lists every interface with status, speed and whether
64-bit counters are available.

A switch without a forwarding table still works for port traffic: LANLeecher counts every port, but
cannot attribute the traffic to clients, so clients behind that switch do not appear in the
rankings. After two successful but empty FDB reads in a row (one is normal right after a reboot) the
switch list marks the switch with **no MAC table** and the log says so once.

## FDB adapters

| Adapter | Reads |
| --- | --- |
| `auto` (default) | Q-BRIDGE-MIB; falls back to BRIDGE-MIB when Q-BRIDGE is empty or reports bridge ports that `dot1dBasePortIfIndex` does not list |
| `qbridge` | `dot1qTpFdbPort` only |
| `bridge` | `dot1dTpFdbPort` only |
| `cisco-vlan` | BRIDGE-MIB once per VLAN via community `<community>@<vlan>` or v3 context `vlan-<id>` (Cisco IOS) |

## Tested models

| Vendor / model | Firmware | SNMP | Result | Notes |
| --- | --- | --- | --- | --- |
| TP-Link Omada 10-port Gigabit (8× PoE+) | – | v2c | works | see below |
| TP-Link Omada 8-port 2.5G + 2× 10G SFP+ (8× PoE+) | – | v2c | works | see below |
| TP-Link JetStream T1500G-8T | – | v2c | works | see below |
| Grandstream GWN7721 (8× 2.5G + 2× 10G SFP+) | 1.0.7.128 | v2c | partly: port traffic only | no forwarding table, see below |

**TP-Link Omada / JetStream:**

- `ifType` is 6 (ethernetCsmacd) for every interface, including `Vlan-interface…` and
  `port-channel …`. LANLeecher therefore also classifies interfaces by name: VLAN interfaces are
  virtual, port channels are LAGs; both start as uplinks.
- BRIDGE-MIB is indexed with a VLAN prefix (`<vlan>.<mac>`), like Q-BRIDGE-MIB. LANLeecher always
  reads the MAC from the last six index parts.
- No `ifStackTable` and no IEEE8023-LAG-MIB: members of a port channel are not reported. They have
  LLDP neighbors and are marked "probably uplink" – set them to uplink.
- LLDP local port IDs use subtype interfaceName and match `ifName`.
- JetStream (T1500G-8T): `dot1qTpFdbPort` reports port numbers (1–8), while `dot1dBasePortIfIndex`
  and `dot1dTpFdbPort` use ifIndex values (49153…). The Q-BRIDGE ports cannot be mapped, so `auto`
  uses BRIDGE-MIB. If FDB entries cannot be assigned to a known interface, the log shows
  "fdb entries on unknown interfaces ignored" – then try the `bridge` adapter.

**Grandstream GWN7721 (firmware 1.0.7.128):**

- The SNMP agent has **no forwarding table**: neither `dot1dTpFdbTable` (BRIDGE-MIB) nor
  `dot1qTpFdbTable` (Q-BRIDGE-MIB), not even for a single known MAC. Grandstream's own subtree
  (`1.3.6.1.4.1.42397.1.3.1`) only holds model, MAC, firmware and IP address; the published
  GRANDSTREAM-GWN-MIB with a client table belongs to Grandstream's wireless access points, not to
  the switches. LANLeecher therefore counts traffic per port, but cannot attribute it to clients
  (the switch is marked **no MAC table**). The MAC table exists only in the switch's web interface.
- Present: system group (`sysObjectID` is the bare enterprise OID `1.3.6.1.4.1.42397`), IF-MIB with
  64-bit counters (`ifIndex` = port number, `ifName` "2.5GE1" … "SFP+10", `ifType` 6),
  `dot1dBasePortIfIndex` and `dot1qPvid`. Missing: `ifCounterDiscontinuityTime`, LLDP-MIB,
  IEEE8023-LAG-MIB, the ARP table of other hosts.
- The agent does not answer requests with more than four variable bindings, or with several
  objects it does not implement. LANLeecher asks for at most three objects per request and walks
  tables column by column, so this does not matter here; keep it in mind for other tools.
- Do not run port scans against the switch: a TCP connection to its HTTPS port that the client
  resets (as `nmap` does) reboots the switch.
- Workaround for client rankings: when the GWN hangs off a port of a switch that has a forwarding
  table, that switch sees the GWN's clients on this port. Leave the port as an access port (not an
  uplink); LANLeecher then attributes its traffic to the clients behind it as a port group, like a
  small unmanaged switch at a table. Adding the GWN itself in addition only gives its port-level
  traffic.

Please report your switch: open an issue with vendor, model, firmware, SNMP version and the output of
**Test connection** (remove anything private), or a pull request for this table.
