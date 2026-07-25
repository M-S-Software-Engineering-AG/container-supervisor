# Container Supervisor

Container Supervisor lets you run multiple processes inside a container. The can depend on another or run in parallel.

## Motivation

Containers are happiest running a single process, but real workloads rarely fit
that cleanly. A small preparation script — seeding a database, generating config,
warming a cache — doesn't warrant its own container and orchestration just to run
before the main process. A periodic job that belongs next to a service shouldn't
have to become a separate cronjob-per-container.

Container Supervisor covers those cases: run the prep step as a `one_shot` that
the main service `depends_on`, or schedule recurring work as a `cron` process —
all inside the one container, without pulling in an init system or a second
orchestration layer.

## Compared to supervisord

[supervisord](http://supervisord.org/) is the established, feature-rich process
control system, and for most non-container use it does more: a web UI, an XML-RPC
control interface, log rotation, process groups, and runtime `supervisorctl`
management. If you need those, use it.

Container Supervisor deliberately does less. It is a single static binary that
runs on distroless images with no shell, Python runtime, or package manager —
where supervisord's Python dependency doesn't fit. Beyond the minimal footprint,
its focus is process orchestration for the lifetime of a container: a dependency
graph (`depends_on`), one-shot vs. service vs. cron process types, and per-process
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
alone. `arguments` is a list; from an env var it's split on whitespace
(`ARGUMENTS="--dry-run --limit 10"`), so use the YAML list form when an argument
must itself contain a space.

### Schema

Top-level keys:

| Key | Type | Used for |
| --- | --- | --- |
| `loglevel` | string | Log verbosity: `trace`, `debug`, `info` (default), `warn`, `error`. |
| `log_output_format` | string | `console` (default) or `json`. See [Logging](#logging). |
| `hide_labels` | bool | Default for every process's `hide_label` (below). Defaults to `false`. |
| `processes` | map | Process name → process definition (below). |

Per-process keys (`processes.<name>.*`):

| Key | Type | Required | Used for |
| --- | --- | --- | --- |
| `path` | string | yes | Path to the executable to run. |
| `type` | string | yes | `service` (long-running; exiting at all aborts the run), `one_shot` (expected to finish; exit 0 is clean), or `cron` (runs on a schedule). |
| `arguments` | list | no | Args passed to the executable verbatim. |
| `environment` | map | no | Env vars added on top of the supervisor's own environment, overriding inherited keys of the same name. |
| `cron` | string | for `cron` | Standard 5-field cron expression (`minute hour day-of-month month day-of-week`), validated at load. |
| `on_failure` | string | no | What to do when the process ends unexpectedly (see below). |
| `hide_label` | bool | no | Overrides the global `hide_labels` for this process. Drops the process name from its output: no `[name] ` prefix in console (lines start at column 0), in json a line that is already json is passed through untouched, anything else becomes a bare `{"message": ...}`. Defaults to `false`. |
| `depends_on` | map | no | Gate startup on upstream outcomes. Maps an upstream process name → `{ exit: <outcome> }`, where `<outcome>` is `success` (exit 0), `failure` (non-zero), or `any` (exited at all). The upstream may not be a `service` — it is not expected to exit, so the dependent would never start. |

`on_failure` values depend on the process type:

| Type | Allowed values |
| --- | --- |
| `service` | `exit` (default, aborts the run), `restart` (rerun until the run is cancelled), `continue` (tolerate). |
| `one_shot` / `cron` | `fail` (default, aborts the run), `continue` (tolerate), `retry <count>` (rerun up to count times, then fail). |

## Dependency semantics

`depends_on` gates a process on upstream processes exiting with the required
`exit` outcome (`success`, `failure`, or `any`) — for init/migration steps that
must finish before dependents start. Processes with no dependencies start
immediately and run in parallel. If an upstream exits with an outcome that does
not satisfy the condition, the dependent is skipped (never started).

Any process type may declare `depends_on`, so a `service` can wait on a
migration `one_shot`. What an upstream may *be* is restricted instead: gating
waits for the upstream to exit, and a `service` is not expected to, so
depending on one would park the dependent for the life of the container.
That is rejected at startup.

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

## Logging

Log level is set via `loglevel` / `SUPERVISOR_LOGLEVEL` (default `info`).

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
