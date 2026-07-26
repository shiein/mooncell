// SFTP 目录懒加载与上传入口
import React from 'react';
import { Btn, Spinner, EmptyState, Icon, toast } from '../primitives.jsx';
import {
  listServerFiles, serverDownloadUrl, initServerUpload,
  uploadServerChunk, completeServerUpload, cancelServerUpload, sha256Hex,
} from '../../lib/serverops-api.js';

function fmtSize(n) {
  if (n == null || n < 0) return '—';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + ' MB';
  return (n / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
}

export function FileTree({ resourceId, sessionId, onTransfer }) {
  const [path, setPath] = React.useState('.');
  const [cwd, setCwd] = React.useState('');
  const [entries, setEntries] = React.useState([]);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState(null);
  const fileInputRef = React.useRef(null);
  const abortRef = React.useRef(false);

  const load = React.useCallback(async (p) => {
    if (!sessionId) return;
    setLoading(true);
    setError(null);
    try {
      const d = await listServerFiles(resourceId, sessionId, p);
      setCwd(d.path || p);
      setPath(d.path || p);
      setEntries(d.entries || []);
    } catch (e) {
      setError(e.message || '读取目录失败');
      setEntries([]);
    } finally {
      setLoading(false);
    }
  }, [resourceId, sessionId]);

  React.useEffect(() => {
    load('.');
  }, [load]);

  const crumbs = React.useMemo(() => {
    if (!cwd || cwd === '.') return [{ label: '~', path: '.' }];
    const parts = cwd.split('/').filter(Boolean);
    const out = [{ label: '/', path: '/' }];
    let acc = '';
    for (const p of parts) {
      acc += '/' + p;
      out.push({ label: p, path: acc });
    }
    return out;
  }, [cwd]);

  const enter = (e) => {
    if (e.type === 'directory') load(e.path);
  };

  const download = (e) => {
    if (e.type === 'directory') return;
    // 直接 <a> 导航，禁止 fetch+blob
    const a = document.createElement('a');
    a.href = serverDownloadUrl(resourceId, sessionId, e.path);
    a.download = e.name;
    a.rel = 'noopener';
    document.body.appendChild(a);
    a.click();
    a.remove();
  };

  const uploadFiles = async (fileList) => {
    const files = Array.from(fileList || []);
    if (!files.length) return;
    abortRef.current = false;
    for (const file of files) {
      await uploadOne(file);
      if (abortRef.current) break;
    }
    load(cwd || '.');
  };

  const uploadOne = async (file) => {
    const transfer = {
      id: 'local-' + Date.now(),
      name: file.name,
      size: file.size,
      transferred: 0,
      state: 'uploading',
      cancel: () => { abortRef.current = true; },
    };
    onTransfer && onTransfer(transfer);

    let transferId = null;
    try {
      const init = await initServerUpload(resourceId, sessionId, {
        directory: cwd || '.',
        filename: file.name,
        size: file.size,
        overwrite: false,
      });
      transferId = init.transferId;
      transfer.id = transferId;
      const chunkSize = init.chunkSize || (8 << 20);
      let offset = init.nextOffset || 0;

      while (offset < file.size) {
        if (abortRef.current) {
          await cancelServerUpload(resourceId, sessionId, transferId);
          transfer.state = 'cancelled';
          onTransfer && onTransfer({ ...transfer });
          return;
        }
        const end = Math.min(offset + chunkSize, file.size);
        const blob = file.slice(offset, end);
        const buf = await blob.arrayBuffer();
        const hex = await sha256Hex(buf);
        let tries = 0;
        // 每块最多重试 3 次
        for (;;) {
          try {
            const res = await uploadServerChunk(resourceId, sessionId, transferId, offset, blob, hex);
            offset = res.nextOffset != null ? res.nextOffset : end;
            transfer.transferred = offset;
            onTransfer && onTransfer({ ...transfer });
            break;
          } catch (err) {
            tries++;
            if (err.code === 'CHUNK_OFFSET_MISMATCH' && err.nextOffset != null) {
              offset = err.nextOffset;
              continue;
            }
            if (tries >= 3) throw err;
            await new Promise((r) => setTimeout(r, 500 * tries));
          }
        }
      }
      await completeServerUpload(resourceId, sessionId, transferId);
      transfer.state = 'completed';
      transfer.transferred = file.size;
      onTransfer && onTransfer({ ...transfer });
      toast(`${file.name} 上传完成`);
    } catch (e) {
      transfer.state = 'failed';
      onTransfer && onTransfer({ ...transfer });
      if (transferId) {
        try { await cancelServerUpload(resourceId, sessionId, transferId); } catch (_) {}
      }
      toast(e.message || `${file.name} 上传失败`, { tone: 'error' });
    }
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', minHeight: 0 }}>
      <div style={{
        display: 'flex', alignItems: 'center', gap: 6, padding: '8px 10px',
        borderBottom: '1px solid var(--border)', flex: 'none', flexWrap: 'wrap',
      }}>
        <span style={{ fontSize: 12.5, fontWeight: 600 }}>文件</span>
        <div style={{ flex: 1 }} />
        <Btn size="sm" variant="ghost" icon="rotate" title="刷新" onClick={() => load(cwd || '.')} />
        <Btn size="sm" variant="outline" icon="upload" onClick={() => fileInputRef.current && fileInputRef.current.click()}>上传</Btn>
        <input ref={fileInputRef} type="file" multiple hidden
          onChange={(e) => { uploadFiles(e.target.files); e.target.value = ''; }} />
      </div>
      <div style={{
        display: 'flex', gap: 4, padding: '6px 10px', fontSize: 11.5, flexWrap: 'wrap',
        borderBottom: '1px solid var(--border)', color: 'var(--muted-fg)',
      }}>
        {crumbs.map((c, i) => (
          <React.Fragment key={c.path + i}>
            {i > 0 ? <span>/</span> : null}
            <button type="button" className="link-btn" style={{ fontSize: 11.5 }}
              onClick={() => load(c.path)}>{c.label}</button>
          </React.Fragment>
        ))}
      </div>
      <div style={{ flex: 1, overflow: 'auto', minHeight: 0 }}>
        {loading ? <div style={{ padding: 16, textAlign: 'center' }}><Spinner size={14} /></div> : null}
        {error ? <EmptyState icon="alert" title="无法列出目录" desc={error} /> : null}
        {!loading && !error && entries.length === 0 ? (
          <EmptyState icon="folder" title="空目录" />
        ) : null}
        <div style={{ display: 'flex', flexDirection: 'column' }}>
          {entries.map((e) => (
            <div key={e.path}
              style={{
                display: 'flex', alignItems: 'center', gap: 8, padding: '6px 10px',
                fontSize: 12.5, borderBottom: '1px solid var(--border)', cursor: e.type === 'directory' ? 'pointer' : 'default',
              }}
              onDoubleClick={() => enter(e)}
            >
              <Icon name={e.type === 'directory' ? 'folder' : 'fileText'} size={14}
                style={{ color: e.type === 'directory' ? 'var(--primary)' : 'var(--muted-fg)', flex: 'none' }} />
              <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                title={e.name}
                onClick={() => e.type === 'directory' && enter(e)}
              >{e.name}{e.type === 'symlink' ? ' →' : ''}</span>
              <span className="mono" style={{ fontSize: 10.5, color: 'var(--muted-fg)', flex: 'none' }}>
                {e.type === 'directory' ? '—' : fmtSize(e.size)}
              </span>
              {e.type !== 'directory' ? (
                <Btn size="sm" variant="ghost" icon="download" title="下载" onClick={() => download(e)} />
              ) : null}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
