#!/usr/bin/env node
// 图标完整性检查（R1-m2）：后端步骤/作业项 Meta.Icon 必须在
// src/components/primitives.jsx 的 IC 图标表里有对应键，否则画布渲染空白。
// 用法：node scripts/check-icons.mjs（pnpm build 自动执行）
import { readFileSync, readdirSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');

// 1) 后端 Meta.Icon 集合：steps/*.go + actions/*.go 中 Icon: "xxx"
const need = new Set();
for (const dir of ['internal/etl/steps', 'internal/etl/actions']) {
  for (const f of readdirSync(join(root, dir))) {
    if (!f.endsWith('.go') || f.endsWith('_test.go')) continue;
    const src = readFileSync(join(root, dir, f), 'utf8');
    for (const m of src.matchAll(/Icon:\s*"([a-z0-9-]+)"/g)) need.add(m[1]);
  }
}

// 2) 前端 IC 表键：primitives.jsx 顶层 `key:`（含 'kebab-case': 引号键）
const prim = readFileSync(join(root, 'src/components/primitives.jsx'), 'utf8');
const ic = prim.slice(prim.indexOf('const IC = {'), prim.indexOf('function Icon'));
const have = new Set();
for (const m of ic.matchAll(/^\s*(?:'([a-z0-9-]+)'|([a-zA-Z0-9_]+))\s*:/gm)) {
  have.add(m[1] ?? m[2]);
}

const missing = [...need].filter((n) => !have.has(n));
if (missing.length) {
  console.error('[icons] 以下 Meta.Icon 在 IC 表中无对应图标：');
  for (const n of missing) console.error(`  ${n}`);
  process.exit(1);
}
console.log(`[icons] 通过：${need.size} 个步骤/作业项图标全部有对应图形`);
