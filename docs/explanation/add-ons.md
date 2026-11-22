# Add-ons

Agent Kourier talks to every agent in one way: streaming A2A, through an AgentBackend. Some agents people want behind a
Binding do not serve A2A, and some teams want an agent to start from. An add-on is an optional component, kept and
released in this repository, that does a job which does not belong in the broker. One add-on puts an A2A endpoint in
front of an agent that has none; another is an agent of its own that serves A2A. A Binding reaches either like any other
agent.

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
  [Limits](../reference/limits.md#add-ons) with every limit it has.

## Supported add-ons

| Add-on | What it does | Chart block | Pages |
|---|---|---|---|
| Claude Managed Agents | An A2A server in front of one agent in Claude Managed Agents, Anthropic's hosted agent runtime. A thread is one session of the agent, and a tool call whose permission policy asks first waits on Approve and Deny in the thread. Experimental: tested against a self-hosted environment with a Haiku agent; cloud environments are untested. | `addons.cma` | [Connect Claude Managed Agents](../how-to/connect-claude-managed-agents.md), [Claude Managed Agents add-on](../reference/claude-managed-agents.md) |
| AX | An agent on Amazon Bedrock or the Anthropic API, built with Google's Agent Development Kit (ADK), that serves A2A. A thread is one ADK session, and a question the agent asks with its one tool, `ask_user`, waits for a reply in the thread. The chart runs it beside Agent Kourier, and it can run inside Google AX as a Task. Experimental: tested in kind through the chart with a Haiku model on the Anthropic API, on SQLite; Bedrock is tested against a mock of its API, and its routes through an IAM role (IRSA, EKS Pod Identity) have not been tested on EKS. | `addons.ax` | [Connect the AX add-on](../how-to/connect-ax.md), [AX add-on](../reference/ax.md) |

The 0.6.0 chart has no `addons` block, and its values schema refuses one: use the chart of a release whose
[release notes](../release-notes.md) list the add-on.

## Why a component and not a backend type

The add-on for Claude Managed Agents could have been a backend type inside the broker, which would reuse the broker's
store, sessions and leader election and save a network hop. It would also have put the Anthropic SDK in the broker's
module and a second agent protocol beside A2A, for one vendor. As an add-on, the broker keeps one contract, A2A
([Why A2A](why-a2a.md)), and the price is the hop and a store of the add-on's own, which keeps its tasks and sessions
across a restart.

The AX add-on is an agent, so it was never a candidate for a backend type. It is an add-on, and not an example, so that
it is released, scanned and deployed like the broker: the chart runs it, and a release pins its image.
