// Mooncell — 系统(仅 admin):Console 自更新 + 预留系统级操作(版本信息、DB 备份等)
import React from 'react';
import { useMC } from '../lib/data.js';
import { Btn, Field, Icon, Spinner, EmptyState, toast, confirmDialog } from '../components/primitives.jsx';
import { PageHead } from '../components/Shell.jsx';
import { getConsoleInfo, consoleSelfUpdate } from '../lib/api.js';

function SystemPage() {
  const store = useMC();
  const [info, setInfo] = React.useState(null); // { version, os }
  const [busy, setBusy] = React.useState(false); // 上传/升级中
  const [version, setVersion] = React.useState("");
  const fileRef = React.useRef(null);

  const reload = React.useCallback(() => { getConsoleInfo().then(setInfo); }, []);
  React.useEffect(() => { reload(); }, [reload]);

  if (!store.can("admin")) {
    return <EmptyState icon="shield" title="无权访问" desc="系统页仅管理员可见" />;
  }

  // 升级后轮询 /api/console/info:版本变为新版即重启完成;超时提示手工核对。
  const waitRestart = (wantVer) => new Promise((resolve) => {
    const deadline = Date.now() + 30000;
    const tick = () => {
      getConsoleInfo().then((cur) => {
        if (cur && cur.version === wantVer) { resolve(true); return; }
        if (Date.now() > deadline) { resolve(false); return; }
        setTimeout(tick, 1000);
      }).catch(() => {
        // 重启瞬间断连属正常,继续轮询直到超时
        if (Date.now() > deadline) { resolve(false); return; }
        setTimeout(tick, 1000);
      });
    };
    // 先给后端 1s 完成 self-exec,再开始轮询
    setTimeout(tick, 1000);
  });

  const submit = async () => {
    const file = fileRef.current && fileRef.current.files[0];
    if (!file) { toast("请选择 Console 二进制文件", { tone: "warn" }); return; }
    const cur = info && info.version ? info.version : "?";
    const decl = version.trim();
    // 醒目提示先备份 mooncell.db(跨版本迁移风险)+ 就地重启/断连/在飞操作中断。
    const ok = await confirmDialog({
      title: "升级 Console", tone: "danger", icon: "rotate", confirmText: "上传并升级", width: 520,
      message:
        `将上传新 Console 二进制并就地替换自身重启(同 PID,约数秒断连)。\n\n` +
        `当前版本:${cur}${decl ? `\n目标版本:${decl}` : ""}\n\n` +
        `⚠ 升级前请先备份 mooncell.db(跨版本迁移风险,自更新只换二进制不动数据库)。\n` +
        `⚠ 重启瞬间在飞操作(部署/还原 SSE、分块上传会话、巡检)会被切断,建议在空闲窗口升级。\n` +
        `⚠ 进行中的上传会话是进程内状态,重启即丢,需重传。\n\n` +
        `校验链:sha256 + ELF 架构 + --selftest + --version,任一不过即保持旧版(无损)。\n` +
        `失败可手工 mv <可执行文件>.old 回滚。`,
    });
    if (!ok) return;
    setBusy(true);
    try {
      const r = await consoleSelfUpdate(file, decl);
      const want = r.version || decl || "?";
      toast(`已替换为 ${want},Console 正在就地重启...`);
      // 后端先返回 200 再 self-exec;轮询直到版本变为新版或超时。
      const confirmed = await waitRestart(want);
      if (confirmed) {
        toast(`已升级到 ${want},重启完成`, { icon: "check" });
        reload();
      } else {
        toast("30s 内未确认重启完成,请检查进程或用 <可执行文件>.old 手工回滚", { tone: "warn", icon: "alert" });
      }
      setVersion(""); if (fileRef.current) fileRef.current.value = "";
    } catch (e) {
      toast(e.message || "升级失败", { tone: "error", icon: "alert" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <PageHead title="系统" desc="Console 版本信息与自更新 · 预留系统级操作(DB 备份/运行参数等)"
        actions={null} />

      <div className="card card-pad" style={{ marginBottom: 14 }}>
        <h4 style={{ fontSize: 13.5, marginBottom: 10, display: "flex", alignItems: "center", gap: 7 }}>
          <Icon name="settings" size={14} style={{ color: "var(--primary)" }} />Console 版本
        </h4>
        <div style={{ display: "flex", gap: 24, flexWrap: "wrap", alignItems: "center" }}>
          <div>
            <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginBottom: 3 }}>当前版本</div>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <span className="mono" style={{ fontSize: 15, fontWeight: 600 }}>{info ? info.version : <Spinner size={14} />}</span>
            </div>
          </div>
          <div>
            <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginBottom: 3 }}>运行环境</div>
            <div className="mono" style={{ fontSize: 13 }}>{info ? info.os : "—"}</div>
          </div>
        </div>
      </div>

      <ConsoleUpgradeCard info={info} version={version} setVersion={setVersion}
        fileRef={fileRef} busy={busy} onSubmit={submit} />
    </div>
  );
}

// Console 升级包:管理员从浏览器直传新 Console 二进制(含内嵌前端 = 前后端一起升级)。
function ConsoleUpgradeCard({ info, version, setVersion, fileRef, busy, onSubmit }) {
  return (
    <div className="card card-pad">
      <h4 style={{ fontSize: 13.5, marginBottom: 4, display: "flex", alignItems: "center", gap: 7 }}>
        <Icon name="rotate" size={14} style={{ color: "var(--primary)" }} />Console 升级包
      </h4>
      <div style={{ fontSize: 12, color: "var(--muted-fg)", marginBottom: 12 }}>
        上传新 Console 二进制(//go:embed 内嵌前端,换一个二进制 = 前后端一起升级)。后端校验 sha256 + ELF 架构 +
        --selftest + --version 后替换自身并 self-exec 就地重启(同 PID,适配 nohup 无监管场景)。
      </div>

      <div style={{ display: "flex", gap: 10, alignItems: "flex-end", flexWrap: "wrap" }}>
        <div style={{ width: 160 }}>
          <Field label="声明版本号(可选,用于核对)">
            <input className="input mono" value={version} onChange={(e) => setVersion(e.target.value)} placeholder="留空则以二进制自报为准" />
          </Field>
        </div>
        <div>
          <Field label="Console 二进制文件">
            <input ref={fileRef} type="file" className="input" style={{ fontSize: 12, paddingTop: 6 }} />
          </Field>
        </div>
        <Btn variant="primary" icon="upload" disabled={busy} onClick={onSubmit}>{busy ? <Spinner size={12} /> : "上传并升级"}</Btn>
      </div>

      <div style={{ fontSize: 11.5, color: "var(--muted-fg)", marginTop: 10, lineHeight: 1.6 }}>
        仅接受匹配本机架构的 linux ELF;darwin 上跑的 Console(开发机)会拒绝任何上传(自更新是 Linux 部署特性)。<br />
        允许同版本覆盖(管理员常忘记改版本号);任一校验不过即保持旧版、删 <span className="mono">.new</span>,无损。
        失败可用备份 <span className="mono">&lt;可执行文件&gt;.old</span> 手工 <span className="mono">mv</span> 回滚。
      </div>

      {info && info.os && !info.os.startsWith("linux/") ? (
        <div style={{ display: "flex", gap: 9, padding: "10px 13px", borderRadius: 9, fontSize: 12.5, marginTop: 12, background: "var(--warn-soft)", color: "var(--warn)" }}>
          <Icon name="alert" size={15} style={{ flex: "none", marginTop: 1 }} />
          <span>当前 Console 运行于 {info.os},非 Linux 部署环境,自更新不可用(上传将被拒绝)。</span>
        </div>
      ) : null}
    </div>
  );
}

export { SystemPage };
