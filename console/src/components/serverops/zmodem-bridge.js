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
 * @param {(u8: Uint8Array) => Promise<void>} opts.sendToRemote 写入 WebSocket 二进制
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

  /** @type {null | { kind: 'rx'|'tx', machine: any, pendingInput: Uint8Array, fileParts?: Uint8Array[], fileName?: string, fileSize?: number, receivedBytes?: number, cancelled?: boolean }} */
  let active = null;
  let detectBuf = new Uint8Array(0);
  let processing = Promise.resolve();

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

  async function flushOutgoing(machine) {
    let sent = 0;
    // 循环排空，避免一次 drain 后还有排队
    for (let i = 0; i < 64; i++) {
      const out = machine.drainOutgoing();
      if (!out || out.length === 0) break;
      await sendToRemote(out);
      sent += out.length;
      // drainOutgoing 已 auto-advance
    }
    return sent;
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
    active = {
      kind: 'rx',
      machine: rx,
      pendingInput: new Uint8Array(0),
      fileParts: [],
      fileName: '',
      fileSize: 0,
      receivedBytes: 0,
    };
    await processActiveInput(seed);
  }

  async function startSender(seed) {
    // initiator=false：等待远端 ZRINIT（rz）
    const tx = new Sender(false);
    active = { kind: 'tx', machine: tx, pendingInput: new Uint8Array(0), cancelled: false };
    await processActiveInput(seed);

    const file = await pickLocalFile();
    if (!file || !active || active.machine !== tx || active.cancelled) {
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
    await flushOutgoing(tx);

    // 驱动发送：pollFile → feedFile → drain
    const pump = async () => {
      while (active && active.kind === 'tx' && active.machine === tx) {
        let req = tx.pollFile();
        while (req) {
          const end = Math.min(req.offset + req.len, file.size);
          const slice = file.slice(req.offset, end);
          const buf = new Uint8Array(await slice.arrayBuffer());
          tx.feedFile(buf);
          await flushOutgoing(tx);
          req = tx.pollFile();
        }
        await flushOutgoing(tx);
        // feedIncoming 可能因 pendingRequest 暂停并留下同一 WS 包的尾部；
        // 文件请求满足后主动继续消费，不依赖远端再发一个包来“唤醒”。
        await enqueue(() => processActiveInput());
        if (!active || active.machine !== tx) return;
        const ev = tx.pollEvent();
        if (ev === SenderEvent.FileComplete || ev === 'FileComplete') {
          tx.finishSession();
          await flushOutgoing(tx);
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
    let progressed = false;
    for (;;) {
      const ev = rx.pollEvent();
      if (ev == null) break;
      progressed = true;
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
        active.receivedBytes = 0;
        onStart && onStart({ name, size, direction: 'download' });
      } else if (ev === ReceiverEvent.FileComplete || ev === 'FileComplete') {
        // drain remaining file data
        for (;;) {
          const part = rx.drainFile();
          if (!part || part.length === 0) break;
          active.fileParts.push(part);
          active.receivedBytes += part.length;
          if (active.receivedBytes > maxBytes) {
            active = null;
            onError && onError(new Error('接收数据超过 zmodem 上限'));
            return true;
          }
        }
        downloadBlob(active.fileName || 'download', active.fileParts);
        onComplete && onComplete({ name: active.fileName, direction: 'download' });
      } else if (ev === ReceiverEvent.SessionComplete || ev === 'SessionComplete') {
        active = null;
        return true;
      }
    }
    // 持续 drain 文件数据
    if (active && active.kind === 'rx') {
      for (;;) {
        const part = rx.drainFile();
        if (!part || part.length === 0) break;
        progressed = true;
        active.fileParts.push(part);
        active.receivedBytes += part.length;
        if (active.receivedBytes > maxBytes) {
          active = null;
          onError && onError(new Error('接收数据超过 zmodem 上限'));
          return true;
        }
      }
    }
    return progressed;
  }

  // feedIncoming 可能只消费输入前缀；每次先排空 outgoing/file/event，
  // 再继续喂未消费尾部，直到状态机确实需要等待新的远端数据。
  async function processActiveInput(input) {
    if (!active) return;
    if (input && input.length) {
      active.pendingInput = concat(active.pendingInput, input);
    }
    for (let i = 0; i < 1024 && active; i++) {
      const current = active;
      let progressed = (await flushOutgoing(current.machine)) > 0;
      if (!active || active !== current) return;
      if (current.kind === 'rx' && pumpReceiverEvents(current.machine)) {
        progressed = true;
      }
      if (!active || active !== current) return;
      if ((await flushOutgoing(current.machine)) > 0) {
        progressed = true;
      }
      if (!current.pendingInput.length) return;
      // Sender 有待满足的本地文件请求时，feedIncoming 会返回 0 且不会接收新 wire 数据。
      // 先保留尾部，pump 满足请求后会再次调度本函数。
      if (current.kind === 'tx' && current.machine.pollFile()) return;

      const consumed = current.machine.feedIncoming(current.pendingInput);
      if (!Number.isInteger(consumed) || consumed < 0 || consumed > current.pendingInput.length) {
        throw new Error('ZMODEM 状态机返回了无效的消费长度');
      }
      if (consumed > 0) {
        current.pendingInput = current.pendingInput.slice(consumed);
        progressed = true;
      } else {
        // zmodem2 的 HeaderReader 会内部缓存不完整 header，但此时公开返回值仍为 0。
        // outgoing/file/event 已全部排空且 Sender 无 pollFile，说明本批输入已进入内部缓存。
        current.pendingInput = new Uint8Array(0);
        return;
      }
      if (!progressed) return;
    }
    if (active?.pendingInput.length) {
      throw new Error('ZMODEM 状态机未能收敛');
    }
  }

  function enqueue(work) {
    const next = processing.then(work);
    processing = next.catch((e) => {
      active = null;
      onError && onError(e instanceof Error ? e : new Error(String(e)));
    });
    return processing;
  }

  /**
   * 处理终端方向的二进制输出。
   * @param {Uint8Array} u8
   */
  async function processTerminalOutput(u8) {
    if (!u8 || u8.length === 0) return;

    // 已在传输中：全部喂给状态机
    if (active) {
      await processActiveInput(u8);
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

    // 帧前文本仍写终端；起始标记可能与帧类型跨 WebSocket 包，先等待到可判定角色。
    if (idx > 0) {
      writeToTerm(detectBuf.subarray(0, idx));
      detectBuf = detectBuf.subarray(idx);
    }
    const mode = guessMode(detectBuf);
    if (!mode) {
      if (detectBuf.length > 64) {
        writeToTerm(detectBuf.subarray(0, detectBuf.length - DETECT_LOOKAHEAD));
        detectBuf = detectBuf.subarray(detectBuf.length - DETECT_LOOKAHEAD);
      }
      return;
    }
    const seed = detectBuf;
    detectBuf = new Uint8Array(0);

    try {
      await ensureLib();
    } catch (e) {
      // 库不可用：原样写终端，避免吞数据
      writeToTerm(seed);
      onError && onError(e instanceof Error ? e : new Error(String(e)));
      return;
    }

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

  function onTerminalOutput(u8) {
    if (!u8 || u8.length === 0) return processing;
    // WebSocket message 回调不会等待 Promise；显式串行，防止并发 feed 同一状态机。
    const copy = u8.slice();
    return enqueue(() => processTerminalOutput(copy));
  }

  /**
   * 判定初始握手帧；返回 null 表示 header 尚未收完整。
   * hex 头 **\x18B 后是两位 frame type，binary 头在 A/C 后是原始 frame byte。
   * ZRQINIT=0 → 远端发 → 浏览器 rx
   * ZRINIT=1 → 远端收 → 浏览器 tx
   */
  function guessMode(seed) {
    const hex = (b) => {
      if (b >= 0x30 && b <= 0x39) return b - 0x30;
      if (b >= 0x61 && b <= 0x66) return b - 0x61 + 10;
      if (b >= 0x41 && b <= 0x46) return b - 0x41 + 10;
      return -1;
    };
    for (let i = 0; i < seed.length - 1; i++) {
      if (seed[i] !== ZDLE) continue;
      const encoding = seed[i + 1];
      let frame = -1;
      if (encoding === 0x42 /* B / ZHEX */) {
        if (i + 3 >= seed.length) return null;
        const hi = hex(seed[i + 2]);
        const lo = hex(seed[i + 3]);
        if (hi < 0 || lo < 0) return null;
        frame = (hi << 4) | lo;
      } else if (encoding === 0x41 || encoding === 0x43 /* A/C / ZBIN/ZBIN32 */) {
        if (i + 2 >= seed.length) return null;
        frame = seed[i + 2];
        if (frame === ZDLE) {
          if (i + 3 >= seed.length) return null;
          frame = seed[i + 3] ^ 0x40;
        }
      } else {
        continue;
      }
      if (frame === 0x01) return 'tx';
      if (frame === 0x00) return 'rx';
      return null;
    }
    return null;
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
