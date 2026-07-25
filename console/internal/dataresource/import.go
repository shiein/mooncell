// CSV/XLSX 导入：预览 → 字段映射 → 参数化 INSERT 批量执行。
//
// 设计文档第四节「导入」：
//   - 仅 CSV、XLSX。默认最大 100MB，可配置。
//   - 先上传预览前 20 行，再选择工作表、表头和字段映射。
//   - 仅执行参数化 INSERT，不支持 upsert、忽略冲突或部分成功。
//   - 整单独立事务；任一行失败则全部回滚（与「不支持部分成功」一致）。
//     ImportBatchSize 仅用于周期检查 ctx 取消，不拆分提交。
//   - 有活动手工事务/其他导入/就地编辑时禁止导入。
//   - 临时文件在完成、取消或 30 分钟超时后删除。
package dataresource

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// 导入常量。
const (
	ImportPreviewRows     = 20               // 预览行数
	ImportBatchSize       = 500              // 周期检查取消的行间隔（非整单分批提交）
	ImportTempTimeout     = 30 * time.Minute // 临时文件超时
	importMaxSessionsUser = 3                // 每用户活动导入会话上限
	importMaxOpenFiles    = 20               // 全局活动导入会话上限
	importMaxFieldsPerRow = 1000             // 单行最大字段数，防恶意宽行
)

// excelOpenOptions 限制解压体积，避免 zip bomb 占满磁盘（默认 excelize 16GB）。
func (s *Service) excelOpenOptions() excelize.Options {
	maxMB := s.importMaxMB
	if maxMB <= 0 {
		maxMB = 100
	}
	// 解压上限取上传限制的 8 倍，且不低于 64MB、不高于 512MB
	limit := int64(maxMB) << 23 // *8 MiB
	if limit < 64<<20 {
		limit = 64 << 20
	}
	if limit > 512<<20 {
		limit = 512 << 20
	}
	return excelize.Options{
		UnzipSizeLimit:    limit,
		UnzipXMLSizeLimit: 32 << 20,
	}
}

// importDir 返回权限 0700 的私有导入目录。
func importDir() (string, error) {
	dir := filepath.Join(os.TempDir(), "mooncell-import")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ImportSession 是一个导入会话的内存状态。
type ImportSession struct {
	ID         string
	ResourceID string
	Username   string
	FilePath   string     // 临时文件路径
	FileName   string     // 原始文件名
	Format     string     // csv 或 xlsx
	Sheet      string     // XLSX 工作表名
	HeaderRow  int        // 表头行号（0-based）
	Preview    [][]string // 预览数据（含表头）
	Columns    []string   // 文件列名（来自表头）
	CreatedAt  time.Time
	// inUse 为 true 时表示正在 execute，禁止并发二次执行（由 importMu 保护）。
	inUse bool
}

// ImportPreviewResult 是预览返回。
type ImportPreviewResult struct {
	ImportID string     `json:"importId"`
	Format   string     `json:"format"`
	Sheets   []string   `json:"sheets,omitempty"` // XLSX 的工作表列表
	Sheet    string     `json:"sheet,omitempty"`  // 当前预览工作表
	Columns  []string   `json:"columns"`          // 表头列名
	Preview  [][]string `json:"preview"`          // 预览行（含表头）
}

// ImportExecuteResult 是执行结果。
type ImportExecuteResult struct {
	ImportedRows int    `json:"importedRows"`
	DurationMs   int64  `json:"durationMs"`
	Error        string `json:"error,omitempty"`
	ErrorRow     int    `json:"errorRow,omitempty"` // 失败行号（1-based）
}

// ImportPreviewHandler 处理 POST /api/data-resources/{id}/imports/preview
// 接收上传的文件，保存到临时路径，返回前 20 行预览。
func (s *Service) ImportPreviewHandler(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	id := r.PathValue("id")
	mode, _ := UserAccessMode(s.db, user, role, id)
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return
	}
	// 仅 write/admin 可导入
	if mode != "admin" && mode != AccessWrite {
		writeErr(w, http.StatusForbidden, "READ_ONLY", "只读授权不允许导入")
		return
	}
	// 有活动手工事务时禁止导入（execute 时再用 TryBeginImport 原子占用）
	if s.pools.HasActiveTx(id) {
		writeErr(w, http.StatusConflict, "TX_ACTIVE", "存在活动手工事务，请先提交或回滚后再导入")
		return
	}

	// 配额检查与占位同锁：避免并发 preview 双双通过后再写入导致超限
	sessionID := newID()
	s.importMu.Lock()
	userN, totalN := 0, 0
	for _, sess := range s.importSessions {
		totalN++
		if sess.Username == user {
			userN++
		}
	}
	if userN >= importMaxSessionsUser {
		s.importMu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "IMPORT_QUOTA", "导入会话过多，请先完成或取消已有导入")
		return
	}
	if totalN >= importMaxOpenFiles {
		s.importMu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "IMPORT_QUOTA", "系统导入会话已满，请稍后重试")
		return
	}
	s.importSessions[sessionID] = &ImportSession{
		ID: sessionID, ResourceID: id, Username: user, CreatedAt: time.Now(),
	}
	s.importMu.Unlock()

	releaseReservation := func(tmpPath string) {
		s.importMu.Lock()
		delete(s.importSessions, sessionID)
		s.importMu.Unlock()
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}

	// 解析 multipart 表单
	maxBytes := int64(s.importMaxMB) << 20
	if maxBytes <= 0 {
		maxBytes = 100 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		releaseReservation("")
		writeErr(w, http.StatusBadRequest, "FILE_TOO_LARGE", "文件过大或格式错误")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		releaseReservation("")
		writeErr(w, http.StatusBadRequest, "NO_FILE", "未上传文件")
		return
	}
	defer file.Close()

	// 判断格式
	ext := strings.ToLower(filepath.Ext(header.Filename))
	format := ""
	switch ext {
	case ".csv":
		format = "csv"
	case ".xlsx":
		format = "xlsx"
	default:
		releaseReservation("")
		writeErr(w, http.StatusBadRequest, "BAD_FORMAT", "仅支持 CSV 和 XLSX")
		return
	}

	// 私有目录 + 0600 临时文件
	dir, err := importDir()
	if err != nil {
		releaseReservation("")
		writeErr(w, http.StatusInternalServerError, "TMP_ERROR", "创建导入目录失败")
		return
	}
	tmpPath := filepath.Join(dir, "mc-import-"+sessionID+ext)
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		releaseReservation("")
		writeErr(w, http.StatusInternalServerError, "TMP_ERROR", "创建临时文件失败")
		return
	}
	if _, err := io.Copy(tmpFile, file); err != nil {
		tmpFile.Close()
		releaseReservation(tmpPath)
		writeErr(w, http.StatusInternalServerError, "TMP_ERROR", "保存临时文件失败")
		return
	}
	tmpFile.Close()

	// 解析预览
	session := &ImportSession{
		ID: sessionID, ResourceID: id, Username: user,
		FilePath: tmpPath, FileName: header.Filename,
		Format: format, CreatedAt: time.Now(),
	}

	result, err := s.parsePreview(session)
	if err != nil {
		releaseReservation(tmpPath)
		writeErr(w, http.StatusBadRequest, "PARSE_ERROR", err.Error())
		return
	}

	// 用完整会话覆盖占位（保留同一 id）
	s.importMu.Lock()
	if _, ok := s.importSessions[sessionID]; !ok {
		s.importMu.Unlock()
		os.Remove(tmpPath)
		writeErr(w, http.StatusConflict, "IMPORT_EXPIRED", "导入会话已过期，请重新上传")
		return
	}
	s.importSessions[sessionID] = session
	s.importMu.Unlock()

	writeOK(w, result)
}

// parsePreview 解析文件前 20 行作为预览。
func (s *Service) parsePreview(session *ImportSession) (*ImportPreviewResult, error) {
	switch session.Format {
	case "csv":
		return parseCSVPreview(session)
	case "xlsx":
		return s.parseXLSXPreview(session)
	}
	return nil, fmt.Errorf("不支持的格式: %s", session.Format)
}

func parseCSVPreview(session *ImportSession) (*ImportPreviewResult, error) {
	f, err := os.Open(session.FilePath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()
	reader := newImportCSVReader(f)
	var preview [][]string
	for i := 0; i <= ImportPreviewRows; i++ {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析 CSV 失败: %w", err)
		}
		if len(row) > importMaxFieldsPerRow {
			return nil, fmt.Errorf("单行字段数超过上限 %d", importMaxFieldsPerRow)
		}
		preview = append(preview, row)
	}
	if len(preview) == 0 {
		return nil, fmt.Errorf("CSV 文件为空")
	}
	session.Preview = preview
	session.Columns = preview[0]
	return &ImportPreviewResult{
		ImportID: session.ID,
		Format:   "csv",
		Columns:  preview[0],
		Preview:  preview,
	}, nil
}

// newImportCSVReader 预览与执行共用的 CSV 解析配置，避免字段切分不一致导致列映射错位。
func newImportCSVReader(r io.Reader) *csv.Reader {
	// 跳过 UTF-8 BOM（若有）
	br := &bomSkipReader{r: r}
	reader := csv.NewReader(br)
	reader.LazyQuotes = true
	reader.ReuseRecord = false
	reader.FieldsPerRecord = -1 // 允许行宽不一致；上限见 importMaxFieldsPerRow
	return reader
}

// bomSkipReader 在首次读时跳过 UTF-8 BOM。
type bomSkipReader struct {
	r       io.Reader
	checked bool
}

func (b *bomSkipReader) Read(p []byte) (int, error) {
	if !b.checked {
		b.checked = true
		var bom [3]byte
		n, err := io.ReadFull(b.r, bom[:])
		switch {
		case n == 3 && bom[0] == 0xEF && bom[1] == 0xBB && bom[2] == 0xBF:
			// 已跳过 BOM
		case n > 0:
			// 非 BOM：把已读字节拼回后续读取
			b.r = io.MultiReader(bytes.NewReader(bom[:n]), b.r)
		case err != nil && err != io.EOF && err != io.ErrUnexpectedEOF:
			return 0, err
		}
	}
	return b.r.Read(p)
}

func (s *Service) openXLSX(path string) (*excelize.File, error) {
	opts := excelize.Options{UnzipSizeLimit: 512 << 20, UnzipXMLSizeLimit: 32 << 20}
	if s != nil {
		opts = s.excelOpenOptions()
	}
	return excelize.OpenFile(path, opts)
}

func (s *Service) parseXLSXPreview(session *ImportSession) (*ImportPreviewResult, error) {
	f, err := s.openXLSX(session.FilePath)
	if err != nil {
		return nil, fmt.Errorf("打开 XLSX 失败: %w", err)
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("XLSX 无工作表")
	}
	if session.Sheet == "" {
		session.Sheet = sheets[0]
	}
	found := false
	for _, sheet := range sheets {
		if sheet == session.Sheet {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("工作表不存在: %s", session.Sheet)
	}
	// 流式只读前 N+1 行（含表头），避免 GetRows 整表入内存
	iter, err := f.Rows(session.Sheet)
	if err != nil {
		return nil, fmt.Errorf("读取 XLSX 失败: %w", err)
	}
	defer iter.Close()
	var preview [][]string
	for len(preview) <= ImportPreviewRows && iter.Next() {
		row, err := iter.Columns()
		if err != nil {
			return nil, fmt.Errorf("读取 XLSX 行失败: %w", err)
		}
		// Columns 返回底层缓冲复用风险：拷贝一份
		preview = append(preview, append([]string(nil), row...))
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("读取 XLSX 失败: %w", err)
	}
	if len(preview) == 0 {
		return nil, fmt.Errorf("XLSX 文件为空")
	}
	session.Preview = preview
	session.Columns = preview[0]
	return &ImportPreviewResult{
		ImportID: session.ID,
		Format:   "xlsx",
		Sheets:   sheets,
		Sheet:    session.Sheet,
		Columns:  preview[0],
		Preview:  preview,
	}, nil
}

// ImportSelectSheetHandler 切换 XLSX 工作表并重新生成预览。
func (s *Service) ImportSelectSheetHandler(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	id := r.PathValue("id")
	mode, _ := UserAccessMode(s.db, user, role, id)
	if mode != "admin" && mode != AccessWrite {
		writeErr(w, http.StatusForbidden, "READ_ONLY", "只读授权不允许导入")
		return
	}
	importID := r.PathValue("importId")
	var body struct {
		Sheet string `json:"sheet"`
	}
	if err := jsonDecodeBody(w, r, &body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	if strings.TrimSpace(body.Sheet) == "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "工作表不能为空")
		return
	}

	s.importMu.Lock()
	session, exists := s.importSessions[importID]
	if !exists || session.ResourceID != id || session.Username != user {
		s.importMu.Unlock()
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "导入会话不存在")
		return
	}
	if session.Format != "xlsx" {
		s.importMu.Unlock()
		writeErr(w, http.StatusBadRequest, "BAD_FORMAT", "CSV 不支持切换工作表")
		return
	}
	if session.inUse {
		s.importMu.Unlock()
		writeErr(w, http.StatusConflict, "IMPORT_IN_PROGRESS", "该导入会话正在执行中")
		return
	}
	session.inUse = true
	s.importMu.Unlock()

	oldSheet := session.Sheet
	session.Sheet = strings.TrimSpace(body.Sheet)
	result, err := s.parseXLSXPreview(session)
	if err != nil {
		session.Sheet = oldSheet
	}

	s.importMu.Lock()
	session.inUse = false
	s.importMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "PARSE_ERROR", err.Error())
		return
	}
	writeOK(w, result)
}

// ImportExecuteHandler 处理 POST /api/data-resources/{id}/imports/{importId}/execute
// 根据字段映射执行参数化 INSERT。
func (s *Service) ImportExecuteHandler(w http.ResponseWriter, r *http.Request) {
	user, role, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	id := r.PathValue("id")
	importID := r.PathValue("importId")

	mode, _ := UserAccessMode(s.db, user, role, id)
	if mode == "" {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "无权访问该资源")
		return
	}
	if mode != "admin" && mode != AccessWrite {
		writeErr(w, http.StatusForbidden, "READ_ONLY", "只读授权不允许导入")
		return
	}

	// 原子占用会话，防止同一 importId 并发 execute 双插
	s.importMu.Lock()
	session, exists := s.importSessions[importID]
	if !exists || session.ResourceID != id || session.Username != user {
		s.importMu.Unlock()
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "导入会话不存在")
		return
	}
	if session.inUse {
		s.importMu.Unlock()
		writeErr(w, http.StatusConflict, "IMPORT_IN_PROGRESS", "该导入会话正在执行中")
		return
	}
	session.inUse = true
	s.importMu.Unlock()
	// 失败可重试：清除 inUse；成功则 removeImportSession
	releaseInUse := true
	defer func() {
		if releaseInUse {
			s.importMu.Lock()
			if session != nil {
				session.inUse = false
			}
			s.importMu.Unlock()
		}
	}()

	// 与手工事务互斥：原子占用导入槽，堵住 HasActiveTx 检查与 Begin 之间的 TOCTOU
	if !s.pools.TryBeginImport(id) {
		writeErr(w, http.StatusConflict, "TX_ACTIVE", "存在活动手工事务，请先提交或回滚后再导入")
		return
	}
	defer s.pools.EndImport(id)

	var body struct {
		TableName     string   `json:"tableName"`
		Schema        string   `json:"schema"`
		ColumnMapping []string `json:"columnMapping"` // 目标列名，顺序与文件列对应；空字符串表示跳过
	}
	if err := jsonDecodeBody(w, r, &body); err != nil {
		writeJSONBodyError(w, err)
		return
	}
	if strings.TrimSpace(body.TableName) == "" {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "表名不能为空")
		return
	}

	// 获取适配器
	res, found, _ := GetDataResource(s.db, id)
	if !found {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	db, err := s.pools.GetDB(res)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "POOL_ERROR", "获取连接失败")
		return
	}
	adapter, err := NewAdapter(db, res.DBType, BoundSchema(res))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ADAPTER_ERROR", "创建适配器失败")
		return
	}

	ictx, cancel := context.WithTimeout(r.Context(), ImportTimeout)
	defer cancel()
	result, err := executeImport(ictx, adapter, session, body.TableName, body.Schema, body.ColumnMapping, s.excelOpenOptions())
	if err != nil {
		s.auditLog(user, "导入数据", session.FileName+" → "+body.TableName, "失败")
		writeErr(w, http.StatusInternalServerError, "IMPORT_ERROR", err.Error())
		return
	}
	if result.Error != "" {
		// 审计不落库原始错误全文，仅记行号
		audit := "失败"
		if result.ErrorRow > 0 {
			audit = fmt.Sprintf("失败·行%d", result.ErrorRow)
		}
		s.auditLog(user, "导入数据", session.FileName+" → "+body.TableName, audit)
		writeOK(w, result)
		return
	}

	s.auditLog(user, "导入数据", session.FileName+" → "+body.TableName, fmt.Sprintf("成功·%d行", result.ImportedRows))
	// 成功：不再释放 inUse，直接销毁会话
	releaseInUse = false
	s.removeImportSession(importID)
	writeOK(w, result)
}

// executeImport 执行导入：校验目标表/列来自元数据，参数化 INSERT 在同一事务内执行。
// xlsxOpts 控制 XLSX 解压上限（与 importMaxMB 配置一致）；CSV 路径忽略该参数。
func executeImport(ctx context.Context, adapter DataSourceAdapter, session *ImportSession, tableName, schema string, mapping []string, xlsxOpts excelize.Options) (*ImportExecuteResult, error) {
	start := time.Now()
	result := &ImportExecuteResult{}

	if len(mapping) != len(session.Columns) {
		result.Error = "列映射数量与导入文件不一致"
		return result, nil
	}

	// 构建目标列名（过滤空映射）
	var targetCols []string
	seenTargetCols := make(map[string]struct{})
	for _, col := range mapping {
		col = strings.TrimSpace(col)
		if col != "" {
			key := strings.ToLower(col)
			if _, exists := seenTargetCols[key]; exists {
				result.Error = "同一目标列不能重复映射"
				return result, nil
			}
			seenTargetCols[key] = struct{}{}
			targetCols = append(targetCols, col)
		}
	}
	if len(targetCols) == 0 {
		result.Error = "无有效列映射"
		return result, nil
	}

	// 设计：动态对象名只能来自已读取的元数据——Describe 校验表与列存在
	resolvedCols, nullableByCol, err := validateImportTarget(ctx, adapter, schema, tableName, targetCols)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	// 将 mapping 中的目标列替换为库内真实列名（保留大小写）
	resolvedMapping := make([]string, len(mapping))
	ri := 0
	for j, col := range mapping {
		if strings.TrimSpace(col) == "" {
			continue
		}
		resolvedMapping[j] = resolvedCols[ri]
		ri++
	}

	// 构建 INSERT SQL
	qualified := adapter.QuoteIdentifier(schema) + "." + adapter.QuoteIdentifier(tableName)
	if schema == "" {
		qualified = adapter.QuoteIdentifier(tableName)
	}
	placeholders := make([]string, len(resolvedCols))
	for i := range placeholders {
		// 1-based；PG 系为 $n，MySQL/DM 为 ?
		placeholders[i] = adapter.Placeholder(i + 1)
	}
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		qualified,
		joinIdentifiers(adapter, resolvedCols),
		strings.Join(placeholders, ", "))

	// 整单独立事务；任一行失败则全部回滚（不支持部分成功）
	tx, err := adapter.Begin(ctx, false)
	if err != nil {
		result.Error = fmt.Sprintf("开启事务失败: %v", err)
		return result, nil
	}

	imported := 0
	rowNum := 1 // 1-based 含表头；数据行从 2 起
	err = streamImportRows(session, xlsxOpts, func(row []string, isHeader bool) error {
		if isHeader {
			return nil
		}
		rowNum++
		args := make([]any, 0, len(resolvedCols))
		for j, col := range resolvedMapping {
			if col == "" {
				continue
			}
			if j < len(row) {
				// 空单元格与短行缺列统一为 NULL（可空列）；避免 "" 写入 integer/date 必败
				args = append(args, importCellValue(row[j], nullableByCol[col]))
			} else {
				args = append(args, nil)
			}
		}
		if _, err := tx.Exec(ctx, insertSQL, args...); err != nil {
			return &importRowError{row: rowNum, msg: adapter.NormalizeError(err).Message}
		}
		imported++
		// 周期检查上下文取消（非分批提交）
		if imported%ImportBatchSize == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		tx.Rollback()
		if re, ok := err.(*importRowError); ok {
			result.Error = fmt.Sprintf("第 %d 行插入失败: %s", re.row, re.msg)
			result.ErrorRow = re.row
			return result, nil
		}
		result.Error = err.Error()
		return result, nil
	}

	if err := tx.Commit(); err != nil {
		result.Error = fmt.Sprintf("提交失败: %v", err)
		return result, nil
	}

	result.ImportedRows = imported
	result.DurationMs = time.Since(start).Milliseconds()
	return result, nil
}

type importRowError struct {
	row int
	msg string
}

func (e *importRowError) Error() string { return e.msg }

// validateImportTarget 通过 Describe 确认表存在，且映射列均在表结构中。
// 返回解析后的真实列名，以及列名 → 是否可空（用于空单元格转 NULL）。
func validateImportTarget(ctx context.Context, adapter DataSourceAdapter, schema, tableName string, targetCols []string) ([]string, map[string]bool, error) {
	obj := MetadataNode{Kind: NodeTable, Schema: schema, Name: tableName}
	structure, err := adapter.Describe(ctx, obj)
	if err != nil {
		return nil, nil, fmt.Errorf("无法读取目标表结构: %w", err)
	}
	if len(structure.Columns) == 0 {
		return nil, nil, fmt.Errorf("目标表不存在或无字段: %s", tableName)
	}
	exact := map[string]string{} // lower -> actual
	nullable := map[string]bool{}
	for _, c := range structure.Columns {
		exact[c.Name] = c.Name
		exact[strings.ToLower(c.Name)] = c.Name
		nullable[c.Name] = c.IsNullable
	}
	resolved := make([]string, 0, len(targetCols))
	for _, col := range targetCols {
		if actual, ok := exact[col]; ok {
			resolved = append(resolved, actual)
			continue
		}
		if actual, ok := exact[strings.ToLower(col)]; ok {
			resolved = append(resolved, actual)
			continue
		}
		return nil, nil, fmt.Errorf("目标表不存在列: %s", col)
	}
	return resolved, nullable, nil
}

// importCellValue 将 CSV/XLSX 单元格转为绑定参数。
// 空串且列可空 → NULL（与短行缺列语义一致）；不可空列保留 "" 由数据库约束决定成败。
func importCellValue(cell string, nullable bool) any {
	if cell == "" && nullable {
		return nil
	}
	return cell
}

// streamImportRows 逐行回调文件内容；CSV 与 XLSX 均流式读取，避免整表入内存。
func streamImportRows(session *ImportSession, xlsxOpts excelize.Options, fn func(row []string, isHeader bool) error) error {
	switch session.Format {
	case "csv":
		return streamCSVRows(session.FilePath, fn)
	case "xlsx":
		return streamXLSXRows(session.FilePath, session.Sheet, xlsxOpts, fn)
	}
	return fmt.Errorf("不支持的格式")
}

func streamCSVRows(path string, fn func(row []string, isHeader bool) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := newImportCSVReader(f)
	first := true
	for {
		row, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if len(row) > importMaxFieldsPerRow {
			return fmt.Errorf("单行字段数超过上限 %d", importMaxFieldsPerRow)
		}
		if err := fn(row, first); err != nil {
			return err
		}
		first = false
	}
}

// streamXLSXRows 用 excelize Rows 迭代器逐行读取，不整表装入 [][]string。
// opts 须与 Service.excelOpenOptions 一致，避免硬编码绕过 importMaxMB。
func streamXLSXRows(path, sheet string, opts excelize.Options, fn func(row []string, isHeader bool) error) error {
	if opts.UnzipSizeLimit <= 0 {
		opts.UnzipSizeLimit = 512 << 20
	}
	if opts.UnzipXMLSizeLimit <= 0 {
		opts.UnzipXMLSizeLimit = 32 << 20
	}
	f, err := excelize.OpenFile(path, opts)
	if err != nil {
		return err
	}
	defer f.Close()
	iter, err := f.Rows(sheet)
	if err != nil {
		return err
	}
	defer iter.Close()
	first := true
	for iter.Next() {
		row, err := iter.Columns()
		if err != nil {
			return err
		}
		// 拷贝行，避免迭代器复用缓冲被回调侧持有时脏读
		cp := append([]string(nil), row...)
		if err := fn(cp, first); err != nil {
			return err
		}
		first = false
	}
	return iter.Error()
}

// ImportDeleteHandler 处理 DELETE /api/data-resources/{id}/imports/{importId}
func (s *Service) ImportDeleteHandler(w http.ResponseWriter, r *http.Request) {
	user, _, ok := userFromCtx(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "未登录")
		return
	}
	id := r.PathValue("id")
	importID := r.PathValue("importId")

	s.importMu.Lock()
	session, exists := s.importSessions[importID]
	if !exists || session.ResourceID != id || session.Username != user {
		s.importMu.Unlock()
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "导入会话不存在")
		return
	}
	if session.inUse {
		s.importMu.Unlock()
		writeErr(w, http.StatusConflict, "IMPORT_IN_PROGRESS", "该导入会话正在执行中")
		return
	}
	delete(s.importSessions, importID)
	s.importMu.Unlock()
	os.Remove(session.FilePath)
	writeOK(w, map[string]bool{"ok": true})
}

// removeImportSession 删除导入会话和临时文件。
func (s *Service) removeImportSession(id string) {
	s.importMu.Lock()
	session, ok := s.importSessions[id]
	if ok {
		delete(s.importSessions, id)
	}
	s.importMu.Unlock()
	if ok {
		os.Remove(session.FilePath)
	}
}

// joinIdentifiers 用适配器的 QuoteIdentifier 引用并拼接列名。
func joinIdentifiers(adapter DataSourceAdapter, names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = adapter.QuoteIdentifier(n)
	}
	return strings.Join(out, ", ")
}

// cleanupExpiredImports 清理超时的导入会话和临时文件。
func (s *Service) cleanupExpiredImports() {
	s.importMu.Lock()
	var expiredPaths []string
	now := time.Now()
	for id, session := range s.importSessions {
		if !session.inUse && now.Sub(session.CreatedAt) > ImportTempTimeout {
			delete(s.importSessions, id)
			expiredPaths = append(expiredPaths, session.FilePath)
		}
	}
	s.importMu.Unlock()
	for _, path := range expiredPaths {
		os.Remove(path)
	}
}
