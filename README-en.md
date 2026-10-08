# Lightweight TR-069 ACS

[简体中文](README.md) | **English**

A single-binary, zero-external-dependency TR-069/CWMP ACS (Go + SQLite) for managing a fleet of ONTs / FTTR
gateways: **onboard → inspect → configure → diagnose → manage sub-devices and clients**.

![CI](https://github.com/hakureiyuyuko/go-acs/actions/workflows/ci.yml/badge.svg)
![Go](https://img.shields.io/badge/Go-1.27-00ADD8)
![License](https://img.shields.io/badge/license-AGPL--3.0-blue)

## Features

- **Single-file deployment**: one static binary plus one SQLite file — no Redis / MySQL / message queue
- **Standard protocols only**: Inform, GetParameterValues / Names, SetParameterValues, GetRPCMethods, Reboot,
  IPPingDiagnostics, Connection Request (with HTTP Digest); no vendor-private services involved
- **Probe first, then display**: capability probing decides which blocks the UI shows (WAN, FTTR sub-devices…);
  if it can't be probed, the whole block is hidden — no empty shells
- **Faithful to actual device behavior**: parameters that are writable but not readable, wireless parameters that
  take effect asynchronously, fields the device never reported (shown as `N/A`) — no invented numbers
- **Bilingual panel (Chinese / English)**: one-click switch in the top-right (`?lang=` + cookie + browser language
  negotiation), with tests guarding against untranslated strings
- **No actions offered on offline devices**: buttons that need the device to cooperate (re-fetch, wake, reboot,
  diagnostics) are greyed out while offline; local operations such as deleting a device still work
- **Online status derived from the device's own interval**: if nothing is reported for more than
  `report interval × 2`, the ACS probes the device with Connection Request three times, and only marks it offline
  when all probes fail (a powered-off device sends no TR-069 notification, so this is the only way to confirm)
- **Operable**: persistent task queue, one-click device wake-up, reboot / delete device, a dedicated panel login
  page (session cookie, with logout), and retention limits for task and inform history
- **Vendor-specific parameters have a mapping table**: every vendor names optical power / temperature
  differently (and some report raw optical-module register values), so those mappings live in the
  database (`param_aliases`) — supporting a new ONT is a few rows of data (`acs alias add`), and
  unregistered models still fall back to leaf-name heuristics rather than showing nothing or invented numbers
- **No command line needed to change listen ports**: after editing the addresses on the settings page, click
  “Restart the service now” and the process swaps itself (unchanged ports hand their sockets to the new
  process, so device informs never drop; a port that won't come up is rolled back on the spot)

## Screenshots

> Data comes from the CPE simulator bundled with this repository; no real device information is included.

Overview: device list (online status, serial number, software version, data model, last inform time, number of
collected parameters, wireless client count — plus Rx / Tx optical power columns whenever any device reports
them; filterable by online / offline, with three states —
online / probing / offline — and a 5-second auto-refresh toggle in the top-right) plus wireless overview

![Overview](docs/images/overview.png)

Basic information (including optical module readings: Rx / Tx power, temperature, voltage, bias current),
WAN connections, Ethernet ports, operations (wake / reboot / delete), notes; the same 5-second auto-refresh
toggle (shared with the list page) is in the top-right

![Device detail](docs/images/device.png)

FTTR sub-devices and network diagnostics: model, networking mode, optical power, and client counts per sub-device
(refreshed periodically, on the same cadence as the wireless overview);
diagnostics are ICMP sent by the device itself

![FTTR and diagnostics](docs/images/fttr.png)

Client list: grouped by host / sub-gateway, each client showing signal strength, hostname, and IP
(fields the device never reported are shown as `N/A`)

![Client list](docs/images/clients.png)

Settings: listen addresses for the ACS and the panel, plus the panel login credentials
(port changes take effect after restart, credentials take effect immediately); after changing an address you
can click “Restart the service now” instead of going back to the command line

![Settings](docs/images/settings.png)

## Install (release package)

Download the package for your architecture from
[Releases](https://github.com/hakureiyuyuko/go-acs/releases), extract it, and install it as a systemd service
with one command:

```bash
VERSION=1.2.3                                   # replace with the version you downloaded
tar xzf acs-$VERSION-linux-amd64.tar.gz
cd acs-$VERSION-linux-amd64
sudo ./install.sh                                                # by default both CWMP and panel use :7547
sudo ./install.sh --web-listen :8080 --web-user admin --web-pass 'change-me'
```

When done, it prints the panel address and the ACS URL to configure on the ONT (`http://<host-ip>:7547/acs`).
Afterwards:

```bash
sudo ./update.sh         # upgrade: verify SHA256 → stop → backup → swap → health check, auto-rollback on failure
sudo ./uninstall.sh      # uninstall: keeps database and config by default; --purge removes data too
```

Default locations: binary `/usr/local/bin/acs`, data `/var/lib/acs`, config `/etc/default/acs`,
service `acs`. The `README.md` inside the package documents all options and directories.

## Quick start (from source)

```bash
go build -o acs ./cmd/acs        # Go 1.27+; CGO_ENABLED=0 for a static binary
./acs -listen :9090 -db acs.db   # panel http://<IP>:9090/ , CWMP http://<IP>:9090/acs
```

Building release packages:

```bash
scripts/build-release.sh v1.2.3   # output in dist/: amd64 + arm64 tarballs and SHA256SUMS
```

On the device side, set the ACS URL to `http://<IP>:9090/acs`; real devices have also been seen configured with
the root path `/`, so both are accepted. When the panel is moved to its own port (`-web-listen :8080`), the CWMP
side **accepts any path**, so you don't have to worry about how carrier-customized devices write it.

No device needed to try it: the repository ships a simulator (with FTTR sub-devices, writable-but-unreadable
parameters, asynchronous diagnostics, and more).

```bash
go build -o cpesim ./test/cpesim
./cpesim -acs http://127.0.0.1:9090/acs -serial DEMO0123 -fttr 3 -fttr-optical
```

## Configuration

Command-line flags and environment variables map one-to-one (environment variable name = `ACS_` + flag name
uppercased, hyphens replaced with underscores).

| Flag | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `-listen` | `ACS_LISTEN` | `:7547` | CWMP listen address |
| `-web-listen` | `ACS_WEB_LISTEN` | empty | Panel listen address; empty = same port as CWMP |
| `-path` | `ACS_PATH` | `/acs` | CWMP endpoint path (used when sharing a port) |
| `-db` | `ACS_DB` | `acs.db` | SQLite file path |
| `-user` / `-password` | `ACS_USER` / `ACS_PASSWORD` | empty | Device-side HTTP authentication (CPE basic auth) |
| `-web-user` / `-web-pass` | `ACS_WEB_USER` / `ACS_WEB_PASS` | empty | Panel login credentials (seeded on first start; afterwards the settings page wins) |
| `-max-params-per-request` | `ACS_MAX_PARAMS_PER_REQUEST` | `200` | How many parameter names to send per GetParameterValues |
| `-task-history-limit` | `ACS_TASK_HISTORY_LIMIT` | `500` | Task records retained per device (`0` = unlimited) |
| `-inform-history-limit` | `ACS_INFORM_HISTORY_LIMIT` | `500` | Inform records retained per device (`0` = unlimited) |
| `-offline-after` | `ACS_OFFLINE_AFTER` | `10m` | How long without an inform counts as offline (fallback when the device does **not** report its interval) |
| `-offline-probe` | `ACS_OFFLINE_PROBE` | `true` | Probe actively before marking offline; `false` = pure timeout |
| `-offline-probe-factor` | `ACS_OFFLINE_PROBE_FACTOR` | `2` | Start probing after `report interval × this factor` with no inform |
| `-offline-probe-attempts` | `ACS_OFFLINE_PROBE_ATTEMPTS` | `3` | Maximum probe attempts before marking offline |
| `-offline-probe-interval` | `ACS_OFFLINE_PROBE_INTERVAL` | `15s` | Interval between two probes |
| `-offline-probe-grace` | `ACS_OFFLINE_PROBE_GRACE` | `30s` | How long to wait after the last probe before marking offline |
| `-offline-probe-max` | `ACS_OFFLINE_PROBE_MAX` | `0` | Upper bound for `interval × factor` (`0` = none; useful when a device reports a very large interval) |
| `-offline-check-interval` | `ACS_OFFLINE_CHECK_INTERVAL` | `30s` | How often the background checker sweeps online status |
| `-auto-fetch-wifi` | `ACS_AUTO_FETCH_WIFI` | `true` | Automatically collect wireless overview and host list on onboarding |
| `-task-wake-interval` | `ACS_TASK_WAKE_INTERVAL` | `10s` | How often to send a Connection Request when tasks are queued (`0` = off: wait for periodic reports) |
| `-auto-refresh-wifi` | `ACS_AUTO_REFRESH_WIFI` | `10m` | Auto-refresh interval for the wireless / client overview **and FTTR sub-devices** (the "collected at" time on the panel moves accordingly; `0` = collect only once at onboarding) |
| `-probe-capabilities` | `ACS_PROBE_CAPABILITIES` | `true` | Run capability probing once on onboarding (decides UI blocks) |
| `-log-level` | `ACS_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `-log-soap` | `ACS_LOG_SOAP` | `false` | Print raw SOAP messages (for troubleshooting) |

### Panel login

The panel uses a **dedicated login page + session cookie** (not HTTP Basic): any page accessed while logged out
redirects to `/login`; once logged in the session lasts 7 days and is renewed automatically while in use, with a
"Logout" button in the top bar. The session is an HMAC-signed cookie (HttpOnly + SameSite=Lax, key stored in the
database so it survives restarts); **changing the credentials immediately invalidates all existing sessions**.
API routes (`/api/*`) return 401 JSON when logged out; static assets require no login.

Forgot the password: start once with `ACS_WEB_AUTH=off` to get in and change it, or clear `web_user`/`web_pass`
from the database.

### How online status is decided

When a device loses power, TR-069 sends the ACS **no** notification (the ACS only knows a device is alive when the
device connects to it), so online status is derived from "how long has it been silent", tied to the interval the
device itself reports:

```
last inform + report interval × 2     → start active probing: send Connection Request
  up to 3 probes (15s apart)          → one succeeds: device is still alive, stays online
  all 3 fail + 30s more               → mark offline
device reports no interval            → fall back to -offline-after (default 10 minutes) pure timeout
```

The UI has three states: **online** / **probing** (overdue, being probed) / **offline**.
As soon as the device informs again (including reconnecting after being woken), it returns to online and its
probing progress is cleared.

## Verification

```bash
go test ./...                   # unit tests: protocol parsing / store / web
bash scripts/verify-s1.sh       # 368 end-to-end checks: simulator drives real HTTP + SOAP, asserted one by one
bash scripts/verify-interop.sh  # 8 interoperability checks against GenieACS's official JS simulator
```

## Load testing

```bash
scripts/loadtest.sh                     # ladder 10/50/100/200/400, each step on a fresh database
scripts/loadtest.sh -n 200 -e 4484      # run a single step (-e = number of X_HW_APDevice subtree params per device)
```

The device template follows a real Huawei V271-20 (TR-098, 3 FTTR sub-devices, 4484 parameters in the
`X_HW_APDevice` subtree, ~4700 parameters per device), with all devices sending `1 BOOT` simultaneously to
simulate **a large-scale power-recovery storm**.

Measured on this machine (4 cores): about **4 devices/second** (16k–18k parameter rows/s) — 100 devices up at once
finish in 26 seconds, 200 take about a minute, 400 about two minutes; all data is persisted, it's just that
devices exceeding the CPE's 30-second timeout wrap up on their next periodic inform. The bottleneck is SQLite's
single-connection serialized writes (**don't increase the connection pool** — it loses writes in practice).
Methodology and full data in `docs/notes/loadtest.md`.

## Layout

```
cmd/acs/            program entry point (listeners, graceful shutdown, dual-port routing)
internal/cwmp/      protocol core: SOAP encode/decode, sessions, tasks, diagnostics, Connection Request
internal/store/     SQLite: devices / parameters / tasks / inform records (migrations via user_version)
internal/web/       web UI and JSON API (templates + a little vanilla JS, no frontend framework)
test/cpesim/        purpose-built CPE simulator (many switches; the verification suite relies on it)
deploy/             install / update / uninstall scripts and systemd unit template for release packages
scripts/            packaging, verification, load testing, reference-implementation fetch, dev start/stop scripts
docs/               requirements docs, release notes, and implementation notes
```

## More

- `docs/requirements.md` — requirements and implementation progress
- `docs/notes/deploy.md` — implementation notes for the installer and deploy scripts (systemd hardening,
  upgrade rollback, how it was verified) — *in Chinese*
- `docs/notes/i18n.md` — the panel's i18n approach (why Chinese literals are used as keys, how to add a language)
  — *in Chinese*
- `docs/notes/loadtest.md` — **load test records**: concurrency ceiling for a mass power-on, the bottleneck
  (SQLite single connection), and measured data — *in Chinese*
- `docs/notes/implementation-notes.md` — **real-device pitfalls and measurements**: protocol edge cases (per-call
  GetParameterValues limits, type case-sensitivity on write-back, diagnostics needing `Requested` set last),
  device quirks (writable-but-unreadable, asynchronous effect, identity keys rewritten by metadata), operations
  pitfalls (don't blindly delete when a mount drops, don't leave tasks stuck in running)… fixes and verification
  included — *in Chinese*

Real-device samples and documents in this repository are **sanitized** (serial numbers, MACs, SSIDs, private IPs,
and client names replaced with example values); the runtime database `data/` and logs are not committed.

## License

[AGPL-3.0](LICENSE). You are free to use, modify, and distribute it (including commercially); however, if you
offer a **modified** version to others as a network service, you must release the corresponding source under the
same license.

## Credits

This project was developed with assistance from **Deepseek V4.1 Flash**.

<img src="docs/images/thanks.png" alt="小肥鱼太棒了！" width="300">

## Sponsorship — Buy me a coffee

**TRC20 TJAHCw3UKdysUnkqDLZxPn39LJ6FigCdoB**
