# Prompting image models for logo work

What 48 OpenRouter runs (GPT-5.4 Image 2, Gemini 3 Pro Image, Gemini 3.1 Flash Image) taught
us about getting usable marks, and the prompt template and techniques to use from round 8 on.
Run log: `generated/runs.json`; every prompt: `generated/prompts/`.

## 1. What moved results, in order of effect

1. **The lead sentence decides the genre.** Rounds 1 to 5 opened with a product description
   ("a Kubernetes-native broker that carries chat to agents and brings the reply back") and
   every result was a diagram of that sentence. Round 6 opened with a feeling ("friendly
   helper, one idea, one form, a silhouette you could draw from memory") and produced the
   first marks the owner reacted to. The model weighs the opening most; put the register
   there, never the architecture.
2. **Bans beat wishes.** "No outlines, no eye highlights, no mouth, no limbs" (round 7) turned
   cartoons into marks in one pass. "Friendly but restrained" (round 6) did nothing. Name the
   failure you saw in the last round and forbid it.
3. **Name reference marks, not adjectives.** "The register of the Fluentd bird and the Falco
   mark" steered better than "flat, geometric, modern". Image models know famous logos.
4. **Exact hex values and a one-element accent rule are obeyed** by GPT and Gemini 3 Pro;
   Flash drifts. "One warm accent #F59E0B on exactly one small element" held every time.
5. **Shape budgets work.** "At most five shapes" produced five shapes. Without it, GPT adds
   detail; Gemini adds outlines.
6. **Composition instructions are cheap and reliable:** 1:1, pure white, central 70 percent,
   optically centred. Set them once and never think about them again.
7. **Exclusion lists make rounds genuinely different.** Round 5 banned every motif from
   rounds 1 to 4 and none reappeared. Without the list, rounds 2 to 4 recombined the same
   five ingredients.
8. **Literal metaphors fail at reduction.** Needle, satchel, pneumatic tube and bridge each
   became a different object (a thermometer, a handbag, a swing). If the idea needs a caption,
   the model cannot draw it small.

## 2. What we never used, and should

- **Reference images.** Both model families accept image inputs. Sending three CNCF icons
  plus our own hand-drawn SVG as a PNG, with "match the construction, palette and weight of
  these; do not copy their subjects", constrains style far more tightly than any sentence.
- **Edit turns instead of rerolls.** Send the previous image back with one change ("deepen
  the notch between the ear tufts; keep everything else identical"). OpenAI's own guide
  says single-change follow-ups beat rewriting the prompt. We rerolled every time.
- **Variation sheets.** Ask for "four variations in a 2 by 2 grid, each a different
  treatment of X, numbered" and crop. One call explores four ideas for the price of one.
  Best for choosing a direction, not for finals.
- **A critique pass.** Feed the result to a vision model with the checklist in section 4 and
  regenerate with the critique appended. Closes the loop without a human in it.
- **GPT for finals, Gemini 3 Pro for sheets and exploring, Flash never for marks.**

## 3. The template

```
[REGISTER, one sentence: the feeling and two named reference marks. Never the product.]
[CONCEPT, one paragraph: the shapes, positions, colours and what each part means, written
 for an illustrator. One idea. If it needs a second sentence to explain, cut it.]
RENDERING: flat vector mark, solid fills, at most N shapes. Palette, exact: #326CE5, #1E3A8A,
white, one accent #F59E0B on exactly one small element. Pure white background, 1:1, mark in
the central 70 percent, optically centred. No text.
BANS: [every failure seen in the previous round, named plainly] + the standing list: no
gradients, shadows, 3D, texture, glow, outlines, photorealism, wordmark.
EXCLUDE: [motifs already explored this project, so the round is new].
[REFERENCES, when images are attached: "Match the construction, stroke weight and palette of
 the attached marks. Do not copy their subjects."]
```

## 4. The critique pass is mandatory

Owner's rule (2026-10-09): every batch, generated or hand-drawn, goes to an independent critic
subagent before the owner sees it, with the rendered sheet, the brief, the neighbour logos and
the checklist below. The critic names specific defects and applies a "would a professional
designer ship this" bar. The author does not recommend anything the critic has not passed.

### The checklist

1. Can it be drawn from memory after one look?
2. Does it read at 16 px as the same thing it is at 128 px?
3. Does it survive one colour?
4. Does it survive a circle crop?
5. Is anything in it an explanation rather than a form?
6. Does it resemble kagent, Botkube, a generic AI-chat icon, or a famous owl?
7. Would a CNCF maintainer put it next to Helm and Flux without wincing?

## 5. Mechanics

`generated/genlogo.py` takes `--ref img...` (style references, sent as image inputs),
`--edit img "instruction"` (one-change follow-up on a previous result) and `--sheet` (asks
for a numbered 2 by 2 grid). Routing: `provider.only google-vertex` for Gemini; OpenAI
needs the account's Allowed Providers and ZDR switch set (see `logo-brief.md` section 3).
