// 数据资源工作台：元数据树 + SQL 编辑器 + 结果
import React from 'react';
import CodeMirror from '@uiw/react-codemirror';
import { sql } from '@codemirror/lang-sql';
import { format as formatSQL } from 'sql-formatter';
import { useMC } from '../lib/data.js';
import { Btn, Icon, Spinner, EmptyState, Dialog, toast, Badge } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import {
  createWorkspace, deleteWorkspace, executeSQL, patchAutoCommit,
  commitWorkspace, rollbackWorkspace, metadataChildren, metadataDDL,
  sqlTemplate, listSavedSQL, createSavedSQL, deleteSavedSQL, exportWorkspace,
} from '../lib/dataresource-api.js';

function dialectFor(dbType) {
  if (dbType === 'mysql') return 'mysql';
  if (dbType === 'dm') return 'plsql';
  return 'postgresql';
}

function cellDisplay(v) {
  if (v == null) return <span style={{ color: 'var(--muted-fg)' }}>NULL</span>;
  if (typeof v === 'object' && v.type === 'binary') {
    return <span className="mono" style={{ fontSize: 11 }}>&lt;binary {v.size}B&gt;</span>;
  }
  return String(v);
}

function DataWorkspacePage({ resource, onBack }) {
  const store = useMC();
  const dark = document.documentElement.getAttribute('data-theme') === 'dark';
  const canWrite = resource.accessMode === 'write' || resource.accessMode === 'admin' || store.can('admin');

  const [wsId, setWsId] = React.useState(null);
  const [autoCommit, setAutoCommit] = React.useState(true);
  const [txState, setTxState] = React.useState('none');
  const [sqlText, setSqlText] = React.useState('SELECT 1');
  const [busy, setBusy] = React.useState(false);
  const [result, setResult] = React.useState(null);
  const [msg, setMsg] = React.useState('');
  const [tree, setTree] = React.useState([]);
  const [expanded, setExpanded] = React.useState({});
  const [childrenMap, setChildrenMap] = React.useState({});
  const [saved, setSaved] = React.useState([]);
  const [saveOpen, setSaveOpen] = React.useState(false);
  const [saveName, setSaveName] = React.useState('');
  const [loadOpen, setLoadOpen] = React.useState(false);

  // 创建工作台
  React.useEffect(() => {
    let alive = true;
    createWorkspace(resource.id).then((d) => {
      if (!alive) return;
      setWsId(d.workspaceId);
      setAutoCommit(!!d.autoCommit);
      setTxState(d.txState || 'none');
    }).catch((e) => toast(e.message || '创建工作台失败', { tone: 'error' }));
    metadataChildren(resource.id, '').then((ch) => {
      if (alive) setTree(Array.isArray(ch) ? ch : []);
    }).catch(() => {});
    listSavedSQL(resource.id).then((list) => {
      if (alive) setSaved(Array.isArray(list) ? list : []);
    }).catch(() => {});
    return () => {
      alive = false;
      if (wsId) deleteWorkspace(resource.id, wsId).catch(() => {});
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [resource.id]);

  // 卸载时清理
  React.useEffect(() => {
    return () => {
      if (wsId) deleteWorkspace(resource.id, wsId).catch(() => {});
    };
  }, [wsId, resource.id]);

  const loadChildren = async (node) => {
    const id = node.id || '';
    if (childrenMap[id]) {
      setExpanded((e) => ({ ...e, [id]: !e[id] }));
      return;
    }
    try {
      const ch = await metadataChildren(resource.id, id);
      setChildrenMap((m) => ({ ...m, [id]: Array.isArray(ch) ? ch : [] }));
      setExpanded((e) => ({ ...e, [id]: true }));
    } catch (e) {
      toast(e.message || '加载元数据失败', { tone: 'error' });
    }
  };

  const runSQL = async (confirmed = false) => {
    if (!wsId || !sqlText.trim()) return;
    setBusy(true);
    setMsg('');
    try {
      const res = await executeSQL(resource.id, wsId, { sql: sqlText, limit: 100, confirmed });
      setResult(res);
      setTxState(res.txState || 'none');
      if (res.statementType && res.statementType !== 'SELECT') {
        setMsg(`${res.statementType} · 影响 ${res.affectedRows ?? 0} 行 · ${res.durationMs}ms`);
      } else {
        setMsg(`返回 ${res.returnedRows ?? 0} 行` + (res.totalStatus === 'available' ? ` / 共 ${res.total}` : '') + ` · ${res.durationMs}ms`);
      }
    } catch (e) {
      if (e.code === 'DANGEROUS_SQL') {
        if (window.confirm((e.message || '危险操作') + '\n\n确认执行？')) {
          setBusy(false);
          return runSQL(true);
        }
      }
      setResult(null);
      setMsg(e.message || '执行失败');
      toast(e.message || '执行失败', { tone: 'error' });
    } finally {
      setBusy(false);
    }
  };

  const toggleAC = async () => {
    if (!wsId) return;
    try {
      const d = await patchAutoCommit(resource.id, wsId, !autoCommit);
      setAutoCommit(!!d.autoCommit);
      setTxState(d.txState || 'none');
    } catch (e) {
      toast(e.message || '切换失败', { tone: 'error' });
    }
  };

  const doCommit = async () => {
    try {
      const d = await commitWorkspace(resource.id, wsId);
      setTxState(d.txState || 'none');
      toast('已提交');
    } catch (e) {
      toast(e.message || '提交失败', { tone: 'error' });
    }
  };

  const doRollback = async () => {
    try {
      const d = await rollbackWorkspace(resource.id, wsId);
      setTxState(d.txState || 'none');
      toast('已回滚');
    } catch (e) {
      toast(e.message || '回滚失败', { tone: 'error' });
    }
  };

  const doFormat = () => {
    try {
      const out = formatSQL(sqlText, { language: dialectFor(resource.dbType) });
      setSqlText(out);
    } catch (e) {
      toast('格式化失败，已保留原文', { tone: 'warn' });
    }
  };

  const doExport = async (format) => {
    if (!wsId) return;
    try {
      const blob = await exportWorkspace(resource.id, wsId, { sql: sqlText, format });
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = `export.${format === 'xlsx' ? 'xlsx' : 'csv'}`;
      a.click();
      URL.revokeObjectURL(a.href);
    } catch (e) {
      toast(e.message || '导出失败', { tone: 'error' });
    }
  };

  const insertTemplate = async (node, op) => {
    try {
      const d = await sqlTemplate(resource.id, node.id, op);
      if (d.sql || d.template) setSqlText(d.sql || d.template);
    } catch (e) {
      toast(e.message || '生成模板失败', { tone: 'error' });
    }
  };

  const showDDL = async (node) => {
    try {
      const d = await metadataDDL(resource.id, node.id);
      setSqlText(d.ddl || d.SQL || '');
    } catch (e) {
      toast(e.message || '获取 DDL 失败', { tone: 'error' });
    }
  };

  const renderNode = (node, depth = 0) => {
    const id = node.id || node.name;
    const kids = childrenMap[id];
    const isOpen = expanded[id];
    const isLeaf = node.kind === 'table' || node.kind === 'view' || node.kind === 'matview';
    return (
      <div key={id}>
        <div style={{
          display: 'flex', alignItems: 'center', gap: 4, padding: '3px 6px', paddingLeft: 8 + depth * 12,
          cursor: 'pointer', fontSize: 12.5, borderRadius: 6,
        }}
          onClick={() => loadChildren(node)}
          onDoubleClick={() => isLeaf && insertTemplate(node, 'SELECT')}
          onContextMenu={(e) => {
            if (!isLeaf) return;
            e.preventDefault();
            const acts = [
              { label: '生成 SELECT', fn: () => insertTemplate(node, 'SELECT') },
              { label: '查看 DDL', fn: () => showDDL(node) },
            ];
            if (canWrite) {
              acts.push(
                { label: '生成 INSERT', fn: () => insertTemplate(node, 'INSERT') },
                { label: '生成 UPDATE', fn: () => insertTemplate(node, 'UPDATE') },
                { label: '生成 DELETE', fn: () => insertTemplate(node, 'DELETE') },
              );
            }
            // 简易菜单：用 confirm 列表不优雅，直接 toast 快捷
            const choice = window.prompt(acts.map((a, i) => `${i + 1}. ${a.label}`).join('\n') + '\n输入序号');
            const n = Number(choice);
            if (n >= 1 && n <= acts.length) acts[n - 1].fn();
          }}
        >
          <Icon name={isLeaf ? 'box' : (isOpen ? 'folder' : 'folder')} size={13} />
          <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{node.name}</span>
          <span style={{ fontSize: 10, color: 'var(--muted-fg)', marginLeft: 4 }}>{node.kind}</span>
        </div>
        {isOpen && kids ? kids.map((c) => renderNode(c, depth + 1)) : null}
      </div>
    );
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: 'calc(100vh - 120px)', minHeight: 480 }}>
      <PageHead
        title={resource.name}
        desc={`${resource.host}:${resource.port} / ${resource.databaseName} · ${resource.accessMode || ''}`}
        actions={<Btn variant="ghost" icon="arrow-left" onClick={onBack}>返回列表</Btn>}
      />

      <div style={{ display: 'flex', flex: 1, gap: 10, minHeight: 0 }}>
        {/* 元数据树 */}
        <div className="card" style={{ width: 260, flex: 'none', overflow: 'auto', padding: '8px 6px' }}>
          <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--muted-fg)', padding: '4px 8px 8px' }}>元数据</div>
          {tree.length === 0 ? <div style={{ padding: 12, fontSize: 12, color: 'var(--muted-fg)' }}>加载中或为空</div> : tree.map((n) => renderNode(n))}
        </div>

        {/* 编辑器 + 结果 */}
        <div style={{ flex: 1, display: 'flex', flexDirection: 'column', gap: 8, minWidth: 0 }}>
          <div className="card" style={{ padding: 8, display: 'flex', flexWrap: 'wrap', gap: 6, alignItems: 'center' }}>
            <Btn size="sm" variant="primary" icon="play" disabled={busy || !wsId} onClick={() => runSQL(false)}>执行</Btn>
            <Btn size="sm" variant="ghost" disabled={busy} onClick={toggleAC}>
              自动提交: {autoCommit ? '开' : '关'}
            </Btn>
            <Btn size="sm" variant="ghost" disabled={autoCommit || txState !== 'active'} onClick={doCommit}>提交</Btn>
            <Btn size="sm" variant="ghost" disabled={autoCommit || txState === 'none'} onClick={doRollback}>回滚</Btn>
            <Btn size="sm" variant="ghost" onClick={doFormat}>格式化</Btn>
            <Btn size="sm" variant="ghost" onClick={() => doExport('csv')}>导出 CSV</Btn>
            <Btn size="sm" variant="ghost" onClick={() => doExport('xlsx')}>导出 XLSX</Btn>
            <Btn size="sm" variant="ghost" onClick={() => setSaveOpen(true)}>保存 SQL</Btn>
            <Btn size="sm" variant="ghost" onClick={() => setLoadOpen(true)}>加载 SQL</Btn>
            <Badge tone={txState === 'active' ? 'warn' : 'info'}>事务 {txState}</Badge>
            {!canWrite ? <Badge tone="info">只读</Badge> : null}
          </div>

          <div className="card" style={{ padding: 0, overflow: 'hidden', flex: '0 0 220px' }}>
            <CodeMirror
              value={sqlText}
              height="220px"
              theme={dark ? 'dark' : 'light'}
              extensions={[sql()]}
              onChange={(v) => setSqlText(v)}
              basicSetup={{ lineNumbers: true, foldGutter: true }}
            />
          </div>

          <div className="card" style={{ flex: 1, overflow: 'auto', padding: 0, minHeight: 160 }}>
            {msg ? <div style={{ padding: '8px 12px', fontSize: 12.5, color: 'var(--muted-fg)', borderBottom: '1px solid var(--border)' }}>{msg}</div> : null}
            {result && result.columns ? (
              <table className="table">
                <thead>
                  <tr>{result.columns.map((c) => <th key={c}>{c}</th>)}</tr>
                </thead>
                <tbody>
                  {(result.rows || []).map((row, i) => (
                    <tr key={i}>{row.map((cell, j) => <td key={j} style={{ fontSize: 12.5 }}>{cellDisplay(cell)}</td>)}</tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <EmptyState icon="box" title="结果区" desc="执行 SQL 后在此显示结果" />
            )}
          </div>
        </div>
      </div>

      <Dialog open={saveOpen} onClose={() => setSaveOpen(false)} width={400} title="保存 SQL"
        foot={<React.Fragment>
          <Btn variant="ghost" onClick={() => setSaveOpen(false)}>取消</Btn>
          <Btn variant="primary" onClick={async () => {
            try {
              await createSavedSQL(resource.id, { name: saveName.trim(), sqlText });
              toast('已保存');
              setSaveOpen(false);
              setSaved(await listSavedSQL(resource.id));
            } catch (e) { toast(e.message || '保存失败', { tone: 'error' }); }
          }}>保存</Btn>
        </React.Fragment>}>
        <input className="input" placeholder="名称" value={saveName} onChange={(e) => setSaveName(e.target.value)} />
      </Dialog>

      <Dialog open={loadOpen} onClose={() => setLoadOpen(false)} width={480} title="加载已保存 SQL"
        foot={<Btn variant="ghost" onClick={() => setLoadOpen(false)}>关闭</Btn>}>
        {(saved || []).length === 0 ? <div style={{ color: 'var(--muted-fg)', fontSize: 13 }}>暂无保存的 SQL</div> : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
            {saved.map((s) => (
              <div key={s.id} style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <Btn size="sm" variant="ghost" onClick={() => { setSqlText(s.sqlText || s.sql_text || ''); setLoadOpen(false); }}>{s.name}</Btn>
                <Btn size="sm" variant="ghost" icon="trash" onClick={async () => {
                  try {
                    await deleteSavedSQL(resource.id, s.id);
                    setSaved(await listSavedSQL(resource.id));
                  } catch (e) { toast(e.message, { tone: 'error' }); }
                }} />
              </div>
            ))}
          </div>
        )}
      </Dialog>
    </div>
  );
}

export { DataWorkspacePage };
