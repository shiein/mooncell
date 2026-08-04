package dataresource

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := MigrateDataResources(db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateIdempotent(t *testing.T) {
	db := testDB(t)
	// 再次迁移不应报错
	if err := MigrateDataResources(db); err != nil {
		t.Fatalf("重复迁移失败: %v", err)
	}
}

func TestCreateGetResource(t *testing.T) {
	db := testDB(t)
	r := DataResource{
		ID: "r1", Name: "PG Prod", DBType: DriverPostgreSQL,
		Host: "127.0.0.1", Port: 5432, DatabaseName: "mydb", Username: "u1",
		CredentialCipher: "cipher", SSLMode: "disable",
		CreatedBy: "admin", CreatedAt: 1, UpdatedAt: 1,
	}
	if err := CreateDataResource(db, r); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	got, ok, err := GetDataResource(db, "r1")
	if err != nil || !ok {
		t.Fatalf("查询失败: err=%v ok=%v", err, ok)
	}
	if got.Name != "PG Prod" || got.CredentialCipher != "" {
		t.Errorf("查询结果不符: %+v", got)
	}
}

func TestMigrateClearsLegacyCredentialCipher(t *testing.T) {
	db := testDB(t)
	r := DataResource{ID: "legacy", Name: "Legacy", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", SSLMode: "disable", CreatedBy: "a", CreatedAt: 1, UpdatedAt: 1}
	if err := CreateDataResource(db, r); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE data_resources SET credential_cipher = 'legacy-secret' WHERE id = 'legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateDataResources(db); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetDataResource(db, "legacy")
	if err != nil || !ok || got.CredentialCipher != "" {
		t.Fatalf("迁移必须清空历史密码密文: ok=%v err=%v cipher=%q", ok, err, got.CredentialCipher)
	}
}

func TestNameCaseInsensitiveUnique(t *testing.T) {
	db := testDB(t)
	r1 := DataResource{ID: "r1", Name: "Prod", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 1, UpdatedAt: 1}
	if err := CreateDataResource(db, r1); err != nil {
		t.Fatalf("创建 r1 失败: %v", err)
	}
	r2 := DataResource{ID: "r2", Name: "prod", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 2, UpdatedAt: 2}
	if err := CreateDataResource(db, r2); err == nil {
		t.Fatal("大小写不敏感唯一约束未生效: 'prod' 与 'Prod' 应冲突")
	}
}

func TestDeleteResourceCascades(t *testing.T) {
	db := testDB(t)
	r := DataResource{ID: "r1", Name: "R1", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 1, UpdatedAt: 1}
	CreateDataResource(db, r)
	UpdateTestStatus(db, "r1", TestStatusOK)
	SetUserGrants(db, "u1", []DataResourceGrant{{ResourceID: "r1", Username: "u1", AccessMode: AccessRead}}, "admin")
	CreateSavedSQL(db, SavedSQL{ID: "s1", Username: "u1", ResourceID: "r1", Name: "q1", SQLText: "SELECT 1", CreatedAt: 1, UpdatedAt: 1})

	if err := DeleteDataResource(db, "r1"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	grants, _ := UserGrants(db, "u1")
	if len(grants) != 0 {
		t.Errorf("删除资源后授权未清理: %d 条", len(grants))
	}
	saved, _ := ListSavedSQL(db, "u1", "r1")
	if len(saved) != 0 {
		t.Errorf("删除资源后保存 SQL 未清理: %d 条", len(saved))
	}
}

func TestSetUserGrantsTransactional(t *testing.T) {
	db := testDB(t)
	r := DataResource{ID: "r1", Name: "R1", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 1, UpdatedAt: 1}
	CreateDataResource(db, r)
	UpdateTestStatus(db, "r1", TestStatusOK)

	// 正常写入
	grants := []DataResourceGrant{
		{ResourceID: "r1", Username: "u1", AccessMode: AccessRead},
	}
	if err := SetUserGrants(db, "u1", grants, "admin"); err != nil {
		t.Fatalf("写入授权失败: %v", err)
	}
	got, _ := UserGrants(db, "u1")
	if len(got) != 1 || got[0].AccessMode != AccessRead {
		t.Errorf("授权查询不符: %+v", got)
	}

	// 全量替换
	grants2 := []DataResourceGrant{
		{ResourceID: "r1", Username: "u1", AccessMode: AccessWrite},
	}
	SetUserGrants(db, "u1", grants2, "admin")
	got2, _ := UserGrants(db, "u1")
	if len(got2) != 1 || got2[0].AccessMode != AccessWrite {
		t.Errorf("授权替换后不符: %+v", got2)
	}

	// 非法 access_mode 应失败且不留半成品
	grants3 := []DataResourceGrant{
		{ResourceID: "r1", Username: "u1", AccessMode: "invalid"},
	}
	if err := SetUserGrants(db, "u1", grants3, "admin"); err == nil {
		t.Fatal("非法 access_mode 应返回错误")
	}
	got3, _ := UserGrants(db, "u1")
	if len(got3) != 1 || got3[0].AccessMode != AccessWrite {
		t.Errorf("失败后授权应保持原值: %+v", got3)
	}

	// 未通过只读事务认证时禁止授予 read
	UpdateTestStatus(db, "r1", TestStatusOKNoRO)
	if err := SetUserGrants(db, "u1", []DataResourceGrant{{ResourceID: "r1", Username: "u1", AccessMode: AccessRead}}, "admin"); err == nil {
		t.Fatal("ok_no_ro 状态应拒绝授予 read")
	}
	// write 仍可授
	if err := SetUserGrants(db, "u1", []DataResourceGrant{{ResourceID: "r1", Username: "u1", AccessMode: AccessWrite}}, "admin"); err != nil {
		t.Fatalf("write 授权不应依赖只读认证: %v", err)
	}
}

func TestUpdateTestStatusAndRevokeRead(t *testing.T) {
	db := testDB(t)
	r := DataResource{ID: "r1", Name: "R1", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 1, UpdatedAt: 1}
	if err := CreateDataResource(db, r); err != nil {
		t.Fatal(err)
	}
	if err := UpdateTestStatus(db, "r1", TestStatusOK); err != nil {
		t.Fatal(err)
	}
	if err := SetUserGrants(db, "reader", []DataResourceGrant{{ResourceID: "r1", Username: "reader", AccessMode: AccessRead}}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := SetUserGrants(db, "writer", []DataResourceGrant{{ResourceID: "r1", Username: "writer", AccessMode: AccessWrite}}, "admin"); err != nil {
		t.Fatal(err)
	}

	revoked, err := UpdateTestStatusAndRevokeRead(db, "r1", TestStatusOKNoRO, r.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0] != "reader" {
		t.Fatalf("被撤销用户不符: %v", revoked)
	}
	readerGrants, err := UserGrants(db, "reader")
	if err != nil || len(readerGrants) != 0 {
		t.Fatalf("只读授权未撤销: grants=%v err=%v", readerGrants, err)
	}
	writerGrants, err := UserGrants(db, "writer")
	if err != nil || len(writerGrants) != 1 || writerGrants[0].AccessMode != AccessWrite {
		t.Fatalf("写授权不应被撤销: grants=%v err=%v", writerGrants, err)
	}
	got, ok, err := GetDataResource(db, "r1")
	if err != nil || !ok || got.LastTestStatus != TestStatusOKNoRO {
		t.Fatalf("测试状态未原子更新: resource=%+v ok=%v err=%v", got, ok, err)
	}
	// CAS：配置已变更（updated_at 不一致）则拒绝写回
	if _, err := UpdateTestStatusAndRevokeRead(db, "r1", TestStatusOK, r.UpdatedAt+999); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("期望 CONFIG_CHANGED, got %v", err)
	}
}

func TestUserAccessMode(t *testing.T) {
	db := testDB(t)
	r := DataResource{ID: "r1", Name: "R1", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 1, UpdatedAt: 1}
	CreateDataResource(db, r)
	UpdateTestStatus(db, "r1", TestStatusOK)
	SetUserGrants(db, "u1", []DataResourceGrant{{ResourceID: "r1", Username: "u1", AccessMode: AccessRead}}, "admin")

	// admin 隐式 admin
	mode, _ := UserAccessMode(db, "admin", "admin", "r1")
	if mode != "admin" {
		t.Errorf("admin 应返回 admin,实际 %s", mode)
	}
	// 授权用户
	mode, _ = UserAccessMode(db, "u1", "user", "r1")
	if mode != AccessRead {
		t.Errorf("u1 应返回 read,实际 %s", mode)
	}
	// 未授权用户
	mode, _ = UserAccessMode(db, "u2", "user", "r1")
	if mode != "" {
		t.Errorf("u2 应返回空,实际 %s", mode)
	}
}

func TestVisibleResources(t *testing.T) {
	db := testDB(t)
	r1 := DataResource{ID: "r1", Name: "R1", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 1, UpdatedAt: 1}
	r2 := DataResource{ID: "r2", Name: "R2", DBType: DriverMySQL, Host: "h", Port: 3306, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 2, UpdatedAt: 2}
	CreateDataResource(db, r1)
	CreateDataResource(db, r2)
	UpdateTestStatus(db, "r1", TestStatusOK)
	SetUserGrants(db, "u1", []DataResourceGrant{{ResourceID: "r1", Username: "u1", AccessMode: AccessRead}}, "admin")

	// admin 看全部
	all, _ := VisibleResources(db, "admin", "admin")
	if len(all) != 2 {
		t.Errorf("admin 应看到 2 个资源,实际 %d", len(all))
	}
	// 普通用户仅看授权的
	vis, _ := VisibleResources(db, "u1", "user")
	if len(vis) != 1 || vis[0].ID != "r1" {
		t.Errorf("u1 应只看到 r1,实际 %+v", vis)
	}
}

func TestSavedSQLUserIsolation(t *testing.T) {
	db := testDB(t)
	r1 := DataResource{ID: "r1", Name: "R1", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 1, UpdatedAt: 1}
	r2 := DataResource{ID: "r2", Name: "R2", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", CredentialCipher: "c", SSLMode: "disable", CreatedBy: "a", CreatedAt: 2, UpdatedAt: 2}
	CreateDataResource(db, r1)
	CreateDataResource(db, r2)

	CreateSavedSQL(db, SavedSQL{ID: "s1", Username: "u1", ResourceID: "r1", Name: "q1", SQLText: "SELECT 1", CreatedAt: 1, UpdatedAt: 1})
	CreateSavedSQL(db, SavedSQL{ID: "s2", Username: "u2", ResourceID: "r1", Name: "q2", SQLText: "SELECT 2", CreatedAt: 2, UpdatedAt: 2})

	// u1 只看到自己的
	l1, _ := ListSavedSQL(db, "u1", "r1")
	if len(l1) != 1 || l1[0].ID != "s1" {
		t.Errorf("u1 应只看到 s1,实际 %+v", l1)
	}
	// u2 不能更新 u1 的 SQL
	if err := UpdateSavedSQL(db, "s1", "u2", "r1", "hack", "DROP TABLE"); err == nil {
		t.Fatal("u2 不应能更新 u1 的 SQL")
	}
	// u2 不能删除 u1 的 SQL
	if err := DeleteSavedSQL(db, "s1", "u2", "r1"); err == nil {
		t.Fatal("u2 不应能删除 u1 的 SQL")
	}
	// 正确 owner 但 path 资源 id 不一致：不得改删
	if err := UpdateSavedSQL(db, "s1", "u1", "r2", "hack", "SELECT 9"); err == nil {
		t.Fatal("跨资源 path 不应能更新 SQL")
	}
	if err := DeleteSavedSQL(db, "s1", "u1", "r2"); err == nil {
		t.Fatal("跨资源 path 不应能删除 SQL")
	}
	// 正确 owner + 资源可更新
	if err := UpdateSavedSQL(db, "s1", "u1", "r1", "q1b", "SELECT 3"); err != nil {
		t.Fatalf("owner 更新失败: %v", err)
	}
}

func TestAuthVsPoolAffectingChanged(t *testing.T) {
	prev := DataResource{
		DBType: DriverDM, Host: "h", Port: 5236, DatabaseName: "", DefaultSchema: "OLD",
		Username: "u", SSLMode: "disable",
	}
	// 仅改 defaultSchema：不撤权，但要重建池
	in := DataResourceInput{
		Name: "R", DBType: DriverDM, Host: "h", Port: 5236, DefaultSchema: "NEW",
		Username: "u", SSLMode: "disable",
	}
	if authAffectingChanged(prev, in) {
		t.Fatal("仅改 defaultSchema 不应 authAffecting")
	}
	if !poolAffectingChanged(prev, in) {
		t.Fatal("仅改 defaultSchema 应对达梦 poolAffecting（DSN schema）")
	}
	// 改 host：两者皆 true
	in2 := in
	in2.Host = "h2"
	in2.DefaultSchema = "OLD"
	if !authAffectingChanged(prev, in2) || !poolAffectingChanged(prev, in2) {
		t.Fatal("改 host 应同时影响 auth 与 pool")
	}
	// 纯改名
	in3 := DataResourceInput{
		Name: "R2", DBType: prev.DBType, Host: prev.Host, Port: prev.Port,
		DatabaseName: prev.DatabaseName, DefaultSchema: prev.DefaultSchema,
		Username: prev.Username, SSLMode: prev.SSLMode,
	}
	if authAffectingChanged(prev, in3) || poolAffectingChanged(prev, in3) {
		t.Fatal("纯改名不应影响 auth/pool")
	}
}

func TestValidateInput(t *testing.T) {
	cases := []struct {
		name  string
		input DataResourceInput
		ok    bool
	}{
		{"正常", DataResourceInput{Name: "R", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", SSLMode: "disable"}, true},
		{"空名称", DataResourceInput{DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", SSLMode: "disable"}, false},
		{"非法类型", DataResourceInput{Name: "R", DBType: "oracle", Host: "h", Port: 5432, DatabaseName: "d", Username: "u", SSLMode: "disable"}, false},
		{"端口越界", DataResourceInput{Name: "R", DBType: DriverPostgreSQL, Host: "h", Port: 99999, DatabaseName: "d", Username: "u", SSLMode: "disable"}, false},
		{"非法SSL", DataResourceInput{Name: "R", DBType: DriverPostgreSQL, Host: "h", Port: 5432, DatabaseName: "d", Username: "u", SSLMode: "verify-full"}, false},
		{"DM拒绝require", DataResourceInput{Name: "R", DBType: DriverDM, Host: "h", Port: 5236, DatabaseName: "d", Username: "u", SSLMode: "require"}, false},
		{"DM允许disable", DataResourceInput{Name: "R", DBType: DriverDM, Host: "h", Port: 5236, DatabaseName: "d", Username: "u", SSLMode: "disable"}, true},
		{"DM无库名仅用户", DataResourceInput{Name: "R", DBType: DriverDM, Host: "h", Port: 5236, Username: "SYSDBA", SSLMode: "disable"}, true},
		{"DM用户名含冒号", DataResourceInput{Name: "R", DBType: DriverDM, Host: "h", Port: 5236, Username: "u:x", SSLMode: "disable"}, false},
		{"DM主机含分隔符", DataResourceInput{Name: "R", DBType: DriverDM, Host: "h@x", Port: 5236, Username: "u", SSLMode: "disable"}, false},
		{"DM schema 含问号", DataResourceInput{Name: "R", DBType: DriverDM, Host: "h", Port: 5236, DefaultSchema: "APP?x", Username: "u", SSLMode: "disable"}, false},
		{"DM schema 含与号", DataResourceInput{Name: "R", DBType: DriverDM, Host: "h", Port: 5236, DatabaseName: "APP&x", Username: "u", SSLMode: "disable"}, false},
	}
	for _, c := range cases {
		in := c.input
		err := ValidateInput(&in)
		if (err == nil) != c.ok {
			t.Errorf("%s: 期望 ok=%v,实际 err=%v", c.name, c.ok, err)
		}
	}
	// Trim 必须回写
	pad := DataResourceInput{Name: "  R  ", DBType: DriverPostgreSQL, Host: " h ", Port: 5432, DatabaseName: " d ", Username: " u ", SSLMode: "disable"}
	if err := ValidateInput(&pad); err != nil {
		t.Fatal(err)
	}
	if pad.Name != "R" || pad.Host != "h" || pad.DatabaseName != "d" || pad.Username != "u" {
		t.Fatalf("ValidateInput 应 Trim 字段: %+v", pad)
	}
}
