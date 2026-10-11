# Agent Kourier film

A 17-second silent explainer for developers and Kubernetes operators, in 16:9, 9:16 and 1:1.

> Agent Kourier is a Kubernetes-native broker that carries Alertmanager alerts and Slack
> messages to any A2A agent, wakes it if needed, and brings its findings back into the Slack
> thread, where people keep the session going.

## Files

| File | What |
|---|---|
| `scene.html` | The film: one SVG, `renderAt(t)` per frame. Copy, palette and layouts are at the top. Open `scene.html?ar=9x16&t=9.9` to preview any frame. |
| `render.mjs` | Renders video, posters, contact sheets and storyboard with headless Chromium and ffmpeg. |
| `out/agent-kourier-{16x9,9x16,1x1}.mp4` | Final video, H.264, 30 fps, 17 s. |
| `out/poster-*.png` | Poster frame (end card, t = 16.5 s). |
| `out/contact-sheet-*.png` | One frame per second. |
| `out/storyboard.{pdf,png,html}` | Five shots with picture, on-screen copy, and the spec section behind each claim. |

## Re-render

```sh
npm install
npx playwright install chromium
FFMPEG=$(command -v ffmpeg) node render.mjs all     # about 7 minutes
node render.mjs frames 2.2,5.8,9.9                  # quick stills into out/frames/
```

ffmpeg needs libx264 (`brew install ffmpeg`).

## Rules the film keeps

- Kubernetes palette: #326CE5, white, navy #0A1530. No light tints. #E5484D only for the firing alert.
- No product screens, no metrics. The thread is an abstract three-row glyph, not a Slack UI.
- Only real names: Alertmanager, Slack, A2A, kagent, and the resource kinds AgentBackend,
  ChatConnection and Binding. The API group is not shown because it changes with the rename.
- Logo: draft A (hex K) recoloured to Kubernetes blue. Swap `kourierMark()` if another draft wins.

## v2: the marketing cut (current)

Narrated, with real Slack footage. The v1 files above are kept in `out/v1/`; they use a retired logo.

| File | What |
|---|---|
| `out/agent-kourier-demo-{16x9,9x16,1x1}.mp4` | The demo cut, 102 s: animated problem and solution, real footage, animated setup and close. |
| `out/agent-kourier-teaser-{16x9,9x16,1x1}.mp4` | The 20 s teaser cut from it. |
| `out/poster-*.png`, `out/contact-sheet-{demo,teaser}-*.png`, `out/storyboard.{pdf,png,html}` | Poster frame, contact sheets, storyboard. |
| `film.html` | The motion graphics and the frame that sits around footage. Copy, palette and layouts at the top; the mark is `docs/brand/assets/svg/agent-kourier-mark-dark.svg`. |
| `build.mjs` | The edit: beats, footage in and out points, framings, speeds. Renders, cuts, mixes narration. |
| `narration.json`, `narrate.mjs` | The narrator's lines and the generator (ElevenLabs Eleven v4 through OpenRouter). `build.mjs` reads the set named by `VOICE` and its slide lengths are timed to it. |
| `script-v2.md` | Script, capture plan, privacy rules, recording runbook. |
| `raw/` (git-ignored) | The screen recordings. They show the whole screen; only cropped frames are published. |

```sh
# The film's voice: Eric, directed measured, lowered one semitone with a little low-end warmth.
# Only lines whose text or treatment changed are regenerated.
node narrate.mjs --voice eric --direction "confident, measured, authoritative" --pitch -1 --bass 2 --name measured-deeper
node build.mjs demo                     # all three formats; add 16x9 to build one
node build.mjs teaser
REDO=close node build.mjs demo          # rebuild one beat, reuse the rest
node build.mjs sheets && node build.mjs storyboard
```

Rules the v2 film keeps: every Slack frame is a real recording of the kind sandbox (seeded faults, real agent
output), cropped to the message list or the thread so the workspace name never shows; sped-up footage carries a
speed label; the Binding on screen is the one the demo ran with; Slack is named as the first chat tool, not the only one.
The Chrome extension's pointer is painted out only where it sat on blank chrome; shots where it covered text were reframed or retimed.

