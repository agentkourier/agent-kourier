# Your first agent in Slack

For anyone who wants to see Agent Kourier work before installing it: at the end, a read-only SRE agent answers your
question in a Slack thread, follows up in the same thread, and asks you a question of its own.

You run everything on your machine in a kind cluster. The agent is `sandbox-sre`, a kagent agent on a real model,
looking at six small Deployments, five of them with faults seeded into them. It can read the cluster and cannot change it.

## What you need

- macOS or Linux, with Docker running and enough room for one kind cluster.
- `git`, `make`, `kind`, `kubectl`, Helm 4, `jq`, `curl`, `openssl` and Go on your `PATH`. The bring-up builds
  test binaries with Go on your machine; use the version that `go.mod` names.
- A Slack workspace where you may create apps. Use a sandbox workspace, not your company's.
- An API key for the demo agent's model provider: OpenRouter, Anthropic, OpenAI, or Amazon Bedrock.

The model settings below configure `sandbox-sre`. Agent Kourier connects to that agent through A2A;
the agent calls the model provider you choose.

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
| `MODEL_PROVIDER` | The provider selected below. The example file starts with `openrouter`. |
| `MODEL_NAME` | The model name or slug for that provider. OpenRouter uses a `vendor/model` slug. |
| `MODEL_BASE_URL` | Optional for OpenRouter only. Omit it to use `https://openrouter.ai/api/v1`. |
| `MODEL_API_KEY` | Your API key for that provider. |

Choose one provider tab and replace the model lines in the file with its settings. Keep the Slack lines you filled in.

=== "OpenRouter"

    The example file uses this setup. Replace `REPLACE` with your OpenRouter API key, which starts with `sk-or-v1-`.

    ```dotenv
    MODEL_PROVIDER=openrouter
    MODEL_NAME=anthropic/claude-haiku-4.5
    # Optional; omit this line to use the default.
    MODEL_BASE_URL=https://openrouter.ai/api/v1
    MODEL_API_KEY=REPLACE
    ```

    `MODEL_BASE_URL` is accepted only with `MODEL_PROVIDER=openrouter`. If you set it, use an HTTPS endpoint without
    credentials, a query, or a fragment; the sandbox validates that shape before it touches the cluster. You can select
    another model using its `vendor/model` slug. See [OpenRouter's model page](https://openrouter.ai/anthropic/claude-haiku-4.5)
    and [OpenAI-compatible API configuration](https://openrouter.ai/docs/guides/community/openai-sdk).

=== "Anthropic"

    Set `MODEL_NAME` to the Anthropic model you want to use and `MODEL_API_KEY` to your Anthropic API key,
    which starts with `sk-ant-`.

    ```dotenv
    MODEL_PROVIDER=anthropic
    MODEL_NAME=REPLACE
    MODEL_API_KEY=REPLACE
    ```

=== "OpenAI"

    Set `MODEL_NAME` to the OpenAI model you want to use and `MODEL_API_KEY` to your OpenAI API key.
    This choice uses OpenAI's own endpoint.

    ```dotenv
    MODEL_PROVIDER=openai
    MODEL_NAME=REPLACE
    MODEL_API_KEY=REPLACE
    ```

=== "Amazon Bedrock"

    Set `MODEL_NAME` to a Bedrock model ID or inference profile ID and `MODEL_API_KEY` to a Bedrock API key.
    Set `MODEL_REGION` to the region where you access the model.

    ```dotenv
    MODEL_PROVIDER=bedrock
    MODEL_NAME=REPLACE
    MODEL_API_KEY=REPLACE
    MODEL_REGION=us-east-1
    ```

Include `MODEL_REGION` only when using Bedrock. For every other provider, leave that line commented out or remove it.

Check the file:

```sh
hack/kind/sandbox/check-env.sh
```

It prints `ok` and the names of the keys. It never prints a value.

`make sandbox-up` reads this file as configuration, puts `MODEL_API_KEY` in the `kagent/sandbox-model` Secret, and
renders the provider's `ModelConfig` to refer to that Secret. The key is not put in the model manifest, the agent
manifest, or the prompt ConfigMap. For an OpenRouter endpoint or model change, edit the file, run the check again, and
re-run `make sandbox-up`; it safely reapplies the Secret and the rendered model configuration.

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

If it stops before that line, read the last error it printed:

- Exit code 3 means everything is deployed but Slack rejected the tokens. Check `SLACK_BOT_TOKEN` and
  `SLACK_APP_TOKEN` in the secrets file and run `make sandbox-up` again.
- A refused secrets file names the key that is wrong. `hack/kind/sandbox/check-env.sh` checks the file on its own.
- For anything else, see
  [If it does not come up](https://github.com/agentkourier/agent-kourier/blob/main/hack/kind/sandbox/README.md#if-it-does-not-come-up)
  in the sandbox guide.

## 5. Change the agent prompt (optional)

The demo agent's system instructions live in the `systemPrompt` key of
[`examples/recipes/sre-triage/agent/agent-prompt.yaml`](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/sre-triage/agent/agent-prompt.yaml).
The file is the `kagent/sandbox-sre-prompt` ConfigMap. [`agent.yaml`](https://github.com/agentkourier/agent-kourier/blob/main/examples/recipes/sre-triage/agent/agent.yaml)
selects that key with `spec.template.systemPromptFrom`; the prompt does not grant Kubernetes access. Keep the
read-only instructions and tools when editing it.

After editing `data.systemPrompt`, apply the ConfigMap and then the Agent so kagent compiles a new prompt revision:

```sh
kubectl --context kind-agent-kourier-full -n kagent apply -f examples/recipes/sre-triage/agent/agent-prompt.yaml
kubectl --context kind-agent-kourier-full -n kagent apply -f examples/recipes/sre-triage/agent/agent.yaml
```

In a runtime probe against the pinned kagent 1.0.0-alpha5 sandbox release, the ConfigMap update changed the prompt
revision: a fresh A2A context used the new prompt while a reused A2A context retained its original prompt. The probe
sent A2A requests directly; it did not measure Slack threads. Agent Kourier's chat intake maps the channel and root
message to the session key and creates a session for a new mentioned root message, so the Slack consequence is an
inference from that mapping: start a new root message in the channel and mention Agent Kourier for a fresh context.
Reply in the earlier thread to reuse its context. Keep that thread open when comparing the revisions, and do not use
an existing thread to check a prompt change.

## 6. Ask the agent

In your channel, mention Agent Kourier and ask about one of the seeded faults:

```text
@Agent Kourier why is checkout crashlooping?
```

A thread opens under your message. The answer streams into it, with a step card for each tool call the agent makes,
such as `k8s_get_pod_logs`. The answer quotes the `FATAL: cannot resolve payments-db` line from the pod's log and says
what the agent checked.

## 7. Follow up in the thread

Reply in the same thread, without a mention:

```text
and which pods does that affect?
```

The answer arrives in the same thread and builds on the first one.

## 8. Let the agent ask you

Start a new message in the channel:

```text
@Agent Kourier ask me which app to look at, then look at it
```

The thread shows the agent's question with its choices and a **Submit** button. Choose an app and submit. The agent
carries on with your answer.

## 9. Clean up

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
- [Start from a recipe](../how-to/recipes.md): the `docs-agent` recipe is an agent that runs on any cluster, with no
  kagent, so it is the shortest way to a working agent once Agent Kourier is installed.
