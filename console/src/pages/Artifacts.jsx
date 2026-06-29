// Mooncell — 制品仓库(版本化制品库)
// 上传一次 → 留存(sha256 + 版本标签)→ 可部署到任意应用/Agent,或一键重部署历史制品。
// 与文件柜的区别:文件柜是临时中转(过期/提取码/可匿名);制品仓库是部署制品的版本化留存
// (无过期、需登录、面向重部署)。部署对话框可选用已留存制品免重复上传。
import React from 'react';
import { useMC, fmtTime, timeAgo, fmtBytes } from '../lib/data.js';
import { Btn, Icon, Badge, Progress, EmptyState, Spinner, toast } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import { listArtifacts, uploadArtifact, deleteArtifact } from '../lib/api.js';

function ArtifactsPage() {
  const store = useMC();
  const [rows, setRows] = React.useState(null); // null=加载中;数组=已加载
  const [uploading, setUploading] = React.useState(false);
  const [prog, setProg] = React.useState(0);
  const [version, setVersion] = React.useState("");
  const fileRef = React.useRef(null);
  const canWrite = store.can("write");

  const refresh = React.useCallback(() => {
    listArtifacts().then((r) => setRows(Array.isArray(r) ? r : []));
  }, []);
  React.useEffect(() => { refresh(); }, [refresh]);

  const onUpload = async (file) => {
    if (!file) return;
    setUploading(true); setProg(40);
    try {
      const res = await uploadArtifact(file, version);
      setProg(100);
      if (res && res.deduped) {
        toast(`制品已存在(sha256 去重):${res.artifact.name}`, { tone: "info", icon: "archive" });
      } else {
        toast(`制品「${file.name}」已留存到制品库`, { icon: "check" });
      }
      setVersion("");
      refresh();
    } catch (e) {
      toast(e.message || "上传失败", { tone: "error", icon: "alert" });
    } finally {
      setUploading(false); setProg(0);
      if (fileRef.current) fileRef.current.value = "";
    }
  };

  const onDelete = async (row) => {
    if (!window.confirm(`确认删除制品「${row.name}」(${row.version || "无版本"})?\n制品库中的二进制将被清理,已部署的应用不受影响。`)) return;
    try {
      await deleteArtifact(row.id);
      toast("制品已删除", { icon: "trash" });
      refresh();
    } catch (e) {
      toast(e.message || "删除失败", { tone: "error", icon: "alert" });
    }
  };

  const dl = (row) => {
    const a = document.createElement("a");
    a.href = `/api/artifacts/${encodeURIComponent(row.id)}/download`;
    document.body.appendChild(a); a.click(); a.remove();
  };

  const zone = (
    <div className="upload-zone" data-disabled={String(!canWrite)}
      onClick={() => canWrite && !uploading && fileRef.current && fileRef.current.click()}
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e) => { e.preventDefault(); if (canWrite && e.dataTransfer.files[0]) onUpload(e.dataTransfer.files[0]); }}>
      <input type="file" ref={fileRef} style={{ display: "none" }} onChange={(e) => onUpload(e.target.files[0])} />
      {uploading ? (
        <div style={{ maxWidth: 320, margin: "0 auto" }}>
          <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 8 }}>上传中 · {Math.min(100, prog | 0)}%</div>
          <Progress value={prog} height={6} />
        </div>
      ) : (
        <React.Fragment>
          <Icon name="archive" size={20} style={{ color: "var(--muted-fg)" }} />
          <div style={{ fontWeight: 600, marginTop: 7, fontSize: 13.5 }}>{canWrite ? "拖拽制品到此处,或点击选择文件" : "当前角色为只读,无上传权限"}</div>
          <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginTop: 3 }}>
            上传后留存(sha256 去重)· 可在部署对话框选用,免重复传大文件
          </div>
          {canWrite ? (
            <div style={{ marginTop: 10, display: "flex", gap: 8, alignItems: "center", justifyContent: "center" }}
              onClick={(e) => e.stopPropagation()}>
              <input className="input mono" style={{ width: 180, fontSize: 12.5 }} placeholder="版本标签(可选,如 v1.2.3)"
                value={version} onChange={(e) => setVersion(e.target.value)} />
            </div>
          ) : null}
        </React.Fragment>
      )}
    </div>
  );

  return (
    <div>
      <PageHead title="制品仓库 Artifacts" desc="版本化制品库 · 上传一次留存,可重复部署到任意应用 / Agent"
        actions={<Btn icon="rotate" onClick={refresh}>刷新</Btn>} />

      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        {zone}
        <div className="card" style={{ overflow: "hidden" }}>
          <table className="table">
            <thead><tr><th>制品</th><th>版本</th><th>大小</th><th>sha256</th><th>上传者</th><th>上传时间</th><th style={{ width: 90 }}></th></tr></thead>
            <tbody>
              {(rows || []).map((row) => (
                <tr key={row.id}>
                  <td><span className="mono" style={{ fontSize: 12, fontWeight: 500 }}>{row.name}</span></td>
                  <td>{row.version ? <Badge tone="info">{row.version}</Badge> : <span style={{ fontSize: 12, color: "var(--muted-fg)" }}>—</span>}</td>
                  <td><span className="mono" style={{ fontSize: 12 }}>{fmtBytes(row.size)}</span></td>
                  <td><span className="mono" style={{ fontSize: 11, color: "var(--muted-fg)" }}>{row.sha256.slice(0, 12)}…</span></td>
                  <td style={{ fontSize: 12.5 }}>{row.uploader || "—"}</td>
                  <td><span style={{ fontSize: 12, color: "var(--muted-fg)" }}>{fmtTime(row.createdAt)}({timeAgo(row.createdAt)})</span></td>
                  <td>
                    <div style={{ display: "flex", gap: 4, justifyContent: "flex-end" }}>
                      <Btn size="sm" variant="ghost" icon="download" title="下载" onClick={() => dl(row)}></Btn>
                      {canWrite ? <Btn size="sm" variant="ghost" icon="trash" title="删除" onClick={() => onDelete(row)}></Btn> : null}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {rows && rows.length === 0 ? <EmptyState icon="archive" title="制品库是空的" desc="上传第一个制品,后续部署可免重复上传" /> : null}
          {rows === null ? <div style={{ padding: 20, textAlign: "center" }}><Spinner size={16} /></div> : null}
        </div>
      </div>
    </div>
  );
}

export { ArtifactsPage };
