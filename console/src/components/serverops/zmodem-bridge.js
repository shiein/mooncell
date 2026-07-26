/**
 * zmodem2 Sender/Receiver 状态机桥接。
 *
 * 协议角色（相对浏览器）：
 * - 远端 sz → 浏览器 Receiver（下载到本地）
 * - 远端 rz → 浏览器 Sender（上传本地文件）
 *
 * 检测：输出流中出现 ZPAD(0x2A)+ZDLE(0x18) 起头的 ZMODEM 帧后进入传输态，
 * 期间二进制不再写入 xterm，避免污染终端。
 */

const ZPAD = 0x2a;
const ZDLE = 0x18;
// 简单探测窗口：避免误伤普通 `*` 输出
const DETECT_LOOKAHEAD = 8;

/**
 * @param {object} opts
 * @param {(u8: Uint8Array) => void} opts.sendToRemote 写入 WebSocket 二进制
 * @param {(u8: Uint8Array) => void} opts.writeToTerm 写入 xterm（非 ZMODEM 数据）
 * @param {number} opts.maxBytes zmodem 单次传输上限
 * @param {(info: {name: string, size: number, direction: string}) => void} [opts.onStart]
 * @param {(info: {name: string, direction: string}) => void} [opts.onComplete]
 * @param {(err: Error) => void} [opts.onError]
 */
export function createZmodemBridge(opts) {
  const {
    sendToRemote,
    writeToTerm,
    maxBytes = 512 * 1024 * 1024,
    onStart,
    onComplete,
    onError,
  } = opts;

  let Sender;
  let Receiver;
  let SenderEvent;
  let ReceiverEvent;
  let ready = false;
  let loading = null;

  /** @type {null | { kind: 'rx'|'tx', machine: any, fileParts?: Uint8Array[], fileName?: string, fileSize?: number, cancelled?: boolean }} */
  let active = null;
  let detectBuf = new Uint8Array(0);

  async function ensureLib() {
    if (ready) return true;
    if (!loading) {
      loading = import('zmodem2').then((mod) => {
        Sender = mod.Sender;
        Receiver = mod.Receiver;
        SenderEvent = mod.SenderEvent;
        ReceiverEvent = mod.ReceiverEvent;
        if (!Sender || !Receiver) {
          throw new Error('zmodem2 未导出 Sender/Receiver');
        }
        ready = true;
        return true;
      }).catch((e) => {
        loading = null;
        throw e;
      });
    }
    return loading;
  }

  function concat(a, b) {
    const out = new Uint8Array(a.length + b.length);
    out.set(a, 0);
    out.set(b, a.length);
    return out;
  }

  function findZmodemStart(u8) {
    for (let i = 0; i < u8.length - 1; i++) {
      if (u8[i] === ZPAD && u8[i + 1] === ZDLE) return i;
      if (i + 2 < u8.length && u8[i] === ZPAD && u8[i + 1] === ZPAD && u8[i + 2] === ZDLE) return i;
    }
    return -1;
  }

  function flushOutgoing(machine) {
    // 循环排空，避免一次 drain 后还有排队
    for (let i = 0; i < 64; i++) {
      const out = machine.drainOutgoing();
      if (!out || out.length === 0) break;
      sendToRemote(out);
      // drainOutgoing 已 auto-advance
    }
  }

  function pickLocalFile() {
    return new Promise((resolve) => {
      const input = document.createElement('input');
      input.type = 'file';
      input.style.display = 'none';
      document.body.appendChild(input);
      input.onchange = () => {
        const f = input.files && input.files[0];
        input.remove();
        resolve(f || null);
      };
      // 用户取消：部分浏览器不触发 onchange
      const t = setTimeout(() => {
        if (document.body.contains(input)) {
          input.remove();
          resolve(null);
        }
      }, 120000);
      input.addEventListener('change', () => clearTimeout(t), { once: true });
      input.click();
    });
  }

  function downloadBlob(name, parts) {
    const blob = new Blob(parts, { type: 'application/octet-stream' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = name || 'download';
    a.rel = 'noopener';
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30_000);
  }

  async function startReceiver(seed) {
    const rx = new Receiver();
    active = { kind: 'rx', machine: rx, fileParts: [], fileName: '', fileSize: 0 };
    rx.feedIncoming(seed);
    flushOutgoing(rx);
    pumpReceiverEvents(rx);
  }

  async function startSender(seed) {
    // initiator=false：等待远端 ZRINIT（rz）
    const tx = new Sender(false);
    active = { kind: 'tx', machine: tx, cancelled: false };
    tx.feedIncoming(seed);
    flushOutgoing(tx);

    const file = await pickLocalFile();
    if (!file || active?.cancelled) {
      active = null;
      onError && onError(new Error(file ? '已取消' : '未选择文件'));
      return;
    }
    if (file.size > maxBytes) {
      active = null;
      onError && onError(new Error(`超过 zmodem 上限 ${Math.floor(maxBytes / (1024 * 1024))} MiB，请改用 SFTP`));
      return;
    }
    onStart && onStart({ name: file.name, size: file.size, direction: 'upload' });
    tx.startFile(file.name, file.size, file.lastModified || undefined);
    flushOutgoing(tx);

    // 驱动发送：pollFile → feedFile → drain
    const pump = async () => {
      while (active && active.kind === 'tx' && active.machine === tx) {
        let req = tx.pollFile();
        while (req) {
          const end = Math.min(req.offset + req.len, file.size);
          const slice = file.slice(req.offset, end);
          const buf = new Uint8Array(await slice.arrayBuffer());
          tx.feedFile(buf);
          flushOutgoing(tx);
          // 背压：等待 WebSocket 缓冲下降由调用方 sendToRemote 侧处理
          req = tx.pollFile();
        }
        flushOutgoing(tx);
        const ev = tx.pollEvent();
        if (ev === SenderEvent.FileComplete || ev === 'FileComplete') {
          tx.finishSession();
          flushOutgoing(tx);
        }
        if (ev === SenderEvent.SessionComplete || ev === 'SessionComplete') {
          onComplete && onComplete({ name: file.name, direction: 'upload' });
          active = null;
          return;
        }
        // 短暂让出，等待更多 feedIncoming
        await new Promise((r) => setTimeout(r, 10));
        if (!active) return;
      }
    };
    pump().catch((e) => {
      active = null;
      onError && onError(e);
    });
  }

  function pumpReceiverEvents(rx) {
    for (;;) {
      const ev = rx.pollEvent();
      if (ev == null) break;
      if (ev === ReceiverEvent.FileStart || ev === 'FileStart') {
        const name = rx.getFileName() || 'download';
        const size = rx.getFileSize() || 0;
        if (size > maxBytes) {
          active = null;
          onError && onError(new Error(`远端文件超过 zmodem 上限，请改用 SFTP 下载`));
          return;
        }
        active.fileName = name;
        active.fileSize = size;
        active.fileParts = [];
        onStart && onStart({ name, size, direction: 'download' });
      } else if (ev === ReceiverEvent.FileComplete || ev === 'FileComplete') {
        // drain remaining file data
        for (;;) {
          const part = rx.drainFile();
          if (!part || part.length === 0) break;
          active.fileParts.push(part);
        }
        downloadBlob(active.fileName || 'download', active.fileParts);
        onComplete && onComplete({ name: active.fileName, direction: 'download' });
      } else if (ev === ReceiverEvent.SessionComplete || ev === 'SessionComplete') {
        active = null;
        return;
      }
    }
    // 持续 drain 文件数据
    if (active && active.kind === 'rx') {
      for (;;) {
        const part = rx.drainFile();
        if (!part || part.length === 0) break;
        active.fileParts.push(part);
        let total = 0;
        for (const p of active.fileParts) total += p.length;
        if (total > maxBytes) {
          active = null;
          onError && onError(new Error('接收数据超过 zmodem 上限'));
          return;
        }
      }
    }
  }

  /**
   * 处理终端方向的二进制输出。
   * @param {Uint8Array} u8
   */
  async function onTerminalOutput(u8) {
    if (!u8 || u8.length === 0) return;

    // 已在传输中：全部喂给状态机
    if (active) {
      try {
        active.machine.feedIncoming(u8);
        flushOutgoing(active.machine);
        if (active.kind === 'rx') pumpReceiverEvents(active.machine);
      } catch (e) {
        active = null;
        onError && onError(e instanceof Error ? e : new Error(String(e)));
      }
      return;
    }

    // 探测阶段：合并缓冲寻找 ZMODEM 起头
    detectBuf = concat(detectBuf, u8);
    const idx = findZmodemStart(detectBuf);
    if (idx < 0) {
      // 保留尾部少量字节以防帧跨包
      if (detectBuf.length > DETECT_LOOKAHEAD) {
        writeToTerm(detectBuf.subarray(0, detectBuf.length - DETECT_LOOKAHEAD));
        detectBuf = detectBuf.subarray(detectBuf.length - DETECT_LOOKAHEAD);
      }
      return;
    }

    // 帧前文本仍写终端
    if (idx > 0) writeToTerm(detectBuf.subarray(0, idx));
    const seed = detectBuf.subarray(idx);
    detectBuf = new Uint8Array(0);

    try {
      await ensureLib();
    } catch (e) {
      // 库不可用：原样写终端，避免吞数据
      writeToTerm(seed);
      onError && onError(e instanceof Error ? e : new Error(String(e)));
      return;
    }

    // 区分 sz(ZRQINIT 远端发) / rz(ZRINIT 远端收)：
    // 启发式：seed 中 hex 头常见 "B0000" 等；优先尝试 Receiver，若无进展且像接收端再切 Sender。
    // 更稳妥：同时看帧类型字节。ZRQINIT 帧类型 '0' / ZRINIT '1'（hex header 中）。
    const mode = guessMode(seed);
    try {
      if (mode === 'tx') {
        await startSender(seed);
      } else {
        await startReceiver(seed);
      }
    } catch (e) {
      active = null;
      writeToTerm(seed);
      onError && onError(e instanceof Error ? e : new Error(String(e)));
    }
  }

  /**
   * 粗判模式：hex 头 **\x18B0x 中 x 为帧类型十六进制。
   * ZRQINIT=0 → 远端发 → 浏览器 rx
   * ZRINIT=1 → 远端收 → 浏览器 tx
   */
  function guessMode(seed) {
    // 寻找 "B0" 后的类型半字节
    for (let i = 0; i < seed.length - 4; i++) {
      if (seed[i] === 0x42 /*B*/ && seed[i + 1] === 0x30 /*0*/) {
        const t = seed[i + 2];
        // '0' = ZRQINIT, '1' = ZRINIT
        if (t === 0x31) return 'tx';
        if (t === 0x30) return 'rx';
      }
    }
    // 默认按 sz（下载）处理
    return 'rx';
  }

  function cancel() {
    if (active) active.cancelled = true;
    active = null;
  }

  function isBusy() {
    return !!active;
  }

  return { onTerminalOutput, cancel, isBusy, ensureLib };
}
