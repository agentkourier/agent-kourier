# Prompt: recreate the Agent Kourier video

Paste everything below the line into a new agent session opened at the repository root. It was written
on 2026-10-10 from the session that made the first video; `README.md` and `script-v2.md` in this
directory are the reference it points at. Edit the "What I want this time" section first.

---

I want you to rebuild the Agent Kourier marketing video. A previous session built it end to end and
left the whole pipeline in `docs/brand/film/`. Your job is to reuse that pipeline, not to design a new
one. Read `docs/brand/film/README.md` (the "v2" section) and `docs/brand/film/script-v2.md` before
doing anything else; they hold the script, the file map, the build commands and the recording runbook.

## What I want this time

<!-- Owner: say what changed. Examples: "the product now supports Teams, update the close",
     "re-record the alert shot against the new UI", "same film, new voice". If nothing is listed,
     rebuild the same film and tell me what you could and could not reproduce. -->

## What the video is

Two cuts, each in 16:9, 9:16 and 1:1, narrated, 1080p, H.264, 30 fps:

- a demo of about 100 seconds: the problem (animated), "Introducing Agent Kourier" (animated), the
  product working in Slack (real screen recordings), setup and close (animated);
- a 20-second teaser cut from the same material.

The audience is developers and Kubernetes platform people. The deliverables are the six videos, a
poster frame, contact sheets, a storyboard, and the source files.

## Decisions already made, which you should keep unless I say otherwise

- The name is always "Agent Kourier", never "Kourier" alone. Identifiers are `agent-kourier`.
- The category line is "The always-on chat gateway for A2A agents on Kubernetes". The hook is "Every
  thread, a live agent session."
- The pitch is general, not on-call only. Alerts and chat are named as sources, with the first
  supported tool beside each ("Alertmanager first", "Slack first"). Do not write copy that makes Slack
  or Alertmanager sound like the only tools the design allows.
- Never invent a product screen or a metric. Every Slack frame is a real recording of the real
  product; the YAML on screen is the Binding the demo ran with, with a placeholder for the channel ID
  (real workspace identifiers stay out of the film and its source). If a claim was not demonstrated, it is
  not in the film ("wakes agents scaled to zero" was dropped for that reason).
- Kubernetes colours on dark navy. No light or washed-out shades.
- The logo is the winged visor in `docs/brand/assets/`; its rules are in that directory's README (never
  in a hexagon).
- The narrator is ElevenLabs Eleven v4, voice Eric, directed "confident, measured, authoritative",
  lowered one semitone with a 2 dB low shelf. I chose it after auditioning nine voices. The exact
  command is in the film README, and the chosen takes are tracked in
  `audio/elevenlabs-eleven-v4-eric-measured-deeper/`.
- Footage that is sped up is disclosed once in the note under every footage shot, not with a badge on
  each shot. The browser extension's pointer must not appear in the footage.

## First, find out which job this is

`raw/` (the screen recordings) and `out/` (the renders) are git-ignored, so they exist only on the
machine that recorded them. Check before planning:

1. **`raw/session-1.mp4` and `raw/session-4-alert-retake.mp4` exist.** This is a re-cut. You can change
   copy, timing, layout, narration and voice and rebuild everything without touching Slack.
2. **They do not exist.** The animated parts and the narration can be rebuilt as they are, but every
   footage shot has to be recorded again, and the in and out points and framings in `build.mjs` have to
   be measured again from the new recordings. Tell me this before starting; it needs my time (see
   "Recording").

## Rebuilding (both jobs)

Tools: Node, `npm install` and `npx playwright install chromium` in `docs/brand/film/`, and an ffmpeg
with libx264. The ffmpeg on my Mac has no `drawtext` or `rubberband` filter, so the pipeline does not
use them.

How the pieces fit, so you change the right one:

- `narration.json` is what is said. `film.html` is what is written on the animated slides (the `COPY`
  object at the top). `build.mjs` is the edit: beat order, slide lengths, footage in and out points,
  framings, speeds and footage captions. A wording change usually touches all three, and
  `script-v2.md` has a table that copies them for reading; keep it in step.
- The picture is timed to the narration, not the reverse. After regenerating any line, read its length
  from the voice folder's `manifest.json` and adjust that beat's `dur` and `lead` in `build.mjs`, and
  the reveal times in `film.html` (`subAt`, `pillAt`, the chip delay) so text appears as it is spoken.
  `build.mjs` refuses to build if a line would run past the next cue.
- `REDO=p1,close node build.mjs demo` rebuilds only the named beats. Use it; a full build of three
  formats takes a while.

Narration goes through OpenRouter's speech endpoint with the key in the repository `.env`
(`OPEN_ROUTER_API_KEY`). Never print the key. If the request fails with "No allowed providers", the
account's Allowed Providers list is missing `elevenlabs`; that is mine to fix in the OpenRouter
settings. `narration.json` carries pronunciation respellings ("Agent Courier", "K agent", "A-to-A");
captions never use them.

You cannot hear the result, so check it the ways you can: transcribe every generated line back
(OpenRouter's `/api/v1/audio/transcriptions` with `elevenlabs/scribe-v2`) and compare it with the
script, confirm a bracketed direction was not read aloud, and tell me plainly that tone and pacing are
mine to judge. If I ask for a different voice, generate the same one or two lines in each candidate
and send me those to choose from before regenerating everything.

After every build, run `node build.mjs sheets` and `node build.mjs storyboard`, then look at the
contact sheets for all three formats yourself. The first film needed two layout passes: captions
drifting off balance, text colliding in 9:16, a command box overflowing its column. Do not report
success from the 16:9 sheet alone.

## Recording (only if footage is needed)

The runbook, the list of takes and the privacy rules are in `script-v2.md`. What it does not say, and
what cost takes last time:

- **One consumer per Slack app.** Socket Mode splits events between every process connected with the
  same app token. My cluster deployment and the kind sandbox share one app, so a thread reply and a
  resolve were silently lost. Stop every other consumer first (ask me before stopping anything outside
  kind), then confirm `num_connections: 1` in Agent Kourier's "socket mode connected" log line.
- **Prove the model key works before recording.** `make sandbox-up` reads the secrets file
  (`~/.config/agent-kourier/sandbox.env`; on my Mac it may still be under `agent-courier`, in which
  case pass `SANDBOX_ENV`). Last time the Bedrock key was rejected and the sandbox was patched live to
  use OpenRouter through the OpenAI provider. Send one test mention and get a real answer before
  starting a take.
- **Do not record in my real alert channel.** `hack/kind/sandbox/webhook.sh` and the alert Binding point
  at a channel where production alerts arrive. Record alerts in a fresh channel with its own incoming
  webhook and a Binding that names that channel and that webhook's bot ID. Last time this was done
  with live patches; check whether the repository has an override by now.
- **Start from empty channels.** I create them. You must not delete Slack messages or channels; if a
  channel has history, ask me for a new one.
- **The screen.** Record the built-in Retina display with
  `ffmpeg -nostdin -f avfoundation -framerate 30 -capture_cursor 1 -i "1:none" -c:v h264_videotoolbox -q:v 62 -r 30 raw/<name>.mp4`
  and stop it with `pkill -INT -f "avfoundation.*<name>"`. Without `-nostdin` it hangs. Keep the display
  awake with `caffeinate -d -u`. Confirm from a still that the Slack window is really on that display
  before a take.
- **Geometry.** `FRAMINGS` and `SRC` in `build.mjs` assume a 2880 by 1864 recording of Slack in Chrome
  at 125% zoom with the dark theme, thread pane open. A different window size or zoom means measuring
  the crops again.
- **What I do, and what you do.** I set Focus mode, Chrome zoom, the Slack theme, a neutral display
  name, the bot's name and icon, and I create channels and webhooks. With my say-so in the session you
  may bring the sandbox up, type the demo messages in Slack as me through the Chrome extension, start
  and stop the recording, and fire alerts with `make sandbox-alert`. Ask before each kind of action
  the first time.

Then update the shot list in `build.mjs` to the new files and timestamps. Where the pointer sits on
blank chrome, the build paints it out with a patch copied from beside it; where it covers text,
reframe or retime the shot instead.

## Privacy and safety

- Publish only cropped frames: message list and thread pane, never the sidebar, workspace name, URL
  bar or another channel. Check every contact sheet for names, emails, avatars, channel names we did
  not create, IDs, tokens and hostnames, and tell me what you checked.
- Raw recordings stay in `raw/` and are never committed or uploaded.
- Webhook URLs and tokens are secrets. Never echo them, and remind me to remove demo webhooks
  afterwards.
- Do not enter passwords, change system settings, or permanently delete anything. Ask before sending a
  message as me, pushing, or uploading.

## Putting it on the project README

The README plays the 16:9 demo from a GitHub attachment, which is capped at 10 MB on our plan. The film
README has the two-pass encode command and the steps. The upload goes through a GitHub form in my
browser; the draft-release form has a file input that works, and nothing is published from it. Replace
the attachment URL in the root `README.md`, and remember that nothing shows on GitHub until it is on
`main` and pushed. Ask me before pushing.

## Finishing

Leave my machine as you found it: `make sandbox-down`, restart anything you stopped, and list what is
mine to undo (demo webhooks, Slack display name, Focus mode). Update the film README, `script-v2.md`
and `tasks/plans/kourier-film.md` so they describe the film as built.

Report in this order: what was rebuilt and where the files are; what you verified and how; what you
could not verify (audio, anything you did not watch); anything that differs from the previous film
and why; what is committed, what is pushed, and what is waiting on me.
