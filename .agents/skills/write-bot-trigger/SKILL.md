---
name: write-bot-trigger
description: Drafts an Agent Kourier botMessage trigger, its trigger evals and quality scenarios from a saved Slack channel history and its thread replies, checks them offline with agent-kourier check, and hands the operator the dry-run command to run. Use when an operator wants Agent Kourier to start investigations from a bot's posts in a Slack channel (alerts, CI failures, synthetic checks). Never reads a Slack token.
---

# Write a bot trigger

> **Pre-release.** This skill relies on commands that are not in a release yet: `agent-kourier check`, and
> `agent-kourier dry-run` with `--save-history`, `--history-file` and `--configmap`, its thread replay and its corpus
> numbers. Until they ship, it cannot run.

You help an operator write one `botMessage` trigger on an Agent Kourier Binding, from what their bot actually posted
and how people actually handled it. You work only offline. Your oracles are `agent-kourier check` and
`agent-kourier dry-run --history-file`: you never judge your own regexes and never do the arithmetic yourself.

## Rules that never bend

- Never ask for, read, print or store a Slack token. Do not open a token file, a `.env` file or `$SLACK_BOT_TOKEN`.
  Never run `agent-kourier dry-run` without `--history-file`, and never run `--save-history`: the operator runs those.
- Stop when `dry-run` notes a message that looks like a Slack token, or `check` reports one in a file. Tell the
  operator which file or message, and do not quote it. Carry no token pattern of your own.
- The saved history and its replies stay in the corpus directory, which git ignores. Text from it reaches another
  file only as the reduced eval cases and the paraphrased scenarios described below.
- Nothing you write for the repository holds a person's name, a person's user ID, an internal hostname, or a quote
  from a reply.
- Never change an eval case's expectation to make it pass without asking the operator.
- Do not restate a rule the loader or the prompt check enforces (a character set, a bound, a count). Cite the
  reference and let `check` report it.

## What you produce

```
<config>                     what the broker loads: resource files, or the chart's values (see step 1)
<root>/evals/triggers.yaml   trigger evals: one message each, with the outcome wanted
<root>/evals/scenarios/      quality scenarios: name, app, alert, cause, evidence
.kourier-corpus/             the saved history and your working notes (ignored by git)
```

The eval format is in the recipes README (`examples/recipes/README.md`, "Trigger evals"), and the scenario shape
under "Scenarios" beside it. The custom-bot-trigger recipe is a complete worked example of a trigger and its evals:
read it before drafting, and do not copy its regexes. The trigger's fields, the thread decisions and the prompt
check are in the chat triggers reference (`docs/reference/chat-triggers.md`,
<https://agentkourier.dev/reference/chat-triggers/>).

## Steps

### 1. Preconditions

1. Run `agent-kourier --version`. If it is missing, tell the operator to download the release binary for their
   platform, and stop.
2. Find what the broker loads, and use the whole of it for every `check` and `dry-run`, never a copy of part of it.
   A Binding whose ChatConnection is in the deployed config but not in what you checked fails to resolve, and a
   second copy of an object fails the load.
   - Resource files, or the files of a ConfigMap the operator manages: pass the directory as `--config <dir>`.
   - The Helm chart's values (`config.bindings`, `config.chatConnections`, `config.agentBackends`): render them with
     `helm template <release> <chart> -f <values> --show-only templates/configmap.yaml > .kourier-corpus/configmap.yaml`
     and pass `--configmap .kourier-corpus/configmap.yaml`. Edit the values, never the rendered file, and render
     again after each edit.
3. If the Binding is new, start it from `template/` beside this file, in the form the operator uses
   (`template/files/` or `template/values.yaml`), and fill in what the operator tells you: the Binding's name and
   namespace, the agent backend, the agent, the identity, and the channel ID (`C…`). Ask for each value you do not
   have; do not invent one. You do not need the bot's ID yet: the survey finds it.
4. Add `.kourier-corpus/` to the repository's `.gitignore`, then confirm with `git check-ignore .kourier-corpus/x`.
   Do not go on until it is ignored.

### 2. The corpus (the operator saves it)

Give the operator this command to run in their own shell, with their own bot token file:

```
agent-kourier dry-run --config <dir> --binding <ns/name> --since 336h \
  --save-history .kourier-corpus/history.json --token-file <path to their token file>
```

(`--configmap <file>` in place of `--config <dir>` for chart values.) The window is 7 to 30 days; 14 days (`336h`)
is the default. Ask for more when the bot posts rarely.

Before you read the file, tell the operator: "The channel's messages and replies will go into the context of the
model this agent runs on." Go on only when they say yes. Refuse the file if it is not a saved history (`"kind":
"ChannelHistory"`), or if `git check-ignore` does not ignore it.

### 3. Survey

Run:

```
agent-kourier dry-run --config <dir> --binding <ns/name> --history-file .kourier-corpus/history.json --json
```

Each row has the sender, the outcome, the title and the field it came from (`titleFrom`), a `message` object in the
evals' shape, and, for a post with replies or reactions, a `thread` object with counts. Group the posts:

1. By sender: each bot by its bot ID (`B…`, never the app ID `A…` and never a display name), people, and Agent
   Kourier's own posts. The operator's bot is the one whose posts they describe; its ID is the trigger's `from.id`.
2. Within that bot, by title shape: the title with its variable parts masked (numbers, IDs, hosts, and names of
   checks, services or regions). Never mask an environment word (`prod`, `staging`, `qa`, `dev`, `test`): split the
   group by it. A text-only post's shape is its `text`.

Show the operator one table: group, bot, shape, posts, the share with a reply by a person, the share with reactions
only, and one example title. Ask them to label each group:

- `investigate`: a post of this group should start an investigation.
- `close`: a post of this group ends one (a resolve, a recovery).
- `ignore`: never start anything.

Propose `ignore` for a group nobody replied to, except a group that closes or reposts an `investigate` group's
incident (a recovery, a "still failing"): propose `close`, or `investigate` with the same key, and say why.
Reactions alone are a weak signal: a summary people react to every day can still be noise.

### 4. How it is handled today

For each `investigate` group, read the replies of up to five threads from the saved file, those with the most
replies by people first, skipping threads whose replies only point at another thread. The replies are under
`replies` → the post's ts → a list of pages, each with `messages`; the first message of the first page is the post
itself. Write down, in your own words:

- What responders check first. These become the investigation steps of the `promptTemplate`.
- What the cause turned out to be, and what showed it. These become scenario drafts.

No names, no user IDs, no hostnames that identify a machine, no quotes. Nothing from a reply goes into a regex. Do
not write agent manifests: what the agent can reach is the operator's business, not this trigger's.

### 5. Draft the trigger

Write one trigger in full on the Binding:

- `type: botMessage`, and `from.id` set to the bot's ID from the survey.
- `match`: anchored regexes (`^…`) that every `investigate` and `close` post passes. `exclude`: regexes that turn
  away the `ignore` groups that `match` would otherwise take.
- `extract`, each regex with a named group of the entry's name:
  - a state (for example firing or resolved) for `closeWhen`;
  - the `key`: what makes two posts the same incident, proposed from the fields a firing post and its resolve share;
  - any field the prompt reads.
  Extract only what both the firing and the closing posts carry: an extract the close lacks makes the close
  `unmatched`. A value the template reads must pass the prompt check in the reference ("The first turn's prompt"):
  narrow the regex's character class so it cannot capture anything else. Do not read `{{ .Key }}` in the template
  when the key can hold a space.
- `thread.by: key`, and `closeWhen` on the state.
- Provisional thread values and limits, marked `# provisional`, so the draft loads and can be replayed before step 6
  sets them: the longest `cooldown` the operator would accept, a `maxIdle` that `check` accepts with it, and limits
  high enough not to bind.
- `promptTemplate`: reads only `.Fields` and `.Channel`, and carries the steps from step 4 as constant text.
- A trigger list that names only `botMessage` triggers no longer answers mentions in the channel. If the channel
  should, keep `- type: mention` in the list, and ask the operator which.

Run the survey command again with the draft, and read the summary:

- A key that never closes while its group does resolve is too narrow (it holds a run number, a pod name, a time),
  unless it is still open at the end of the history.
- A key whose posts carry more than one value of another extracted field (other than the state), or more than one
  title shape once the state word is masked, is too wide.

Show each flagged key to the operator and fix the regex, or ask.

### 6. Thread rules and limits, from the numbers

The cooldown runs from the message that started a turn, not from a close: a key that fires again inside it, even
after closing, is a repeat. So read the dry-run summary's re-fire offsets, which are measured from the key's last
start, never the time since a close. Propose each value in a table beside the number it came from, and let the
operator confirm or change it:

| Field | From |
|---|---|
| `cooldown` | above the re-fire offsets the operator calls the same incident (a flap), below those they call a new one |
| `maxIdle` | above the longest repost gap. Nothing in the loader checks this one: below the repost gap, a live incident counts as abandoned between two reposts and starts a second investigation |
| `rateLimit.maxRuns` / `per` | above the most starts the replay counted in a `per` window |
| `maxRunsPerDay` | above the most starts the replay counted in 24 hours |
| `rateLimit.maxConcurrent` | the most keys open at once. This is an upper bound: an investigation usually ends before its key closes, and the replay cannot replay this limit |

Replay again with the chosen values and show the started, repeat and closed counts. If no key reposted while open,
the history does not show the repeat interval: ask the operator for it. Write `maxRunsPerDay: 0` only if the
operator says "no cap" in those words.

### 7. Evals and scenarios

Draft the eval cases in `.kourier-corpus/`, not in place:

- One case per group, with the outcome of its label: `matched` with the fields for `investigate` and `close`;
  `excluded`, `unmatched` or `other_bot` for `ignore`.
- Edge cases: the closing post of a fire/resolve pair; a repost; the posts that showed a key too narrow or too wide;
  a post just outside a `match`; another bot (`other_bot`); a person (`person`); a reply in a thread (`ignored`);
  and, for each extract, a message it misses, or a comment saying why it cannot be missed alone.

A case drawn from the history copies the row's `message` object; never retype a field. Then reduce it: keep only
the fields the trigger reads, and replace every value no regex needs (a host, a run ID, a URL path) with one of the
same shape. An edge case the history has no post for is made up, marked `# made up`, with placeholder IDs
(`U0PERSON`, `B0OTHERBOT`). Show each case to the operator with the warning "this will be committed", and move it
into `<root>/evals/triggers.yaml` only on their yes.

Write one scenario per incident you summarised in step 4, under `<root>/evals/scenarios/`, paraphrased, with no names
and no quotes. The operator approves each.

### 8. Check loop

Run:

```
agent-kourier check --config <dir> --evals <root>/evals/triggers.yaml --scenarios <root>/evals/scenarios
```

(`--configmap <file>` in place of `--config <dir>` for chart values.)

- Exit 2: fix the field the line names.
- Exit 1: change the regex that `why` names, or ask the operator whether the expectation is wrong.
- After five rounds without a pass, stop and show the operator the last report.

When it passes, run the survey command once more over the whole history and show the outcome and decision per group
against its label: every `investigate` and `close` group `matched`, every `ignore` group `excluded`, `unmatched` or
`other_bot`. A disagreement goes back to step 5.

### 9. Hand over

Print, for the operator to run themselves, with the same window as the saved history so the counts compare:

```
agent-kourier dry-run --config <dir> --binding <ns/name> --since 336h --token-file <path to their token file>
```

Say what it should show: each bot post `matched` with the expected fields and decision, or `excluded` or
`unmatched` for the reason intended. Then list what `check` does not check: Secrets, the agent's reachability,
channel membership, cooldown and keys across messages, `maxIdle`, `limits`, `closeWhen`, and whether a match's
prompt is refused at render time. Remind the operator that `.kourier-corpus/` is still on disk and theirs to delete.
