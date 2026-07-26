package serverops

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateIdempotent(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaHasNoCredentialColumns(t *testing.T) {
	db := openTestDB(t)
	rows, err := db.Query(`PRAGMA table_info(server_resources)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, strings.ToLower(name))
	}
	for _, forbidden := range []string{"password", "password_cipher", "private_key", "private_key_cipher", "passphrase"} {
		for _, c := range cols {
			if c == forbidden || strings.Contains(c, "password") || strings.Contains(c, "private_key") {
				t.Fatalf("schema must not contain credential column %q (found %q)", forbidden, c)
			}
		}
	}
}

func TestResourceCRUDAndGrantFilter(t *testing.T) {
	db := openTestDB(t)
	now := int64(1_700_000_000_000)
	adminRes := ServerResource{
		ID: "srv_a", Name: "prod-01", Host: "10.0.0.1", Port: 22, Username: "ops",
		CreatedBy: "admin", CreatedAt: now, UpdatedAt: now,
	}
	if err := createResource(db, adminRes); err != nil {
		t.Fatal(err)
	}
	other := ServerResource{
		ID: "srv_b", Name: "prod-02", Host: "10.0.0.2", Port: 22, Username: "ops",
		CreatedBy: "admin", CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	if err := createResource(db, other); err != nil {
		t.Fatal(err)
	}

	// 普通用户无授权 → 空列表
	list, err := VisibleResources(db, "alice", "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty, got %d", len(list))
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := SetUserGrantsTx(tx, "alice", []ServerResourceGrant{{ResourceID: "srv_a"}}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	list, err = VisibleResources(db, "alice", "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "srv_a" {
		t.Fatalf("expected only srv_a, got %+v", list)
	}

	// 未授权 ID → RequireAccess 404
	_, err = RequireAccess(db, "alice", "user", "srv_b")
	if err == nil {
		t.Fatal("expected not found")
	}
	if ae, ok := err.(*APIError); !ok || ae.Code != CodeNotFound {
		t.Fatalf("expected NOT_FOUND, got %v", err)
	}

	// admin 可见全部
	all, err := VisibleResources(db, "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("admin expected 2, got %d", len(all))
	}
}

func TestValidateResourceInput(t *testing.T) {
	cases := []struct {
		in   ResourceInput
		want bool
	}{
		{ResourceInput{Name: "a", Host: "10.0.0.1", Port: 22, Username: "ops"}, true},
		{ResourceInput{Name: "a", Host: "http://evil", Port: 22, Username: "ops"}, false},
		{ResourceInput{Name: "a", Host: "host/path", Port: 22, Username: "ops"}, false},
		{ResourceInput{Name: "a", Host: "ok.example.com", Port: 0, Username: "ops"}, false},
		{ResourceInput{Name: "", Host: "ok", Port: 22, Username: "ops"}, false},
	}
	for _, c := range cases {
		in := c.in
		err := validateResourceInput(&in)
		if c.want && err != nil {
			t.Fatalf("input %+v expected ok, got %v", c.in, err)
		}
		if !c.want && err == nil {
			t.Fatalf("input %+v expected error", c.in)
		}
	}
}

func TestSessionManagerGeneration(t *testing.T) {
	m := newSessionManager()
	s := &Session{
		ID: "ssh_1", ResourceID: "srv_a", Username: "alice",
		ResourceGen: 0, UserGrantGen: 0,
	}
	// 假 client 为空；Close 仍可调用
	m.Register(s)
	if m.Count() != 1 {
		t.Fatal("expected 1 session")
	}
	m.BumpGrant("alice", "srv_a")
	// 关闭后从 map 移除
	if m.Count() != 0 {
		t.Fatalf("expected 0 after bump grant, got %d", m.Count())
	}
}

func TestOverwriteColumnAndTransferInsert(t *testing.T) {
	db := openTestDB(t)
	tr := FileTransfer{
		ID: "tx1", ResourceID: "r1", Username: "u", Direction: DirectionUpload,
		RemotePath: "/tmp/a", RemoteTempPath: "/tmp/.a.part",
		ExpectedSize: 10, TransferredSize: 0, Overwrite: true,
		State: TransferUploading, CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9999999999999,
	}
	// 资源行非必须；insert 不校验 FK
	if err := insertTransfer(db, tr); err != nil {
		t.Fatal(err)
	}
	got, ok, err := getTransfer(db, "tx1")
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if !got.Overwrite {
		t.Fatal("overwrite not persisted")
	}
}

func TestRecordTransferChunkAndResumeProof(t *testing.T) {
	db := openTestDB(t)
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	tr := FileTransfer{
		ID: "tx_chunks", ResourceID: "r1", Username: "u", Direction: DirectionUpload,
		RemotePath: "/tmp/a", RemoteTempPath: "/tmp/.a.mooncell-upload-x.part",
		ExpectedSize: 12, State: TransferUploading, CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9999999999999,
	}
	if err := insertTransfer(db, tr); err != nil {
		t.Fatal(err)
	}
	if err := recordTransferChunk(db, tr.ID, 0, 8, hashA, 8, 2); err != nil {
		t.Fatal(err)
	}
	if err := recordTransferChunk(db, tr.ID, 8, 4, hashB, 12, 3); err != nil {
		t.Fatal(err)
	}
	got, ok, err := getTransfer(db, tr.ID)
	if err != nil || !ok || got.TransferredSize != 12 {
		t.Fatalf("transfer progress: got=%+v ok=%v err=%v", got, ok, err)
	}
	chunks, err := listTransferChunks(db, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateResumeProof(12, chunks, []UploadChunkProof{
		{Offset: 0, Size: 8, SHA256: strings.ToUpper(hashA)},
		{Offset: 8, Size: 4, SHA256: hashB},
	}); err != nil {
		t.Fatalf("matching proof rejected: %v", err)
	}
	if err := validateResumeProof(12, chunks, []UploadChunkProof{
		{Offset: 0, Size: 8, SHA256: hashA},
		{Offset: 8, Size: 4, SHA256: hashA},
	}); err == nil {
		t.Fatal("different local prefix must be rejected")
	}
	if err := recordTransferChunk(db, tr.ID, 8, 1, hashA, 9, 4); err == nil {
		t.Fatal("stale offset must not overwrite chunk proof or progress")
	}
}

func TestSessionReservationIsAtomic(t *testing.T) {
	m := newSessionManager()
	const workers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes atomic.Int32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			if err := m.Reserve("alice", 1, 1); err == nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("expected exactly one reservation, got %d", got)
	}
	m.ReleaseReservation("alice")
	if err := m.Reserve("alice", 1, 1); err != nil {
		t.Fatalf("released reservation should be reusable: %v", err)
	}
}

func TestTransferReservationIsAtomic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SFTPMaxTransfersTotal = 1
	cfg.SFTPMaxTransfersPerUser = 1
	svc := NewService(openTestDB(t), cfg)
	const workers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes atomic.Int32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		id := i
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.reserveTransfer("tx_"+strconv.Itoa(id), "alice"); err == nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("expected exactly one transfer reservation, got %d", got)
	}
}

func TestUploadLockRemainsStableWhenSlotIsReleased(t *testing.T) {
	cfg := DefaultConfig()
	svc := NewService(openTestDB(t), cfg)
	const id = "tx_stable_lock"
	first := svc.uploadLock(id)
	if _, err := svc.reserveTransfer(id, "alice"); err != nil {
		t.Fatal(err)
	}
	svc.transferMu.Lock()
	svc.transferLastAct[id] = time.Now().Add(-transferSlotIdle - time.Minute)
	svc.transferMu.Unlock()
	svc.reapIdleTransferSlots()
	if got := svc.uploadLock(id); got != first {
		t.Fatal("idle slot reap must not replace a published upload mutex")
	}
	if _, err := svc.reserveTransfer(id, "alice"); err != nil {
		t.Fatal(err)
	}
	svc.releaseTransfer(id)
	if got := svc.uploadLock(id); got != first {
		t.Fatal("terminal slot release must not replace a published upload mutex")
	}
}

func TestCompletingRecoveryState(t *testing.T) {
	knownMissing := remotePathState{known: true}
	cases := []struct {
		name         string
		temp, target remotePathState
		want         string
		resolved     bool
	}{
		{
			name:   "temp remains",
			temp:   remotePathState{known: true, exists: true, size: 10},
			target: remotePathState{},
			want:   TransferCancelled, resolved: true,
		},
		{
			name:   "rename completed",
			temp:   knownMissing,
			target: remotePathState{known: true, exists: true, size: 10},
			want:   TransferCompleted, resolved: true,
		},
		{
			name: "neither exists",
			temp: knownMissing, target: knownMissing,
			want: TransferCancelled, resolved: true,
		},
		{
			name:     "target differs",
			temp:     knownMissing,
			target:   remotePathState{known: true, exists: true, size: 9},
			resolved: false,
		},
		{
			name: "stat uncertain",
			temp: remotePathState{}, target: knownMissing,
			resolved: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, resolved := completingRecoveryState(tc.temp, tc.target, 10)
			if got != tc.want || resolved != tc.resolved {
				t.Fatalf("got state=%q resolved=%v, want state=%q resolved=%v",
					got, resolved, tc.want, tc.resolved)
			}
		})
	}
}

func TestReadUploadChunkRejectsActualOverflow(t *testing.T) {
	exactReq := httptest.NewRequest("PUT", "/", bytes.NewReader(make([]byte, ChunkSize)))
	if got, err := readUploadChunk(httptest.NewRecorder(), exactReq); err != nil || len(got) != ChunkSize {
		t.Fatalf("exact chunk rejected: len=%d err=%v", len(got), err)
	}
	overflowReq := httptest.NewRequest("PUT", "/", bytes.NewReader(make([]byte, ChunkSize+1)))
	overflowReq.ContentLength = -1 // 模拟 chunked 请求，不能依赖 Content-Length。
	if _, err := readUploadChunk(httptest.NewRecorder(), overflowReq); !errors.Is(err, errChunkTooLarge) {
		t.Fatalf("expected actual overflow rejection, got %v", err)
	}
}

func TestManagedUploadTempPath(t *testing.T) {
	if !isManagedUploadTemp("/tmp/report.csv", "/tmp/.report.csv.mooncell-upload-abcd.part") {
		t.Fatal("expected managed temp path")
	}
	for _, temp := range []string{
		"/etc/passwd",
		"/tmp/.other.csv.mooncell-upload-abcd.part",
		"/var/tmp/.report.csv.mooncell-upload-abcd.part",
		"/tmp/.report.csv.part",
	} {
		if isManagedUploadTemp("/tmp/report.csv", temp) {
			t.Fatalf("must reject unmanaged temp path %q", temp)
		}
	}
}

func TestExpireTransferQueuesExactCleanupAndReleasesSlot(t *testing.T) {
	db := openTestDB(t)
	cfg := DefaultConfig()
	cfg.SFTPMaxTransfersTotal = 1
	cfg.SFTPMaxTransfersPerUser = 1
	svc := NewService(db, cfg)
	now := time.Now().UnixMilli()
	tr := FileTransfer{
		ID: "tx_expired", ResourceID: "r1", Username: "alice", Direction: DirectionUpload,
		RemotePath: "/tmp/a", RemoteTempPath: "/tmp/.a.mooncell-upload-x.part",
		ExpectedSize: 10, State: TransferUploading, CreatedAt: 1, UpdatedAt: 1, ExpiresAt: now - 1,
	}
	if err := insertTransfer(db, tr); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.reserveTransfer(tr.ID, tr.Username); err != nil {
		t.Fatal(err)
	}
	svc.expireStaleTransfers()
	got, ok, err := getTransfer(db, tr.ID)
	if err != nil || !ok {
		t.Fatalf("get expired transfer: ok=%v err=%v", ok, err)
	}
	if got.State != TransferCleanupPending {
		t.Fatalf("expected cleanup_pending, got %s", got.State)
	}
	if _, err := svc.reserveTransfer("tx_next", tr.Username); err != nil {
		t.Fatalf("expired transfer slot was not released: %v", err)
	}
}

func TestStuckCompletingKeepsStateAndReleasesSlot(t *testing.T) {
	db := openTestDB(t)
	cfg := DefaultConfig()
	cfg.SFTPMaxTransfersTotal = 1
	cfg.SFTPMaxTransfersPerUser = 1
	svc := NewService(db, cfg)
	now := time.Now().UnixMilli()
	tr := FileTransfer{
		ID: "tx_completing", ResourceID: "r1", Username: "alice", Direction: DirectionUpload,
		RemotePath: "/tmp/a", RemoteTempPath: "/tmp/.a.mooncell-upload-x.part",
		ExpectedSize: 10, TransferredSize: 10, State: TransferCompleting,
		CreatedAt: 1, UpdatedAt: now - int64(31*time.Minute/time.Millisecond),
		ExpiresAt: now + int64(time.Hour/time.Millisecond),
	}
	if err := insertTransfer(db, tr); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.reserveTransfer(tr.ID, tr.Username); err != nil {
		t.Fatal(err)
	}
	svc.expireStaleTransfers()
	got, ok, err := getTransfer(db, tr.ID)
	if err != nil || !ok {
		t.Fatalf("get completing transfer: ok=%v err=%v", ok, err)
	}
	if got.State != TransferCompleting {
		t.Fatalf("ambiguous completing state must be preserved, got %s", got.State)
	}
	if _, err := svc.reserveTransfer("tx_next", tr.Username); err != nil {
		t.Fatalf("stuck completing slot was not released: %v", err)
	}
	recoverable, err := listRecoverableTransfers(db, tr.ResourceID)
	if err != nil || len(recoverable) != 1 || recoverable[0].ID != tr.ID {
		t.Fatalf("completing transfer must be recoverable: list=%+v err=%v", recoverable, err)
	}
}

func TestSessionIdleReap(t *testing.T) {
	m := newSessionManager()
	s := &Session{
		ID: "ssh_idle", ResourceID: "srv_a", Username: "alice",
		ExpiresAt:   time.Now().Add(time.Hour),
		IdleTimeout: time.Second,
	}
	s.lastActivityUnix.Store(time.Now().Add(-2 * time.Second).Unix())
	m.Register(s)
	if n := m.ReapTimedOut(); n != 1 {
		t.Fatalf("reap want 1 got %d", n)
	}
	if m.Count() != 0 {
		t.Fatal("session should be gone")
	}
}
