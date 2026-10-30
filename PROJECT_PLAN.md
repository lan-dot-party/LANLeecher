# LANLeecher – Project Plan

Oct 22, 2026 · @Stephan

## Goal & Overview

For each event, LANLeecher counts how much every client in the LAN downloads and uploads and turns that into rankings – in a web UI and through an API. The data comes from the switches via SNMP; the MAC address is the key, IP and hostname are additional information with history.

- Repository: `github.com/lan-dot-party/LANLeecher`, MIT license, releases with `.deb`, `.rpm` and Arch packages – like [FlowGauge](https://lan.party/flowgauge.php) and [LANPilot](https://lan.party/lanpilot.php).
- Core principle: only deltas between two polls are stored, never raw counters. A counter reset on a switch therefore never resets a MAC's total.
- Every port is counted, including server and uplink ports. Whether a port (or a device) appears in the rankings (top 10/20/50) is a separate switch; servers and uplinks are hidden by default. Ports with several clients appear as one shared entry.
- All data is kept across events and can be searched by event name – download and upload alike, also historically.

**Mind the direction:** the counters are seen from the switch port. `ifHCInOctets` (from the client into the switch) is therefore the client's **upload**, `ifHCOutOctets` its **download**. This mapping lives in exactly one place in the code and is tested.

## Tech Stack & Architecture

Decided: Go, like FlowGauge – a single static binary, trivial packaging, and with `gosnmp` a mature library for SNMP v2c/v3.

| Area | Choice |
| --- | --- |
| Language | Go 1.24+ |
| SNMP | `github.com/gosnmp/gosnmp` |
| HTTP / router | `net/http` + `chi` |
| Web UI | Server-rendered templates (`html/template`) + htmx, assets embedded in the binary |
| Storage | SQLite only (`modernc.org/sqlite`, no CGO) – see ADR 0005 |
| Migrations | `goose`, embedded |
| Configuration | `/etc/lanleecher/config.yaml` (like FlowGauge); switches, ports and users live in the database |
| Logging | `log/slog` |
| i18n | German and English, like LANPilot |

### Modules (one Go interface each)

Each module is an interface with interchangeable implementations. Vendor quirks, new resolvers or storage backends can be added without touching the core.

1. **Collector** – reads interface counters and port names via SNMP.
2. **FDB source** – delivers MAC → port assignments (`bridge-mib`, `qbridge-mib`, vendor adapters such as Cisco community@vlan).
3. **Accountant** – computes deltas, detects resets/wraps, assigns traffic to MACs or port groups.
4. **Resolver** – finds IP and hostname for a MAC, also via NetBIOS and mDNS (`snmp-arp`, `local-neigh`, `dhcp-leases`, `dns-ptr`).
5. **Store** – persistence in SQLite.
6. **API / Web** – HTTP layer, auth, templates.
7. **Exporter** (optional) – Prometheus `/metrics`.

Only the accountant writes traffic; collector, FDB source and resolver merely deliver raw data to it or to the store.

### Repository layout

```
cmd/lanleecher/          # CLI: server, config init, migrate, backup, user, event, switch, version
internal/collector/      # interface + snmp
internal/fdb/            # interface + bridge, qbridge, vendor adapters, LLDP
internal/accounting/     # delta logic, reset detection, attribution
internal/resolver/       # arp, neigh, netbios, mdns, llmnr, dns
internal/store/          # SQLite store, migrations/
internal/auth/           # passwords, sessions, API tokens
internal/api/            # REST v1 + OpenAPI
internal/web/            # handlers, templates/, static/
internal/scheduler/      # poll loop per switch
packaging/               # systemd unit, sysusers, install scripts, package config
configs/lanleecher.example.yaml
docs/
```

## SNMP Polling, Counter Wraps & Reset Detection

Each switch is polled in its own goroutine loop (default every 30 s, configurable per switch). A poll walks these OIDs with `BulkWalk`:

| Value | OID | Purpose |
| --- | --- | --- |
| `sysUpTime` | 1.3.6.1.2.1.1.3.0 | Reboot detection |
| `ifName` | 1.3.6.1.2.1.31.1.1.1.1 | Port name, e.g. `Gi1/0/12` |
| `ifAlias` | 1.3.6.1.2.1.31.1.1.1.18 | Port description from the switch config |
| `ifDescr` | 1.3.6.1.2.1.2.2.1.2 | Fallback for the name |
| `ifType` | 1.3.6.1.2.1.2.2.1.3 | Physical port, LAG or virtual interface |
| `ifOperStatus` | 1.3.6.1.2.1.2.2.1.8 | Port up/down |
| `ifHighSpeed` | 1.3.6.1.2.1.31.1.1.1.15 | Plausibility limit for deltas |
| `ifHCInOctets` | 1.3.6.1.2.1.31.1.1.1.6 | Client upload (64 bit) |
| `ifHCOutOctets` | 1.3.6.1.2.1.31.1.1.1.10 | Client download (64 bit) |
| `ifCounterDiscontinuityTime` | 1.3.6.1.2.1.31.1.1.1.19 | Time of the last counter reset |

### SNMP versions

- **v2c** (default) and **v3** are supported. Both deliver the 64-bit HC counters that gigabit ports require.
- **v1 is deliberately not supported:** it has no Counter64, and a 32-bit counter wraps after about 34 s at 1 Gbit/s – not reliable for a LAN party.
- A port without HC counters is marked "unsupported" in the UI and not counted, instead of silently delivering wrong numbers.
- **v3**: user name, security level (`noAuthNoPriv`, `authNoPriv`, `authPriv`), auth protocol (MD5, SHA, SHA-224/256/384/512), privacy protocol (DES, AES-128/192/256), optional context name.
- Community strings and v3 passwords are stored encrypted in the database (AES-GCM, key in `/etc/lanleecher/secret.key`), like LANPilot does with server credentials.
- Each switch has a "Test connection" button in the UI that shows `sysDescr`, `sysName` and the port list.

### Delta computation

For each port and direction, the last raw value is stored together with `sysUpTime` and `ifCounterDiscontinuityTime` (`port_counter_state`). The computation therefore survives a restart of LANLeecher.

1. `sysUpTime` smaller than at the last poll, or `ifCounterDiscontinuityTime` changed → **reset**. The new value is the traffic since the reset, so delta = new value. The plausibility check (step 4) uses the time since the reboot according to sysUpTime instead of the poll interval.
2. New value < old value without a reset signal → **wrap**. Delta = new + 2^64 − old.
3. Otherwise delta = new − old.
4. **Plausibility check:** delta > `ifHighSpeed` × elapsed seconds × 1.1 → drop and log (protects against fantasy values from switch bugs).
5. Port down or poll failed → no delta, state is kept; the next successful poll continues normally.

A MAC's total is exclusively the sum of its deltas. A reset on the switch never changes it.

### Keeping the logic testable

The delta function is a pure function without I/O and is tested table-driven (reset, 64-bit wrap, reboot, fantasy value, gap). For integration tests, `snmpsim` serves simulated switch data: `testdata/snmpsim/testswitch.snmprec`, generated by `testdata/snmpsim/gen.py`. Its counters grow at fixed rates (snmpsim's `numeric` module), so the integration test checks real deltas, the direction mapping, port groups and uplinks. Install snmpsim with `pip install snmpsim pysmi` (some distribution packages of snmpsim 0.4 do not work with current `pyasn1`).

**Experience with real switches (TP-Link Omada/JetStream):**

- `ifType` is 6 (ethernetCsmacd) for every interface, including `Vlan-interface100` and `port-channel 1`. VLAN interfaces and LAGs are therefore also recognized by name; LAGs and virtual interfaces start with the role `uplink`.
- Even the standard BRIDGE-MIB is indexed with a VLAN prefix (`<vlan>.<mac>`). The MAC is always read from the last six index parts.
- `ifStackTable` and IEEE8023-LAG-MIB are missing; LAG members are detected via LLDP (`lldpLocPortId` with subtype interfaceName = `ifName`). Ports with an LLDP neighbor count as "probably uplink".

## MAC Assignment, IP/Hostname & History

SNMP delivers traffic per port, not per MAC. The bridge is the switch's forwarding database (FDB): it says which MAC was learned on which port.

### Reading the FDB

- `dot1dTpFdbPort` (1.3.6.1.2.1.17.4.3.1.2) plus `dot1dBasePortIfIndex` (1.3.6.1.2.1.17.1.4.1.2) to map bridge ports to `ifIndex`.
- `dot1qTpFdbPort` (1.3.6.1.2.1.17.7.1.2.2.1.2) for VLAN-aware switches, MAC per VLAN.
- Vendor adapters for quirks, e.g. Cisco with community `public@<vlan>` per VLAN or v3 context `vlan-<id>`.
- The FDB is read less often than the counters (default every 60 s) and cached.

The switches in use mostly speak the standard MIBs; a dedicated vendor adapter is not planned. The adapter interface stays anyway, in case a model misbehaves later.

### Assigning traffic to a MAC

On every counter poll, the port delta is assigned like this:

- **Port role `client`, exactly one MAC learned** → the delta goes to that MAC.
- **Port role `client`, several MACs** (small switch under the table) or role **`multi-client`** → the delta goes to a **port group** (switch + port). The group lists all MACs seen there and appears as one ranking entry.
- **Role `server`** → assigned like `client`; the port is hidden in rankings by default.
- **Role `uplink`** (trunk to other switches/the router) → counted on the port, but never assigned to a MAC. Otherwise the traffic behind it would be counted twice. Hidden by default.
- **No MAC learned** (e.g. right after link-up) → the delta stays on the port as "unassigned" and is not credited to the next MAC that shows up.

Regardless of the role, every port delta is additionally booked in `port_traffic_totals`. Every port is therefore fully tracked; whether it appears in rankings is controlled only by "Hide in rankings" (see below).

If a guest moves their computer to another port or switch, their total continues under the same MAC.

**Uplink detection:** a port with more than N MACs (default 10) or with an LLDP neighbor is marked "probably uplink" in the UI. An operator confirms it by setting the role `uplink`, or confirms that it is *not* an uplink (e.g. a hypervisor with many VMs), which keeps the hint off. An unmarked uplink is the most common mistake and skews everything – hence prominent in UI and docs.

### Finding IP and hostname

Resolvers run in a configurable order; each delivers (MAC, IP, hostname, source, time):

1. **`snmp-arp`** – ARP/neighbor table of the router or L3 core via SNMP (`ipNetToPhysicalPhysAddress` 1.3.6.1.2.1.4.35.1.4, fallback `ipNetToMediaPhysAddress` 1.3.6.1.2.1.4.22.1.2). Covers IPv4 and IPv6.
2. **`local-neigh`** – the neighbor table of the LANLeecher host, if it is in the same network.
3. **`dhcp-leases`** – optional: Kea DHCP via API (see below); delivers MAC, IP and the hostname the client reported. Adapters for ISC dhcpd and dnsmasq may follow.
4. **`netbios`** – NetBIOS node status query (NBSTAT) via unicast to UDP 137 of the client IP; returns the computer name of Windows machines.
5. **`mdns`** – reverse query (`<ip>.in-addr.arpa` PTR) via mDNS on UDP 5353, directly by unicast to the client IP and additionally by multicast (224.0.0.251 / ff02::fb) in the local segment; returns `<name>.local` from Linux, macOS and newer Windows versions.
6. **`llmnr`** (optional) – fallback for Windows clients without NetBIOS, UDP 5355.
7. **`dns-ptr`** – reverse DNS for the IP found, if available.

**Kea DHCP (optional):** whoever runs Kea enters the API credentials in the web UI under Admin → DHCP sources. Kea before 3.0 is reached through the Control Agent (one URL, a `service` list per command); from Kea 3.0 on, each DHCP server has its own HTTP(S) control socket, so a source has a kind – Control Agent, DHCPv4 server or DHCPv6 server – and both response formats are accepted. Details: [docs/kea.md](docs/kea.md). Kea then becomes the most important source for MAC → IP and hostname. Without Kea, everything continues via the other resolvers.

- **Configuration:** URL of the Kea control interface (Control Agent or HTTP control socket, depending on the Kea version), user name and password, optional client certificate. Several Kea servers are possible, e.g. an HA pair. A "Test connection" button checks reachability, certificate and whether the hook `libdhcp_lease_cmds` responds.
- **Queries:** `lease4-get-page` and `lease6-get-page` (paged, so large lease tables do not end up in one response) every 30 s, configurable. New or changed leases immediately trigger NetBIOS and mDNS queries for the IP.
- **Read-only:** LANLeecher only sends lease queries, never modifying commands. The documentation recommends a dedicated Kea user for LANLeecher.
- **Stork:** untouched. LANLeecher talks to Kea directly; Stork keeps monitoring Kea.
- `snmp-arp` stays as a complement for devices with static IPs (servers, infrastructure) that never use DHCP.

**Transport security for the Kea API:**

- HTTPS only. Plain HTTP is rejected except for `localhost`.
- Certificate verification is always on. Either the certificate of an internal CA or the Kea server's self-signed certificate can be stored (PEM upload or paste); a stored self-signed certificate is then trusted, all others are rejected. Alternatively, pinning by SHA-256 fingerprint. The UI shows issuer and expiry date and warns 14 days before expiry. Turning verification off is only possible with an explicit switch and a warning in UI and log.
- At least TLS 1.2. Optional mTLS with a client certificate if Kea is configured that way.
- Password and private key are stored encrypted in the database (AES-GCM, like the SNMP credentials), are never shown again in the UI and never appear in logs or API responses.

**Accepting a certificate with one click (like a browser):** an admin does not have to upload a PEM but can accept the certificate directly in the connection test.

1. The admin enters the Kea URL and clicks "Test connection".
2. If the certificate is not trusted (self-signed, unknown CA, expired, hostname mismatch), the UI shows a warning page like a browser: reason, issuer, subject and SANs, validity period and SHA-256 fingerprint.
3. "Trust certificate" stores exactly this certificate for this DHCP source. From then on the connection is green; other certificates are still rejected.
4. If the certificate changes later (e.g. after renewal), LANLeecher stops querying this source, shows a warning on the dashboard and offers the old and new fingerprints side by side on the same warning page. Only another click accepts the new certificate.

Only admins may accept certificates; every acceptance is written to the audit log with its fingerprint. An accepted certificate can be viewed and revoked in the source at any time.

NetBIOS and mDNS are mandatory, not optional: guest computers are usually not in the local DNS, and their own computer name is often the only reliable name source.

- **Query rhythm:** immediately for every new MAC or IP, then every 10 min per active IP. Results are cached, queries per second are limited (default 20), timeout 1 s.
- **Network:** NetBIOS and unicast mDNS only need routing to the client IP. Multicast mDNS only works within the same VLAN; if clients are in several VLANs, the host needs an interface there, otherwise only unicast is used.
- **Firewall note for the docs:** the Windows firewall blocks NetBIOS and mDNS in the "Public" profile. Clients must classify the LAN party network as "Private", otherwise only the DHCP hostname remains.
- **Priority on conflicts** (configurable): DHCP hostname, NetBIOS, mDNS, LLMNR, DNS PTR. Every finding is stored with its source in the history; the UI shows the preferred name and, on click, all sources.
- **Implementation:** NetBIOS NBSTAT is a small UDP packet and implemented directly; mDNS and LLMNR use `golang.org/x/net/dns/dnsmessage`. Neither needs root, as they send from an unprivileged source port.

### History

- `mac_ip_history` and `mac_hostname_history` store every pair with `first_seen` and `last_seen`. If nothing changes, only `last_seen` is updated.
- In the UI, every MAC shows its IP and hostname history and the ports where it was seen.

### Reconstructing MAC changes via the hostname

If a new MAC appears with a hostname that another MAC already had in the same event, LANLeecher suggests a merge ("probably the same device"). An admin confirms the merge; afterwards both MACs belong to one **device**. Nothing is ever merged automatically, because hostnames such as `DESKTOP-ABC123` or `PC` are not unique, and randomized MACs (e.g. with Wi-Fi bridges) can mislead.

## Data Model

The model separates master data (switches, ports, devices), running state (last counter values) and results (traffic per event). Timestamps are stored as Unix seconds (UTC), bytes as 64-bit integers; raw 64-bit counters are stored as `int64` with the same bit pattern.

| Table | Important fields | Purpose |
| --- | --- | --- |
| `switches` | id, name, host, port, snmp\_version, credentials\_enc, poll\_interval, fdb\_adapter, arp\_source, enabled | Configured switches |
| `ports` | id, switch\_id, if\_index, if\_name, if\_alias, if\_type, physical, lldp\_neighbor, role, label, hidden\_in\_rankings, mac\_count, uplink\_suspect, uplink\_dismissed, last\_seen | Ports with role (`client`, `multi-client`, `server`, `uplink`), display name (also the name of the port group), ranking visibility, uplink hint and its "not an uplink" confirmation |
| `port_counter_state` | port\_id, in\_raw, out\_raw, counter\_bits, sys\_uptime, discontinuity, polled\_at | Last raw value for delta computation |
| `port_traffic_totals` | event\_id, port\_id, bytes\_down, bytes\_up, updated\_at | Traffic of every port per event, uplinks included |
| `events` | id, name, slug, started\_at, ended\_at, notes | LAN events; at most one active |
| `event_schedules` | id, name, notes, starts\_at, ends\_at (exclusive), event\_id, created\_at | Planned events; `event_id` links the event once started |
| `devices` | id, display\_name, hidden\_in\_rankings, notes | A device, possibly with several MACs after a merge |
| `macs` | mac (PK), device\_id, vendor (OUI), first\_seen, last\_seen | Known MAC addresses |
| `mac_locations` | mac, port\_id, vlan, first\_seen, last\_seen | Where a MAC was learned |
| `mac_ip_history` | mac, ip, source, first\_seen, last\_seen | IP history (v4 and v6) |
| `mac_hostname_history` | mac, hostname, source, first\_seen, last\_seen | Hostname history |
| `port_groups` | id, port\_id, event\_id | Shared entry for ports with several clients |
| `port_group_macs` | group\_id, mac, first\_seen, last\_seen | All MACs seen in a port group |
| `traffic_totals` | event\_id, subject\_type (`mac`, `port_group`, `unassigned`), subject\_id, port\_id, bytes\_down, bytes\_up, updated\_at | Running total per event and port – basis of the rankings |
| `traffic_buckets` | event\_id, subject\_type, subject\_id, port\_id, bucket\_start (5 min), bytes\_down, bytes\_up | Time series for graphs and rankings over time ranges |
| `merge_suggestions` | id, mac\_a, mac\_b, reason, status | Suggestions from the hostname comparison |
| `users` | id, username, password\_hash, role, language, disabled | Web users |
| `sessions` | id, user\_id, expires\_at, csrf\_token | Login sessions |
| `api_tokens` | id, name, token\_hash, scopes, allowed\_ips, created\_by, expires\_at, last\_used | API access |
| `audit_log` | id, user\_id, action, target, at | Who changed what |

Rankings only read `traffic_totals` (indexed on `event_id, bytes_down` and `bytes_up`), so they load instantly even with many clients. The port is part of the `traffic_totals` key: "Hide in rankings" can thus be filtered per port, even if a device changes ports during an event. Visibility is not written into the totals but filtered at query time (join on `ports` and `devices`). Hiding or unhiding therefore takes effect immediately, also retroactively for past events, without recomputing data.

## Events, Ports & Ranking Visibility

Traffic is always booked into an active event. Without an active event, LANLeecher keeps polling (so the counter state stays current) but books no deltas.

### Events

- An admin starts an event with a name (e.g. "Basement LAN #12") and optional notes. A running event is stopped automatically when a new one starts.
- Stopping an event freezes its totals; the data is kept completely.
- Event list with search by name and time range. Per event: rankings, total traffic split into download and upload, traffic timeline, number of participants (MACs).
- **History in both directions:** download and upload are equally important everywhere, despite the project's name. Per device/MAC, a table shows all events with download, upload, total and up/down ratio, plus a chart of both directions across events ("How much did this computer upload at the last LAN, how much did it download?").
- Compare events side by side, each with top uploaders and top downloaders; also rankings across all events ("Who uploaded the most over all LANs?").
- Export per event as CSV and JSON, always with both directions.
- Via CLI: `lanleecher event start "Name"` and `lanleecher event stop`, e.g. for scripts on setup day.
- **Planned events:** an operator enters name, first and last day (inclusive) and optional start/end times in the server's time zone. A planner goroutine wakes at the next start or end (at least every 30 s), starts the event (stopping a running one, as a manual start does) and stops it at the end. Starting early ("Start now") links the event to the plan, so it still stops on time. An event stopped or replaced by hand is left alone. Plans must not overlap. A period that passed entirely while LANLeecher was down is shown as "missed". Start and stop are audited with actor `schedule`. CLI: `event schedule`, `event schedules`, `event unschedule`.

### Managing ports

- After a switch is added, LANLeecher reads all interfaces with `ifName`, `ifAlias`, status and speed and shows them as a table. From then on, every port is counted.
- Per port: choose a role (`client` by default, `multi-client`, `server`, `uplink`), set a display name (e.g. "Game server rack", "Table 7"; it names the port group in rankings and on the board) and the "Hide in rankings" switch.
- A port flagged as "probably uplink" can be confirmed as **not an uplink** (e.g. a hypervisor with many VMs); the hint stays off until the confirmation is withdrawn or the port becomes an uplink.
- Multi-select, e.g. to set all ports 45–48 to uplink or hide them at once.
- Next to each port: currently learned MACs with hostname and the traffic in the active event.
- Virtual interfaces (VLAN SVIs, loopback) are hidden by a filter in the port list; port channels themselves are typically uplinks.

### Hiding from rankings

- All ports are tracked; hiding only affects the rankings (top 10, top 20, top 50) in the web UI, scoreboard and API.
- The switch exists per port and per device. A hidden device stays hidden even if it moves to another port – handy for a LANcache or streaming PC on a normal table port.
- Defaults: `server` and `uplink` hidden, `client` and `multi-client` visible. Every default can be overridden per port.
- Hidden entries remain visible with full traffic in detail views, the port overview and a separate "Hidden" list.
- In the rankings, a logged-in user can temporarily show hidden entries, e.g. to check how much the cache delivered.
- Typical candidates: game servers, caches (LANcache), file servers and uplinks, which would otherwise top every ranking.

## Web UI & User Management

The web UI is entirely behind a login; roles and usage follow LANPilot so that organizer teams feel at home right away.

### Pages

- **Dashboard** – active event, total traffic, top 10 download/upload, live throughput (htmx refresh every 15 s), switch status. A click into the timeline (dashboard and event page) lists the clients behind the traffic of that 5-minute interval; a drag selects a range.
- **Rankings** – top downloaders, top uploaders, total; filter by event, switch, time range; top 10/20/50 selectable; hidden ports and devices can be shown temporarily.
- **Search** – one search field for MAC (any notation), IP or hostname, also partial matches.
- **Device/MAC detail** – download and upload totals per event, timeline of both directions, IP and hostname history, ports, merge suggestions.
- **Events** – create, start, stop, plan with a date range, search, compare, export.
- **Switches & ports** – add a switch, test the connection, set port roles, uplink hints.
- **Admin** – users, API tokens, audit log, settings overview.

### Roles

| Role | May |
| --- | --- |
| `viewer` | view rankings, search, detail pages, events |
| `operator` | additionally start/stop events, set port roles and ranking visibility, confirm merges |
| `admin` | additionally switches and credentials, users, API tokens, settings, audit log |

### Security

- Passwords with Argon2id; first admin via CLI `lanleecher user create admin --role admin` (like LANPilot), no default password.
- Session cookie `HttpOnly`, `SameSite=Lax`, `Secure` if `public_url` is https; CSRF token for all forms.
- Rate limit and a short lockout after repeated failed logins.
- Security headers (CSP without inline scripts, `X-Frame-Options`), all output escaped – hostnames and `ifAlias` come from the network and are untrusted.
- TLS is handled by a reverse proxy (docs with nginx/Caddy examples), optionally built-in TLS with a certificate in the config.
- Audit log for all write actions.

### Optional: public ranking

A read-only beamer/scoreboard mode under `/board` that can be turned off and only shows the top list of the active event (hostname or label, no MACs/IPs). Off by default. The page takes its data exclusively from the board API (see "Beamer view via the API").

## API & Security

The API is a JSON REST API under `/api/v1`, secured with bearer tokens that admins create in the web UI. Tokens are long-lived and restricted by scope – suitable for Discord bots, Grafana or scripts.

### Security

- Header `Authorization: Bearer llk_<random>`; the prefix `llk_` makes tokens recognizable in logs and secret scanners.
- Only a SHA-256 hash is stored in the database; the plaintext is shown exactly once at creation.
- Scopes: `read:rankings` (top lists only, without MACs/IPs), `read:clients` (detail queries incl. MAC/IP/hostname), `write:events` (start/stop events), plus `read:board` for the beamer view only. A token has one or more scopes.
- Optional: expiry date, IP allowlist per token, `last_used` display, immediate revocation.
- Rate limit per token. Logged-in web sessions may use the API as well (for htmx/graphs), then with CSRF protection.
- `/health` stays without auth but only returns `ok`.

### Endpoints

| Method & path | Scope | Description |
| --- | --- | --- |
| `GET /api/v1/rankings/download` | read:rankings | Top downloaders; `?event=<slug>&limit=10&include_hidden=false` |
| `GET /api/v1/rankings/upload` | read:rankings | Top uploaders, same parameters |
| `GET /api/v1/rankings/total` | read:rankings | Sum of both directions |
| `GET /api/v1/events` | read:rankings | Events, search via `?q=` |
| `GET /api/v1/events/{slug}` | read:rankings | Event details and totals |
| `GET /api/v1/clients/mac/{mac}` | read:clients | Download and upload totals per event (`?events=5` for the last N LANs), IP/hostname history, ports |
| `GET /api/v1/clients/ip/{ip}` | read:clients | MAC(s) that had this IP, with time range |
| `GET /api/v1/clients/hostname/{name}` | read:clients | MAC(s) with this hostname |
| `GET /api/v1/clients/search?q=` | read:clients | Free search over MAC, IP, hostname |
| `GET /api/v1/clients/mac/{mac}/timeseries` | read:clients | 5-minute buckets for graphs |
| `POST /api/v1/events` / `POST /api/v1/events/{slug}/stop` | write:events | Start/stop an event |
| `GET /api/v1/schedules` | read:rankings | Planned events |
| `POST /api/v1/schedules` / `DELETE /api/v1/schedules/{id}` | write:events | Plan / delete a planned event |
| `GET /api/v1/metrics` | read:rankings | Prometheus format (can be turned off) |
| `GET /api/` | – | OpenAPI specification + interactive docs |

`limit` accepts any value up to 500; web UI and scoreboard offer 10, 20 and 50 as shortcuts. Hidden ports and devices are excluded by default; `include_hidden=true` adds them and marks them with `hidden: true`. Each entry contains `rank`, `type` (`mac` or `port_group`), display name, `bytes_down`, `bytes_up` and – only with `read:clients` – MAC, IP and hostname. MAC addresses are accepted in any notation and returned normalized (`aa:bb:cc:dd:ee:ff`). Additionally, `GET /api/v1/ports?event=<slug>` (scope `read:clients`) returns the traffic of all ports, hidden ones included.

### Beamer view via the API

The beamer view `/board` has no logic of its own: it only renders the response of `GET /api/v1/board`. Everything the beamer shows is therefore available identically to any other tool – a Discord bot or a second dashboard.

| Method & path | Scope | Description |
| --- | --- | --- |
| `GET /api/v1/board` | read:board | Complete beamer content as JSON; `?limit=10\|20\|50&event=<slug>` |
| `GET /api/v1/board/stream` | read:board | The same data as Server-Sent Events, a new event after each poll |

Response content: event name and start time, generation time, total traffic (download, upload, number of clients), current throughput in bit/s, top downloaders and top uploaders with rank, display name, `bytes_down` and `bytes_up` and the change since the previous poll (for up/down arrows). As on the beamer, hidden entries, MACs and IPs are missing; devices without a hostname appear as "Client #&lt;id&gt;". The dedicated scope `read:board` allows handing out a token for bots and displays only, without access to client details.

```json
{
  "event": {"name": "Basement LAN #12", "slug": "basement-lan-12", "started_at": "2026-11-13T16:00:00Z"},
  "generated_at": "2026-11-14T21:30:05Z",
  "totals": {"bytes_down": 4823110092288, "bytes_up": 912334102528, "clients": 84},
  "throughput": {"down_bps": 2140000000, "up_bps": 380000000},
  "top_download": [{"rank": 1, "rank_change": 0, "name": "DESKTOP-FRAG42", "type": "mac", "bytes_down": 412000000000, "bytes_up": 18000000000}],
  "top_upload": [{"rank": 1, "rank_change": 2, "name": "Table 7", "type": "port_group", "bytes_down": 98000000000, "bytes_up": 61000000000}]
}
```

## Packaging, Releases, CI

Releases are built by GoReleaser with nFPM as soon as a release with a `vX.Y.Z` tag is published in the GitHub web UI (same flow as FlowGauge). One run produces binaries for linux amd64/arm64/armv7, `.deb`, `.rpm`, the Arch package `.pkg.tar.zst`, `.apk`, `SHA256SUMS` and a multi-arch image on `ghcr.io/<owner>/lanleecher`. As with LANPilot, testing comes before publishing: the workflow builds without publishing, installs every package in a fresh container (Debian 12/13, Ubuntu 24.04, Fedora, Rocky 9, Arch, Alpine), starts the service and removes the package again. Only then does a second, bit-identical build (`-trimpath`, commit timestamps) attach the files to the release, push the images and create the build provenance attestation. The release notes from the web UI are kept (`mode: keep-existing`).

### Packages

- Targets like LANPilot: Debian 12/13, Ubuntu 24.04/26.04, Fedora, Rocky Linux 9, Arch Linux, Alpine; architectures `amd64`, `arm64` and `armv7` (Raspberry Pi as a measurement box).
- Contents: `/usr/bin/lanleecher`, `/etc/lanleecher/config.yaml` (marked as a config file, not overwritten by updates), systemd unit `lanleecher.service`, sysusers entry for the user `lanleecher`, data directory `/var/lib/lanleecher`.
- Post-install: create the user, generate `secret.key` if missing; migrations run automatically on start. No auto-start, so the admin configures first; updates restart a running service.
- systemd hardening: `ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`, no capabilities, only `/var/lib/lanleecher` writable.
- Docker image on `ghcr.io/<owner>/lanleecher` (like FlowGauge), with an example `compose.yaml`.
- If the CLI runs as root (`sudo lanleecher user create …`), files it creates are handed to the owner of the data directory, so the service can still write them.

**Database in the package:** packages and the Docker image start with SQLite at `/var/lib/lanleecher/lanleecher.db` – no database server and no further dependencies. SQLite runs in WAL mode; `lanleecher backup` creates a consistent copy while running (`VACUUM INTO`, default target `<data dir>/backups/`). There is no other database backend: SQLite easily handles the write load of a LAN party and keeps installation and backups trivial.

### Quick start (README)

```
sudo apt install ./lanleecher_*.deb
sudo nano /etc/lanleecher/config.yaml      # public_url, listen
sudo lanleecher user create admin --role admin
sudo systemctl enable --now lanleecher
```

### GitHub Actions

- **CI** on push/PR: `go vet`, `golangci-lint`, `go test -race` incl. the snmpsim integration test, build for all targets.
- **Release** on a published release: GoReleaser, package smoke tests in distribution containers, upload to the GitHub release, checksums and build attestations (`actions/attest-build-provenance`), Docker push.
- Dependabot for Go modules and actions.

### Repository contents

`LICENSE` (MIT), `README.md` in the style of the other lan.party projects, `CHANGELOG.md`, `CONTRIBUTING.md`, `SECURITY.md`, `docs/` with installation, configuration, switch compatibility (tested models), LAN party guide (checklist before/during/after the event), reverse proxy, API and "Writing a new FDB/resolver adapter". Plus a project page on lan.party under Tools.

Deployment details of individual installations (hostnames, paths, users of a specific server) are not part of this repository.

### Reverse proxy note: SSE

The board stream `/api/v1/board/stream` must not be buffered by a reverse proxy. nginx example (before `location /`):

```
location /api/v1/board/stream {
    proxy_pass http://127.0.0.1:9040;
    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_buffering off;
    proxy_read_timeout 1h;
}
```

## Documentation (also for LLMs)

Everything is documented so that a human or an LLM in another project can build on LANLeecher without looking at the source code. The OpenAPI specification is the single source of truth for the API.

### API

- **OpenAPI 3.1** in `internal/api/openapi.yaml`, embedded in the binary and served at `/api/openapi.json`, plus interactive docs at `/api/`.
- Every endpoint with description, all parameters with limits, required scope, example request (`curl`) and example response, also for error cases.
- Uniform error format per RFC 9457 (`application/problem+json`) with stable error codes.
- Unambiguous units everywhere: bytes as integers, throughput in bit/s, timestamps RFC 3339 in UTC, normalized MACs. Field names carry the direction (`bytes_down`, `bytes_up`).
- CI test: routes and the specification must match, and every endpoint is called (including error cases) with its response validated against the spec (`kin-openapi`): status code, content type and body. If code deviates from the docs, the build fails.
- Versioning: `/api/v1` stays stable; breaking changes only in `/api/v2`, announced via a `Deprecation` header and in the changelog.

### For LLMs

- `llms.txt` in the repo root, embedded in the binary and served at `/llms.txt` of a running instance (no data, no auth): short description, key concepts, links to spec and docs.
- `docs/llms-full.txt`: automatically generated complete version of concepts, API docs and spec in one file, to paste directly into a prompt.
- `docs/concepts.md` as a glossary: event, device, MAC, port group, role, hiding, download/upload (incl. mapping to `ifHCOutOctets`/`ifHCInOctets`), reset behavior.
- `examples/`: runnable examples with `curl`, Python and Go – Discord bot with top 10, Grafana query, history of a device.
- `AGENTS.md` / `CLAUDE.md` with architecture, conventions and test commands for AI-assisted contributions.

### Operations and code

- The configuration reference is generated from the Go structs, plus a JSON schema of `config.yaml` for editor validation (`go run ./tools/gendocs`; a test fails when they are out of date).
- Architecture decisions as ADRs in `docs/adr/` (e.g. Go instead of Python, store only deltas, no SNMPv1, hiding at query time).
- Godoc comments on all exported types and interfaces, especially on the module interfaces for new adapters.
- Documentation is mandatory in the PR checklist: no change to API, config or data model without matching docs.
- The whole project – code, comments, docs, commit messages – is in English. The web UI is available in English and German.

## Milestones

Each milestone ends with green tests and a working state; they are worked through in order, with a commit after each.

1. **Skeleton** ✓ – repo structure, `go.mod`, MIT `LICENSE`, `CLAUDE.md` with conventions, `.gitignore` for secrets and data, config loading (YAML), `slog`, CLI (`server`, `config init`, `migrate`, `backup`, `version`), CI workflow with lint and tests.
2. **Storage** ✓ – SQLite implementation, migrations for all tables from "Data Model".
3. **Accounting core** ✓ – pure delta function (reset, 64-bit wrap, reboot, plausibility) and attribution (one MAC / port group / server / uplink / unassigned) with table-driven tests. The most important milestone – no shortcuts here.
4. **SNMP collector** ✓ – v2c/v3 with `gosnmp`, poll loop per switch, port discovery, persistence of `port_counter_state`, connection test; integration tests with `snmpsim`.
5. **FDB** ✓ – BRIDGE-MIB and Q-BRIDGE-MIB, mapping bridge port → `ifIndex`, `mac_locations`, uplink heuristics incl. LLDP.
6. **Events** ✓ – event lifecycle, booking into `traffic_totals` and `traffic_buckets`, CLI `event start/stop`.
7. **Auth & web UI** ✓ – users, roles, Argon2id, sessions, CSRF, CLI `user create`; pages dashboard, rankings, search, detail, events, switches & ports, admin; English/German.
8. **API** ✓ – tokens with scopes, all endpoints, OpenAPI spec with route/spec test and response validation (`kin-openapi`) in CI, board endpoint with SSE stream, rate limit, Prometheus metrics.
9. **Resolvers & history** ✓ – `snmp-arp`, `local-neigh`, `dns-ptr`, NetBIOS, mDNS, LLMNR, IP/hostname history, merge suggestions and merge UI; Kea DHCP via HTTPS for Kea 2.x (Control Agent) and Kea 3.x (server control sockets) with certificate pinning ([docs/kea.md](docs/kea.md), ADR 0008).
10. **Packaging & release** ✓ – GoReleaser/nFPM for deb, rpm, Arch, apk, systemd unit, sysusers, Docker (`dockers_v2`), release workflow with attestations, package smoke tests. Open: upgrade test from the previous version (once a first release exists).
11. **Docs & polish** ✓ – README, `docs/` (installation, generated configuration reference and JSON Schema, reverse proxy, LAN party guide, switch compatibility, concepts, API guide, architecture, security, adapters, roadmap), ADRs, `llms.txt` and generated `llms-full.txt`, `examples/` (Discord, Grafana, Go, curl), CHANGELOG, CONTRIBUTING, SECURITY, Dependabot, screenshots from a simulated demo switch. Open: first test at a real LAN.

### Decisions

- [x] Language: Go, like FlowGauge.
- [x] Counter reset: the first value after a reset is credited in full, protected by the plausibility check.
- [x] Switches: the standard MIBs (BRIDGE-MIB, Q-BRIDGE-MIB) are enough; no dedicated Avaya adapter.
- [x] Kea DHCP: optional source via HTTPS API, hook `libdhcp_lease_cmds` is active, Stork keeps running in parallel.
- [x] Byte notation: decimal (1 GB = 10⁹ bytes), as usual for switch counters and providers.
- [x] Project language: English for code, docs and commits; the UI is bilingual (English default, German).
- [x] Database: SQLite only; PostgreSQL support was dropped (ADR 0005).
