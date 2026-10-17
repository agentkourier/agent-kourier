# Agent Kourier film v2: script and capture plan

Status: built 2026-10-10 (`out/agent-kourier-demo-*.mp4`, `out/agent-kourier-teaser-*.mp4`). The edit itself is `build.mjs`; where this page and that file differ, the file is what shipped.

## Positioning

- **Hook:** Write the agent, not the chat integration. (The README's headline since 2026-10-16. It replaced "Every thread, a live agent session.", which the owner dropped rather than keep as a second line.)
- **Category:** A Kubernetes-native broker between team chat and streaming A2A agents. Slack is the supported platform today and more are planned; no other platform is named, because none is committed. (Replaced "the always-on chat gateway for A2A agents on Kubernetes", 2026-10-16.)
- **Positioning:** lead with what Agent Kourier saves an agent builder: the agent keeps its tools and task logic, Agent Kourier handles the chat side. Plain and factual; the owner finds slogan-style copy cheesy.
- **Hero use case:** an alert opens a session. Second way in: someone asks.

## The script as built

`narration.json` (what is said), `film.html` (what is written on the animated slides) and `build.mjs` (the edit and
the footage captions) are the script. This table is a copy for reading; if it and those files differ, they win.

Both cuts follow the same arc: the problem, the reveal, the product working, the close. Sources are named
generally (alerts, chat) with the supported tool beside them (Alertmanager first, Slack today).

### Demo cut, 97.6 s

| # | Beat | Narration | On screen |
|---|---|---|---|
| 1 | Problem, animated | You've put AI agents on Kubernetes. They can read logs, check rollouts, and find the cause. | **You've put AI agents on Kubernetes.** They can read logs, check rollouts and find the cause. Chips: logs, rollouts, events. |
| 2 | Problem, animated | But they only work when someone opens a prompt and asks. | **But they only work when someone opens a prompt and asks.** |
| 3 | Problem, animated | Alerts fire in your monitoring. Questions come up in chat. Your agents hear neither. | **Alerts fire in your monitoring. Questions come up in chat.** Your agents hear neither. Nodes: Alerts (Alertmanager first), Chat (Slack today). |
| 4 | Reveal, animated | Introducing Agent Kourier: a Kubernetes-native broker between team chat and A2A agents. | **Introducing Agent Kourier.** A Kubernetes-native broker between team chat and streaming A2A agents. |
| 5 | Solution, animated | An alert, or a question, becomes an agent session, in the thread where it landed. Slack today. | **An alert or a question becomes an agent session.** In the thread. Slack today. |
| 6 | Real footage: alert | When an alert lands in the channel, Agent Kourier picks it up, opens the thread, and hands it to your agent. It investigates right there, step by step, and reports what it found. | **An alert opens a session.** Nobody typed a prompt. |
| 7 | Real footage: reply | Reply in the thread, and the same session continues. Same context, nothing re-explained. | **Reply in the thread.** Same session, full context. |
| 8 | Real footage: mention | Or skip the alert, and just ask. | **Or just ask.** A mention opens its own thread. |
| 9 | Real footage: question | When the agent needs a decision, it asks, and waits for your answer. | **It asks when it needs to.** Answer in the thread and it carries on. |
| 10 | Setup, animated | Teams add one Binding, in YAML, next to their agent. The platform wiring is shared. | **One Binding per channel.** Declared in YAML, next to your agent. The Binding the demo ran with, its channel ID replaced by a placeholder. |
| 11 | Close, animated | It works with kagent, or any A2A agent that streams. It's a two-way session: it answers as it works, asks when it needs you, and keeps the context when you reply. Slack today; more chat platforms are planned. Agent Kourier. Write the agent, not the chat integration. | Pills: kagent, or any A2A agent that streams · It answers, asks back, and keeps the context · Slack today, more chat platforms planned. End card: **Agent Kourier.** Write the agent, not the chat integration. A Kubernetes-native broker between team chat and streaming A2A agents. |

### Teaser, 20 s

| # | Beat | Narration | On screen |
|---|---|---|---|
| 1 | Problem, animated | Your agent is ready. The chat integration is another project. | **Your agent is ready.** The chat integration is another project. |
| 2 | Reveal, animated | Introducing Agent Kourier: a Kubernetes-native broker between team chat and A2A agents. | **Introducing Agent Kourier.** A Kubernetes-native broker between team chat and streaming A2A agents. |
| 3 | Real footage | Alerts and questions become agent sessions in the thread. Slack today, more platforms planned. | An alert opens a session. · Reply in the thread. · Or just ask. |
| 4 | End card | | **Agent Kourier.** Write the agent, not the chat integration. |

### The close

The three closing points answer what a viewer still wants to know after watching it work: does my agent
qualify, is this a real conversation or a one-shot bot, and am I tied to these tools. An earlier close said
"Never in your paging path"; the owner found it jargon and too narrowly about alerts (2026-10-11), so the
alerting-safety point lives in the README and the close sells the general product. Not claimed, because they
are not built or would overstate: steering an agent mid-turn (a reply is queued until the turn ends) and
tool approvals from Slack (planned).

The restart scene was cut (owner, 2026-10-16): it added little, the footage shows a graceful pod deletion rather
than a crash, and its line had to be hedged to stay true (one delivery window is accepted; see the delivery
guarantees page). The README and docs cover recovery; the film does not show it.

### Narration

Voice: ElevenLabs Eleven v4, "Eric", through OpenRouter, given the direction "confident, measured, authoritative" and then
lowered one semitone with a 2 dB low-shelf boost (the owner's pick, 2026-10-10; the command is in README.md). The speech model is sent
"Agent Courier", "K agent" and "A-to-A" for Agent Kourier, kagent and A2A; every line was transcribed back to check it.
Footage that is sped up is disclosed once, in the note under every footage shot ("sped up in places");
`SPEED_CHIP=1 node build.mjs demo` puts a speed label on each sped-up shot instead.

## Brand assets (`docs/brand/assets/`)

The v1 film's hex K is retired: the mark is the winged visor, and the brand rules say never to put it
in a hexagon.

| Where | File |
|---|---|
| The mark in the diagram, the reveal and the 20-second teaser | `svg/agent-kourier-mark-dark.svg` (the film's background is navy) |
| End card, 16:9 | `svg/lockup-horizontal-dark.svg` |
| End card, 9:16 and 1:1 | `svg/lockup-stacked-dark.svg` |
| The bot's avatar in the real Slack footage | `social/slack-app-icon-512.png`, set on the sandbox Slack app before recording |
| Poster and social card background reference | `social/og-image-dark-1200x630.png` |

The lockups carry live text in Inter, which is not installed here. The film will load Inter as a web
font for the render so the wordmark matches the brand, not the Helvetica fallback.

## What to capture

All from one sandbox session. The demo cut needs all of it; the teaser uses R1 to R3.

| ID | Kind | What | Used for |
|---|---|---|---|
| R1 | Video | Alert post, thread opens, investigation streams to the end | Demo 4, teaser 3 |
| R2 | Video | Follow-up reply in R1's thread, answered in the thread | Demo 5, teaser 4 |
| R3 | Video | @mention in the ask channel, thread opens, answer streams | Demo 6, teaser 5 |
| R4 | Video | Agent asks a question (input blocks and Submit), person answers, agent carries on | Demo 7 |
| R5 | Video | Kourier pod deleted mid-stream, the thread still completes | Cut from the film, 2026-10-16 |
| S1 | Still | R1's finished thread, whole thread visible | Poster frame, README hero, social card |
| S2 | Still | R4's prompt with choices and Submit | Docs |
| T1 | Text | The sandbox Binding (`hack/kind/sandbox/agent-kourier/config/binding.yaml`) | Demo 9. Typeset from the file, no recording needed |

## Keeping private information out

Three layers. Any one of them alone is not enough.

### 1. Record in an environment that holds nothing private

- **Sandbox workspace only.** A dedicated browser profile signed in to that one workspace, so no other
  workspace, bookmark, extension or autofill can appear.
- **Fresh channels.** `#shop-alerts` for alerts and `#ask-sre` for questions, created for the recording,
  so there is no history to scroll into view.
- **A neutral profile.** In the sandbox workspace, set your display name to a role ("On-call engineer")
  and a plain avatar, unless you want to appear as yourself.
- **The bot's name and avatar.** The bot is `Agent Kourier`, as `slack-app-manifest.yaml` names it; an app made from an
  older copy of the manifest is `courier` and needs renaming. Set its icon to `assets/social/slack-app-icon-512.png`
  before recording.
- **Focus mode on,** Dock hidden, no other windows. Nothing can slide in.
- **No terminal on screen.** Run every command in a second window outside the recorded area. The shell
  prompt, kube contexts and the secrets path stay unrecorded.

### 2. Capture wide, publish cropped

Record the Slack content area. I crop every published frame to the message list and thread pane. The
sidebar, workspace name, URL bar and channel list are never in the output.

### 3. Leak review before anything is published

- Raw recordings go in `docs/brand/film/raw/`, which is gitignored. They never leave this machine.
- For each cropped clip I build a contact sheet (two frames per second) and check it for names, emails,
  avatars, workspace or channel names we did not create, URLs, IDs, tokens and hostnames.
- You approve the contact sheets. Then the film is cut. Raw files are deleted when the film ships.

## Blocker: the alert clip (R1)

The alert trial is wired to the owner's real alert channel. `ALERT_CHANNEL_ID` in `webhook.sh` and
the webhook bot ID in `alert-binding.yaml` are constants that point at
the owner's critical-alerts channel, where production alerts also arrive. Recording there puts
production alerts in the raw footage.

**Recommended:** make those two values overridable, add an incoming webhook in the sandbox workspace
that posts to `#shop-alerts`, and record there. It is a small change to `hack/kind/sandbox/` and its
tests, done as a work package. Until it lands, R2 to R5 and S2 can still be recorded: they only need
the ask channel.

**Not recommended:** record in the real channel and rely on the crop. The published film would be
clean, but the raw footage would not be.

## Recording runbook

### Once

1. Create `#ask-sre` (and `#shop-alerts` once the blocker is fixed) in the sandbox workspace. Invite the bot.
2. Put `#ask-sre`'s channel ID in the secrets file as `SLACK_CHANNEL_ID`.
3. Rename the bot, set your profile, pick Slack's dark theme and zoom to 125%.
4. `make sandbox-up` and wait for `ok: Agent Kourier is connected to Slack`.
5. Browser window about 1600 × 1000 points on the Retina display. Cmd+Shift+5, Record Selected
   Portion over the Slack content area, microphone off, mouse clicks off.

### Takes

Start recording before each action and stop ten seconds after the thread goes quiet. Do each twice.

| Take | Do | Wait for |
|---|---|---|
| R3 | In `#ask-sre`: `@Agent Kourier why is catalog stuck?` Open the thread pane as soon as it appears. | ImagePullBackOff and the missing tag |
| R2 style follow-up | In that thread: `and which pods does that affect?` | An answer that uses the first turn |
| R4 | `@Agent Kourier ask me which app to look at, then look at it`. Pick one, press Submit. | The agent carrying on after Submit |
| R5 | `@Agent Kourier check every deployment in shop and tell me about each`. While it streams, in the other window: `kubectl --context kind-agent-kourier-full -n agent-kourier delete pod -l app.kubernetes.io/instance=agent-kourier-sandbox` | The thread completing, with no duplicate post |
| R1, R2 | `make sandbox-alert ALERT=KubePodCrashLooping`. The post takes up to 30 seconds. Open its thread pane. Then reply in the thread: `and which pods does that affect?` | The `payments-db` line, then the follow-up answer |
| S1, S2 | Cmd+Shift+4 over the finished R1 thread and the R4 prompt | |

Afterwards: `make sandbox-alert ALERT=KubePodCrashLooping STATE=resolved`, then `make sandbox-down`.

## Open decisions

1. Demo alert channel: do the override work package first, or ship a cut without R1?
2. Appear as yourself or as a neutral profile?
3. Call to action on the end card: repo URL, site, or none until public?
4. Slack's brand guidelines for product screenshots in marketing: not yet checked.
5. The claim "Wakes agents scaled to zero" is in neither cut: it is in the spec but I have not
   seen it demonstrated. Restore it once it has been.
