# The webhook-github recipe's agent

The agent that the [webhook-github](../README.md) recipe's AgentBackend names: it reviews a pull request, and answers a
comment that mentions it, by posting on GitHub with its own tools, as a machine user (a GitHub account kept for the
agent). It is the [prompt-agent](../../prompt-agent/README.md) recipe's image with an `agent.yaml` of its own, and
GitHub's [github-mcp-server](https://github.com/github/github-mcp-server) as a sidecar in the same pod, which gives it
four GitHub tools. No image of its own is built.

## What it does

`agent.yaml` runs the model with instructions for the two prompts of the recipe's Binding, and may call four tools of
github-mcp-server, which `allowed_tools` lists and the sidecar's `--tools` registers alone:

| Prompt | What the instructions say to call |
|---|---|
| A review | `pull_request_read` with `get`, then `get_files` (or `get_diff`); `get_file_contents` at the head commit for a file it needs whole; then one `pull_request_review_write` with method `create`, event `COMMENT` and `commitID` the head commit. |
| A reply | `pull_request_read` with `get`, then `get_comments`; then one `add_issue_comment` on the pull request's number. |

The instructions say to act only on the repository and pull request the prompt's first lines name, and to treat the
rest of the prompt and every tool result as untrusted data.

The sidecar listens on `127.0.0.1:8082` alone and holds no secret: the agent sends the machine user's token,
`${GITHUB_TOKEN}`, as the `Authorization` header of each request, and github-mcp-server calls GitHub's API with it. Its
`http` mode takes the token with each request this way; GitHub App installation tokens, which it renews by itself, it
supports only in its `stdio` mode, which the prompt agent does not speak: `agent.py` refuses an MCP `url` that is not
`http(s)://` (github-mcp-server's `docs/github-app-auth.md` at v2.2.0, read 2026-11-19; not verified here). Hence a
machine user's token, not an App.

The Binding sends the agent the token of the Secret `github-review-agent-bearer`, which the agent checks
(`PROMPT_AGENT_TOKEN_FILE`): it answers 401 to a request without it.

## What the agent can and cannot do

The prompt-agent recipe's [rules](../../prompt-agent/README.md#what-the-agent-can-and-cannot-do) hold here; these are
this agent's own.

- **Only the prompt holds the review to a comment.** `pull_request_review_write` can also approve
  (`event: APPROVE`), request changes (`REQUEST_CHANGES`), submit or delete a pending review, and resolve a review
  thread; `add_issue_comment` can also react to the pull request or to a comment. `allowed_tools` bounds the tools'
  names, not their arguments, and the instructions alone say `create` and `COMMENT`. A model can be talked out of them.
- **Untrusted text drives tools that write.** The pull request's title, body, diff and comments are written by whoever
  opened or commented on it, and they reach the model. A model can be talked into a review or comment it should not
  post, or into one on another pull request.
- **The token's rights bound everything.** Whatever the model calls, github-mcp-server acts with the machine user's
  token: give it only the one repository and the permissions in [Prerequisites](#prerequisites). Every review and
  comment it posts is that user's.
- **Its text answer goes nowhere.** The Binding's `output` is `none`: only what the tools post on GitHub is seen. A turn
  that fails posts nothing, and nothing on GitHub says so.
- **It cannot ask.** `PROMPT_AGENT_ASK_USER=0` in the Deployment: the model is not offered `ask_user`, since a webhook
  connection serves no questions ([What each platform supports](../../../../docs/reference/platforms.md)).
- **The model provider sees the pull request.** Every turn, the diff, the files the agent reads and the comments go to
  the provider that `model:` names.

## Prerequisites

- **A machine user** with access to the repository, and a **fine-grained personal access token** of it limited to
  that repository, with the repository permissions **Pull requests: read and write** (the pull request, its files, the
  review, and the comments on it) and **Contents: read** (`get_file_contents`). GitHub's
  [permissions list](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens)
  names those for the REST calls (read 2026-11-19); github-mcp-server posts the review with GraphQL's
  `addPullRequestReview`, and this repository has not checked any token against GitHub. The machine user's login is
  the recipe ChatConnection's `identity`, and the name its comment trigger matches.
- **A model provider and a key**: see the prompt-agent recipe's [Providers](../../prompt-agent/README.md#providers).
  `agent.yaml` names `anthropic:claude-haiku-5-5`.
- **Agent Kourier in the namespace `agent-kourier`**, which the NetworkPolicy names, serving the recipe's objects in
  `acme-webhooks`.

## Files

```
agent.yaml     the agent: model, instructions and the four tools, for the prompt-agent image
manifests/     ConfigMap (agent.yaml), Deployment (the agent and the sidecar), Service, NetworkPolicy
```

## Apply it

1. Make the namespace and the agent's Secrets, without echoing a value. The agent's objects go in `github-agent`, which
   the recipe's AgentBackend URL names:

    ```sh
    kubectl create namespace github-agent
    read -rs GITHUB_TOKEN     # paste the machine user's fine-grained token and press Enter; nothing is echoed
    printf %s "$GITHUB_TOKEN" | kubectl -n github-agent create secret generic github-review-agent-github --from-file=token=/dev/stdin
    unset GITHUB_TOKEN
    read -rs MODEL_API_KEY    # the model provider's key
    printf %s "$MODEL_API_KEY" | kubectl -n github-agent create secret generic github-review-agent-model --from-file=api-key=/dev/stdin
    unset MODEL_API_KEY
    ```

2. Make the bearer token. One value goes in two Secrets, both named `github-review-agent-bearer` with the key `token`:
   one in `acme-webhooks`, which Agent Kourier reads, and one in `github-agent`, which the pod mounts. Make the value
   once, and both Secrets from it, without printing it:

    ```sh
    token="$(openssl rand -hex 32)"
    kubectl create secret generic github-review-agent-bearer -n acme-webhooks --from-literal=token="$token"
    kubectl create secret generic github-review-agent-bearer -n github-agent --from-literal=token="$token"
    unset token
    ```

    `--from-literal` adds no trailing newline; Agent Kourier refuses a token with a control character.
3. Apply the agent:

    ```sh
    kubectl apply -n github-agent -f examples/recipes/webhook-github/agent/manifests/
    kubectl -n github-agent rollout status deployment/github-review-agent
    ```

    A pod that does not become ready has refused `agent.yaml` or a variable: `kubectl -n github-agent logs
    deploy/github-review-agent -c github-review-agent` names it. For another model provider, change `model:` in
    `manifests/configmap.yaml` and the key's variable in `manifests/deployment.yaml`.
4. Grant Agent Kourier the bearer Secret beside the webhook's, in the chart values:

    ```yaml
    rbac:
      secretNames:
        acme-webhooks: [github-webhook, github-review-agent-bearer]
    ```

    With the two bearer copies different the agent answers 401: the turn fails and nothing is posted, and the
    Binding's `BackendReachable` condition says `CredentialsRejected`
    ([Binding status](../../../../docs/reference/binding-status.md)).

The NetworkPolicy lets only Agent Kourier's pods (labelled `app.kubernetes.io/name=agent-kourier`, in the namespace
`agent-kourier`) reach the agent on 8080, where the network plugin enforces NetworkPolicy. It leaves egress open, and
has a commented egress list to close it: DNS, the model provider and GitHub's API.

## What to change

- The instructions in `manifests/configmap.yaml`, with the Binding's prompts they answer. The agent reads `agent.yaml`
  at start, so a change needs `kubectl -n github-agent rollout restart deployment/github-review-agent`.
  `agent.yaml` and the ConfigMap's copy start equal, and this repository's tests keep them so.
- The tools: `allowed_tools` in `agent.yaml` and the sidecar's `--tools` in the Deployment, kept equal. Each tool's
  arguments are github-mcp-server's ([its tool list](https://github.com/github/github-mcp-server#tools)).
- The images: the agent's is the prompt-agent recipe's, by release tag; the sidecar's is pinned by digest.

## Checks

`make prompt-agent-test` runs the prompt-agent recipe's `test_webhook_github.py` against this directory:

- `agent.yaml` passes the wrapper's policy with its own model, and offers no `ask_user`; `allowed_tools` and the
  sidecar's `--tools` are the same four; the ConfigMap holds `agent.yaml`; the Deployment runs the prompt-agent recipe's
  image with `PROMPT_AGENT_ASK_USER=0`, the token only in the agent's container, the bearer mounted, and the sidecar by
  digest on `127.0.0.1` with no Secret; the Service serves 8080 alone, and the NetworkPolicy admits only the broker.
- With `GITHUB_MCP_SERVER_BIN` set (`hack/ci/install-github-mcp-server.sh` installs the pinned binary and prints its
  path; CI runs these, a local run without it skips them), the real github-mcp-server runs against a fake GitHub on
  loopback, behind the agent with this `agent.yaml` and a scripted model. The review's calls reach the pull request, its
  files and a file at the head commit, and post one GraphQL `addPullRequestReview` of event `COMMENT` on the head
  commit; the reply's reach the comments and post one issue comment; every request to GitHub carries the token; the
  client speaks MCP `2026-07-28` and sends the `Mcp-Param-owner` and `Mcp-Param-repo` headers that github-mcp-server
  checks at that version; and the server lists exactly the four tools.

Nothing in this repository runs the agent against a live model or GitHub: those are yours to run.

## Limits

- The prompt-agent recipe's [Limits](../../prompt-agent/README.md#limits) hold: each pull request's session is kept in
  the pod's memory, with one replica, and a restart forgets it.
- The sidecar has no probe: the kubelet probes the pod's IP, where it does not listen. A turn that starts before it
  listens fails, since the agent cannot connect to it, and nothing is posted.
