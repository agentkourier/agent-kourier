# Add-ons

Agent Kourier talks to every agent in one way: streaming A2A, through an AgentBackend. Some agents people want behind a
Binding do not serve A2A. An add-on is an optional component, kept and released in this repository, that does a job
which does not belong in the broker. The add-on there is today puts an A2A endpoint in front of such an agent, so that a
Binding reaches it like any other.

## What makes a component an add-on

- **A module of its own.** An add-on is a Go module under `addons/` with its own `go.mod`, so its dependencies, such as
  a vendor's SDK, never enter the broker's module or its binary.
- **A public contract with the broker, and nothing else.** The broker does not import an add-on. An add-on that bridges
  to an agent serves A2A, and a Binding reaches it through an AgentBackend with the `a2a` dialect, as it would reach
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
  [Limits](../reference/limits.md#add-ons) with every limit it has.

## Supported add-ons

| Add-on | What it does | Chart block | Pages |
|---|---|---|---|
| Claude Managed Agents | An A2A server in front of one agent in Claude Managed Agents, Anthropic's hosted agent runtime. A thread is one session of the agent, and a tool call whose permission policy asks first waits on Approve and Deny in the thread. Experimental: tested against a self-hosted environment with a Haiku agent; cloud environments are untested. | `addons.cma` | [Connect Claude Managed Agents](../how-to/connect-claude-managed-agents.md), [Claude Managed Agents add-on](../reference/claude-managed-agents.md) |

The 0.6.0 chart has no `addons` block, and its values schema refuses one: use the chart of a release whose
[release notes](../release-notes.md) list the add-on.

## Why a component and not a backend type

The add-on for Claude Managed Agents could have been a backend type inside the broker, which would reuse the broker's
store, sessions and leader election and save a network hop. It would also have put the Anthropic SDK in the broker's
module and a second agent protocol beside A2A, for one vendor. As an add-on, the broker keeps one contract, A2A
([Why A2A](why-a2a.md)), and the price is the hop and a store of the add-on's own, which keeps its tasks and sessions
across a restart.
