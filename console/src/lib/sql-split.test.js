// 与 Go 侧 TestSplitSQLSharedCases 共用用例：node --test src/lib/sql-split.test.js
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import { splitStatements, statementAt, looksLikeMissingSemicolon } from './sql-split.js';

const here = path.dirname(fileURLToPath(import.meta.url));
const doc = JSON.parse(readFileSync(path.join(here, '../../internal/dataresource/testdata/sql_split_cases.json'), 'utf8'));

for (const c of doc.cases) {
  test(`拆分：${c.name}`, () => {
    const got = splitStatements(c.sql, c.dialect);
    assert.deepEqual(got.map((s) => s.sql), c.expect);
    for (const s of got) assert.equal(c.sql.slice(s.from, s.to), s.sql);
  });
}

test('光标所在语句', () => {
  const src = 'SELECT 1;\n\nSELECT 2;\n-- tail';
  const stmts = splitStatements(src, 'pg');
  assert.equal(statementAt(stmts, 0).sql, 'SELECT 1;');
  assert.equal(statementAt(stmts, 9).sql, 'SELECT 1;'); // 紧跟分号
  assert.equal(statementAt(stmts, 10).sql, 'SELECT 1;'); // 两条之间的空行取前一条
  assert.equal(statementAt(stmts, 13).sql, 'SELECT 2;');
  assert.equal(statementAt(stmts, src.length).sql, 'SELECT 2;');
  assert.equal(statementAt([], 0), null);
});

test('疑似漏写分号', () => {
  assert.equal(looksLikeMissingSemicolon('SELECT 1\n\nSELECT 2'), true);
  assert.equal(looksLikeMissingSemicolon('WITH a AS (SELECT 1)\n\nSELECT * FROM a'), true);
  assert.equal(looksLikeMissingSemicolon('SELECT a,\n  b\nFROM t'), false);
});
