# Your first agent in Slack

For anyone who wants to see Agent Kourier work before installing it: at the end, a read-only SRE agent answers your
question in a Slack thread, follows up in the same thread, and asks you a question of its own.

You run everything on your machine in a kind cluster. The agent is `sandbox-sre`, a kagent agent on a real model,
looking at six small Deployments with faults seeded into them. It can read the cluster and cannot change it.

## What you need

- macOS or Linux, with Docker running and enough room for one kind cluster.
- `git`, `make`, `kind`, `kubectl`, Helm 4, `jq`, `curl` and `openssl` on your `PATH`.
- A Slack workspace where you may create apps. Use a sandbox workspace, not your company's.
- An API key for one model provider: Anthropic, OpenAI, or Amazon Bedrock.

## 1. Get the source

```sh
git clone https://github.com/agentkourier/agent-kourier.git
cd agent-kourier
```

Run every command below from this directory.

## 2. Create the Slack app

1. Open <https://api.slack.com/apps>, choose **Create New App**, then **From a manifest**, and pick your sandbox
   workspace.
2. Open the **YAML** tab and paste the contents of
   [`hack/kind/sandbox/slack-app-manifest.yaml`](https://github.com/agentkourier/agent-kourier/blob/main/hack/kind/sandbox/slack-app-manifest.yaml).
   Create the app. The manifest names the app and its bot user **Agent Kourier**.
3. Under **Basic Information**, **App-Level Tokens**, choose **Generate Token and Scopes**, add the scope
   `connections:write`, and generate it. Copy the token. It starts with `xapp-`.
4. Under **Install App**, install the app to the workspace. Copy the **Bot User OAuth Token**. It starts with `xoxb-`.
5. Create a channel for the tutorial. In it, send `/invite @Agent Kourier`.
6. Open the channel's details. At the bottom is the channel ID, which starts with `C`. Copy it.

## 3. Write the secrets file

```sh
mkdir -p ~/.config/agent-kourier
cp hack/kind/sandbox/sandbox.env.example ~/.config/agent-kourier/sandbox.env
chmod 600 ~/.config/agent-kourier/sandbox.env
```

Open `~/.config/agent-kourier/sandbox.env` in an editor and replace every `REPLACE`:

| Key | Value |
|---|---|
| `SLACK_BOT_TOKEN` | The `xoxb-` token from step 2. |
| `SLACK_APP_TOKEN` | The `xapp-` token from step 2. |
| `SLACK_CHANNEL_ID` | The channel ID from step 2. |
| `MODEL_PROVIDER` | `anthropic`, `openai` or `bedrock`. |
| `MODEL_NAME` | A model name of that provider. For `bedrock`, a model ID or inference profile ID. |
| `MODEL_API_KEY` | The provider's API key. For `bedrock`, a Bedrock API key. |
| `MODEL_REGION` | Only for `bedrock`: remove the `#` and set the region, for example `us-east-1`. |

Check the file:

```sh
hack/kind/sandbox/check-env.sh
```

It prints `ok` and the names of the keys. It never prints a value.

## 4. Bring up the sandbox

```sh
make sandbox-up
```

The first run creates the kind cluster `agent-kourier-full` with kagent, Dex and a verifying front door, builds the
Agent Kourier image, and installs the sandbox on top. Later runs reuse the cluster.

Wait for this line:

```text
ok: Agent Kourier is connected to Slack
```

## 5. Ask the agent

In your channel, mention Agent Kourier and ask about one of the seeded faults:

```text
@Agent Kourier why is checkout crashlooping?
```

A thread opens under your message. The answer streams into it, with a step card for each tool call the agent makes,
such as `k8s_get_pod_logs`. The answer quotes the `FATAL: cannot resolve payments-db` line from the pod's log and says
what the agent checked.

## 6. Follow up in the thread

Reply in the same thread, without a mention:

```text
and which pods does that affect?
```

The answer arrives in the same thread and builds on the first one.

## 7. Let the agent ask you

Start a new message in the channel:

```text
@Agent Kourier ask me which app to look at, then look at it
```

The thread shows the agent's question with its choices and a **Submit** button. Choose an app and submit. The agent
carries on with your answer.

## 8. Clean up

```sh
make sandbox-down
```

This removes the sandbox and keeps the kind cluster for next time. To remove the cluster too:

```sh
make e2e-full-down
```

Then, in the Slack app's settings, revoke the two tokens, and delete `~/.config/agent-kourier/sandbox.env`.

## What you did

You ran Agent Kourier against a real Slack workspace and a real model, started an agent session from a mention,
continued it from a thread reply, and answered the agent's question in the thread.

Next:

- [Your first alert investigation](first-alert-investigation.md) starts a session from an alert instead of a mention.
- [Threads and sessions](../explanation/threads-and-sessions.md) explains what happened in the thread.
- [Install with Helm](../how-to/install-with-helm.md) runs Agent Kourier on your own cluster.
