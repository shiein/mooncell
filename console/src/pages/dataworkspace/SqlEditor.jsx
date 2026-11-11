// SQL 编辑器：直接管理 EditorView（不走受控 value），每个查询标签保留独立 EditorState（含撤销历史）。
//
// 性能要点：扩展只在挂载时创建一次；补全 schema / 主题经 Compartment 局部重配，
// 打字不触发 React 渲染（文本经 onDocChange 回调写入 ref，由父组件节流持久化）。
import React from 'react';
import { basicSetup, oneDark } from '@uiw/react-codemirror';
import { EditorState, Compartment, Prec, StateEffect, StateField, RangeSetBuilder } from '@codemirror/state';
import { EditorView, keymap, Decoration, ViewPlugin, placeholder } from '@codemirror/view';
import { sql, MySQL, PostgreSQL, PLSQL } from '@codemirror/lang-sql';
import { splitStatements, statementAt } from '../../lib/sql-split.js';

const HIGHLIGHT_MAX_DOC = 200000; // 超大文档不做当前语句高亮，避免每次按键全量扫描

function langDialect(dbType) {
  if (dbType === 'mysql') return MySQL;
  if (dbType === 'dm') return PLSQL;
  return PostgreSQL;
}

// ---- 错误位置标记 ----
const setErrorMark = StateEffect.define();
const errorField = StateField.define({
  create: () => Decoration.none,
  update(value, tr) {
    let next = tr.docChanged ? Decoration.none : value;
    for (const e of tr.effects) {
      if (e.is(setErrorMark)) {
        if (!e.value) next = Decoration.none;
        else {
          const { from, to } = e.value;
          const len = tr.state.doc.length;
          const f = Math.max(0, Math.min(from, len));
          const t = Math.max(f, Math.min(to, len));
          next = t > f ? Decoration.set([Decoration.mark({ class: 'cm-dr-error' }).range(f, t)]) : Decoration.none;
        }
      }
    }
    return next;
  },
  provide: (f) => EditorView.decorations.from(f),
});

// ---- 当前语句高亮（执行 Ctrl+Enter 时将运行的范围） ----
function statementHighlighter(dialect) {
  const lineDeco = Decoration.line({ class: 'cm-dr-stmt' });
  const build = (view) => {
    const { state } = view;
    const sel = state.selection.main;
    if (!sel.empty || state.doc.length > HIGHLIGHT_MAX_DOC) return Decoration.none;
    const stmts = splitStatements(state.doc.toString(), dialect);
    if (stmts.length < 2) return Decoration.none; // 只有一条时不打扰
    const st = statementAt(stmts, sel.head);
    if (!st) return Decoration.none;
    const builder = new RangeSetBuilder();
    const first = state.doc.lineAt(st.from).number;
    const last = state.doc.lineAt(Math.max(st.from, st.to - 1)).number;
    for (let n = first; n <= last; n += 1) {
      const line = state.doc.line(n);
      builder.add(line.from, line.from, lineDeco);
    }
    return builder.finish();
  };
  return ViewPlugin.fromClass(class {
    constructor(view) { this.decorations = build(view); }
    update(u) {
      if (u.docChanged || u.selectionSet) this.decorations = build(u.view);
    }
  }, { decorations: (v) => v.decorations });
}

const baseTheme = EditorView.theme({
  '&': { height: '100%', fontSize: '12.5px' },
  '.cm-scroller': { overflow: 'auto', fontFamily: 'var(--font-mono, ui-monospace, SFMono-Regular, Menlo, monospace)' },
  '.cm-dr-stmt': { backgroundColor: 'color-mix(in srgb, var(--primary) 7%, transparent)' },
  '.cm-dr-error': { textDecoration: 'underline wavy #e5484d', backgroundColor: 'rgba(229,72,77,.14)' },
});

/** 从语句中粗略提取表引用（FROM/JOIN/UPDATE/INTO 之后的标识符），用于按需加载列。 */
export function extractTableRefs(text) {
  const out = new Set();
  const re = /\b(?:FROM|JOIN|UPDATE|INTO|TABLE)\s+((?:[`"]?[\w$\u0080-￿]+[`"]?\.)?[`"]?[\w$\u0080-￿]+[`"]?)/gi;
  let m;
  while ((m = re.exec(text)) && out.size < 12) {
    out.add(m[1].replace(/[`"]/g, ''));
  }
  return [...out];
}

const SqlEditor = React.forwardRef(function SqlEditor({
  tabId, initialDoc, dbType, dialect, dark, schemaConfig, handlersRef, onDocChange, onTableRefs,
}, ref) {
  const hostRef = React.useRef(null);
  const viewRef = React.useRef(null);
  const statesRef = React.useRef(new Map());
  const currentTabRef = React.useRef(tabId);
  const langConf = React.useRef(new Compartment()).current;
  const themeConf = React.useRef(new Compartment()).current;
  const langExtRef = React.useRef(null);
  const themeExtRef = React.useRef(null);
  const onDocChangeRef = React.useRef(onDocChange);
  const onTableRefsRef = React.useRef(onTableRefs);
  onDocChangeRef.current = onDocChange;
  onTableRefsRef.current = onTableRefs;
  const refsTimer = React.useRef(null);

  const makeLang = React.useCallback((cfg) => sql({
    dialect: langDialect(dbType),
    upperCaseKeywords: true,
    schema: cfg?.schema || undefined,
    defaultSchema: cfg?.defaultSchema || undefined,
  }), [dbType]);

  // 扩展只创建一次（dialect 在页面生命周期内固定）
  const extensionsRef = React.useRef(null);
  if (!extensionsRef.current) {
    langExtRef.current = makeLang(schemaConfig);
    themeExtRef.current = dark ? oneDark : [];
    const run = (name) => () => {
      const fn = handlersRef.current?.[name];
      if (fn) fn();
      return true;
    };
    extensionsRef.current = [
      Prec.highest(keymap.of([
        { key: 'Mod-Enter', run: run('runCurrent') },
        { key: 'Shift-Mod-Enter', run: run('runAll') },
        { key: 'Mod-s', run: run('save'), preventDefault: true },
        { key: 'Shift-Alt-f', run: run('format') },
        { key: 'Mod-Shift-f', run: run('format') },
      ])),
      basicSetup({ lineNumbers: true, foldGutter: true, highlightActiveLine: true, autocompletion: true }),
      langConf.of(langExtRef.current),
      themeConf.of(themeExtRef.current),
      baseTheme,
      placeholder('输入 SQL… Ctrl/⌘+Enter 执行光标所在语句，Ctrl/⌘+Shift+Enter 执行全部；多条语句用 ; 分隔'),
      errorField,
      statementHighlighter(dialect),
      EditorView.updateListener.of((u) => {
        if (!u.docChanged) return;
        const text = u.state.doc.toString();
        onDocChangeRef.current?.(currentTabRef.current, text);
        clearTimeout(refsTimer.current);
        refsTimer.current = setTimeout(() => {
          const view = viewRef.current;
          if (!view) return;
          const stmts = splitStatements(view.state.doc.toString(), dialect);
          const st = statementAt(stmts, view.state.selection.main.head);
          if (st) onTableRefsRef.current?.(extractTableRefs(st.sql));
        }, 450);
      }),
    ];
  }

  const createState = React.useCallback((doc) => EditorState.create({
    doc: doc || '',
    extensions: extensionsRef.current,
  }), []);

  // 挂载
  React.useLayoutEffect(() => {
    const view = new EditorView({ state: createState(initialDoc), parent: hostRef.current });
    viewRef.current = view;
    currentTabRef.current = tabId;
    return () => {
      clearTimeout(refsTimer.current);
      view.destroy();
      viewRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 切换标签：保存旧状态，恢复/新建目标状态，并同步当前 Compartment 配置
  React.useLayoutEffect(() => {
    const view = viewRef.current;
    if (!view || currentTabRef.current === tabId) return;
    statesRef.current.set(currentTabRef.current, view.state);
    currentTabRef.current = tabId;
    const cached = statesRef.current.get(tabId);
    view.setState(cached || createState(initialDoc));
    view.dispatch({ effects: [langConf.reconfigure(langExtRef.current), themeConf.reconfigure(themeExtRef.current)] });
    view.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tabId]);

  React.useEffect(() => {
    const view = viewRef.current;
    themeExtRef.current = dark ? oneDark : [];
    if (view) view.dispatch({ effects: themeConf.reconfigure(themeExtRef.current) });
  }, [dark, themeConf]);

  React.useEffect(() => {
    const view = viewRef.current;
    langExtRef.current = makeLang(schemaConfig);
    if (view) view.dispatch({ effects: langConf.reconfigure(langExtRef.current) });
  }, [schemaConfig, makeLang, langConf]);

  React.useImperativeHandle(ref, () => ({
    getDoc: () => viewRef.current?.state.doc.toString() || '',
    /** 选区非空取选区，否则取光标所在语句；返回 {from, to, sql, isSelection} */
    getTarget() {
      const view = viewRef.current;
      if (!view) return null;
      const { state } = view;
      const sel = state.selection.main;
      if (!sel.empty) return { from: sel.from, to: sel.to, sql: state.sliceDoc(sel.from, sel.to), isSelection: true };
      const st = statementAt(splitStatements(state.doc.toString(), dialect), sel.head);
      return st ? { from: st.from, to: st.to, sql: st.sql, isSelection: false } : null;
    },
    replaceRange(from, to, text) {
      const view = viewRef.current;
      if (!view) return;
      view.dispatch({ changes: { from, to, insert: text }, selection: { anchor: from + text.length } });
      view.focus();
    },
    insertAtCursor(text) {
      const view = viewRef.current;
      if (!view) return;
      const sel = view.state.selection.main;
      view.dispatch({ changes: { from: sel.from, to: sel.to, insert: text }, selection: { anchor: sel.from + text.length } });
      view.focus();
    },
    setDoc(text) {
      const view = viewRef.current;
      if (!view) return;
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: text }, selection: { anchor: text.length } });
      view.focus();
    },
    markError(from, to) {
      const view = viewRef.current;
      if (!view) return;
      const len = view.state.doc.length;
      const f = Math.max(0, Math.min(from, len));
      view.dispatch({
        effects: [setErrorMark.of({ from: f, to: Math.max(f + 1, to) }), EditorView.scrollIntoView(f, { y: 'center' })],
        selection: { anchor: f },
      });
      view.focus();
    },
    clearError() {
      viewRef.current?.dispatch({ effects: setErrorMark.of(null) });
    },
    focus: () => viewRef.current?.focus(),
    /** 关闭标签时丢弃缓存状态 */
    dropTab: (id) => statesRef.current.delete(id),
  }), [dialect]);

  return <div ref={hostRef} className="dr-editor-host" />;
});

export default React.memo(SqlEditor);
