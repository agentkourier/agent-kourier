# Write an add-on on the kit

For a contributor who wants a new agent, or a bridge to one, behind a Binding: at the end, an add-on in this repository
serves A2A in front of your backend, and the kit's tests drive it end to end on SQLite and on Postgres. The add-on's
own code is its backend; the kit (`addons/kit`) runs the rest of the process.

[Add-ons](../explanation/add-ons.md) says what an add-on is and what the kit holds. The kit is a module of this
repository, not one you can fetch: an add-on lives under `addons/` beside the two there are, and requires the kit
through a `replace` directive.

The code on this page is the kit's reference add-on, echo (`addons/kit/kittest/echo`), included from its source. It
answers each message with its own text, asks a question when a message says `pause`, and runs until it is canceled when
one says `hold`.

## What the kit does, and what you write

The kit's runtime owns the command and its flag (`--version`), the settings, the telemetry, the caller check, the store,
the replay of a resent message, the leader election and a standby's forwarding, the leader's takeover, the upkeep of the
store, and the order of the stop ([Every add-on](../reference/limits.md#every-add-on) has its limits). You write:

- **An `Addon`**: the add-on's names, its own settings, its agent card, how its store opens, and how its backend is
  built.
- **A backend**: an `a2asrv.AgentExecutor` from a2a-go, whose `Execute` runs one turn, plus the hooks it needs.
- **Its tests**: a `kittest.Subject` that tells the kit's cases which messages complete, pause and hold a turn.

## 1. Make the module

Make the directory `addons/<name>`, a Go module of its own, and require the kit from the directory beside it, as the
two add-ons' `go.mod` files do:

```text
require github.com/agentkourier/agent-kourier/addons/kit v0.0.0-00010101000000-000000000000

replace github.com/agentkourier/agent-kourier/addons/kit => ../kit
```

The name is lower-case letters and digits, a letter first, which the kit's telemetry refuses otherwise: it becomes the
metrics' prefix (`agentkourier_<name>_`), the spans' (`agentkourier.<name>.`), the service name
(`agent-kourier-<name>`), the default SQLite file and the chart's block.

## 2. Describe the add-on

`runtime.Addon` is the add-on. `Name` and `Noun` are required: the noun is what the add-on's messages call its process,
such as "agent listening" and "Interrupted by an agent restart.". `Open` opens the store and `Build` makes the backend
once it is open; both are required too.

```go
--8<-- "echo.go:addon"
```

`Telemetry` is the help of the metrics whose words are the add-on's: the outcomes of its turns, the operations of its
backend and the reasons its caller check refuses. `Card` is the name and description the agent card shows, with any A2A
extension the add-on speaks; the kit fills in the endpoint (`PUBLIC_URL`) and the version. `OIDC`, if set, builds the
verifier of an OIDC caller check (package `auth/oidc`), as the AX add-on's does; without it the add-on does not read the
`OIDC_*` variables.

Echo has no command of its own. An add-on's `main` is one line, `runtime.Main`, with the version set at build time, as
in `addons/ax/cmd/agent/main.go`:

```go
// version is set at build time: -ldflags "-X main.version=<value>".
var version = "dev"

func main() { runtime.Main(addon()) }
```

It exits with 0 once a stop has finished, 2 for a setting it cannot use (with one log line, `configuration`, that names
the variable), 1 for a failure once it runs, and 5 when a replica on Postgres loses the lease.

## 3. Declare the add-on's own settings

The kit reads the variables every add-on shares (each add-on's reference lists them:
[Claude Managed Agents](../reference/claude-managed-agents.md#every-add-ons), [AX](../reference/ax.md#every-add-ons)). An add-on's
own are `settings.Var` values in `Addon.Settings`, read in order before the shared ones:

```go
--8<-- "echo.go:setting"
```

- `settings.Required(key)` is a variable that must be set; `settings.Duration(key, def)` a Go duration of zero or more,
  `def` when unset; `settings.Func(key, def, parse)` anything else. `parse` gets the value, or `def`, and the values of
  the variables before it.
- The error of `parse` is the whole message: name the variable in it, and never show a value that may be a secret.
- A variable may not have the name of a shared one, or of another of the add-on's: the add-on does not start.
- `Build` reads each value with the variable's `From`, as echo's `Build` reads `greeting`. `From` of a variable that is
  not in `Addon.Settings` is its zero value, so the runtime refuses to start an add-on whose `Build` read one ("the
  agent read settings it does not declare"), and each `kittest` case fails when the add-on read one at any time.

An empty value is an unset one, for the add-on's variables as for the shared ones.

## 4. Open the store

The kit's store keeps the A2A tasks, the IDs of the messages that started or joined them, and when each context was
last active, on SQLite or on Postgres. Every write of a leader on Postgres checks its lease in the same transaction, so
a leader that has lost the lease cannot write. Echo's store is the kit's task state alone:

```go
--8<-- "echo.go:open"
```

- **`store.Options`.** `Noun` names the process in the store's messages ("another agent has the SQLite file open").
  `MigrationLockKey` is the Postgres advisory lock under which replicas that start together migrate one at a time: give
  the add-on a key of its own, since `OpenPostgres` refuses 0. Pass the runtime's `Log` and `OnDeposed` through.
  `Prepare` runs before the kit's migrations, for an add-on that must refuse or set aside a layout an earlier build of
  it wrote.
- **`store.TaskOptions`.** With `ContextActivity`, each create and update of a task marks its context active, as echo
  and the AX add-on do; without it, a context's activity is what the runtime marks when it accepts a message and what
  the add-on records itself, as the Claude Managed Agents add-on does when a turn starts or ends. `ReleaseContext` runs
  in the transaction that deletes an idle context, for what the add-on keeps of it in the same database (the AX
  add-on's ADK session).
- **Tables of the add-on's own.** Embed `*store.Store` in a type of the add-on's, apply its migrations with
  `DB.Migrate` (files named `NNNN_name.sql`, after the kit's), and write them through `DB.Write`, which runs the lease's
  check first.
- **The SQLite driver.** The kit opens the `database/sql` driver named `sqlite` and links none, since the two drivers
  the add-ons use both register that name and cannot be linked together. The add-on imports one: the Claude Managed
  Agents add-on imports `modernc.org/sqlite`, and the AX add-on `github.com/glebarez/sqlite`, which ADK's sessions need.

## 5. Write the backend

The backend is an `a2asrv.AgentExecutor`. Each `Execute` is one turn, which the kit counts and traces; a message whose
ID the store has recorded joins its first send's task and never reaches the backend.

```go
--8<-- "echo.go:backend"
```

```go
--8<-- "echo.go:execute"
```

- A turn of a new task yields the submitted task first: a2a-go stores the task, and with it the message's ID, at that
  event. Then the turn's updates, its artifacts, and a last status: `completed`, `failed`, `canceled`, or
  `input-required` to wait for an answer, which comes as the next message on the same task (`ec.StoredTask`).
- The turn's context ends when the add-on stops. A turn cut short leaves its task as it was, and the next leader fails
  it with "Interrupted by an agent restart." (the noun is the add-on's). A paused task is kept.
- `Cancel` ends a task that a caller cancels: echo's yields `canceled`.

### The hooks

The kit finds each hook by a type assertion on the backend. Assert each one you mean to implement, as echo does, so
that a changed signature fails the build instead of dropping the hook; a backend that wraps another must expose the
inner one's hooks itself.

| Hook | Implement it when | Used by |
|---|---|---|
| `MessageValidator` | The backend cannot take some messages. `ValidateMessage` checks a new message, or an answer to a paused task, before a turn starts; its error refuses the request as invalid parameters, the message is not counted as a turn, its ID is not recorded, and a paused task stays paused. | Echo, both add-ons |
| `Restorer` | The backend keeps something outside the store that the leader must reconcile when it takes over, before the interrupted turns are failed. Its error stops the takeover. | Claude Managed Agents (its sessions) |
| `Interrupter` | The backend must act on the tasks a takeover failed, before the leader serves. | Claude Managed Agents (interrupts their sessions) |
| `ContextReleaser` | The backend keeps a context outside the store. The upkeep calls `ReleaseContext` for each context idle past `SESSION_IDLE_TTL` with no turn running, then forgets the context and its tasks. An error leaves the context for the next sweep; wrap `runtime.ErrContextBusy` when a turn of it has begun since the store listed it. Without the hook, the store deletes idle contexts in its own transaction. | Claude Managed Agents (archives the session) |
| `IdleReporter` | The add-on counts what the store's own deletion of idle contexts removed. | AX (sessions deleted) |
| `TurnClassifier` | Some failed turns have an outcome of their own in the turns metric. | Claude Managed Agents (`timeout`) |
| `Describer` | The `listening` log line should report settings of the add-on's own. | Both add-ons |
| `Readiness` | Another server answers for the add-on's readiness: `/readyz` is its handler. | AX, beside the AX task runner |
| `Routes` | The add-on serves routes of its own beside the kit's. | AX: `/metadata/`, proxied to the AX task runner |

```go
--8<-- "echo.go:validate"
```

## 6. Test it with kittest

`kittest` runs the add-on under the kit's runtime on a loopback port and drives it as Agent Kourier does. A
`kittest.Subject` names the add-on and the messages that drive its backend; its `Build` should make a backend with no
real service behind it (a scripted model, a fake API):

```go
--8<-- "echo.go:subject"
```

`BadAnswer` is an answer to the pause that the backend refuses, and `BadMessage` a new message it refuses; a nil
`BadMessage` skips that case, for a backend that accepts every new message. `AnswerMetadata` is for a backend that reads
an answer from the message's metadata, as the Claude Managed Agents add-on reads a tool approval.

Run the cases from a test of the add-on's, which links its SQLite driver:

```go
func TestTheKitCases(t *testing.T) {
	t.Run("SQLite", func(t *testing.T) { kittest.RunSQLite(t, subject()) })
	t.Run("Postgres", func(t *testing.T) { kittest.RunPostgres(t, subject()) })
}
```

The cases check that a resent message joins its task, that a refused answer keeps the task paused and a refused message
starts no turn, that a cancel ends the turn canceled, that a start fails the turns a restart cut off, that a stop leaves
a running turn for the next start, and that an idle context is released. On Postgres they add two replicas: a standby
forwards with its hop header and takes over a paused task, a leader settles before it serves, and a leader whose write
is deposed, or that loses the lease, exits. The Postgres cases skip unless `AGENTKOURIER_POSTGRES_DSN` names a database;
`hack/postgres/run.sh` starts a throwaway one for a command:

```sh
make kit-test                                   # the kit, with echo on Postgres when the DSN is set
hack/postgres/run.sh make addon-test-<name>     # the add-on, with the kit's packages under its module graph
make addon-lint-<name>
```

## 7. Wire it into the repository

- **The list.** Add the name to `hack/addons.txt`. The Makefile's `addon-test-<name>`, `addon-lint-<name>` and
  `addon-image-<name>` targets, with their aliases `<name>-test`, `<name>-lint` and `<name>-image`, the release scripts
  and the workflows' add-on legs follow the list, and a test holds the workflows to it. A name whose alias the Makefile
  already gives to a rule of its own, such as `release` (`release-image`), fails `hack/addons_test.sh`, which
  `make test-scripts` runs. The image builds from `addons/<name>/Dockerfile` with the context `addons/`, which holds every
  add-on; `addons/<name>/Dockerfile.dockerignore`, which BuildKit reads in place of the context's own, lets in only the
  kit and the add-on.
- **The chart.** An add-on has:
    - a values block, `addons.<name>`, with the keys every add-on has and its own;
    - a block for it in `values.schema.json`, whose `addons` refuses any key it does not list, so Helm refuses a values
      block that has no schema; the common keys take the same rules as the other add-ons';
    - its name in the list that `templates/addons/_addon.tpl` keeps;
    - a directory `templates/addons/<name>/`: `env.tpl` defines what is its own, `deployment.yaml` defines its startup
      probe and renders its Deployment, and `service.yaml`, `serviceaccount.yaml`, `pvc.yaml`,
      `poddisruptionbudget.yaml`, `networkpolicy.yaml` and `servicemonitor.yaml` are one line each that includes the
      common template of that kind.

    The comment at the top of `_addon.tpl` lists each define. The chart's tests in `test/chart` cover each add-on of
    their `addOns` list, and a test fails when that list, the chart's list, the template directories, the values blocks
    and the schema's blocks name different add-ons.

- **The docs.** A how-to, and a reference page whose settings include the shared table:

    ```text
    ;--8<-- ".addon-settings.md:settings"
    ```

    A section in [Limits](../reference/limits.md#add-ons) holds every limit that is the add-on's own, each value that
    is a constant written in a code span followed by a marker `<!--addon-limit:<name>.<id>-->`. A test in the package
    that holds the constant calls `limitsdoc.Count`, `Bytes`, `Duration` or `Text` with the ID, and one test of the
    module calls `limitsdoc.EveryMarkerHasAGuard(t, "<name>.")`. The kit's limits carry `kit.` markers, which the kit's
    tests hold.
