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
| `out/agent-kourier-demo-{16x9,9x16,1x1}.mp4` | The demo cut, 98 s: animated problem and solution, real footage, animated setup and close. |
| `out/agent-kourier-teaser-{16x9,9x16,1x1}.mp4` | The 20 s teaser cut from it. |
| `out/poster-*.png`, `out/contact-sheet-{demo,teaser}-*.png`, `out/storyboard.{pdf,png,html}` | Poster frame, contact sheets, storyboard. |
| `film.html` | The motion graphics and the frame that sits around footage. Copy, palette and layouts at the top; the mark is `docs/brand/assets/svg/agent-kourier-mark-dark.svg`. |
| `build.mjs` | The edit: beats, footage in and out points, framings, speeds. Renders, cuts, mixes narration. |
| `narration.json`, `narrate.mjs` | The narrator's lines and the generator (ElevenLabs Eleven v4 through OpenRouter). `build.mjs` reads the set named by `VOICE` and its slide lengths are timed to it. |
| `script-v2.md` | Script, capture plan, privacy rules, recording runbook. |
| `recreate-prompt.md` | A prompt to hand a new agent session to rebuild or re-record the film: the decisions to keep, how the pieces fit, and what went wrong the first time. |
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
output), cropped to the message list or the thread so the workspace name never shows; sped-up footage is
disclosed in the note under every footage shot; the Binding on screen is the one the demo ran with, except its channel ID, which is a placeholder; Slack is named as the supported platform today, with more planned, and no other platform is named.
The Chrome extension's pointer is painted out only where it sat on blank chrome; shots where it covered text were reframed or retimed.

### The copies on the site

The site (`docs/index.md` and `docs/demo/index.md`) plays the 16:9 teaser and demo, and the demo page also plays the
cross-team incident film, from the R2 bucket
`agentkourier-media`, public only at `https://media.agentkourier.dev`; `r2.dev` access is off and the bucket has
no CORS rule. Each render is stored under `<name>.<first 12 hex digits of its SHA-256>.mp4`, so a re-cut gets a
new URL and never replaces the one a deployed page plays. A zone transform rule sets
`Cache-Control: public, max-age=31536000, immutable` on the domain's 200 and 206 responses. The posters and the
captions stay in `docs/assets/film/`: the content check reads the captions, and a same-origin `<track>` needs no CORS.

After a re-cut:

1. Upload the new renders, which prints each URL:
   `hack/docs/upload-film.sh out/agent-kourier-teaser-16x9.mp4 out/agent-kourier-demo-16x9.mp4`
   (after `npx --yes wrangler@4.147.0 login`). Or, in the dashboard, upload copies renamed to
   `<name>.<sha12>.mp4` (`shasum -a 256` gives the hash) to the bucket's root.
2. Put the new URLs in the `<source>` and download link of `docs/index.md` and `docs/demo/index.md`.
3. Regenerate the captions (`docs/assets/film/*.en.vtt`) from `narration.json`, with cue times from
   `ffmpeg -i <render> -af silencedetect=noise=-35dB:d=0.25 -f null -`, and replace each poster with the cut's end card
   at 1280x720.
4. Leave the old objects in the bucket until no deployed page plays them.

The incident film is not built here: it is cut from a recording of the
[incident demo repository](https://github.com/agentkourier/kourier-incident-demo) with its own narration and frame. Its
copy on the site follows the same rules: `agent-kourier-incident-16x9.<sha12>.mp4` in the bucket, and
`agent-kourier-incident.en.vtt` and its poster (the cut's title card, since the cut ends on a bare lockup) in
`docs/assets/film/`. Its captions are cut into short cues at the pauses in the narration, not timed with silencedetect
on the render.

### The copy in the project README

The project README plays the 16:9 demo from a GitHub attachment
(`https://github.com/user-attachments/assets/1af66d63-78cd-437d-a3a2-2e178904d177`). Attachments on the
free plan are capped at 10 MB, so that copy is a two-pass encode of the finished cut at 570 kb/s
(8.7 MB, 1080p). After a re-cut, encode again, drop the file into any GitHub comment box in this
repository without submitting it, and replace the URL in `README.md` with the one GitHub inserts.

```sh
ffmpeg -y -i out/agent-kourier-demo-16x9.mp4 -c:v libx264 -preset veryslow -b:v 570k -pass 1 -an -f null /dev/null
ffmpeg -y -i out/agent-kourier-demo-16x9.mp4 -c:v libx264 -preset veryslow -b:v 570k -pass 2 \
  -c:a aac -b:a 112k -movflags +faststart build/readme/agent-kourier-demo.mp4
```

Anyone who can see the repository can play the attachment; the repository is public.
An attachment is authorized for the repository it was uploaded to: when the repository was re-created after its
history was rewritten (2026-10-16), the old link stopped playing and the video had to be uploaded again.
