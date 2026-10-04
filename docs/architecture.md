# Architecture

Courier connects declarative messaging bindings to agents deployed with a chosen
runtime. The runtime hosts and executes the agents. Courier owns the messaging
connection, conversation lifecycle, and delivery state.

The [README](../README.md#how-it-works) shows the basic idea. This diagram expands
the current broker and marks the Google ax integration target separately.

```mermaid
flowchart TB
    Git["Git repository<br/>AgentBackend · ChatConnection · Binding<br/>Runtime deployment manifests"]
    GitOps["GitOps deployment"]
    Slack["People in Slack<br/>Messages, thread replies, question answers"]

    subgraph K8s["Kubernetes"]
        Files["Mounted configuration YAML<br/>ConfigMap"]
        Secrets["Kubernetes Secrets<br/>Slack credentials + Binding identity"]

        subgraph Broker["Courier broker · one active ChatConnection per process"]
            Config["Configuration loader + reloader<br/>Validate references and namespace access"]
            Identity["Secret resolver + Binding identity"]
            Chat["Slack adapter<br/>Socket Mode intake + outbound API calls"]
            Intake["Chat intake<br/>Choose Binding and handle thread controls"]
            Session["Session manager<br/>Durable reply queue · serial turns · recovery"]
            A2A["Agent client pool + dialect<br/>Send · follow · get · cancel"]
            Interact["Interaction handling<br/>Persist questions and presses · validate answers"]
            Render["Renderer<br/>Stream text, render questions, finalize output"]
            Outbox["Durable outbox<br/>Post, adopt, retry, record delivery"]
            Telemetry["Prometheus metrics + audit logging<br/>Optional OTLP traces"]
        end

        DB[("SQLite on persistent storage<br/>Sessions · replies · interactions · presses<br/>Outbox · audit · trace context")]

        subgraph Runtime["Agent runtime · managed independently"]
            Frontdoor["Authenticated A2A endpoint"]
            Agent["kagent-hosted or other A2A agent"]
            Tools["Model, tools, and execution environment"]
            Ax["A2A-facing integration + Google ax<br/>Integration target; no native Courier adapter"]
        end
    end

    Observability["Metrics scraper + trace collector"]

    Git --> GitOps
    GitOps --> Files
    GitOps --> Secrets
    GitOps -->|Runtime deployment| Agent
    GitOps -.->|Future integration deployment| Ax
    Files --> Config
    Secrets --> Identity
    Config -.->|Routing snapshot| Intake
    Config -.->|Backend and Binding settings| A2A
    Identity -.->|Service credentials| A2A
    Identity -.->|Slack credentials| Chat

    Slack <-->|Events, acknowledgements, output| Chat
    Chat -->|Inbound messages| Intake
    Intake -->|Queue a turn| Session
    Intake -->|Human answer or button press| Interact
    Session <-->|Task lifecycle and events| A2A
    A2A <-->|A2A 1.0 JSON-RPC + streaming| Frontdoor
    Frontdoor <--> Agent
    Frontdoor -.->|A2A integration required| Ax
    Agent <--> Tools

    Session -->|Task output and pauses| Render
    Session -->|Pause handoff| Interact
    Interact -->|Question or interaction card| Render
    Interact -->|Validated answer through same session| Session
    Render -->|Stream and edit| Chat
    Render -->|Durable posts and notices| Outbox
    Outbox -->|Deliver or find an earlier post| Chat

    Session <-->|Conversation state| DB
    Interact <-->|Interaction state| DB
    Outbox <-->|Delivery state| DB
    Telemetry -->|Audit records| DB
    Broker -.->|Operation observations| Observability

    classDef config fill:#eef2ff,stroke:#6366f1,color:#312e81
    classDef broker fill:#ecfeff,stroke:#0891b2,color:#164e63
    classDef core fill:#0f766e,stroke:#115e59,color:#ffffff
    classDef runtime fill:#ecfdf5,stroke:#059669,color:#064e3b
    classDef store fill:#f1f5f9,stroke:#475569,color:#0f172a
    classDef planned fill:#fff7ed,stroke:#ea580c,color:#9a3412,stroke-dasharray:5 5
    class Git,GitOps,Files,Secrets,Config,Identity config
    class Chat,Intake,A2A,Interact,Render,Outbox,Telemetry broker
    class Session core
    class Frontdoor,Agent,Tools runtime
    class DB,Slack,Observability store
    class Ax planned
```

Solid arrows show the implemented configuration and conversation paths. Dotted
arrows inside the broker show configuration or credential inputs; orange nodes
and their dotted deployment/runtime paths show the ax integration target.
Telemetry is shared across broker operations, rather than a required step in
message delivery. Audit records are durable in SQLite; traces are exported only
when configured.

## Configuration and deployment

The current pilot reads resource-shaped YAML files from `COURIER_CONFIG_DIR`.
GitOps deploys those files as a ConfigMap alongside Secrets, the Courier
Deployment, and persistent storage. The reloader validates changes and keeps the
last accepted configuration if a reload fails. Secret access uses Kubernetes
RBAC; cross-namespace resource references use configured namespace allowlists.

The broker selects one active `ChatConnection` per process. Multiple bindings
can route its channels to different agents and backends. The current deployment
uses SQLite; this diagram does not imply an active-active, shared-database broker.

Live Courier CRDs and a reconciliation controller are planned. Courier does not
deploy an agent when it loads an `AgentBackend` or `Binding`: the runtime's own
manifests, operator, or deployment tooling handle that lifecycle.

## One conversation

1. The messaging adapter feeds an inbound message to chat intake, which selects
   the binding and applies the channel and thread rules.
2. The session manager persists the reply and runs turns in order. The agent
   client uses the binding's service identity and backend dialect to call A2A.
3. Task output feeds the renderer. Live streams and edits go through the chat
   adapter; durable posts and notices go through the outbox.
4. When the agent needs a human answer, interaction handling persists the pause
   and renders its question. An accepted answer resumes the same agent task.
5. On restart, persisted sessions, pending interactions, and outbound rows allow
   recovery and reconciliation. Saved trace context connects asynchronous work.

Structured questions and approvals depend on the backend's interaction contract.
The generic A2A dialect supports text continuation; the kagent dialect understands
its structured HITL extension. Google ax needs an A2A-facing integration or an
A2A agent that delegates work to it. The current broker has no native ax adapter.

## Source map

| Responsibility | Source |
|---|---|
| Resource types | [`api/v1alpha1`](../api/v1alpha1) |
| Composition and shutdown | [`internal/broker`](../internal/broker) |
| Configuration and Secret resolution | [`internal/config`](../internal/config) |
| Messaging contracts and Slack adapter | [`internal/chat`](../internal/chat) |
| Conversation execution and recovery | [`internal/session`](../internal/session) |
| A2A clients and backend behavior | [`internal/agent`](../internal/agent), [`internal/dialect`](../internal/dialect) |
| Human interactions | [`internal/interact`](../internal/interact) |
| Output and durable delivery | [`internal/render/live`](../internal/render/live), [`internal/outbox`](../internal/outbox) |
| Persistence and observations | [`internal/store/sqlite`](../internal/store/sqlite), [`internal/telemetry`](../internal/telemetry), [`internal/audit`](../internal/audit) |

See the [specification](../spec.md#6-architecture) for the detailed contracts and
[observability guide](observability.md) for metrics and trace boundaries.
