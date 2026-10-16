# Reference

For looking things up: these pages describe Agent Kourier's resources, settings, metrics and stored data, completely
and without instructions. The resource and Helm value pages are generated from the source, so they match the code.

## Resources

API group `agentkourier.dev/v1alpha1`. All three kinds are namespaced. They are loaded from YAML
files, usually a mounted ConfigMap, rather than served as custom resources.

| Kind | Configures | Typical owner |
|---|---|---|
| [Binding](binding.md) | An agent, its service identity, and the channel connected to it | Application team |
| [AgentBackend](agentbackend.md) | The agent endpoint, its dialect, and the namespaces allowed to use it | Platform team |
| [ChatConnection](chatconnection.md) | The chat platform connection and its Secret | Platform team |

## Agents

- [Agent contract](agent-contract.md): what an agent must do beyond speaking A2A: methods, message shape, replies,
  task states and errors.

## Configuration

- [Helm values](helm-values.md): every value of the chart, with its default.
- [Command line and environment](cli.md): the binary's environment variables, flags, subcommand and exit codes.
- [Chat triggers](chat-triggers.md): what starts a turn, and what the `botMessage` trigger does.

## Operation

- [Metrics](metrics.md): every Prometheus metric.
- [Logs and traces](logs-and-traces.md): trace settings, span attributes and what a log line carries.
- [Stored data](stored-data.md): what is kept, for how long, and what never is.
- [Glossary](glossary.md): the terms these pages use.
