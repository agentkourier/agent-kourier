# Agent Kourier brand assets

Master: `svg/agent-kourier-mark.svg` (the a1 winged visor: Kubernetes-blue feathers, navy head,
white visor, navy eyes). Everything else is derived from it; regenerate the PNGs with
`node docs/brand/assets/export.mjs` after editing an SVG, then rebuild `favicon.ico`.

Palette: Kubernetes blue `#326CE5`, navy `#1E3A8A`, white. Dark backgrounds use the dark
variant (feathers `#5B8DEF`, head `#326CE5`) because navy on near-black is 1.6:1.

## SVG (vector source)
| File | Use |
|---|---|
| `svg/agent-kourier-mark.svg` | The mark on white or light backgrounds |
| `svg/agent-kourier-mark-dark.svg` | The mark on dark backgrounds |
| `svg/agent-kourier-mark-white.svg` | One colour, white, on Kubernetes blue |
| `svg/agent-kourier-mark-mono.svg` | One colour, black: print, stamps, embossing |
| `svg/favicon-head.svg`, `-dark.svg` | The head alone for 16 and 24 px, where the full mark reads as an insect |
| `svg/lockup-horizontal.svg`, `-dark.svg` | Mark plus "Agent Kourier", Inter 600, single weight so neither word dominates |
| `svg/lockup-stacked.svg`, `-dark.svg` | Centred mark over the name |

The lockups carry live `<text>`. Before distributing them, outline the text in Inter (not
installed on the machine that built these; the PNGs below used the Helvetica fallback).

## Favicon and app icons (`favicon/`)
| File | Use |
|---|---|
| `favicon.ico` | 16 (head only), 32 and 48 (full mark); `<link rel="icon" href="/favicon.ico">` |
| `favicon-16.png`, `favicon-24.png` | Head only |
| `favicon-32.png`, `-48.png`, `-64.png` | Full mark, transparent |
| `apple-touch-icon-180.png` | iOS home screen; opaque white, mark at 82 percent |
| `android-chrome-192.png`, `-512.png` | Android and PWA; opaque white |
| `maskable-512.png` | PWA maskable: white mark on blue, inside the 80 percent safe zone |
| `site.webmanifest` | References the three above |

HTML head:
```html
<link rel="icon" href="/favicon.ico" sizes="16x16 32x32 48x48">
<link rel="icon" type="image/svg+xml" href="/agent-kourier-mark.svg">
<link rel="apple-touch-icon" href="/apple-touch-icon-180.png">
<link rel="manifest" href="/site.webmanifest">
<meta name="theme-color" content="#326CE5">
```

## GitHub (`github/`)
| File | Use |
|---|---|
| `avatar-1024.png` | Organisation and repository avatar; GitHub circle-crops, the mark sits at 76 percent so the wings survive |
| `avatar-dark-1024.png` | Alternative avatar for a dark profile |
| `social-preview-1280x640.png` | Repository Settings > Social preview |

## Social (`social/`)
| File | Use |
|---|---|
| `og-image-1200x630.png`, `og-image-dark-1200x630.png` | Open Graph and Twitter card: `<meta property="og:image">` |
| `x-profile-400.png`, `x-header-1500x500.png` | X profile and header |
| `linkedin-profile-300.png`, `linkedin-cover-1128x191.png` | LinkedIn page |
| `slack-app-icon-512.png` | Slack app directory icon (white, mark at 76 percent; Slack rounds the corners) |
| `youtube-avatar-800.png` | YouTube channel |

## PNG ladders (`png/`)
`mark-{16..1024}.png` transparent, `mark-dark-*` transparent for dark UIs, `mark-white-on-blue-*`,
`mark-mono-*`, and the lockups at 2x for READMEs.

## Rules
- 16 and 24 px: head only. 32 px and up: the full mark.
- Clear space: one head-width on every side of the ink.
- Never recolour the head purple or put the mark in a hexagon (kagent).
- Always "Agent Kourier", never "Kourier" alone (Knative's net-kourier).
