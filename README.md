# LANLeecher

**Who's pulling the most?** At LAN parties, LANLeecher counts how much every guest downloads and
uploads and turns it into rankings – in the browser, on the beamer and via API. The data comes
straight from the switches via SNMP; nothing has to be installed on the guests' computers.

> **Status: pre-release.** Polling via SNMP v2c/v3, accounting per MAC and event, uplink detection,
> IP and hostname discovery (Kea DHCP, ARP, NetBIOS, mDNS, LLMNR, DNS), web UI in English and German,
> beamer view, REST API, packages and Docker image. Tested with TP-Link Omada and JetStream switches.
> Next up: the first real LAN party. Progress is tracked in [docs/ROADMAP.md](docs/ROADMAP.md).

![The dashboard during an event: totals, live throughput, top downloaders and uploaders](docs/images/dashboard.png)

![The same dashboard in the dark theme](docs/images/dashboard-dark.png)

![The beamer view at /board, updated live after every poll](docs/images/board.png)

## What LANLeecher does

- **Rankings per event:** top downloaders and uploaders, totals, live throughput, timeline;
  top 10/20/50, per switch or time range, and across all LAN parties.
- **History per device:** download and upload of every machine at every event, with its IP and
  hostname history and the ports it was seen on.
- **Beamer view** `/board` with live updates, and the same data via API for Discord bots and
  other tools.
- **Switch and port management** with a connection test, port roles (client, multi-client, server,
  uplink), labels, and hints for ports that look like uplinks (LLDP neighbor, many MACs).
- **Hostnames from Kea DHCP** (Kea 2.x and 3.x, read-only, certificate pinning) and from the
  network itself (NetBIOS, mDNS, LLMNR, DNS), with history per device; vendor names from the IEEE
  registry for MAC addresses.
- **Hiding** of game servers, caches and uplinks from rankings – they are still counted, and hiding
  applies retroactively.
- **Events** to start and stop, compare and export as CSV or JSON – or plan them with a date range
  so they start and stop by themselves.
- **Accounts** with three roles (viewer, operator, admin), API tokens with scopes, and an audit log.
- **Light and dark theme**, following the system or chosen per user; English and German.
- **Packages** for Debian/Ubuntu, Fedora/RHEL, Arch Linux and Alpine, and a multi-arch Docker image.

![A switch with its ports: roles, labels, learned MACs, traffic in the event and live rates](docs/images/ports-dark.png)

## Design principles

- **Counters from the switches, nothing on the clients.** LANLeecher reads 64-bit interface
  counters and the forwarding table via SNMP. The MAC address is the key; IP and hostname are
  extra information with history.
- **Only deltas are stored.** A total is the sum of the deltas between two polls, so a switch reboot,
  a counter reset or a LANLeecher restart never reduces it ([ADR 0002](docs/adr/0002-store-deltas-only.md)).
- **Every port is counted, hiding happens at query time.** Uplinks are counted on the port but never
  credited to a guest; servers and caches can be hidden without losing data.
- **A single binary.** Go, built-in SQLite, everything embedded – runs on a Raspberry Pi without
  internet access.
- **Secure by default.** Everything requires a login, SNMP credentials are encrypted, names from the
  network are treated as untrusted. See [SECURITY.md](SECURITY.md).

## Switch support

| Switch | SNMP | State |
| --- | --- | --- |
| Any switch with IF-MIB 64-bit counters and BRIDGE-MIB or Q-BRIDGE-MIB | v2c, v3 | **supported** |
| TP-Link Omada (10-port PoE+, 8× 2.5G + 2× 10G), JetStream T1500G-8T | v2c | **tested** ([notes](docs/switch-compatibility.md#tested-models)) |
| Cisco IOS (FDB per VLAN via `community@vlan`) | v2c, v3 | adapter available, untested |
| Switches with SNMP v1 only | v1 | not supported ([why](docs/adr/0003-no-snmp-v1.md)) |

## Quick start

Download the package for your system from the [releases](https://github.com/lan-dot-party/LANLeecher/releases):

```bash
sudo apt install ./lanleecher_<version>_amd64.deb
sudo nano /etc/lanleecher/config.yaml           # listen, public_url
sudo lanleecher user create admin --role admin
sudo systemctl enable --now lanleecher
```

Then log in, add your switches under **Switches**, mark the uplinks and start the event. The
[LAN party guide](docs/lan-party-guide.md) walks through it. Docker: see
[docs/installation.md](docs/installation.md#docker).

## Documentation

- [docs/lan-party-guide.md](docs/lan-party-guide.md) – checklist for organizers: before, during and after the event
- [docs/installation.md](docs/installation.md) – packages, Docker, command line, backup, upgrade
- [docs/configuration.md](docs/configuration.md) – every setting (generated from the source), with a [JSON Schema](docs/config.schema.json)
- [docs/reverse-proxy.md](docs/reverse-proxy.md) – nginx, Caddy, Traefik, or none
- [docs/switch-compatibility.md](docs/switch-compatibility.md) – required MIBs, FDB adapters, tested models
- [docs/kea.md](docs/kea.md) – Kea DHCP 2.x and 3.x: configuration, certificates, troubleshooting
- [docs/concepts.md](docs/concepts.md) – events, devices, port groups, roles, hiding, directions
- [docs/api.md](docs/api.md) – authentication, scopes, endpoints, errors, metrics
- [examples/](examples/) – Discord bot, Grafana, Go and shell clients
- [docs/architecture.md](docs/architecture.md) – how LANLeecher is built
- [docs/security.md](docs/security.md) – threat model and residual risks
- [docs/adapters.md](docs/adapters.md) – supporting a new switch or name source
- [docs/adr/](docs/adr/) – architecture decision records
- [docs/ROADMAP.md](docs/ROADMAP.md) – status and next steps
- [llms.txt](llms.txt) and [docs/llms-full.txt](docs/llms-full.txt) – the documentation for LLMs
- [CONTRIBUTING.md](CONTRIBUTING.md) – development setup and contribution rules
- [SECURITY.md](SECURITY.md) – reporting vulnerabilities

## Development quick start

Requires Go 1.24 or newer; Python 3 only for the SNMP simulator used in tests.

```bash
git clone https://github.com/lan-dot-party/LANLeecher.git
cd LANLeecher
python3 -m venv .venv && .venv/bin/pip install snmpsim pysmi
go test ./...
```

[CONTRIBUTING.md](CONTRIBUTING.md) shows how to run LANLeecher against a simulated 24-port switch.

## Related projects

- [LANPilot](https://github.com/lan-dot-party/LANPilot) – monitor and control game servers at LAN parties.
- [FlowGauge](https://github.com/lan-dot-party/flowgauge) – bandwidth monitoring with multi-WAN support.
- [Discord-Gameserver-Notifier](https://github.com/lan-dot-party/Discord-Gameserver-Notifier) –
  discovers game servers on the LAN and announces them on Discord.

## License

[MIT](LICENSE). MAC vendor names come from the public IEEE Registration Authority listings
(MA-L, MA-M, MA-S), refreshed with `tools/genoui`.
