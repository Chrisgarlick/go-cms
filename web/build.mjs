// Builds web/dist from web/src. Output is committed so `go build` needs no Node.
import * as esbuild from 'esbuild';
import { copyFileSync, mkdirSync, rmSync } from 'node:fs';

const watch = process.argv.includes('--watch');
mkdirSync('dist', { recursive: true });
for (const f of ['icons.svg', 'favicon.svg']) copyFileSync(`src/${f}`, `dist/${f}`);
rmSync('dist/kit', { recursive: true, force: true });

const opts = {
  // admin.css is kit's CSS (src/kit, vendored by `make kit`) and the
  // admin's own, as one minified stylesheet.
  entryPoints: [{ in: 'src/admin.js', out: 'admin' }, { in: 'src/editor/main.js', out: 'editor' }, { in: 'src/admin.entry.css', out: 'admin' }],
  bundle: true,
  format: 'esm',
  minify: !watch,
  sourcemap: false,
  target: ['es2021'],
  outdir: 'dist',
  legalComments: 'none',
  logLevel: 'info',
};
if (watch) {
  const ctx = await esbuild.context(opts);
  await ctx.watch();
} else {
  await esbuild.build(opts);
}
