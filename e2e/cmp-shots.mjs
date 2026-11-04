// Compares two folders of `make shots` PNGs in the browser: prints the share
// of pixels that differ for each shot and writes <name>-diff.png (changed
// pixels in magenta) for any that do. Usage: node cmp-shots.mjs <before> <after>
import { chromium } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';

const [before, after] = process.argv.slice(2);
const browser = await chromium.launch();
const page = await browser.newPage();
const rows = [];
for (const f of fs.readdirSync(before).filter((f) => f.endsWith('.png') && !f.endsWith('-diff.png')).sort()) {
  if (!fs.existsSync(path.join(after, f))) { rows.push({ shot: f, diff: 'missing' }); continue; }
  const [a, b] = [before, after].map((d) => 'data:image/png;base64,' + fs.readFileSync(path.join(d, f)).toString('base64'));
  const res = await page.evaluate(async ([a, b]) => {
    const load = (src) => new Promise((ok) => { const i = new Image(); i.onload = () => ok(i); i.src = src; });
    const [ia, ib] = await Promise.all([load(a), load(b)]);
    const w = Math.max(ia.width, ib.width), h = Math.max(ia.height, ib.height);
    const px = (img) => { const c = new OffscreenCanvas(w, h), x = c.getContext('2d'); x.fillStyle = '#fff'; x.fillRect(0, 0, w, h); x.drawImage(img, 0, 0); return x.getImageData(0, 0, w, h); };
    const da = px(ia), db = px(ib);
    const out = new OffscreenCanvas(w, h), ox = out.getContext('2d'), od = ox.createImageData(w, h);
    let n = 0;
    for (let i = 0; i < da.data.length; i += 4) {
      const d = Math.abs(da.data[i] - db.data[i]) + Math.abs(da.data[i + 1] - db.data[i + 1]) + Math.abs(da.data[i + 2] - db.data[i + 2]);
      if (d > 48) { n++; od.data.set([255, 0, 255, 255], i); } else { const g = (da.data[i] + da.data[i + 1] + da.data[i + 2]) / 3; od.data.set([g, g, g, 60], i); }
    }
    ox.putImageData(od, 0, 0);
    const blob = await out.convertToBlob();
    const buf = new Uint8Array(await blob.arrayBuffer());
    let bin = ''; for (const c of buf) bin += String.fromCharCode(c);
    return { share: n / (w * h), size: `${ia.width}x${ia.height} -> ${ib.width}x${ib.height}`, png: n ? btoa(bin) : null };
  }, [a, b]);
  if (res.png) fs.writeFileSync(path.join(after, f.replace(/\.png$/, '-diff.png')), Buffer.from(res.png, 'base64'));
  rows.push({ shot: f, diff: (res.share * 100).toFixed(2) + '%', size: res.size });
}
console.table(rows);
await browser.close();
