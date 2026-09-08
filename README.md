# Container Supervisor

Container Supervisor lets you run multiple processes inside a container. The can depend on another or run in parallel.

## Motivation

Containers are happiest running a single process, but real workloads rarely fit
that cleanly. A small preparation script — seeding a database, generating config,
warming a cache — doesn't warrant its own container and orchestration just to run
before the main process. A periodic job that belongs next to a service shouldn't
have to become a separate cronjob-per-container.

Container Supervisor covers those cases: run the prep step as a `one_shot` that
the main service `depends_on`, or schedule recurring work as a `cron` process
(or a `ticker`, for intervals shorter than cron's one-minute floor) — all inside
the one container, without pulling in an init system or a second orchestration
layer.

## Compared to supervisord

[supervisord](http://supervisord.org/) is the established, feature-rich process
control system, and for most non-container use it does more: a web UI, an XML-RPC
control interface, log rotation, process groups, and runtime `supervisorctl`
management. If you need those, use it.

Container Supervisor deliberately does less. It is a single static binary that
runs on distroless images with no shell, Python runtime, or package manager —
where supervisord's Python dependency doesn't fit. Beyond the minimal footprint,
its focus is process orchestration for the lifetime of a container: a dependency
graph (`depends_on`), one-shot vs. service vs. cron vs. ticker process types, and per-process
failure policies, all in one YAML file. Trading breadth for a small, dependency-free
binary is the point.

## Usage

Prebuilt binaries are published for all major operating systems and
architectures (Linux, macOS, Windows; amd64 and arm64) — grab the one for your
platform from the releases, no runtime or dependencies required.

Container Supervisor is designed for distroless containers: it's a single
static binary with no dependencies, so it needs no shell, init system, or
package manager in the image. Copy the binary into your image, add your config,
and make it the entrypoint. It runs in the foreground as PID 1 and manages the
child processes for the life of the container.

```dockerfile
FROM gcr.io/distroless/static-debian12
COPY container-supervisor /usr/local/bin/
COPY config.yml /container-supervisor/config.yml
COPY --from=build /bin/api /bin/migrate /bin/
ENTRYPOINT ["container-supervisor"]
```

Pick the binary matching the image's OS/architecture (e.g. `linux/arm64`).

```
container-supervisor [--config|-c PATH]
```

- `--config`, `-c` — path to the config file. Default: `/container-supervisor/config.yml`.

The config file is optional: the configuration can also be supplied through
`SUPERVISOR_`-prefixed environment variables (see
[Configuration](#configuration)), so you can define everything
in the Dockerfile or your orchestrator without shipping a config file at all.

## Configuration

The configuration can be supplied two ways, and they compose: a YAML file, and
`SUPERVISOR_`-prefixed environment variables overlaid on top. Either can be used
alone — the whole config can live in env vars with no file at all — or env vars
can override individual keys of a file.

### In YAML

```yaml
loglevel: info
log_output_format: console
processes:
  db:
    path: /bin/postgres
    type: service
    on_failure: restart
  migrate:
    path: /bin/migrate
    arguments: ["--dry-run"]
    environment:
      DATABASE_URL: postgres://localhost/app
    type: one_shot
    on_failure: retry 3
  seed:
    path: /bin/seed
    type: one_shot
    depends_on:
      migrate:
        exit: success
```

### In environment variables

The same config as above. Nested keys are joined with `__` (double underscore);
the segment after `PROCESSES__` is the process name:

```sh
SUPERVISOR_LOGLEVEL=info
SUPERVISOR_LOG_OUTPUT_FORMAT=console
SUPERVISOR_PROCESSES__DB__PATH=/bin/postgres
SUPERVISOR_PROCESSES__DB__TYPE=service
SUPERVISOR_PROCESSES__DB__ON_FAILURE=restart
SUPERVISOR_PROCESSES__MIGRATE__PATH=/bin/migrate
SUPERVISOR_PROCESSES__MIGRATE__ARGUMENTS="--dry-run"
SUPERVISOR_PROCESSES__MIGRATE__ENVIRONMENT__DATABASE_URL=postgres://localhost/app
SUPERVISOR_PROCESSES__MIGRATE__TYPE=one_shot
SUPERVISOR_PROCESSES__MIGRATE__ON_FAILURE="retry 3"
SUPERVISOR_PROCESSES__SEED__PATH=/bin/seed
SUPERVISOR_PROCESSES__SEED__TYPE=one_shot
SUPERVISOR_PROCESSES__SEED__DEPENDS_ON__MIGRATE__EXIT=success
```

Any scalar leaf works at any depth, so a whole config can be defined in env vars
alone. `arguments` is a list: a single value is split on whitespace
(`ARGUMENTS="--dry-run --limit 10"`), or number the entries when an argument
must itself contain a space:

```sh
SUPERVISOR_PROCESSES__MIGRATE__ARGUMENTS__0=--dry-run
SUPERVISOR_PROCESSES__MIGRATE__ARGUMENTS__1="--limit 10"
```

The name-to-YAML conversion is [envelope](https://github.com/BaseCrusher/envelope);
see its README for the full naming rules (list indexes, escaping a key that
looks like an index). Names are case-insensitive here — the part after
`SUPERVISOR_` is lowered to match the YAML keys.

### Schema

Top-level keys:

| Key | Type | Used for |
| --- | --- | --- |
| `loglevel` | string | Log verbosity: `trace`, `debug`, `info` (default), `warn`, `error`. |
| `log_output_format` | string | `console` (default) or `json`. See [Logging](#logging). |
| `hide_labels` | bool | Default for every process's `hide_label` (below). Defaults to `false`. |
| `env_dir` | string | Folder scanned for env vars to pass between processes. See [Passing env vars between processes](#passing-env-vars-between-processes). Defaults to `supervisor_environment` next to the config file. |
| `processes` | map | Process name → process definition (below). |

Per-process keys (`processes.<name>.*`):

| Key | Type | Required | Used for |
| --- | --- | --- | --- |
| `path` | string | yes | Path to the executable to run. Ignored for `prepare_secrets`, which runs no external command. |
| `type` | string | yes | `service` (long-running; exiting at all aborts the run), `one_shot` (expected to finish; exit 0 is clean), `cron` (runs on a wall-clock schedule), `ticker` (runs on a fixed interval), or `prepare_secrets` (built-in; resolves `*_FILE` variables into `env_dir`, see [Preparing secrets](#preparing-secrets)). |
| `arguments` | list | no | Args passed to the executable verbatim. |
| `environment` | map | no | Env vars added on top of the supervisor's own environment, overriding inherited keys of the same name. |
| `cron` | string | for `cron` | Standard 5-field cron expression (`minute hour day-of-month month day-of-week`), validated at load. See [Cron semantics](#cron-semantics). |
| `ticker` | string | for `ticker` | Fixed interval between runs, `@every 5sec`, validated at load. See [Ticker semantics](#ticker-semantics). |
| `run_at_start` | bool | no | For `cron` and `ticker` only. Defaults to `false`. `true` also runs the process once at startup, on top of its schedule. |
| `enabled` | bool | no | Defaults to `true`. `false` keeps the process from ever starting; it reports as a `failure` to anything that `depends_on` it, and never aborts the run itself. |
| `on_failure` | string | no | What to do when the process ends unexpectedly (see below). |
| `loglevel` | string | no | Overrides the global `loglevel` for this process's logger: `trace`, `debug`, `info`, `warn`, `error`. Validated at load. Defaults to the global level. |
| `hide_label` | bool | no | Overrides the global `hide_labels` for this process. Drops the process name from its output: no `[name] ` prefix in console (lines start at column 0), in json a line that is already json is passed through untouched, anything else becomes a bare `{"message": ...}`. Defaults to `false`. |
| `for_each` | list | no | Only valid for `one_shot`. Fan the process out into one instance per entry. Each entry is a row of `;`-separated fields; `{{N}}` (1-based) in `path`, `arguments`, and `environment` values is replaced with the Nth field. See [for_each](#for_each). |
| `secrets` | list | for `prepare_secrets` | The `*_FILE` variable names to resolve. See [Preparing secrets](#preparing-secrets). |
| `depends_on` | map | no | Gate startup on upstream outcomes. Maps an upstream process name → `{ exit: <outcome> }`, where `<outcome>` is `success` (exit 0), `failure` (non-zero), or `any` (exited at all). The upstream may not be a `service`, `cron`, or `ticker` — none is expected to exit, so the dependent would never start. |

`on_failure` values depend on the process type:

| Type | Allowed values |
| --- | --- |
| `service` | `exit` (default, aborts the run), `restart` (rerun until the run is cancelled), `continue` (tolerate). |
| `one_shot` / `cron` / `ticker` / `prepare_secrets` | `fail` (default, aborts the run), `continue` (tolerate), `retry <count>` (rerun up to count times, then fail). |

## Passing env vars between processes

A process can hand env vars to a later one by writing a file into `env_dir` (defaults to `supervisor_environment` next to the config file). Before starting any process, the supervisor reads every file in that folder and injects the vars into the child.

**One file per variable: the filename is the key, the file's contents are the value** (a trailing newline is trimmed). This keeps multiline values — PEM keys, certs — intact, which a line-based format cannot:

```sh
# producer.sh
printf %s "$(fetch-token)" > /container-supervisor/supervisor_environment/API_TOKEN
```

Order the producer before the consumer with `depends_on` — that barrier is what guarantees the file exists on disk before the consumer starts:

```yaml
processes:
  producer:
    type: one_shot
    path: /producer.sh
  consumer:
    type: one_shot
    path: /consumer.sh   # sees $API_TOKEN
    depends_on:
      producer: { exit: success }
```

Precedence, lowest to highest: the supervisor's own environment, then `env_dir` files, then the process's `environment:` block (config always wins). Files accumulate for the life of the container; processes are never restarted.

### Preparing secrets

A `prepare_secrets` process is a built-in producer for the Docker/Compose `*_FILE`
convention. You list the `*_FILE` variables to resolve under `secrets`; for each
`NAME_FILE` holding a path, it reads that file and writes `env_dir/NAME`, so
later processes get `$NAME` directly. It runs no external command (no `path`
needed) and the value is the raw file contents, so multiline secrets pass
through unchanged.

```yaml
processes:
  secrets:
    type: prepare_secrets
    secrets: [DB_PASSWORD_FILE, TLS_KEY_FILE]
  app:
    type: service
    path: /app
    depends_on:
      secrets: { exit: success }   # now sees $DB_PASSWORD and $TLS_KEY
```

The listed `*_FILE` variables can come from the container's own environment (how
Docker secrets are wired) or from the process's `environment:` block. A listed
variable that is unset, or whose file cannot be read, fails the process. As with
any producer, gate consumers behind it with `depends_on`.

## Dependency semantics

`depends_on` gates a process on upstream processes exiting with the required
`exit` outcome (`success`, `failure`, or `any`) — for init/migration steps that
must finish before dependents start. Processes with no dependencies start
immediately and run in parallel. If an upstream exits with an outcome that does
not satisfy the condition, the dependent is skipped (never started).

Any process type may declare `depends_on`, so a `service` can wait on a
migration `one_shot`, and so can a `cron` or a `ticker`. What an upstream may
*be* is restricted instead: gating waits for the upstream to exit, and a
`service`, `cron` or `ticker` is not expected to — a scheduled process keeps
running its schedule — so depending on one would park the dependent for the life
of the container. That is rejected at startup.

A process with `enabled: false` never starts, whatever its type. Dependents see
it as a `failure`: those requiring `exit: success` are skipped, those requiring
`failure` or `any` proceed. Disabling never aborts the run, regardless of
`on_failure`.

The dependency graph is validated for cycles and unknown references before
anything starts; a cycle or dangling reference is a fatal startup error.

### Failure policy

Each process has an `on_failure` policy for when it exits non-zero:

- `fail` (default) — abort the whole run: cancel the context, kill the other
  running processes, and exit non-zero.
- `continue` — tolerate the failure and let independent branches keep running.
- `retry <count>` — re-run the process up to `count` times; if it still fails,
  abort like `fail`.

```yaml
processes:
  migrate:
    path: /bin/migrate      # on_failure defaults to fail: a failed migration aborts everything
    type: one_shot
    on_failure: retry 3     # give the migration 3 more tries before giving up
  metrics:
    path: /bin/metrics
    type: service
    on_failure: continue    # a crash here doesn't take the container down
```

## for_each

`for_each` fans one process definition out into several instances that differ
only in a few values — the same binary run per shard, per region, per tenant —
without copy-pasting a block per case. It is only valid on `one_shot`
processes.

Each entry in the list is a row of `;`-separated fields. Every row produces one
instance; `{{N}}` (1-based) in the process's `path`, `arguments`, or
`environment` values is replaced with the row's Nth field:

```yaml
processes:
  worker:
    path: /bin/worker
    type: one_shot
    arguments: ["--shard", "{{1}}", "--zone", "{{2}}"]
    environment:
      ZONE: "{{2}}"
    for_each:
      - "3;eu-west"
      - "4;us-east"
```

This starts two processes: `worker (3)` running `/bin/worker --shard 3 --zone
eu-west` and `worker (4)` running `--shard 4 --zone us-east`. Each instance is
named `<process> (<first field>)`, and that name is what appears in log labels.
A `{{N}}` that points past the fields in a row, an empty first field, or two
rows that yield the same instance name are all startup errors.

Other processes `depends_on` the group by its original name — the dependency is
expanded to every instance, so the dependent waits for **all** of them and its
`exit` condition must hold for each:

```yaml
processes:
  migrate:
    path: /bin/migrate
    type: one_shot
    arguments: ["--tenant", "{{1}}"]
    for_each: ["acme", "globex"]
  api:
    path: /bin/api
    type: service
    depends_on:
      migrate:                # waits for migrate (acme) AND migrate (globex)
        exit: success         # both must exit 0, or api is skipped
```

In environment variables, `for_each` is a list like `arguments`, so number the
rows; the `;`-separated fields stay in each value. A single scalar is rejected —
it must be indexed:

```sh
SUPERVISOR_PROCESSES__WORKER__PATH=/bin/worker
SUPERVISOR_PROCESSES__WORKER__TYPE=one_shot
SUPERVISOR_PROCESSES__WORKER__ARGUMENTS__0="--shard {{1}}"
SUPERVISOR_PROCESSES__WORKER__ARGUMENTS__1="--zone {{2}}"
SUPERVISOR_PROCESSES__WORKER__FOR_EACH__0="3;eu-west"
SUPERVISOR_PROCESSES__WORKER__FOR_EACH__1="4;us-east"
```

## Cron semantics

A `cron` process runs on the schedule in its `cron` field for as long as the
container lives — it has no "done" state, so a config containing one keeps the
supervisor running until it is signalled. (The exception is a cron whose
`depends_on` condition goes unmet: it is skipped like any other process, and
the run can then finish.)

```yaml
processes:
  backup:
    path: /bin/backup
    type: cron
    cron: "0 3 * * *"       # 03:00 every day
    on_failure: continue    # a failed backup shouldn't take the container down
  warm-cache:
    path: /bin/warm-cache
    type: cron
    cron: "*/15 * * * *"    # every 15 minutes
    run_at_start: true      # ...and once at startup, so the cache is never cold
```

The schedule is the only trigger unless `run_at_start: true` is set, in which
case the process also runs once as soon as its dependencies are satisfied.
`on_failure` applies per occurrence, exactly as it does for a `one_shot`: with
the default `fail`, a single failed occurrence aborts the run.

Field syntax is standard: each of the five fields is a comma-separated list of
`*`, a number, or a `lo-hi` range, any of which may carry a `/step`. A step on
a range counts from the low end (`10-50/7` is 10,17,24,…), and a step on a bare
number runs to the end of the field (`5/15` in the minute field is
5,20,35,50). Day-of-week is `0`-`6` with `0` for Sunday; `7` is rejected. When
both `day-of-month` and `day-of-week` are restricted, a day matches if *either*
does — the usual cron rule. Names (`MON`, `JAN`) are not supported.

Schedules are evaluated in the container's local time, so set `TZ` if the
default (UTC on a distroless image) isn't what you want. An expression that no
date can ever satisfy — `0 0 30 2 *`, or an inverted range like `50-10` — is a
startup error, not a process that silently never runs.

Occurrences of one process never overlap. The next fire time is computed after
the previous run finishes, so a run that overshoots its own schedule skips the
slots it covered rather than stacking copies; missed slots are not made up.

## Ticker semantics

A `ticker` is a `cron` that counts an interval instead of matching a wall clock —
for work that should happen "every 5 seconds", which cron's one-minute
granularity cannot express. Everything else is identical: it runs for the life of
the container, honours `run_at_start`, applies `on_failure` per occurrence, and
cannot be depended on.

```yaml
processes:
  poll-queue:
    path: /bin/poll
    type: ticker
    ticker: "@every 5sec"   # 5s after the previous run finishes
    on_failure: continue    # a failed poll shouldn't take the container down
  heartbeat:
    path: /bin/heartbeat
    type: ticker
    ticker: "@every 30s"
    run_at_start: true      # ...and once at startup, so the first beat is immediate
```

The interval is a number and a unit. Units may be spelled out (`ms`, `sec`,
`min`, `hour`) or use Go's letters (`ms`, `s`, `m`, `h`), and may be combined:
`@every 5sec`, `@every 30s`, `@every 2minutes`, `@every 1min30sec`. The `@every`
prefix is optional — `ticker: "5sec"` is the same schedule. A zero, negative, or
unparseable interval is a startup error.

Unlike cron, a ticker's period is measured from the end of the previous run, not
from a fixed grid: an interval of 5s with a run that takes 2s fires every 7s. So
a ticker never overlaps itself and never falls behind, but it also does not stay
in phase with the clock — use a `cron` when the run must land *at* a given time.

## Logging

Log level is set via `loglevel` / `SUPERVISOR_LOGLEVEL` (default `info`), and a
process can override it with its own `loglevel` (see the per-process schema).

Output format is set via `log_output_format` / `SUPERVISOR_LOG_OUTPUT_FORMAT`:

- **`console`** (default) — human-readable, each line prefixed with a bracketed
  source tag. The supervisor's own structured events use the fixed
  `[supervisor]` tag and name the subject process in a field:

  ```
  [supervisor] 20:11:08 DBG process registered process_name=db path=/bin/postgres
  [supervisor] 20:11:08 DBG process registered process_name=api path=/bin/api
  ```

  A child process's raw output is tagged with the process name, padded to the
  width of the longest name so tags line up (`[a   ]` alongside `[abcd]`). The
  `[supervisor]` tag does not count toward that alignment width.

- **`json`** — structured output; the source is carried as a `process` field
  instead of a prefix. The supervisor's own events have `process=supervisor`
  and name the subject process in `process_name`:

  ```json
  {"level":"debug","process":"supervisor","process_name":"api","path":"/bin/api","message":"process registered"}
  ```

  A child process's own output is wrapped in a `{process, ...}` envelope. If the
  line is itself a JSON object or array it's embedded under `output` (json in
  json); otherwise it's a string `message`:

  ```json
  {"process":"api","output":{"lvl":"info","msg":"up"}}
  {"process":"api","message":"plain startup line"}
  ```
