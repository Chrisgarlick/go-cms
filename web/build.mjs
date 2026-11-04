// Builds web/dist from web/src. Output is committed so `go build` needs no Node.
import * as esbuild from 'esbuild';
import { copyFileSync, cpSync, mkdirSync, rmSync } from 'node:fs';

const watch = process.argv.includes('--watch');
mkdirSync('dist', { recursive: true });
for (const f of ['admin.css', 'icons.svg', 'favicon.svg']) copyFileSync(`src/${f}`, `dist/${f}`);
// kit, vendored by `make kit`: CSS, fonts and the bundles, as built.
rmSync('dist/kit', { recursive: true, force: true });
cpSync('src/kit', 'dist/kit', { recursive: true });

const opts = {
  entryPoints: { admin: 'src/admin.js', editor: 'src/editor/main.js' },
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
