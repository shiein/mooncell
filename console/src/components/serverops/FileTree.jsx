// SFTP 目录懒加载、上传与断点续传入口
import React from 'react';
import { Btn, Spinner, EmptyState, Icon, toast } from '../primitives.jsx';
import {
  listServerFiles, serverDownloadUrl, initServerUpload,
  uploadServerChunk, completeServerUpload, cancelServerUpload, sha256Hex,
  listActiveUploads, resumeServerUpload,
} from '../../lib/serverops-api.js';

function fmtSize(n) {
  if (n == null || n < 0) return '—';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  if (n < 1024 * 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + ' MB';
  return (n / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
}

async function buildPrefixProof(file, endOffset, chunkSize) {
  const out = [];
  let offset = 0;
  while (offset < endOffset) {
    const end = Math.min(offset + chunkSize, endOffset);
    const buf = await file.slice(offset, end).arrayBuffer();
    out.push({ offset, size: end - offset, sha256: await sha256Hex(buf) });
    offset = end;
    // 让出主线程，避免纯 JS SHA-256 连续阻塞导致 UI 假死
    if (offset < endOffset) {
      await new Promise((r) => setTimeout(r, 0));
    }
  }
  return out;
}

export function FileTree({ resourceId, sessionId, onTransfer }) {
  const [cwd, setCwd] = React.useState('');
  const [entries, setEntries] = React.useState([]);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState(null);
  const [pending, setPending] = React.useState([]); // 可续传任务
  const [overwrite, setOverwrite] = React.useState(false);
  const fileInputRef = React.useRef(null);
  const resumeInputRef = React.useRef(null);
  const resumeTargetRef = React.useRef(null);
  const abortRef = React.useRef(false);

  const load = React.useCallback(async (p) => {
    if (!sessionId) return;
    setLoading(true);
    setError(null);
    try {
      const d = await listServerFiles(resourceId, sessionId, p);
      setCwd(d.path || p);
      setEntries(d.entries || []);
    } catch (e) {
      setError(e.message || '读取目录失败');
      setEntries([]);
    } finally {
      setLoading(false);
    }
  }, [resourceId, sessionId]);

  const refreshPending = React.useCallback(async () => {
    if (!resourceId) return;
    try {
      const list = await listActiveUploads(resourceId);
      setPending(list || []);
    } catch (_) {
      setPending([]);
    }
  }, [resourceId]);

  React.useEffect(() => {
    load('.');
    refreshPending();
  }, [load, refreshPending]);

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
    const a = document.createElement('a');
    a.href = serverDownloadUrl(resourceId, sessionId, e.path);
    a.download = e.name;
    a.rel = 'noopener';
    document.body.appendChild(a);
    a.click();
    a.remove();
  };

  /** 从 offset 续传/上传公共循环；失败时保留远端 part（不自动 cancel）。 */
  const runChunks = async (file, transferId, startOffset, chunkSize, transfer) => {
    let offset = startOffset;
    while (offset < file.size) {
      if (abortRef.current) {
        // 用户明确取消才删远端 part
        await cancelServerUpload(resourceId, sessionId, transferId);
        transfer.state = 'cancelled';
        onTransfer && onTransfer({ ...transfer });
        return false;
      }
      // 每次循环按当前权威 offset 重新切片，避免 OFFSET_MISMATCH 后复用旧 blob。
      const end = Math.min(offset + chunkSize, file.size);
      const blob = file.slice(offset, end);
      const buf = await blob.arrayBuffer();
      const hex = await sha256Hex(buf);
      let tries = 0;
      let advanced = false;
      while (!advanced) {
        try {
          const res = await uploadServerChunk(resourceId, sessionId, transferId, offset, blob, hex);
          offset = res.nextOffset != null ? res.nextOffset : end;
          transfer.transferred = offset;
          onTransfer && onTransfer({ ...transfer });
          advanced = true;
        } catch (err) {
          tries++;
          if (err.code === 'CHUNK_OFFSET_MISMATCH' && err.nextOffset != null) {
            // 跳出内层，用新 offset 重新 slice
            offset = err.nextOffset;
            break;
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
    return true;
  };

  const uploadOne = async (file, overwrite) => {
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
        overwrite: !!overwrite,
      });
      transferId = init.transferId;
      transfer.id = transferId;
      const chunkSize = init.chunkSize || (8 << 20);
      const ok = await runChunks(file, transferId, init.nextOffset || 0, chunkSize, transfer);
      if (ok) toast(`${file.name} 上传完成`);
    } catch (e) {
      transfer.state = 'failed';
      onTransfer && onTransfer({ ...transfer });
      // 未建立传输的错误不要误导「可续传」
      if (e.code === 'REMOTE_TARGET_EXISTS') {
        toast(e.message || '目标已存在，可勾选覆盖后重试', { tone: 'error' });
      } else if (e.code === 'TRANSFER_LIMIT_REACHED') {
        toast(e.message || '传输数已达上限，请丢弃未完成任务', { tone: 'error' });
      } else if (transferId) {
        toast((e.message || `${file.name} 上传失败`) + ' · 可稍后续传', { tone: 'error' });
        await refreshPending();
      } else {
        toast(e.message || `${file.name} 上传失败`, { tone: 'error' });
      }
    }
  };

  const uploadFiles = async (fileList) => {
    const files = Array.from(fileList || []);
    if (!files.length) return;
    abortRef.current = false;
    const ow = overwrite;
    for (const file of files) {
      await uploadOne(file, ow);
      if (abortRef.current) break;
    }
    load(cwd || '.');
    refreshPending();
  };

  const startResumePick = (task) => {
    resumeTargetRef.current = task;
    resumeInputRef.current && resumeInputRef.current.click();
  };

  const onResumeFile = async (fileList) => {
    const file = fileList && fileList[0];
    const task = resumeTargetRef.current;
    resumeTargetRef.current = null;
    if (!file || !task) return;
    if (file.size !== task.expectedSize) {
      toast(`文件大小不匹配：本地 ${fmtSize(file.size)}，记录 ${fmtSize(task.expectedSize)}`, { tone: 'error' });
      return;
    }
    if (file.name !== task.filename) {
      toast(`请选择同名文件「${task.filename}」`, { tone: 'warn' });
      // 仍允许继续（用户可能重命名本地副本），仅警告
    }
    abortRef.current = false;
    const transfer = {
      id: task.transferId,
      name: task.filename,
      size: task.expectedSize,
      transferred: task.transferredSize || 0,
      state: 'uploading',
      cancel: () => { abortRef.current = true; },
    };
    onTransfer && onTransfer(transfer);
    try {
      const proofChunkSize = task.chunkSize || (8 << 20);
      if ((task.transferredSize || 0) > 32 * 1024 * 1024) {
        toast('正在校验本地文件前缀，大文件可能需要一些时间…', { tone: 'warn' });
      }
      const prefixChunks = await buildPrefixProof(
        file, task.transferredSize || 0, proofChunkSize,
      );
      const r = await resumeServerUpload(resourceId, sessionId, task.transferId, {
        localSize: file.size,
        prefixChunks,
      });
      const chunkSize = r.chunkSize || (8 << 20);
      const offset = r.nextOffset || 0;
      transfer.transferred = offset;
      onTransfer && onTransfer({ ...transfer });
      const ok = await runChunks(file, task.transferId, offset, chunkSize, transfer);
      if (ok) toast(`${task.filename} 续传完成`);
    } catch (e) {
      transfer.state = 'failed';
      onTransfer && onTransfer({ ...transfer });
      toast(e.message || '续传失败 · 记录已保留', { tone: 'error' });
    }
    load(cwd || '.');
    refreshPending();
  };

  const discardPending = async (task) => {
    try {
      await cancelServerUpload(resourceId, sessionId, task.transferId);
      toast('已取消并清理');
      refreshPending();
    } catch (e) {
      toast(e.message || '取消失败', { tone: 'error' });
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
        <label style={{ display: 'flex', alignItems: 'center', gap: 4, fontSize: 11.5, color: 'var(--muted-fg)', cursor: 'pointer' }}
          title="开启后上传完成时用原子 rename 覆盖同名文件">
          <input type="checkbox" checked={overwrite} onChange={(e) => setOverwrite(e.target.checked)} />
          覆盖同名
        </label>
        <Btn size="sm" variant="ghost" icon="rotate" title="刷新" onClick={() => { load(cwd || '.'); refreshPending(); }} />
        <Btn size="sm" variant="outline" icon="upload" onClick={() => fileInputRef.current && fileInputRef.current.click()}>上传</Btn>
        <input ref={fileInputRef} type="file" multiple hidden
          onChange={(e) => { uploadFiles(e.target.files); e.target.value = ''; }} />
        <input ref={resumeInputRef} type="file" hidden
          onChange={(e) => { onResumeFile(e.target.files); e.target.value = ''; }} />
      </div>

      {pending.length > 0 ? (
        <div style={{
          padding: '8px 10px', borderBottom: '1px solid var(--border)',
          background: 'var(--primary-soft, var(--muted))', fontSize: 12,
        }}>
          <div style={{ fontWeight: 600, marginBottom: 6 }}>未完成上传（可续传）</div>
          {pending.map((t) => (
            <div key={t.transferId} style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 4 }}>
              <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {t.filename} · {fmtSize(t.transferredSize)}/{fmtSize(t.expectedSize)}
              </span>
              <Btn size="sm" variant="primary" onClick={() => startResumePick(t)}>续传</Btn>
              <Btn size="sm" variant="ghost" onClick={() => discardPending(t)}>丢弃</Btn>
            </div>
          ))}
          <div style={{ fontSize: 11, color: 'var(--muted-fg)', marginTop: 4 }}>
            续传须重新选择同一本地文件（按已上传分块摘要校验）；「丢弃」会删除远端临时 part。
          </div>
        </div>
      ) : null}

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
                fontSize: 12.5, borderBottom: '1px solid var(--border)',
                cursor: e.type === 'directory' ? 'pointer' : 'default',
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
