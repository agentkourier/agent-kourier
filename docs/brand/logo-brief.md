# Agent Kourier logo brief

Status: **mark chosen by the owner, 2026-10-10: the a1 winged visor** (Kubernetes-blue feathers, navy robot head, white visor). Assets built in `assets/`; see `assets/README.md`. Vector source of truth stays the hand-drawn
SVGs in this folder (`logo-a-hex-k.svg`, `logo-b-relay.svg`, `logo-c-pigeon.svg`). The rasters in
`generated/` are direction studies to pick from, not final assets.

## 1. What the CNCF logos have in common

Surveyed all 216 colour project icons in `cncf/artwork` (`projects/*/icon/color/*.png`,
graduated, incubating and sandbox; archived excluded) on one contact sheet, plus the CNCF brand
guidelines (CNCF blue #0086FF, Clarity City, flat colour only).

- **Flat vector.** Solid fills, no shadows, no texture. Gradients appear in maybe one icon in
  twelve, almost all recent sandbox entries. Nothing is photographic.
- **Blue is the family colour.** Dominant hue of the icon: blue 55%, neutral 10%, red/pink 8%,
  teal 7%, green 7%, orange/yellow 6%, purple 6%. Blues cluster around Kubernetes #326CE5 and
  CNCF #0086FF. Projects that want to stand apart pick one other hue and own it (kagent purple,
  Prometheus orange, Kubewarden red, Linkerd teal).
- **A container shape signals the family.** The seven-sided Kubernetes heptagon is the member
  badge (kubernetes, kured, kubeedge, kubearmor, kubefleet, kubeflow-*, ovn-kubernetes, spinkube,
  kubevela). The hexagon is the next most common frame (akri, artifacthub, cilium, keda, kcp,
  kagent, kaito, kubevirt, metal3, strimzi, llm-d, wasmcloud, confidential-containers). Circles
  third. Raw marks without a frame are mostly mascots or wordplay letters.
- **Three idioms, pick one.** Geometric monogram or letter (containerd, etcd, k3s, Thanos T,
  notary N); literal metaphor drawn in a few shapes (Helm wheel, Harbor lighthouse, Flux wave,
  Rook castle, Prometheus torch, Shipwright ship); flat mascot (Argo octopus, Falco, Fluentd and
  Emissary birds, Jaeger gopher, Porter and Tekton cats, Podman seals, OpenCost pig).
- **One accent at most.** Two colours plus white is the norm; a small third colour marks the
  one thing the project does (the red dot in Dex, the yellow in Headlamp).
- **Built for 16 px.** One to three shapes, thick uniform strokes, round caps, symmetric or
  balanced inside a square. Icons that fail this (detailed scenes, thin lines) read as outliers.
- **Name stays outside the icon.** The wordmark is a plain geometric sans beside or below the
  mark; only a handful bake letters in (helm, gRPC, KEDA, TiKV, Konveyor, SOPS).

Essence in one line: a flat, blue, geometric mark in a polygon frame, with one accent colour,
that survives 16 px and says "Kubernetes family" before it says anything else.

## 2. What that means for Agent Kourier

Agent Kourier is a Kubernetes-native broker: Alertmanager and Slack in, A2A agents out, answers
back into the thread. So it belongs in the Kubernetes family (blue, polygon frame) and its one
accent should mark the thing it alone does: the hand-off, the message in transit. Nearest
neighbours to differ from: kagent (purple robot head in a hexagon), Prometheus (orange torch).
Hence: Kubernetes blue #326CE5, navy #1E3A8A, white, teal #14B8A6 as the single accent, and no
purple or orange in the mark.

## 3. Which OpenRouter models to use

Checked `GET /api/v1/models` (466 models, 12 with image output) and each model's `/endpoints`.
Two account settings at https://openrouter.ai/settings/privacy gate what is reachable: the
Account-Wide Allowed Providers list (originally xai, azure, google-vertex, novita, typesafe,
amazon-bedrock; OpenAI added 2026-10-06) and the Zero Data Retention only switch (turned off
temporarily the same day so the OpenAI image endpoints route). Per-request `provider.only` can
only narrow within the account list, never widen it.

| Model | Served by | Use for | Observed |
|---|---|---|---|
| `google/gemini-3-pro-image` | Google Vertex | Primary. Best prompt adherence for flat geometric marks, honours hex colours and "no text". | 22 to 51 s, about $0.14 per image |
| `google/gemini-3.1-flash-image` | Google Vertex | Iteration and variants, roughly half the price, still clean flat output. | 8 to 10 s, about $0.07 per image |
| `google/gemini-3.1-flash-lite-image` | Google Vertex | Smoke tests only; quality is lower. | 5 s, about $0.03 |
| `google/gemini-2.5-flash-image` | Google Vertex | Older; no reason to prefer it. | not run |
| `openai/gpt-5.4-image-2` | OpenAI | Second primary once enabled. Crisper edges and more exact geometry than Gemini on the same prompt; slower and pricier. Needs OpenAI in Allowed Providers and the ZDR-only switch off (OpenAI image endpoints are not ZDR on OpenRouter). | about 86 s, about $0.23 per image |
| `openai/gpt-5-image`, `gpt-5-image-mini` | OpenAI | Same gates as above; mini for cheap iteration. | not run |
| `google/gemini-nano-banana-2.1` | Google AI Studio only | Blocked by the same setting. | blocked |

None of these produce SVG. Use them to choose a direction, then redraw the winner by hand in SVG
(or trace it) so the mark is a real vector with exact colours.

Prompting lessons and the template for later rounds: `prompting.md`.

Call shape that works: `POST /api/v1/chat/completions` with `modalities: ["image","text"]`,
`image_config: {"aspect_ratio": "1:1"}` and `provider: {"only": ["google-vertex"]}`; the image
arrives base64 in `choices[0].message.images[0].image_url.url`. Script: `generated/genlogo.py`.

## 4. The five directions

Every prompt shares one style block (flat, the four colours, white ground, no text, 16 px rule,
at most three shapes). Prompts in `generated/prompts/`, run metadata in `generated/runs.json`.

1. **Hex K** (`generated/01-hex-k.png`, gemini-3-pro-image). The existing draft A: a K whose
   arms are a chevron route, teal node at the junction. The model returned a heptagon instead of a
   hexagon. Keep that: the heptagon is the Kubernetes badge and it reads better than our hexagon.
2. **Heptagon envelope K** (`02-heptagon-envelope-k.png`, gemini-3-pro-image). Envelope whose
   flap fold extends into a K, teal dot at the hand-off point. Most literal "courier" of the five;
   the K is slightly overdrawn and would be simplified in vector.
3. **Relay** (`03-relay.png`, gemini-3.1-flash-image). Chat bubble, teal broker node with
   envelope, hexagon, in monoline. Explains the product in one glance; weakest at 16 px because
   it is three objects wide.
4. **Carrier pigeon** (`04-carrier-pigeon.png`, gemini-3.1-flash-image; `04b` is the first,
   landscape run on pro). Flat mascot in the Argo/Falco/Fluentd tradition with a teal hexagonal
   message capsule. Most personality, least "infrastructure"; mascots work for CNCF projects
   but need a separate simple icon for favicons.
5. **Thread and return** (`05-thread-return-k.png`, gemini-3-pro-image). A thread indicator
   (dot and stem) whose two chevrons leave for and return from a small hexagon, forming a K. New
   idea: it encodes the round trip (alert out, answer back into the thread), which no other
   direction does. Frameless, so it leans on the wordmark for family membership.

## 4b. Batch 2: the agent as a participant

The first five showed a courier and a message but never the agent, so the product's point
(messaging apps talking to agents) was missing. Batch 2 adds the agent as a four-point spark,
the common AI glyph, deliberately not a robot head because kagent owns that in this space. The
person or messaging app is always a speech bubble.

6. **Dialogue** (`06-dialogue.png`). Two white bubbles in a blue heptagon: dot for the person,
   spark for the agent, and the lens where they overlap is teal, the broker. Strongest of the
   batch: the whole proposition in three shapes, and it holds at 16 px.
7. **Thread to agent** (`07-thread-to-agent.png`). Direction 5 with the hexagon replaced by a
   teal spark in the K's mouth.
8. **Round trip** (`08-round-trip.png`). Bubble, two looping arrows, spark in a hexagon. Clear
   story, too many parts for a favicon.
9. **K with endpoints** (`09-k-endpoints.png`). White K in a heptagon; upper arm ends in a spark,
   lower arm in a circle. Quiet, confident, very CNCF. Second favourite.
10. **Pigeon to agent** (`10-pigeon-spark.png`). The mascot carrying a spark capsule toward a
    spark. Charming, but the agent reads as decoration.

## 4c. Batch 3: messaging apps talking to agents, on GPT-5.4 Image 2

Same style block, prompts rewritten so the chat app and the agent are the two poles and the
courier is what joins them. 6 and 9 rerun on GPT for a side-by-side; 11 and 15 also run on Gemini.

11. **Bubble bridge** (`11o-bubble-bridge-gpt.png`, Gemini in `11g`). Navy bubble with a dot,
    blue bubble with a spark, a teal K-shaped chevron bridging them. The clearest "chat meets
    agent" image of all 18 and the only one where the K and the bridge are the same object.
    Frameless, so family membership comes from colour and the wordmark.
12. **Thread arrow** (`12o`). A thread of three lines whose last line leaves the bubble as an
    arrow into a hexagon with a spark. Most literal; reads as a diagram rather than a mark.
13. **Agent in chat** (`13o`). Blue bubble with a white spark and a teal node, inside a navy
    heptagon outline. Simple and strong at small sizes; says "agent in your chat" rather than
    "broker between them".
15. **K bridge** (`15o`, Gemini in `15g`). The K's stem is a tall speech bubble; the arms reach a
    spark. GPT drew it as a solid K and it works; Gemini drew it outlined and lost weight.

GPT versus Gemini on identical prompts: GPT's shapes are more exact (true heptagon, even
strokes, cleaner tangents) and it follows "solid, no outline" more reliably; Gemini is four
times faster and 40 percent cheaper and occasionally makes a better compositional call (the
heptagon in 1). Use GPT for finals, Gemini for exploring.

## 4d. Round 4 (2026-10-09): neighbours, how agents are drawn, simple versus complex

**Neighbour survey** (`survey/neighbours.png`: GitHub avatars of kagent, k8sgpt, HolmesGPT,
Robusta, Botkube/Kubeshop, Keep, A2A, MCP, LangChain, LangFlow, CrewAI, AutoGen, OpenAI Agents,
Letta, Mastra, Agno, HumanLayer, n8n, Dify, Pixie, plus Slack, PagerDuty, Grafana, Prometheus).

- Kubernetes-plus-chat tools stay in the CNCF idiom: blue or purple, a polygon or circle frame
  (kagent purple hexagon, Robusta teal ring, Botkube purple cube, k8sgpt cyan rings).
- Agent frameworks go the other way: a white mark on a black or very dark square (LangChain,
  LangFlow, Mastra, Letta, Pixie, OpenAI, Anthropic). A mark that works on both a blue polygon
  and a dark square covers both rooms.
- **How "agent" is drawn in logos.** Robot visor or head (kagent, Botkube's older bot), an eye or
  lens (k8sgpt), a node graph with edges (n8n, A2A, MCP, Pixie), or just a monogram. Not one
  neighbour puts the sparkle in its logo. The sparkle is a UI convention for "AI feature"
  (Cloudscape, PatternFly and Red Hat all document it; Red Hat tells designers to avoid robots and
  brains for features) rather than a brand mark. Keep the sparkle as a secondary glyph at most;
  carry the "agent" meaning with the node-graph idiom, which is what the agent protocols
  themselves use.

**Web-readiness criteria** applied to every candidate from here on:

- a single SVG under 1.5 KB with at most seven elements and no gradients, filters or masks;
- reads at 16 px (favicon) and survives a one-colour silhouette for docs navigation and
  `currentColor` use;
- keeps its meaning inside a circle crop (GitHub avatars) and a rounded-square crop (Slack app
  icon), so nothing essential sits in a polygon's corners;
- works on white, on the blue heptagon, and on a dark square;
- the wordmark is plain Inter, Agent regular and Kourier bold, never baked into the icon.

**The matrix** (`generated/round4-matrix.png`, `generated/21*` to `24*`, SVGs in `svg/`,
size test in `svg/size-test.png`): four concepts, each as a favicon-tier icon and a lockup.

1. **Dialogue.** Lockup is lovely. At 32 px and below the two white bubbles with dark marks read
   as an owl's face. Fixable by squaring the bubbles and dropping the tails, but it is a
   warning sign for a mark that has to live at 16 px.
2. **Bubble bridge.** The clearest story and the best hero or lockup. Wide, so it is a weak
   favicon; at 16 px only the teal chevron survives.
3. **Node K.** Reads as a K at every size, holds in one colour, 680 bytes. The node-graph idiom
   says "agent protocol" the way A2A, MCP and n8n do, without a sparkle. In the heptagon with a
   bubble at the stem's top (the lockup) it also says Kubernetes and chat.
4. **Agent in chat.** Simplest file and fine at every size, but it is the generic "AI chat" icon
   a hundred products ship. Drop it.

## 4e. Round 5 (2026-10-09): new metaphors, enhanced prompts

The owner asked for more variety. Every earlier round recombined five ingredients (speech
bubbles, the sparkle, the K chevron, the envelope, the pigeon), so this round bans all of them
in the prompt and tries ten courier and broker metaphors nobody in the neighbour survey uses.
Prompt upgrades that carried over: an explicit EXCLUDE list, a CONSTRUCTION block (circles, 45
and 60 degree angles, uniform strokes, round caps, at most four shapes), an exact palette with
the teal limited to one small element, and "as if exported from Figma" for edge quality.
Sheet: `generated/round5-new-metaphors.png`; files `generated/3NN-*-gpt.png` and `-gem.png`.

| # | Metaphor | Verdict |
|---|---|---|
| 01 | Post horn in heptagon | The oldest courier symbol, but GPT's curl reads as spectacles; needs a hand-drawn horn. Idea worth keeping. |
| 02 | Switchboard patch cable | Clean and simple, two sockets and an arch with a teal bead. Reads as headphones at a glance. Gemini's version is heavier and better. |
| 03 | Winged heptagon | Crisp, badge-like, Kubernetes with wings, "Hermes for the cluster". Strongest new frame; lacks any chat or agent cue. |
| 04 | Messenger satchel | Reads as a handbag. Drop. |
| 05 | Postmark | Navy ring, blue heptagon, three teal cancellation bars. Very CNCF, three shapes, says "delivered through Kubernetes". Strong. |
| 06 | Bridge | Reads as a swing or a gate. Drop. |
| 07 | Thread and needle | The Slack-thread pun does not survive reduction; too many curves. Drop. |
| 08 | Linked rings | Two interlocked rings, navy and blue, teal overlap. Simple, strong, "chat and agent bound by the broker"; close to generic link icons. Keep as a candidate. |
| 09 | Relay baton | Abstract; reads as a toggle or stylus. Gemini's is better but still vague. Drop. |
| 10 | Pneumatic tube | Reads as a thermometer or straw. Drop. |

Keepers from this round: **postmark**, **winged heptagon**, **linked rings**, and the post horn
as a hand-drawing task. None of them shows the agent; they are courier marks. The winged
heptagon or the postmark could frame the node K or the rings to get both.

**OpenRouter credit.** The account (not the key) had used $77.59 of $80 by the end of this
round, so Gemini 3 Pro Image's per-request reservation failed with HTTP 402
`in_flight_budget_exhausted` for eight of ten runs. Top up at
https://openrouter.ai/settings/credits before another round; GPT-5.4 Image 2 at $0.23 a run
means a ten-image round needs about $3 of headroom plus the reservation margin.

## 4f. The agent glyph test (2026-10-09)

Owner's question: is the better representation "AI agents and message exchange", and are
robots not the normal way to draw an agent? Yes on both. The round-5 courier metaphors never
show who is talking, and for agents in chat the robot is the dominant convention (Slack, Discord
and GitHub bot badges, Botkube, kagent); the design-system advice against robots is about AI
feature buttons, not agents. So the content model is fixed: a chat bubble, a two-way courier
link, an agent, one teal broker element.

Test: `svg/bridge-sparkle.svg`, `svg/bridge-hexagon.svg`, `svg/bridge-robot.svg`, identical
except for the agent glyph, rendered at 16 to 128 px on light and dark with circle and
rounded-square crops (`svg/agent-glyph-test.png`, close-up in `agent-glyph-test-zoom.png`).

- **At 16 and 24 px the glyph does not matter.** All three collapse to a white dot in the
  right bubble. The favicon reads as two bubbles and a teal chevron whichever is chosen, which
  is acceptable: that is the exchange.
- **At 32 px** the sparkle is a clear star, the robot is a head with a visor, the hexagon is a
  rounded blob with no meaning.
- **At 48 px and above** the robot says "a bot in the chat" most directly, the sparkle says
  "AI" most generically, and the hexagon reads as a nut or a generic shape. Hexagon is out.

Decision for the owner: robot or sparkle, and it only affects sizes of 32 px and up (README,
site, Slack app icon, GitHub avatar). Recommendation: **robot**, kept this minimal (rounded
head, visor, antenna, white on Kubernetes blue, no purple), because the audience already calls
these things bots and it is the only glyph that still means "agent" at 32 px.

## 4g. Full review of all 37 icons (2026-10-09)

Every icon from the drafts, five rounds and the SVGs on one sheet (`review-all.png`,
`review-all.html`, scores in `generated/scores.json`), scored 0 to 2 on six criteria:
exchange shown (x2), agent shown (x2), Kubernetes cue (x1), legible at 16 px (x2), distinct
from kagent, Botkube and generic icons (x1), easy to draw as SVG (x1). Maximum 18.

| Score | Icon | Why it lands there |
|---|---|---|
| 16 | 09 K endpoints | Everything but a strong exchange: the K's arms reach a spark and a dot, in the heptagon. |
| 16 | SVG dialogue | Full marks except 16 px, where it becomes an owl. |
| 15 | 15 K bridge | Chat bubble as the K's stem, arms reaching the agent, in the heptagon. Loses only on 16 px and vector effort. |
| 15 | 06, 21 Dialogue | Same owl problem as the SVG. |
| 14 | 07, 11, 22, SVG bridge (sparkle, robot) | The clearest story, frameless, weaker favicon. |
| 12 | 05 thread return, 23 node K, 305 postmark | Good marks that each miss one of agent or exchange. |
| 11 and below | everything else | Courier-only metaphors, diagrams, mascots, generic AI-chat icons. |

Three families account for every icon scoring 14 or more, and the sheet makes the pattern
obvious: the **K in the heptagon** (01, 09, 15), the **dialogue** (06, 21, SVG) and the
**bubble bridge** (11, 22, SVG). Everything from round 5 and every mascot sits below them,
which settles the earlier question: the mark has to show the chat, the agent and the courier,
and the Kubernetes frame is what separates it from a generic chat-bot icon.

**Best match: 15, the K bridge.** It is the union of the three families: the chat bubble from
the dialogue and bridge marks, the K whose arms reach the agent from 09, and the heptagon from
01. It is the project's initial, it says Kubernetes before anything else, and it is the only
candidate carrying all three messages in a single letterform. Its two lost points, 16 px and
vector effort, are exactly what a hand drawing fixes: the dialogue SVG proved the heptagon and
bubbles survive 16 px when the shapes are solid, and the K-endpoints render shows the arms
survive. 09 is the fallback if the bubble-stem proves too busy at favicon size, and the bubble
bridge stays the hero for the README and the film, where there is room for the story.

## 4h. Reset (2026-10-09): "none of these feel right"

The owner's verdict on all 37. Diagnosis, agreed with the owner: every round used the same
palette, the same flat style block and a brief that rewarded explaining the product, and the
scoring rubric gave double weight to "shows the exchange" and "shows the agent". That process
yields infographics, not logos. The marks people love in this space (Helm's wheel, Argo's
octopus, Prometheus' torch, Flux's wave) explain nothing; each is one idea and one ownable
form, and the wordmark does the explaining.

Owner's direction for round 6: **a character or mascot, or one abstract gesture**, tone
**friendly helper** (Argo, Tekton, Jaeger territory). Palette stays blue-based but a single
warm accent (#F59E0B) is allowed for friendliness. Mascots get a separate tiny icon for
favicons, as those projects do. The "must show the exchange" rule is withdrawn.

The rubric in 4g is therefore retired. New test for round 6 onward: could someone draw it from
memory after one look, and would they smile.

### Round 6 results (`generated/round6-mascots.png`, files `generated/4*`)

- **Owl with a scroll.** The message carrier of folklore, and the one mascot here that is also a
  quiet nod to "wise" (the agent). Built from circles, big eyes, calm. Passes the draw-from-memory
  test. The accidental owl in the dialogue marks turns out to have been a hint.
- **Robot courier.** Round, waving, orange satchel. The friendliest image in the whole project and
  the only one that says "courier" and "bot" at once. Risk: generic cute robot, and kagent's
  robot head next door. The satchel and the full body are what keep it its own.
- **Retriever.** Fetches and brings back, which is the product, but k9s already owns a dog in
  this ecosystem and the drawing is stock.
- **Ribbon K, out-and-back gesture, fox.** Misses: a bent tube, a ghost with a smile, a furry.

Two real candidates for the first time: the owl and the robot courier. Both would get a
separate tiny icon for favicons (the owl's face; the robot's visor), as Argo and Tekton do.

### Round 7: "too cartoony" (owner on round 6)

The register was wrong, not the ideas: thick outlines, eye highlights, smiles, waving hands.
Friendly without cartoon in this space is the Fluentd bird and Falco: a few geometric shapes, no
outlines, no face details, warmth from proportion and one accent. Round 7 reran the two
candidates that way, plus a quieter idea (`generated/round7-quiet.png`, files `generated/5*`).

- **Owl in the heptagon** (`5o2`). The Kubernetes heptagon with its top vertex notched so the
  two upper corners become ear tufts; eyes and beak as negative space. Kubernetes on the first
  look, an owl on the second. Five shapes, no outline. Redrawn by hand as `svg/heptagon-owl.svg`
  (six elements, about 700 bytes) and tested at 16 to 128 px in `svg/owl-test.png`.
- **Geometric owl** (`5o1`). Clean, calm, the Fluentd register. Reads as an owl at every
  size, but it is "an owl" rather than "this project's owl"; the heptagon version is the
  ownable one.
- **Courier silhouette** (`5o3`). Reads as a microphone. Drop.

Caveat to weigh: owls are crowded outside this ecosystem (Tripadvisor, Hootsuite, Duolingo).
None of those is blue, none is a heptagon, and none is in CNCF, so the heptagon owl is
distinct where it matters, but the owner should know the company it keeps.

### Round 8: first call with the prompting playbook (`generated/601-owl-sheet-refs.png`)

One GPT-5.4 Image 2 call using the `prompting.md` template, four style references attached
(our `heptagon-owl.svg` rendered to PNG, plus the Fluentd, Falco and Kubernetes icons), and
the `--sheet` request for a numbered 2 by 2 grid of treatments. Cost $0.31 (references add
input tokens), 97 seconds.

Result: four usable treatments of the heptagon owl in one image, all in the Fluentd/Falco
register, all obeying the bans (no outlines, no highlights, no mouth), the orange only on the
beak. Treatment 1: shallow notch, eyes touching, the friendliest. Treatment 2: tall tufts,
separated eyes, the most heptagon-like. Treatment 3: medium tufts, large touching eyes,
rounded lower corners, the most "owl". Treatment 4: very tall tufts, small eyes, reads as a
cat. The hand-drawn SVG is closest to 2; the owner should pick between 1, 2 and 3 and the SVG
gets its tuft height and eye spacing from that pick.

Verdict on the playbook: references plus a sheet gave more decision-useful output per call
than any single-image round. Use this shape for every exploratory call from now on.

### Round 9: off the owl; one form that is courier and agent at once

Owner's direction: drop the owl; the mark must represent a courier interfacing with an AI
agent. Brief in the playbook shape: quiet register, one form that is both courier and agent,
no diagram of an exchange; the agent drawn as the visor-band head the ecosystem already
reads as "bot". Four concepts per sheet, one sheet each on GPT and Gemini, four CNCF icons as
style references (`generated/701-courier-agent-concepts-gpt.png`, `-gem.png`).

1. **Winged visor.** Rounded-square navy head, white visor with two eyes, three stepped
   blue feathers each side. Hermes as an agent. Crisp, badge-like, symmetrical, the cleanest
   silhouette on either sheet, and nothing in CNCF looks like it. Best of the round.
2. **Courier cap.** Same head under a blue peaked cap with an orange brim. Charming and
   clearly "delivery bot"; one step more character than 1, still not cartoon.
3. **Satchel visor.** A bag with eyes. Reads as a handbag. Drop.
4. **Visor heptagon.** Blue heptagon, visor, orange chevron. Strong, but a visor face inside
   a polygon is kagent's logo (purple hexagon, robot face). Too close. Drop.

GPT versus Gemini: GPT's are tighter and flatter; Gemini adds a small notch under the visor
that humanises the face and nudges it toward cartoon. GPT for this family.

### Round 10: vetting the courier-agent concepts in SVG (no model calls)

Owner's process from here: vet concepts as SVG first, use the image models only to vary a
chosen one. Five marks drawn by hand (`svg/visor-*.svg`, `svg/v2-*.svg`), two passes, tests in
`svg/courier-agent-test.png` and `courier-agent-test-v2.png`.

Pass one found two problems the GPT renders had hidden: a navy head with a white band reads
as a ninja mask at small sizes, and thin stepped feathers vanish at 16 px. The single-sweep
wings read as horns. The courier cap floated above the head.

Pass two: Kubernetes-blue head (reads as a bot, not a ninja), heavier feathers, the cap seated
on the head with the brim overlapping it.

- **Courier cap v2** (`svg/v2-cap.svg`). The strongest at every size: a bot in a courier's cap,
  unmistakable at 24 px and up, a dot with an orange line at 16 px. The orange brim is the
  palette's single accent doing real work. Compact, so the favicon is large. 568 bytes.
- **Winged visor v2** (`svg/v2-wings.svg`). The blue head fixes the ninja read, but two bars
  per side read as equals signs, not wings. Wings need angled feather shapes to say Hermes;
  worth one more drawing if the owner wants this one.
- **In transit v2** (`svg/v2-transit.svg`). Clean and legible, but speed bars are the generic
  "fast" modifier every delivery app uses.
- **Wings in orange.** The accent on the wings makes them read as bars even more. Drop.
- **Visor heptagon.** The best 16 px read of the five and the kagent problem is unchanged. Keep
  only as a reference.

**The head itself is the finding.** All five share one bot head: blue rounded square, white
visor, two navy eyes. It reads as "agent" at every size on its own. The courier modifier is
the open question, and the cap is winning.

### Round 11: more Kubernetes, more agent (SVG, `svg/k1-*` to `k5-*`, test `courier-agent-test-v3.png`)

Owner asked how to bring in the AI-agent aspect and the CNCF / Kubernetes style. Five
variants of the courier-cap bot, each adding one cue.

Kubernetes cues:
- **Heptagon cap** (`k1`). The cap crown is the top of the Kubernetes heptagon: the bot wears
  Kubernetes. Subtle, ownable, reads as a peaked cap at small sizes and as the heptagon at
  64 px and up. Nothing like kagent.
- **Capped bot in heptagon** (`k2`). The kube-* family convention (kubeflow, kured, kubeedge,
  kubearmor all frame their mark in the heptagon). White head, navy visor, white eyes so it
  contrasts inside the blue frame. The loudest CNCF signal and the best 16 px read of
  everything drawn so far. The cap, the white head, the heptagon and the blue keep it apart
  from kagent's purple hexagon.

Agent cues:
- **Node badge on the cap** (`k3`), the A2A and MCP idiom. Invisible below 64 px. Clutter.
- **Antenna with a teal node** (`k4`). Reads from 32 px, says "receives", but breaks the
  silhouette and nudges toward cartoon. Optional.
- **Spark as the right eye** (`k5`). A dot below 64 px. Ineffective.

Conclusion: the agent read is already carried by the head (visor plus two eyes) and nothing
small enough for a favicon adds to it. The place to say "agent" harder is the wordmark and the
hero illustration, not the icon. The Kubernetes read, by contrast, has two real options: the
heptagon as the cap, or the heptagon as the frame.

### Round 11 crit: independent critic subagent (owner's new rule, 2026-10-09)

Owner: "when you generate icons, use a subagent to judge the visual quality and fit; a lot of
these look terrible." From now on every batch is gated by a critic subagent before the owner
sees it (`tasks/lessons.md`, `prompting.md` section 4). First crit, on the v2 and v3 SVG sets
and the round-9 GPT sheet, scored 1 to 10 with 7 meaning "a professional would ship with minor
tuning":

| Candidate | Score | Verdict in one line |
|---|---|---|
| k2 capped bot in heptagon | 5 | best of the set; five nesting levels, sits 5 units low, brim-to-visor tangency |
| courier cap v2 | 4 | brim is a floating symmetric headband, dome wider than head, fills half the canvas |
| visor heptagon (SVG) | 4 | best 16 px fill, but it is the kagent formula and a sideways smirk |
| winged visor v2 | 3 | 2:1 aspect, wings are UI capsules, reads as a TIE fighter at 16 px |
| GPT satchel | 3 | execution dead, but the only concept that says courier without a face |
| in transit, spark eye, GPT winged, GPT heptagon | 3 | clichés or invisible cues |
| wings in orange, node badge, antenna, GPT cap | 2 | off-palette teal, floating antenna, "bot cop" |
| k1 heptagon cap | 2 | self-intersecting polygon bug, 1.5x oversized hat, conical-hat caricature risk |

Shippable: none. The critic's diagnosis of why the whole family looks amateur:

1. Every candidate starts from the stock bot face (rounded rectangle, capsule visor, two dots),
   which is also Slack's, kagent's and every AI button's; the props bought to add identity
   (wings, caps, antennae, badges) vanish at 16 px, leaving the generic face and a ninja read
   from the light visor band.
2. No construction system: stroke weights of 2, 4, 4.5, 5, 8 and 9 units, a mix of sharp
   quadratics and full capsules, 1 to 2 unit gaps that fuse into tangencies, one floating
   element, one self-intersecting polygon.
3. The marks fill about half the canvas, so every small-size test was lost before it began.

The fix the critic prescribes for the next pass: design in solid black at 16 by 16 first. One
filled silhouette, no interior detail, no colour, that says "carries a message" on its own, as
the satchel nearly does. Kill anything that fails that test before adding eyes, colour or the
accent. Then a 4-unit grid, two corner radii, at least 6 units between shapes, at least 85
percent canvas fill. Concrete geometric fixes for k2 and the cap are recorded in the crit
transcript and will be applied if the owner keeps the capped-bot direction.

### Rounds 12 to 16: the five-turn critic-gated iteration (2026-10-09)

Process per turn: draw as SVG, render the sheet (black 16 px silhouette first, then colour at
16 to 128 px light and dark with crops), a blind cold read by a context-free agent shown only
the 16 px silhouette ("what object is this?"), then a critic subagent scoring 1 to 10 with
geometric fixes applied verbatim next turn. Files in `svg/iter/`, final in `svg/final/`.

| Turn | Pool | Cold read at 16 px | Best score | What died |
|---|---|---|---|---|
| 1 | satchel x2, mailbag, post horn, capped heptagon bot, cap bot | fish, pawn, briefcase, crewmate | 5 (horn) | mailbag, strap satchel, capped heptagon (kagent formula, trademark-adjacent) |
| 2 | horn x2, cap, satchel | trumpet/bugle (megaphone alt), balloon, handbag | 5 (horn) | cap (bot-face route closed), satchel (kill rule) |
| 3 | horn, boomerang | trumpet/bugle, L-bracket | 5 (horn) | boomerang |
| 4 | horn A, horn B | French horn/trumpet, confidence 3, megaphone gone | 6 (A) | horn B (proud lip reads as a key) |
| 5 | final assets | bugle/trumpet, confidence 4 (favicon and mark) | 8 (mark) | nothing |

Closing crit: mark 8/10, favicon 6/10 as delivered (not truly pixel-hinted) and about 8 after
the pixel-exact replacement, lockup 6/10 as delivered (live text; Inter not installed on the
render machine, so nobody has seen the real one) and about 8 after outlining. The megaphone
residue in the cold reads is judged inherent to any flared horn at 16 px and on-message
("announces"); the magnifier read, the one that would have hurt, is gone.

### Rounds 17 and 18: "a delivery system to interface with AI agents" (owner reframe, 2026-10-09)

Owner's point after the horn: it relates to a courier only historically and to the agent not at
all. New brief: a delivery system, a mechanism rather than a courier figure. Turn 6 pool: a
delivery drone with a parcel (an autonomous vehicle that delivers), a dispatch K (the letter as
a route system with hub and endpoint nodes), the horn as control. `svg/iter/t6-*`, `turn6.png`.

Cold read at 16 px: drone "drone/quadcopter, camera pod below, confidence 4"; K "letter K,
confidence 5"; horn "horn/trumpet, confidence 4".

Critic (turn 6): drone 5 (a 7 is reachable), K 4 and dead, horn craft 8 but brief fit 3.
- **K killed, structurally.** A decorated letter: at 16 px it is a K, and the CNCF landscape is
  saturated with K marks (k3s, k0s, k9s, Knative, Kyverno, KEDA, Karmada, Kong, Kuma, KubeVirt);
  next to kagent it reads as a kagent sub-brand. Strip the stem and it is the share glyph.
- **Drone, conditional.** The single centre post is a gimbal mount, which is why the parcel read
  as a camera. Fix: a splayed two-cable sling and a 56x40 parcel as the dominant mass, ink
  recentred. Pass bar for the next cold read: "box", "package" or "parcel" named unprompted at
  16 or 24 px. And a decision for the owner before more work: the quadcopter silhouette's
  dominant 2026 associations are Amazon Prime Air, military FPV and police surveillance;
  "Agent" plus "autonomous drone" invites an autonomous-weapons reading; "drone" collides with
  Drone CI in the DevOps audience; and a drone is the courier figure again in robot form, which
  the reframe said to avoid.
- **On the brief itself.** None of the three says "interfaces with AI agents", and at 16 px no
  pictogram will. The critic's advice: let the wordmark's "Agent" carry that and judge the mark
  on "delivery system" alone.

Turn 7 applied the drone fix (`svg/iter/t7-drone.svg`, `turn7.png`). Cold read: at 16 px "a
quadcopter drone carrying a box or package/parcel slung underneath, confidence 4"; at 24 px "a
drone with two rotors carrying a package or box, confidence 4". The critic's pass bar ("box",
"package" or "parcel" named unprompted) is met at both sizes on the first try. Not yet critic-
scored; the drone-associations decision is the owner's and gates further work.

### Rounds 19 and 20: "courier, messaging, interface" (owner, 2026-10-09)

Owner rejected the drone ("why a drone?") and set three words: courier, messaging, interface.
Reading: an interface is where two systems meet and things pass through; put a message at the
point of passage. Turn 8 drew four: a message plug entering a socket, a mail slot with a letter
through it, a gateway arch with a letter, an envelope between angle brackets (`svg/iter/t8-*`).

Cold read at 16 px: barbell, T-junction, archway, monitor. All four failed: the envelope was
interior detail and the container read alone.

Critic (turn 8): plug 4, slot 3, gateway 3, brackets 2. Two findings that matter beyond this
turn:
- **Harness bug.** The silhouette step recoloured white knockouts to black, so every mark whose
  identity lived in a white cut (envelope flaps here; eyes and visors in the bot rounds) was
  shown to the cold reader without it. Fixed in `svg/sheet.py` (white stays background). The
  earlier cold reads of white-interior marks are suspect; the horn, drone and K had no white and
  stand.
- **"Passing through" cannot read at 16 px.** It is a verb and a scene; a silhouette carries a
  noun, and a scene collapses to its larger mass, the container. What can read is "a message at
  the threshold" with the envelope as the outline and the flap cut through it.

Containers: socket dead (power-plug icon), arch fatal (horseshoe magnet), brackets fatal (the
code icon; "envelope in brackets" is the stock HTML-email glyph). Slot survives only as the idea
of a letterbox, which leads to the critic's proposal: an interface object that is itself a
messaging object. Turn 9 pool: **the rural mailbox with the flag up** (the courier's pickup
point; it holds the post; an addressable endpoint; the raised flag is the accent and means "a
message is waiting", which is the brief's own rule for the accent) and the **posted letter** as
control (`svg/iter/t9-*`, `turn9.png`).

Turn 9 cold read at 16 px: mailbox "a rural curbside mailbox with the flag raised, confidence
4" (alternatives toaster, kettle on a stand); posted letter "open envelope on a tray, confidence
3". Critic: **mailbox 6/10**, the best cold read in nine turns; posted letter 4 and dropped
(Gmail's M in negative space, the slot cannot read). Defects: flag too light (a 16x16 panel is
2 px at 16 px) and fused to the body, body too squat (2:1), post reads as a stand, right-heavy,
flag clipped by a full-bleed circle crop. Against the three words: messaging strong, courier
medium (a raised flag is literally "carrier, please collect" in the US convention; elsewhere
"has mail"), interface medium (the endpoint between a household and the postal network).
Recognition is universal and helped by the mailbox emoji, which also sits in Slack's picker.
Risk: reads as "email app" first; mitigated by the post and side view. Distinctiveness: no CNCF
mailbox; xbiff is the nearest precedent and a friendly one. Forecast 7 after one tuning turn;
ceiling is clarity, not ownability, because it is an emoji-grade object and says nothing
about agents.

Turn 10 applied the tuning (`svg/iter/t10-mailbox.svg`, avatar variant `t10-mailbox-avatar.svg`
scaled 0.8 for circle crops, `turn10.png`): body 104x44 with an r32 front, 16x32 post, 24x16
flag panel with a 12-unit gap above the body, 4 units left for optical centring.

Turn 10 cold read at 16 px: "US rural curbside mailbox with the flag up, confidence 4"
(alternatives a bird, a loaf of bread); asked what software it would be the logo of: "email or
messaging client; alternatively a notification, inbox or message-delivery service". Not yet
critic-scored after the tuning.

The critic also raised the name: Knative ships an ingress called Kourier (net-kourier), in the
same ecosystem; and Courier (courier.com) is a notifications API. A trademark and
search-confusion check on "Agent Kourier" should precede further identity investment.

### Round 21: owner's position and Codex's second opinion (2026-10-10)

Owner: because the two-word name is a rule, the mark should represent both agent and courier.
Codex (gpt-6.1-sol, high reasoning) was asked for
its opinion and ideas. Its position: one mark can suggest both, but no one should promise that
an unbriefed reader names both from a 16 px silhouette; the principle that can work is one
delivery object whose structure is robotic (wheels, joints, a gripper carry "agent"; the cargo
or letter carries "courier"), never a head with props. Two size treatments of the same object.
It agrees with the critics on the capped-head family and on props vanishing; it disagrees that
the robot head is the only agent glyph or that the agent cannot be the subject.

Its five concepts: autonomous delivery rover (parcel body on two exposed wheels, roof sensor,
amber seal); robotic gripper holding a letter (wrist, two jaws, envelope in the lower half);
walking envelope (upright envelope with jointed legs); mechanical envelope (envelope with
motor housings and two apertures, close to the face convention); messenger automaton in
profile (full-body robot carrying a letter, restrained). Ranking: rover, then gripper, with
the walking envelope as the risky third. Rough construction for the top two is in the file.

Process criticisms worth keeping: the cold read tests recognition, not explanation, so an
incomplete verbal answer is not a failed logo; the 6-unit gap rule is too thin (12 to 15 units
for essential separations); model cold reads are screening tools and real unfamiliar people,
including non-US developers, should see actual-size candidates before a vocabulary is declared
impossible; the brief has changed after every verdict and should be frozen for one exploratory
round comparing structures before refining; and the naming concern is better served by keeping
both words equally prominent (one wordmark weight) than by forcing two meanings into a favicon.

### Round 22: Codex's three concepts on the image models (2026-10-10)

Owner: run the frozen round with the OpenRouter models. Three prompts in the playbook shape with
Codex's construction notes as the concept paragraph, each a 2 by 2 treatment sheet, GPT-5.4
Image 2 and Gemini 3 Pro Image, four CNCF icons as references (`generated/8*-sheet-*.png`,
prompts `generated/prompts/8*`). Six calls, $1.42.

First impression before the crit: the gripper on GPT is the strongest first read of the
project, a claw holding an envelope with the amber flap; the rover reads as a toolbox or
cooler on wheels on both models; the walking envelope is charming and squarely a mascot.
Critic (round 22): best treatments gripper GPT 2, rover GPT 4, walking envelope GPT 1; direction
scores gripper 4, rover 5, walking envelope 3. Gripper: the only courier cue is a white flap gap
that dies at 16 px, and a claw's verb is "grab", not "deliver"; arcade claw. Walking envelope:
cartoon by construction, the V-flap and seal read as a shirt collar and button, so it is the
head-with-props problem again. Rover: a lidded box on wheels reads as a cooler or toolbox; the
roof sensor is a carry handle. None of the six treatments as drawn beats the mailbox.

The critic then built the one direction that can: the rover redrawn as a cab-less delivery van
carrying the mailbox's amber flag (`svg/iter/t11-rover.svg`, `turn11.png`): body 104x48 with a
chamfered nose, wheel wells cut by mask, r12 wheels, the flag pole rising from inside the body.
No navy, no sensor dome (it turns back into a handle), no side label, no eyes. Its own 16 px
render: "delivery van with a flag". Agent is carried only as a one-sentence story (no cab, no
driver, it drives itself and raises the flag when the reply is in), never as a glyph. Its
verdict: no glyph without a face makes a stranger say "AI agent" at 16 px; if the bar is
literal recognition of both words nothing clears it; if the bar is a mark both words explain in
one sentence while staying distinctive, the rover clears it and the mailbox does not.

Model cold read of the rover vector at 16 px: "a toy tractor or small bulldozer/truck;
alternatives a tank, a pickup truck; confidence 3"; software guess: "a deployment/CI-CD tool
that ships builds, or a data pipeline that moves data". Delivery is implied (ships, moves),
courier and agent are not named. The mailbox's read (mailbox, confidence 4, messaging then
message delivery) remains the stronger.

Most valuable next step per the critic: a blind, actual-size human cold read, six to eight
developers who have not seen the project, at least two outside the US, shown the rover, the
mailbox and the horn at 16 and 32 px, asked "what is this?" and "what would software with this
icon do?". If anyone says delivery robot, self-driving or "it brings things" unprompted, the
rover direction ships; if nobody does, "agent" lives in the wordmark and the mailbox stays.

### Round 23: owner keeps the wings, asks how to capture the agent (2026-10-10)

Owner: not a fan of the Codex-concept sheets; likes the wings/feathers of the first SVG pass
(`svg/courier-agent-test.png`) but the "AI agent" is not captured. Codex consulted again with
the three wing sheets attached.

Codex: keep the wings, retire the visor; two dots in a capsule establish "a chatbot avatar" and
say nothing about sensing, deciding or acting; caps and antennae cannot fix it because they
vanish before the face does. Wings that survive 16 px: two feathers per side, diagonal blades
at 35 to 45 degrees with the upper one longer, 12 to 16 units thick with 12-unit gaps, roots
joined into a centre at least half the mark's width. Neighbours: Harley's winged shield,
Goodyear's winged foot, aviation and military badges; Hermes the messenger versus Hermès the
house. Five constructions: a one-eyed messenger automaton (chamfered body, one large sensor,
an amber message held below it), a mechanical head in profile, a winged processing core, a
flying gripper, a message-shaped automaton. First choice: the one-eyed automaton, with full
coordinates. Its stated limit: no treatment it knows makes an unfamiliar reader say "AI
agent" at 16 px while avoiding robot conventions; "robot" or "camera" are the likely reads;
the honest compromise is one winged machine simplified across sizes, with "Agent Kourier"
equally prominent in the wordmark.

Author's parallel proposal: the wings as the mark with the agent reduced to a single round
lens at the centre. Both drawn (`svg/iter/t12-*`, `turn12.png`) and cold read at 16 px:
Codex's automaton "a beetle or scarab; alternatives robot, owl face; confidence 3; software: a
bug tracker or security scanner"; the winged lens "a spider; alternatives ornament, compass;
confidence 2; software: a web crawler". Finding: one eye plus wings reads as an insect. The
single-lens route is closed. The original winged visor, two eyes, read as a bot with wings.

Turn 13: the one untried combination, the two-eye visor on a blue head with Codex's wing
specification (two diagonal blade feathers per side, 35 to 45 degrees, upper longer, 12 to 16
units thick) and an amber message held below the visor (`svg/iter/t13-winged-visor-blades.svg`,
`turn13.png`). Cold read at 16 px: "a bug or beetle from above, scarab or spider; alternatives a
skull with crossed swords, a ninja or masked face; confidence 2; software: a debugger or
security scanner". Finding: diagonal blade feathers on a central body read as insect legs at
16 px regardless of the head. The original horizontal bars (t0) are the only wing geometry so
far that read as "winged bot" rather than an insect, even though the critic scored them as UI
glyphs. Next wing geometry to try: horizontal, tapered feather shapes, two per side, heavy.

### Round 24: six centres on the same wings (2026-10-10)

Owner: likes the winged visor; asks whether the head is really the best representation of an
AI agent. Test: hold the wings constant (two heavy horizontal tapered feathers per side) and
vary only the centre (`svg/iter/c1` to `c6`, `turn14.png`): navy visor head (current), blue
visor head with an amber message, a scanner band with no eyes, a carrier unit holding the
message, a domed helmet, a speech-bubble head.

Cold read at 16 px: five of six read as a bee or beetle (the sixth, the bubble, as an eye).
Any vertical body between horizontal wings is an insect at 16 px whatever the centre is. The
software guesses are the first in the project to describe the product: c1 and c2 "a messaging
or task-dispatch tool that carries notifications between services", c4 "a delivery or queueing
tool that moves messages between systems"; c3 "security scanner", c5 "monitoring agent", c6
"observability tool". Collision to note: Cilium's logo is a bee.

Reading: the centre does not change the 16 px read, only the 32 px and up read. At 64 px the
blue visor (c2) is the generic bot, the scanner (c3) a welding mask, the carrier (c4) a unit
holding a parcel with no face, the helmet (c5) a pilot, the bubble (c6) a chat bot. Two
candidates for the centre: c2 if "agent" must be literal (a bot holding a message), c4 if
distinctiveness matters more (the only one with no face that still drew a delivery read).

Codex's pick: **c4, the carrier unit**, with a square
sensor instead of a dot and a wider housing (62 percent of the mark's width), horizontal
feathers kept. On the owner's question: a visor is the clearest shorthand for a robot but not
the best choice for this brand, because it repeats kagent's and every chatbot's organising
feature; a sensor establishes a machine but cannot establish autonomous reasoning, and that
limit should be accepted rather than solved with another eye. For 16 and 24 px: the carrier
centre alone, no wings (reversing its earlier "wings at every size"); the full winged mark from
32 px. Cilium: its formal icon is a honeycomb and the bee is a mascot; the risk is "another
Kubernetes bee", mitigated by the rectangular housing, flat base, square sensor, angular
feathers, no antennae or stripes. Honest expectation: at 16 px "a small device with a yellow
panel, a locker, occasionally a robot"; at 64 px "a winged machine, a delivery robot, a device
carrying a box"; suggests a mechanical agent and a courier, does not independently identify AI.

Drawn to Codex's coordinates (`svg/iter/t15-carrier-winged.svg`, `t15-carrier-favicon.svg`,
`turn15.png`) and cold read at 16 px: the winged carrier "a bucket or cooking pot; alternatives
acorn, mailbag; confidence 2; software: a container or storage tool"; the wingless favicon "a
phone or tablet; alternatives tombstone, doorway; confidence 3; software: a mobile or messaging
app". Author's reading: the 62-percent housing shrinks the feathers to stubs, so the mark loses
the winged read without gaining anything; the author's narrower c4 (round 24) read as a bee
but drew "a delivery or queueing tool that moves messages between systems", which is closer to
the product than "storage". The wings must stay dominant for the courier to survive.

### Round 25: the agent conventions between the owner's wings (2026-10-10)

Owner: still likes the winged visor; the wings are good; work the middle. Asked for the common
AI-agent representations and how each would replace the centre. The wings are kept exactly as
in the first pass (three stepped bars per side, blue). Six centres from the taxonomy
(`svg/iter/a1` to `a6`, `turn16.png`): the robot face (baseline), the sparkle (navy with an
amber core), a node graph (amber node as the agent), an orb (navy disc, white ring, amber
core), a loop (circular arrow with an amber head), a chip (square with pins, amber core). The
eye and the speech bubble were tested in rounds 23 and 24 (insect and eye reads). Blind reads:

At 16 px (silhouette) all six are bats, moths or winged badges; the centre is invisible, as in
round 24.

At 48 px in colour, asked what it is, what the software would do, and whether it involves AI:
- **robot face**: "winged robot or ninja face, confidence 4; a fast messenger or delivery bot,
  a chatbot that carries messages between systems; AI yes".
- sparkle: "winged four-point star, 3; an assistant or notification service; AI yes".
- node graph: "winged graph, 4; moves data between connected nodes, workflow routing or a
  messaging network; AI unsure".
- orb: "aviation winged badge with a target, 4; observability or alerting; AI unsure".
- loop: "winged refresh icon, 4; sync, retries, CI/CD; AI unsure".
- chip: "winged microchip, 5; compute or an AI/ML inference platform; AI yes".

Finding: the robot face is the only centre that produced both "AI: yes" and "carries messages
between systems" from a cold reader; it is the first candidate in the project to be read as
the product. The chip says AI but compute, not courier; the sparkle says AI but assistant; the
graph, orb and loop say infrastructure without AI. The owner's instinct to keep the winged
visor is supported by the data; the centre to keep is the robot face, and the remaining work
is craft (wing weight, the navy-head ninja read, a wingless favicon), not concept.

### Round 26: image-model ideas for the centre, wings fixed by reference (2026-10-10)

Owner: did not like the six drawn centres; asked the image models for ideas with the original
winged visor supplied as the reference. Two briefs, each a 2 by 2 sheet, GPT and Gemini, the
winged-visor render plus three CNCF icons attached: "agent heads that are not the stock chatbot
face" and "agent centres that are machines, not heads" (`generated/9agent-centre-*`). Four
calls, $0.91.

Critic: of sixteen quadrants, one deserves a vector pass, Gemini heads 1, the courier helmet:
with the wings it becomes the Hermes winged helmet, one object instead of a box with wings
bolted on, distinct from kagent and the robot emoji, and the amber finally means something (the
visor). Scores otherwise 1 to 5: GPT's heads are a ninja with a letter in its teeth, a kagent
hexagon with a camera eye, the reference head on a torso (toy robot), a skewed webcam; the
machines are a USB stick, a phone with mail, a luggage tag, a padlock; Gemini's others are
Instagram's camera, a chatbot with a name tag, a trash can, WALL-E's Eve, a fax, a CPU badge,
a Prime Air drone. Several illegal for the brief (text, outlines, gloss, 3D, navy wings).
The critic flattened the helmet into a vector (`svg/iter/t17-winged-helmet.svg`, `turn17.png`):
squat dome r20 with straight sides and r8 base, amber visor pill 32x16 concentric with the dome,
no chin notch (Among Us), no eyes (navy dots render as a balaclava). Favicon: shell and visor
only. Its verdict: this round moved the courier story forward and the agent problem sideways;
eyes remain the only cue shown to read as AI, and everything that dropped them traded the AI
read for distinctness.

Blind read of the helmet: 16 px silhouette "a bat or moth; alternatives a black helmet with
wings, a beetle; confidence 3"; 48 px colour "a winged pilot or racing helmet, confidence 4; a
fast delivery or courier dispatch service that carries messages between systems; AI: unsure".
So: courier strong, agent unsure, exactly as the critic forecast. Against the robot face (round
25: "a messenger bot, AI yes"), the trade is explicit: the helmet is distinctive and says
courier; the robot face says AI and is everyone's robot.

## 5. Decision and assets (2026-10-10)

The owner chose **a1, the winged visor**: two sets of three horizontal feathers (the courier)
either side of a navy robot head with a white visor and two eyes (the agent). The evidence
behind it: at 48 px it was the only centre a cold reader called "a messenger bot that carries
messages between systems, AI: yes" (round 25); the helmet alternative read as courier with AI
unsure (round 26). Known trade: it is a familiar robot; the wings, the blue and the message
make it this project's.

Assets in `assets/` (SVG masters and variants, favicon ladder with a head-only 16 and 24 px,
Apple and Android icons, maskable PWA icon, web manifest, GitHub avatar and social preview,
Open Graph, X, LinkedIn, Slack and YouTube images, PNG ladders, lockups), regenerated by
an export script that is not kept in the repository. Rules and HTML snippets in `assets/README.md`.

Open before publishing: outline the lockup text in Inter (live text in the SVGs, Helvetica
fallback in the PNGs); the "Agent Kourier" trademark and search-confusion check (Knative's
Kourier ingress, courier.com); a human cold read of the favicon head at 16 px.

Superseded: everything in sections 4a to 4h and rounds 12 to 26 other than as history.
