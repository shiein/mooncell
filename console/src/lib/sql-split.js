// 方言感知的 SQL 词法扫描与语句拆分。
// 与后端 internal/dataresource/sql_scan.go 是同一份规则，共享用例 internal/dataresource/testdata/sql_split_cases.json
// （node --test src/lib/sql-split.test.js）。修改任一侧都必须同步另一侧并跑两边用例。
//
// dialect：'pg' | 'mysql' | 'dm' | 'generic'

/** 资源 dbType → 拆分方言 */
export function splitDialectFor(dbType) {
  if (dbType === 'mysql') return 'mysql';
  if (dbType === 'dm') return 'dm';
  return 'pg';
}

function lexOpts(d) {
  switch (d) {
    case 'mysql': return { hash: true, backslash: true, eString: false, backtick: true, dollar: false };
    case 'pg': return { hash: false, backslash: false, eString: true, backtick: false, dollar: true };
    case 'dm': return { hash: false, backslash: false, eString: false, backtick: false, dollar: false };
    default: return { hash: false, backslash: false, eString: false, backtick: true, dollar: true };
  }
}

const isSpace = (c) => c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === '\f' || c === '\v';
function isWordChar(c) {
  if (!c) return false;
  const code = c.charCodeAt(0);
  return c === '_' || c === '$' || c === '#' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || code >= 0x80;
}
function isWordStart(c) {
  const code = c.charCodeAt(0);
  return c === '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || code >= 0x80;
}

function skipQuoted(s, i, q, backslash) {
  i += 1;
  while (i < s.length) {
    const c = s[i];
    if (backslash && c === '\\') { i += 2; continue; }
    if (c === q) {
      if (s[i + 1] === q) { i += 2; continue; }
      return i + 1;
    }
    i += 1;
  }
  return s.length;
}

function dollarTagAt(s, i) {
  if (i > 0 && isWordChar(s[i - 1])) return null;
  let j = i + 1;
  while (j < s.length && s[j] !== '$') {
    const c = s[j];
    const code = c.charCodeAt(0);
    const ok = c === '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || code >= 0x80 || (j > i + 1 && c >= '0' && c <= '9');
    if (!ok) return null;
    j += 1;
  }
  if (j >= s.length) return null;
  return s.slice(i, j + 1);
}

/** 返回 {kind, start, end}；kind: space|comment|string|word|punct */
export function nextToken(s, i, o) {
  const c = s[i];
  if (isSpace(c)) {
    let j = i + 1;
    while (j < s.length && isSpace(s[j])) j += 1;
    return { kind: 'space', start: i, end: j };
  }
  if ((c === '-' && s[i + 1] === '-') || (o.hash && c === '#')) {
    const j = s.indexOf('\n', i);
    return { kind: 'comment', start: i, end: j < 0 ? s.length : j };
  }
  if (c === '/' && s[i + 1] === '*') {
    const j = s.indexOf('*/', i + 2);
    return { kind: 'comment', start: i, end: j < 0 ? s.length : j + 2 };
  }
  if (c === "'") {
    let bs = o.backslash;
    if (!bs && o.eString && i > 0 && (s[i - 1] === 'E' || s[i - 1] === 'e') && (i < 2 || !isWordChar(s[i - 2]))) bs = true;
    return { kind: 'string', start: i, end: skipQuoted(s, i, "'", bs) };
  }
  if (c === '"') return { kind: 'string', start: i, end: skipQuoted(s, i, '"', o.backslash) };
  if (c === '`' && o.backtick) return { kind: 'string', start: i, end: skipQuoted(s, i, '`', false) };
  if (c === '$' && o.dollar) {
    const tag = dollarTagAt(s, i);
    if (tag) {
      const body = i + tag.length;
      const k = s.indexOf(tag, body);
      return { kind: 'string', start: i, end: k < 0 ? s.length : k + tag.length };
    }
    return { kind: 'punct', start: i, end: i + 1 };
  }
  if (isWordStart(c)) {
    let j = i + 1;
    while (j < s.length && isWordChar(s[j])) j += 1;
    return { kind: 'word', start: i, end: j };
  }
  return { kind: 'punct', start: i, end: i + 1 };
}

const MYSQL_CREATE_STOP = new Set(['TABLE', 'VIEW', 'INDEX', 'UNIQUE', 'DATABASE', 'SCHEMA', 'USER', 'ROLE', 'TEMPORARY', 'ALGORITHM', 'SQL', 'SPATIAL', 'FULLTEXT', 'TABLESPACE', 'SERVER', 'LOGFILE', 'RESOURCE']);

/** 返回 [mode, decided]；mode: none|body|slash */
function detectBlockMode(words, d) {
  if (words.length === 0) return ['none', false];
  if (d === 'mysql') {
    if (words[0] !== 'CREATE') return ['none', true];
    for (let i = 1; i < words.length; i += 1) {
      const w = words[i];
      if (w === 'OR' || w === 'REPLACE' || w === 'AGGREGATE' || w === 'DEFINER') continue;
      if (w === 'PROCEDURE' || w === 'FUNCTION' || w === 'TRIGGER' || w === 'EVENT') return ['body', true];
      if (i >= 2 && words[i - 1] === 'DEFINER') continue;
      if (MYSQL_CREATE_STOP.has(w)) return ['none', true];
      if (i + 1 > 8) return ['none', true];
    }
    return ['none', false];
  }
  if (d === 'dm') {
    if (words[0] === 'DECLARE') return ['body', true];
    if (words[0] === 'BEGIN') {
      if (words.length < 2) return ['none', false];
      if (words[1] === 'WORK' || words[1] === 'TRANSACTION' || words[1] === 'TRAN') return ['none', true];
      return ['body', true];
    }
    if (words[0] === 'CREATE') {
      for (let i = 1; i < words.length; i += 1) {
        const w = words[i];
        if (w === 'OR' || w === 'REPLACE' || w === 'EDITIONABLE' || w === 'NONEDITIONABLE') continue;
        if (w === 'PROCEDURE' || w === 'FUNCTION' || w === 'TRIGGER') return ['body', true];
        if (w === 'PACKAGE' || w === 'TYPE' || w === 'CLASS') {
          if (i + 1 < words.length) return [words[i + 1] === 'BODY' ? 'slash' : 'body', true];
          return ['none', false];
        }
        return ['none', true];
      }
      return ['none', false];
    }
    return ['none', true];
  }
  return ['none', true];
}

/**
 * 按方言拆分脚本。返回 [{from, to, sql}]：sql 为去首尾空白的原文（保留结尾分号，不含 / 与自定义分隔符），
 * from/to 为在原文中的下标（to 不含尾部空白）。
 */
export function splitStatements(s, d = 'pg') {
  const o = lexOpts(d);
  const out = [];
  let delim = ';';
  let stmtStart = -1;
  let lastCodeEnd = 0;
  let words = [];
  let mode = 'none';
  let modeDecided = false;
  let depth = 0;
  let sawEnd = false;
  let pendingEnd = false;
  let endDecremented = false;
  let prevSawEnd = false;

  const reset = () => {
    stmtStart = -1; words = []; mode = 'none'; modeDecided = false;
    depth = 0; sawEnd = false; pendingEnd = false;
  };
  const emit = (end) => {
    if (stmtStart >= 0 && end > stmtStart) {
      const raw = s.slice(stmtStart, end);
      const text = raw.trim();
      if (text && text !== ';') {
        out.push({ from: stmtStart, to: stmtStart + raw.replace(/[ \t\r\n\f\v]+$/, '').length, sql: text });
      }
    }
    reset();
  };
  const atLineStart = (i) => {
    let j = i - 1;
    while (j >= 0 && (s[j] === ' ' || s[j] === '\t')) j -= 1;
    return j < 0 || s[j] === '\n' || s[j] === '\r';
  };
  const lineEnd = (i) => {
    const j = s.indexOf('\n', i);
    return j < 0 ? s.length : j;
  };
  const applyBlockWord = (w) => {
    if (pendingEnd) {
      pendingEnd = false;
      if (w === 'IF' || w === 'LOOP' || w === 'WHILE' || w === 'REPEAT' || w === 'FOR') {
        if (endDecremented) depth += 1;
        sawEnd = prevSawEnd;
        return;
      }
      if (w === 'CASE') return;
    }
    if (w === 'BEGIN' || w === 'CASE') depth += 1;
    else if (w === 'END') {
      prevSawEnd = sawEnd;
      endDecremented = depth > 0;
      if (depth > 0) depth -= 1;
      sawEnd = depth === 0;
      pendingEnd = true;
    }
  };

  let i = 0;
  while (i < s.length) {
    if (d === 'mysql' && stmtStart < 0 && atLineStart(i) && s.length - i >= 10
      && s.slice(i, i + 9).toUpperCase() === 'DELIMITER' && (s[i + 9] === ' ' || s[i + 9] === '\t')) {
      const le = lineEnd(i);
      const nd = s.slice(i + 9, le).trim();
      if (nd) delim = nd;
      i = le;
      continue;
    }
    if (d === 'dm' && s[i] === '/' && atLineStart(i)) {
      const le = lineEnd(i);
      if (s.slice(i + 1, le).trim() === '') {
        emit(lastCodeEnd);
        i = le;
        continue;
      }
    }
    if (delim !== ';' && s.startsWith(delim, i)) {
      emit(lastCodeEnd);
      i += delim.length;
      continue;
    }
    const t = nextToken(s, i, o);
    if (delim !== ';' && (t.kind === 'word' || t.kind === 'punct')) {
      const k = s.slice(t.start, t.end).indexOf(delim);
      if (k > 0) t.end = t.start + k;
    }
    i = t.end;
    if (t.kind === 'space' || t.kind === 'comment') continue;
    if (t.kind === 'punct' && s[t.start] === ';' && delim === ';') {
      if (stmtStart < 0) continue;
      if (mode === 'none' || (mode === 'body' && depth === 0 && (d === 'mysql' || sawEnd))) {
        lastCodeEnd = t.end;
        emit(t.end);
        continue;
      }
    }
    if (stmtStart < 0) stmtStart = t.start;
    lastCodeEnd = t.end;
    if (t.kind !== 'word') {
      pendingEnd = false;
      continue;
    }
    const w = s.slice(t.start, t.end).toUpperCase();
    if (!modeDecided) {
      words.push(w);
      [mode, modeDecided] = detectBlockMode(words, d);
      if (mode === 'body') {
        for (const pw of words.slice(0, -1)) applyBlockWord(pw);
      }
    }
    if (mode === 'body') applyBlockWord(w);
  }
  if (stmtStart >= 0) emit(lastCodeEnd);
  return out;
}

/**
 * 光标所在语句：光标落在语句内或语句后的空白/注释上时取该语句；
 * 落在两条语句之间的空白时取前一条（与常见客户端一致）。无语句时返回 null。
 */
export function statementAt(stmts, pos) {
  if (!stmts.length) return null;
  let prev = null;
  for (const st of stmts) {
    if (pos >= st.from && pos <= st.to) return st;
    if (st.from > pos) return prev || st;
    prev = st;
  }
  return prev;
}

/** 语句是否疑似「多条语句只用空行分隔、忘了写分号」：空行后又以常见语句关键字开头。 */
export function looksLikeMissingSemicolon(sql) {
  return /\n[ \t]*\n[ \t]*(SELECT|INSERT|UPDATE|DELETE|WITH|CREATE|ALTER|DROP|SHOW|EXPLAIN|CALL|SET)\b/i.test(sql);
}
