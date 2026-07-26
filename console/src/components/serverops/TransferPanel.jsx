// 传输进度面板
import React from 'react';
import { Btn, Progress } from '../primitives.jsx';

function fmtBytes(n) {
  if (n == null) return '0 B';
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / (1024 * 1024)).toFixed(1) + ' MB';
}

export function TransferPanel({ items }) {
  if (!items || !items.length) return null;
  return (
    <div style={{
      borderTop: '1px solid var(--border)', padding: '8px 10px',
      maxHeight: 140, overflowY: 'auto', background: 'var(--card)', flex: 'none',
    }}>
      <div style={{ fontSize: 11.5, fontWeight: 600, marginBottom: 6, color: 'var(--muted-fg)' }}>传输</div>
      {items.map((t) => {
        const pct = t.size > 0 ? (t.transferred / t.size) * 100 : 0;
        return (
          <div key={t.id} style={{ marginBottom: 8 }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 12 }}>
              <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{t.name}</span>
              <span className="mono" style={{ fontSize: 10.5, color: 'var(--muted-fg)' }}>
                {fmtBytes(t.transferred)} / {fmtBytes(t.size)}
              </span>
              {t.state === 'uploading' && t.cancel ? (
                <Btn size="sm" variant="ghost" onClick={() => t.cancel()}>取消</Btn>
              ) : (
                <span style={{ fontSize: 10.5, color: t.state === 'completed' ? 'var(--success)' : t.state === 'failed' ? 'var(--error)' : 'var(--muted-fg)' }}>
                  {t.state === 'completed' ? '完成' : t.state === 'failed' ? '失败' : t.state === 'cancelled' ? '已取消' : ''}
                </span>
              )}
            </div>
            <Progress value={pct} height={4}
              color={t.state === 'failed' ? 'var(--error)' : t.state === 'completed' ? 'var(--success)' : 'var(--primary)'} />
          </div>
        );
      })}
    </div>
  );
}
