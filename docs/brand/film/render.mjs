// Renders the Agent Kourier film from scene.html.
//
//   node render.mjs all                  videos, posters, contact sheets, storyboard
//   node render.mjs video [16x9 ...]     MP4s only (default: every format)
//   node render.mjs frames 2.5,6,14      stills at those times, every format
//   node render.mjs posters | contact | storyboard
//
// Needs Playwright's Chromium (npx playwright install chromium) and an ffmpeg with libx264
// on PATH, or FFMPEG=/path/to/ffmpeg.
import { chromium } from 'playwright';
import { spawn } from 'node:child_process';
import { mkdir, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, 'out');
const SCENE = pathToFileURL(join(HERE, 'scene.html')).href;
const FFMPEG = process.env.FFMPEG || 'ffmpeg';

const FORMATS = { '16x9': [1920, 1080], '9x16': [1080, 1920], '1x1': [1080, 1080] };
const POSTER_T = 16.5;

// The storyboard: one row per shot. Every claim traces to spec.md sections 1, 2.2 and 5.
const SHOTS = [
  { at: 2.2, span: '0.0 – 3.3 s', title: 'The gap',
    picture: 'Alertmanager pops in and fires (red rings, FIRING badge). An A2A agent sits asleep. A dashed red line reaches for it and dies halfway.',
    copy: 'An alert fires. / Your agent never hears it.',
    why: 'Agents have no alert or chat front door, and a sleeping agent misses events (spec 2).' },
  { at: 5.8, span: '3.2 – 7.3 s', title: 'Kourier carries it',
    picture: 'The Kourier hex K spins in at centre; Slack joins. Edges draw. The red alert packet enters Kourier and leaves as a blue task; the agent wakes.',
    copy: 'Agent Kourier carries it in. / Alertmanager and Slack, to any A2A agent.',
    why: 'Alerts and Slack messages become agent sessions; Kourier is always on and calls the agent awake (spec 1).' },
  { at: 9.9, span: '7.4 – 11.2 s', title: 'Back into the thread',
    picture: 'Findings travel agent → Kourier → Slack and fill an abstract thread. A white reply travels back to the same agent.',
    copy: 'Findings land in the thread. / Reply there, and the session continues.',
    why: 'The thread is the live session; anyone can reply to continue it (spec 1, 2.2).' },
  { at: 13.4, span: '11.1 – 14.3 s', title: 'What it is',
    picture: 'Diagram recedes. Four capability pills stagger in under the header.',
    copy: 'A Kubernetes-native broker: any A2A agent; always on; declarative resources; an extra receiver.',
    why: 'Real resource kinds only: AgentBackend, ChatConnection, Binding. No metrics.' },
  { at: 16.5, span: '14.3 – 17.0 s', title: 'End card',
    picture: 'Logo spins in, wordmark slides, tagline and stack line follow. Holds to the end (poster frame).',
    copy: 'Agent Kourier / Alerts and chat, carried to your agents. / Alertmanager · Slack · A2A',
    why: 'Brand lockup in the logo drafts idiom: regular "Agent", bold "Kourier".' },
];

async function openScene(browser, ar) {
  const [w, h] = FORMATS[ar];
  const page = await browser.newPage({ viewport: { width: w, height: h }, deviceScaleFactor: 1 });
  await page.goto(`${SCENE}?ar=${ar}&t=0`);
  await page.waitForFunction(() => window.READY === true);
  await page.evaluate(() => document.fonts.ready);
  return page;
}

async function still(page, t, file) {
  await page.evaluate(t => window.renderAt(t), t);
  await page.screenshot({ path: file, type: 'png' });
}

async function video(browser, ar) {
  const page = await openScene(browser, ar);
  const { duration, fps } = await page.evaluate(() => ({ duration: window.DURATION, fps: window.FPS }));
  const file = join(OUT, `agent-kourier-${ar}.mp4`);
  const ff = spawn(FFMPEG, ['-y', '-loglevel', 'error', '-f', 'image2pipe', '-framerate', String(fps), '-i', '-',
    '-c:v', 'libx264', '-preset', 'slow', '-crf', '16', '-pix_fmt', 'yuv420p', '-movflags', '+faststart',
    '-metadata', 'title=Agent Kourier', file], { stdio: ['pipe', 'inherit', 'inherit'] });
  const done = new Promise((res, rej) => ff.on('close', c => (c === 0 ? res() : rej(new Error(`ffmpeg exited ${c}`)))));
  const frames = Math.round(duration * fps);
  for (let i = 0; i < frames; i++) {
    await page.evaluate(t => window.renderAt(t), i / fps);
    const buf = await page.screenshot({ type: 'png' });
    if (!ff.stdin.write(buf)) await new Promise(r => ff.stdin.once('drain', r));
  }
  ff.stdin.end();
  await done;
  await page.close();
  console.log(`wrote ${file} (${frames} frames)`);
}

async function frames(browser, times) {
  await mkdir(join(OUT, 'frames'), { recursive: true });
  for (const ar of Object.keys(FORMATS)) {
    const page = await openScene(browser, ar);
    for (const t of times) await still(page, t, join(OUT, 'frames', `${ar}-${t.toFixed(2)}.png`));
    await page.close();
  }
}

async function posters(browser) {
  for (const ar of Object.keys(FORMATS)) {
    const page = await openScene(browser, ar);
    await still(page, POSTER_T, join(OUT, `poster-${ar}.png`));
    await page.close();
  }
}

// A grid of frames, one per second, with timecodes.
async function contact(browser) {
  for (const ar of Object.keys(FORMATS)) {
    const page = await openScene(browser, ar);
    const duration = await page.evaluate(() => window.DURATION);
    const cells = [];
    for (let t = 0.5; t < duration; t += 1) {
      await page.evaluate(t => window.renderAt(t), t);
      cells.push({ t, src: `data:image/jpeg;base64,${(await page.screenshot({ type: 'jpeg', quality: 85 })).toString('base64')}` });
    }
    await page.close();
    const [w, h] = FORMATS[ar];
    const cols = ar === '9x16' ? 9 : ar === '1x1' ? 6 : 6;
    const cw = ar === '9x16' ? 200 : ar === '1x1' ? 300 : 360;
    const html = `<!doctype html><meta charset="utf-8"><style>
      body{margin:0;background:#0A1530;color:#fff;font:600 15px -apple-system,Helvetica,Arial,sans-serif;padding:28px}
      h1{font-size:22px;margin:0 0 4px} p{margin:0 0 20px;opacity:.7;font-weight:400}
      .g{display:grid;grid-template-columns:repeat(${cols},${cw}px);gap:14px}
      figure{margin:0} img{width:${cw}px;height:${Math.round(cw * h / w)}px;display:block;border-radius:6px;outline:1px solid #1D3F91}
      figcaption{margin-top:6px;font-family:"SF Mono",Menlo,monospace;font-size:13px;opacity:.75}
      </style><h1>Agent Kourier · contact sheet · ${ar.replace('x', ':')}</h1>
      <p>${duration} s at 30 fps · one frame per second, sampled at t + 0.5 s</p>
      <div class="g">${cells.map(c => `<figure><img src="${c.src}"><figcaption>${c.t.toFixed(1)} s</figcaption></figure>`).join('')}</div>`;
    const sheet = await browser.newPage({ viewport: { width: 56 + cols * cw + (cols - 1) * 14, height: 400 } });
    await sheet.setContent(html);
    await sheet.screenshot({ path: join(OUT, `contact-sheet-${ar}.png`), fullPage: true });
    await sheet.close();
  }
  console.log('wrote contact sheets');
}

async function storyboard(browser) {
  const page = await openScene(browser, '16x9');
  const rows = [];
  for (const s of SHOTS) {
    await page.evaluate(t => window.renderAt(t), s.at);
    rows.push({ ...s, src: `data:image/jpeg;base64,${(await page.screenshot({ type: 'jpeg', quality: 88 })).toString('base64')}` });
  }
  await page.close();
  const esc = s => s.replace(/&/g, '&amp;').replace(/</g, '&lt;');
  const html = `<!doctype html><html lang="en"><meta charset="utf-8"><title>Agent Kourier storyboard</title><style>
    @page{size:A4 landscape;margin:12mm}
    body{margin:0;background:#0A1530;color:#fff;font:400 15px/1.45 -apple-system,Helvetica,Arial,sans-serif;padding:32px;-webkit-print-color-adjust:exact;print-color-adjust:exact}
    h1{font-size:26px;margin:0 0 6px;letter-spacing:-.01em} .lead{opacity:.8;margin:0 0 6px;max-width:1100px}
    .meta{font-family:"SF Mono",Menlo,monospace;font-size:13px;opacity:.65;margin:0 0 24px}
    .shot{display:grid;grid-template-columns:480px 1fr;gap:24px;padding:18px 0;border-top:1px solid #1D3F91;break-inside:avoid}
    img{width:480px;height:270px;border-radius:8px;display:block;outline:1px solid #1D3F91}
    .n{font-family:"SF Mono",Menlo,monospace;color:#fff;background:#326CE5;border-radius:4px;padding:1px 7px;font-size:13px;margin-right:8px}
    h2{font-size:19px;margin:2px 0 10px} .span{font-family:"SF Mono",Menlo,monospace;font-size:13px;opacity:.65;margin-left:6px;font-weight:400}
    dl{margin:0;display:grid;grid-template-columns:90px 1fr;gap:6px 12px} dt{opacity:.6;font-size:13px;text-transform:uppercase;letter-spacing:.06em;padding-top:2px} dd{margin:0}
    .copy{font-weight:600}
    </style>
    <h1>Agent Kourier · storyboard</h1>
    <p class="lead">Agent Kourier is a Kubernetes-native broker that carries Alertmanager alerts and Slack messages to any A2A agent, wakes it if needed, and brings its findings back into the Slack thread, where people keep the session going.</p>
    <p class="meta">17 s · 30 fps · 16:9, 9:16, 1:1 · silent · palette #326CE5 / #FFFFFF / #0A1530, signal #E5484D for the firing alert only · audience: developers and Kubernetes operators</p>
    ${rows.map((r, i) => `<section class="shot"><img src="${r.src}" alt="">
      <div><h2><span class="n">${i + 1}</span>${esc(r.title)}<span class="span">${r.span}</span></h2><dl>
      <dt>Picture</dt><dd>${esc(r.picture)}</dd><dt>On screen</dt><dd class="copy">${esc(r.copy)}</dd><dt>Basis</dt><dd>${esc(r.why)}</dd>
      </dl></div></section>`).join('')}
    </html>`;
  await writeFile(join(OUT, 'storyboard.html'), html);
  const sb = await browser.newPage({ viewport: { width: 1280, height: 800 } });
  await sb.setContent(html);
  await sb.screenshot({ path: join(OUT, 'storyboard.png'), fullPage: true });
  await sb.pdf({ path: join(OUT, 'storyboard.pdf'), format: 'A4', landscape: true, printBackground: true });
  await sb.close();
  console.log('wrote storyboard');
}

const [cmd = 'all', ...args] = process.argv.slice(2);
await mkdir(OUT, { recursive: true });
const browser = await chromium.launch();
try {
  if (cmd === 'frames') await frames(browser, (args[0] || '2,6,10,13,16.5').split(',').map(Number));
  else if (cmd === 'video') for (const ar of args.length ? args : Object.keys(FORMATS)) await video(browser, ar);
  else if (cmd === 'posters') await posters(browser);
  else if (cmd === 'contact') await contact(browser);
  else if (cmd === 'storyboard') await storyboard(browser);
  else if (cmd === 'all') {
    for (const ar of Object.keys(FORMATS)) await video(browser, ar);
    await posters(browser);
    await contact(browser);
    await storyboard(browser);
  } else throw new Error(`unknown command ${cmd}`);
} finally {
  await browser.close();
}
