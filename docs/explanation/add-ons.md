# Add-ons

Agent Kourier talks to every agent in one way: streaming A2A, through an AgentBackend. Some agents people want behind a
Binding do not serve A2A, and some teams want an agent to start from.

An add-on is an optional component, kept and released in this repository, that does a job which does not belong in the
broker. One add-on puts an A2A endpoint in front of an agent that has none, and another is an agent of its own that
serves A2A. A Binding reaches either like any other agent.

## What makes a component an add-on

- **A module of its own.** An add-on is a Go module under `addons/` with its own `go.mod`, so its dependencies, such as
  a vendor's SDK, never enter the broker's module or its binary. The code the add-ons share is one more module in the
  same repository, `addons/kit`. Each add-on requires it through a `replace` directive to that directory, so an add-on
  is always built with the kit of its own commit. The kit has no version tags and no image of its own: a release's
  add-ons are built with the kit of the release's commit.
- **A public contract with the broker, and nothing else.** The broker does not import an add-on. An add-on that bridges
  to an agent, or is one, serves A2A, and a Binding reaches it through an AgentBackend with the `a2a` dialect, as it would reach
  any A2A agent. Any other A2A client can call it too.
- **Released with Agent Kourier.** An add-on's image is built, scanned, given its SBOM and provenance attestations,
  and signed by the same release workflow as the broker's, under the same version tag. The chart of that release pins
  the add-on's image by digest, as it pins the broker's.
- **Deployed by the chart, off by default.** Each add-on has a block under `addons` in the Helm chart, turned on with
  `enabled: true`. It runs as a Deployment of its own, named `<fullname>-<add-on>`, with its own Service and
  ServiceAccount. Its objects carry `app.kubernetes.io/name: agent-kourier-<add-on>`, never the broker's
  `agent-kourier`, so nothing that selects the broker's pods selects an add-on's.
- **Checked in CI like the broker.** Its tests (with the race detector, and on Postgres where it has a store), its lint,
  `govulncheck` and a scan of its image for each platform are checks that a release requires to have passed.
- **Documented with its limits.** Each add-on has a how-to, a reference page, and a section in
  [Limits](../reference/limits.md#add-ons) with its own limits, beside [those every add-on
  shares](../reference/limits.md#every-add-on).

## What the add-ons share: the kit

An add-on is an A2A agent in front of a backend, and most of such a process has nothing to do with the backend. The kit,
`addons/kit`, holds that part once, and each add-on supplies the rest: an A2A executor that runs its turns, the hooks it
needs, its own settings, and its store's own tables, if it keeps anything beside the tasks.

| Package | What it holds for every add-on |
|---|---|
| `runtime` | The command and its one flag, `--version`; the A2A endpoint and the agent card; the replay of a resent message, which joins its first send's task instead of starting a second turn; the check of a message before its turn; the takeover of a new leader, which fails the turns a restart cut off; the upkeep that deletes old tasks and releases idle contexts; and the order of the stop |
| `settings` | The variables every add-on reads, and an add-on's own, with their defaults and refusals |
| `store` | The tasks, the IDs of the messages that started them and each context's last activity, on SQLite (one replica) or Postgres (several), with the fence that every write of a leader runs first. It links no SQLite driver: each add-on brings its own |
| `cluster` | The leader election through a lease row in the store, and a standby's forwarding to the leader |
| `auth`, `auth/oidc` | The caller check: a token file, or OIDC for an add-on that offers it, apart so that only that add-on links an OIDC library |
| `telemetry` | The metrics, traces and JSON log, built as Agent Kourier's are |
| `kittest` | The cases every add-on runs, under the runtime on each store, and `kittest/echo`, the reference add-on they run in the kit |

So the add-ons behave alike wherever the kit decides: the same settings, the same task store, readiness and stop, and
the same limits ([Every add-on](../reference/limits.md#every-add-on)). What differs is the backend: what a turn runs,
what a pause waits for, and what an idle context loses.

The broker does not use the kit, and the kit does not use the broker. The broker's packages are internal to its module,
which another module cannot import. Having the broker import the kit would put the add-ons' code and its dependencies
in the broker's module.

The kit's telemetry is built as the broker's is. [Write an add-on on the kit](../how-to/write-an-add-on.md) builds one, step by step.

## Supported add-ons

| Add-on | What it does | Chart block | Pages |
|---|---|---|---|
| Claude Managed Agents | An A2A server in front of one agent in Claude Managed Agents, Anthropic's hosted agent runtime. A thread is one session of the agent, and a tool call whose permission policy asks first waits on Approve and Deny in the thread. Experimental: tested against a self-hosted environment with a Haiku agent, and, on a build before the release (22 and 23 November 2026), on Postgres with two replicas, where a leader killed during an approval left the standby to take over and complete the same task; cloud environments are untested. | `addons.cma` | [Connect Claude Managed Agents](../how-to/connect-claude-managed-agents.md), [Claude Managed Agents add-on](../reference/claude-managed-agents.md) |
| AX | Launches an AX Task for each accepted turn and returns the wrapped command's result over A2A. The Task template selects the image, Workspace, and Gateway. | `addons.ax` | [Connect the AX add-on](../how-to/connect-ax.md), [AX add-on](../reference/ax.md) |

The 0.6.0 chart has no `addons` block, and its values schema refuses one: use the chart of a release whose
[release notes](../release-notes.md) list the add-on.

## Why a component and not a backend type

The add-on for Claude Managed Agents could have been a backend type inside the broker, which would reuse the broker's
store, sessions and leader election and save a network hop. It would also have put the Anthropic SDK in the broker's
module and a second agent protocol beside A2A, for one vendor.

With an add-on, the broker keeps one contract, A2A ([Why A2A](why-a2a.md)). The price is the hop and a store of the
add-on's own, which keeps its tasks and sessions across a restart.

The AX add-on is an agent, so it was never a candidate for a backend type. It is an add-on, and not an example, so that
it is released, scanned and deployed like the broker: the chart runs it, and a release pins its image.
