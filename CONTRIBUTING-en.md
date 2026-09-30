# Contributing

[简体中文](CONTRIBUTING.md) | **English**

> ## ⚠️ This repository only handles issues and pull requests on **PRC (China) working days**
>
> Concretely: the timezone is **Beijing time (UTC+8)**. Saturdays, Sundays and statutory holidays
> (including the State Council's holiday and adjusted-workday arrangements) are **not guaranteed**
> to get a response; the backlog is handled in one go on the first working day after a break.
>
> This is an open-source project maintained by one person — please expect replies on that cadence.
> It is not being ignored, it is batched onto working days. **Security reports go through issues
> as well**, and I'll get to them as soon as possible on a working day.

Issues and pull requests are welcome in **Chinese or English**. Any kind of feedback helps:
bug reports, feature requests, documentation fixes, extra tests, and parameter mappings for
new ONT models (see [Vendor parameter mappings](#vendor-parameter-mappings-adding-a-new-ont)).

---

## Contents

- [What this project cares about](#what-this-project-cares-about)
- [Reporting a problem (Issue)](#reporting-a-problem-issue)
- [Sending a pull request](#sending-a-pull-request)
- [Development environment](#development-environment)
- [Code and documentation conventions](#code-and-documentation-conventions)
- [Verification: what to run before submitting](#verification-what-to-run-before-submitting)
- [Commit messages](#commit-messages)
- [Vendor parameter mappings: adding a new ONT](#vendor-parameter-mappings-adding-a-new-ont)
- [Review and merge](#review-and-merge)
- [License](#license)

---

## What this project cares about

This ACS is a management tool that ships as **one static binary plus one SQLite file** and runs
in ONT / FTTR deployments. These principles decide what gets accepted — **they matter more than
feature count**:

1. **Never invent numbers.** If the device did not report something, show `-` or hide the whole
   block — never guess a plausible-looking value. Capabilities that cannot be probed (WAN, FTTR,
   optical power…) must not leave empty shells in the UI.
2. **Real hardware has the final say.** A capability must be **actually exercised on a real device**
   (or reproduced with a simulator scenario). Reading an `ffmpeg -encoders`-style list, or trusting
   documentation, is not evidence. Pull requests should show what was run and what came out.
3. **No external middleware.** No Redis / message queue / frontend build chain (npm, vite…).
   A new Go dependency needs a stated reason.
4. **UI copy reads like a real product.** Implementation details and protocol internals do not belong
   in the interface — they go into `docs/notes/implementation-notes.md`.
5. **Present device quirks honestly.** Writable-but-unreadable parameters, asynchronous effects,
   misspelled parameter names, inconsistent units… all get recorded as-is (real-device findings go
   into the implementation notes) instead of being smoothed over in the UI.

## Reporting a problem (Issue)

Please include the following — it saves several rounds of back and forth:

- **Version**: the `VERSION` file, `git describe --tags`, or which release you installed
- **Device**: vendor / model / software version / data model (TR-098 or TR-181) / rough parameter count
- **Symptom**: what you expected vs. what happened; screenshots help for UI issues
- **Evidence**: the relevant part of `data/acs.log` (raw SOAP messages via `-log-soap` are even
  better), the result of the corresponding task in the task list, and the parameter names and values

**Sanitize**: replace serial numbers, MACs, SSIDs, public/private addresses and client names with
example values. Every real-device sample committed to this repository is sanitized — please keep it
that way.

## Sending a pull request

- **One PR does one thing.** Opportunistic refactors, formatting and unrelated fixes belong in
  separate PRs.
- **Explain why first** (which field problem, reproduced on which device), then what changed.
- **Attach verification evidence**: which checks you ran and their results; for real-device checks,
  name the model and the conclusion. "The code looks right to me" is not evidence.
- **Call out behaviour changes**: changed display semantics, task-result wording or database
  structure must be stated in the PR description and reflected in the docs.
- **Do not rewrite repository ownership**: please do not put your own fork URL
  (`github.com/<you>/go-acs`) into `README`, `README-en.md`, `deploy/README.md`,
  `deploy/acs.service` or `deploy/update.sh` — after merging upstream, those must point at the
  upstream repository.
- **Do not touch these invariants** (unless the PR explains why):
  - `SetMaxOpenConns(1)` in `store.Open`: load testing showed that raising it causes `SQLITE_BUSY`
    and lost writes;
  - migrations may only be **appended at the end** (they run in `PRAGMA user_version` order;
    inserting in the middle makes existing databases skip a step);
  - i18n uses **Chinese literals as keys** (`{{T "状态"}}`) — do not switch to `overview.title`-style keys.
- **Do not run `git add -A`**: stage only the files you actually changed (the working directory may
  contain unrelated work).
- **English UI copy**: new user-visible Chinese strings need an English entry in
  `internal/i18n/strings_en.go` (tests will fail if you forget, see below).

## Development environment

All you need is Go (1.27+) and an optional simulator — no database server, no npm:

```bash
git clone https://github.com/hakureiyuyuko/go-acs.git && cd go-acs

export PATH=$HOME/.local/go/bin:$PATH   # if Go lives somewhere custom
CGO_ENABLED=0                            # pure-Go modernc.org/sqlite

go build -o acs ./cmd/acs                # the server
go build -o cpesim ./test/cpesim         # the self-built CPE simulator (verification relies on it)
```

Run a local instance in the background (pidfile, log at `data/acs.log`):

```bash
scripts/dev-server.sh start|stop|restart|status|log
```

No real device required — the simulator has plenty of switches (FTTR sub-devices, optical power,
writable-but-unreadable parameters, asynchronous diagnostics…):

```bash
./cpesim -acs http://127.0.0.1:9090/acs -serial DEMO0123 -fttr 3 -fttr-optical -optical
```

## Code and documentation conventions

- **Go**: `gofmt` must be clean; comments explain **why** (especially traps you hit), not what the
  code says. One package, one job; pure logic should be extracted into unit-testable functions.
- **Frontend**: templates plus a little vanilla JS (`internal/web/static/app.js`). No frameworks,
  no build step.
- **Tests**:
  - unit-test pure functions, template rendering and the store layer (`go test ./...`);
  - end-to-end goes through `scripts/verify-s1.sh` (real HTTP + real SOAP + real SQLite) — new
    features should **add assertions**, and the assertions must be verifiable in reverse (they go
    red when you take the fix away);
  - interoperability goes through `scripts/verify-interop.sh`.
- **Documentation**:
  - real-device findings, protocol edge cases, device quirks → `docs/notes/implementation-notes.md`;
  - frontend-facing conventions (i18n, load testing, deployment) → the matching file under `docs/notes/`;
  - release notes → `docs/releases/vX.Y.Z.md` (added by the maintainer at release time only);
  - requirements and progress → `docs/requirements.md`.

## Verification: what to run before submitting

```bash
export PATH=$HOME/.local/go/bin:$PATH
CGO_ENABLED=0

gofmt -l . | grep -v '^reference/'     # expect: no output
go vet ./...                           # expect: no output
go test ./...                          # expect: all ok
bash scripts/verify-s1.sh              # expect: all pass (373 checks at the moment)
bash scripts/verify-interop.sh         # expect: 8 / 0 (needs node and reference/genieacs-sim)
```

Please paste the actual output (or a summary) into the PR description. CI runs most of it as well.

## Commit messages

- Write them in **Chinese** (the project's working language): the first line says what changed, the
  body says why and — when relevant — **what the real device did**;
- Suggested prefixes: `feat:` / `fix:` / `docs:` / `refactor:` / `test:` / `ci:`;
- One commit does one thing; do not commit `dist/`, `data/`, logs or editor config.

Example:

```
fix(web): 无线概况不再显示射频对象造出的假实例（那行「5G ｜ - ｜ 开 ｜ -」）

根因：SSID 一级的 WLANConfiguration.{i} 真机实例号是 1/5，射频一级的
WiFi.Radio.{i} 是 1/2，按实例号硬合并就凭空多出一行。真机三台验证已回归。
```

## Vendor parameter mappings: adding a new ONT

Vendors name (and place, and scale) optical power / temperature / voltage parameters differently,
and a single device may expose both the real value and the **raw optical-module register value**
(SFF-8472 encoding). Those mappings live in the database table `param_aliases`, so **supporting a
new model needs no code change**:

```bash
acs alias kinds                                     # list supported decodings
acs alias ls  --db /var/lib/acs/acs.db              # list existing mappings
acs alias add --db /var/lib/acs/acs.db \
    --field rx_power --contains optical.interface. --suffix .rxpower \
    --decode identity --priority 5 --vendor ZTE --note "ZXHN F610GV9 measured -23.4 dBm"
acs alias rm  --db /var/lib/acs/acs.db 12
```

If mappings are not enough (new field, new decoding), code changes are needed: field definitions
live in `panelFields` in `internal/web/params_map.go`, decodings in `decodeRaw` in the same file.
Please include the **real measured values** (what the device's own page shows vs. what your mapping
produces).

## Review and merge

- The maintainer looks at PRs on **working days** (see the notice at the top) and usually replies
  with two tiers: must-fix items and points for discussion;
- Before merging, the verification suite above is re-run; behaviour changes update the docs and the
  release notes along the way;
- Merges are mostly squashed, and your authorship is preserved (**please keep your own name and
  email in your commits**).

## License

This project is released under [AGPL-3.0](LICENSE). Any contribution you submit is understood to be
**licensed under the same terms** (i.e. you agree to AGPL-3.0 distribution and to the maintainer
redistributing it as part of the project).

---

Not sure how to proceed? Open an issue describing the field problem first — we'll look at it
together **on a working day**.
