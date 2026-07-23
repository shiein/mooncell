// CSV/XLSX 导入：预览 → 字段映射 → 参数化 INSERT 批量执行。
//
// 设计文档第四节「导入」：
//   - 仅 CSV、XLSX。默认最大 100MB，可配置。
//   - 先上传预览前 20 行，再选择工作表、表头和字段映射。
//   - 仅执行参数化 INSERT，不支持 upsert、忽略冲突或部分成功。
//   - 默认每批 500 行，在一个独立事务内执行；任意一行失败则全部回滚。
//   - 有活动手工事务时禁止导入。
//   - 临时文件在完成、取消或 30 分钟超时后删除。
package dataresource

import (
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
	ImportPreviewRows    = 20           // 预览行数
	ImportBatchSize      = 500          // 每批行数
	ImportTempTimeout    = 30 * time.Minute // 临时文件超时
)

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
	// 有活动手工事务时禁止导入
	if s.pools.HasActiveTx(id) {
		writeErr(w, http.StatusConflict, "TX_ACTIVE", "存在活动手工事务，请先提交或回滚后再导入")
		return
	}

	// 解析 multipart 表单
	maxBytes := int64(s.importMaxMB) << 20
	if maxBytes <= 0 {
		maxBytes = 100 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "FILE_TOO_LARGE", "文件过大或格式错误")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
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
		writeErr(w, http.StatusBadRequest, "BAD_FORMAT", "仅支持 CSV 和 XLSX")
		return
	}

	// 保存到临时文件
	tmpPath := filepath.Join(os.TempDir(), "mc-import-"+newID()+ext)
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "TMP_ERROR", "创建临时文件失败")
		return
	}
	if _, err := io.Copy(tmpFile, file); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		writeErr(w, http.StatusInternalServerError, "TMP_ERROR", "保存临时文件失败")
		return
	}
	tmpFile.Close()

	// 解析预览
	session := &ImportSession{
		ID: newID(), ResourceID: id, Username: user,
		FilePath: tmpPath, FileName: header.Filename,
		Format: format, CreatedAt: time.Now(),
	}

	result, err := parsePreview(session)
	if err != nil {
		os.Remove(tmpPath)
		writeErr(w, http.StatusBadRequest, "PARSE_ERROR", err.Error())
		return
	}

	// 保存会话
	s.importMu.Lock()
	s.importSessions[session.ID] = session
	s.importMu.Unlock()

	writeOK(w, result)
}

// parsePreview 解析文件前 20 行作为预览。
func parsePreview(session *ImportSession) (*ImportPreviewResult, error) {
	switch session.Format {
	case "csv":
		return parseCSVPreview(session)
	case "xlsx":
		return parseXLSXPreview(session)
	}
	return nil, fmt.Errorf("不支持的格式: %s", session.Format)
}

func parseCSVPreview(session *ImportSession) (*ImportPreviewResult, error) {
	f, err := os.Open(session.FilePath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()
	// 跳过 BOM
	bom := make([]byte, 3)
	n, _ := f.Read(bom)
	if n < 3 || bom[0] != 0xEF || bom[1] != 0xBB || bom[2] != 0xBF {
		f.Seek(0, io.SeekStart)
	}

	reader := csv.NewReader(f)
	var preview [][]string
	for i := 0; i <= ImportPreviewRows; i++ {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析 CSV 失败: %w", err)
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

func parseXLSXPreview(session *ImportSession) (*ImportPreviewResult, error) {
	f, err := excelize.OpenFile(session.FilePath)
	if err != nil {
		return nil, fmt.Errorf("打开 XLSX 失败: %w", err)
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("XLSX 无工作表")
	}
	session.Sheet = sheets[0]
	rows, err := f.GetRows(session.Sheet)
	if err != nil {
		return nil, fmt.Errorf("读取 XLSX 失败: %w", err)
	}
	var preview [][]string
	for i, row := range rows {
		if i > ImportPreviewRows {
			break
		}
		preview = append(preview, row)
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
		Columns:  preview[0],
		Preview:  preview,
	}, nil
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

	var body struct {
		TableName     string   `json:"tableName"`
		Schema        string   `json:"schema"`
		ColumnMapping []string `json:"columnMapping"` // 目标列名，顺序与文件列对应；空字符串表示跳过
	}
	if err := jsonDecodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求格式错误")
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
	schema := res.DefaultSchema
	if schema == "" {
		schema = res.DatabaseName
	}
	adapter, err := NewAdapter(db, res.DBType, schema)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ADAPTER_ERROR", "创建适配器失败")
		return
	}

	result, err := executeImport(r.Context(), adapter, session, body.TableName, body.Schema, body.ColumnMapping)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "IMPORT_ERROR", err.Error())
		return
	}
	if result.Error != "" {
		s.auditLog(user, "导入数据", session.FileName+" → "+body.TableName, "失败·"+result.Error)
		writeOK(w, result)
		return
	}

	s.auditLog(user, "导入数据", session.FileName+" → "+body.TableName, fmt.Sprintf("成功·%d行", result.ImportedRows))
	// 成功：不再释放 inUse，直接销毁会话
	releaseInUse = false
	s.removeImportSession(importID)
	writeOK(w, result)
}

// executeImport 执行导入：校验目标表/列来自元数据，参数化 INSERT 在同一事务内按批执行。
func executeImport(ctx context.Context, adapter DataSourceAdapter, session *ImportSession, tableName, schema string, mapping []string) (*ImportExecuteResult, error) {
	start := time.Now()
	result := &ImportExecuteResult{}

	// 构建目标列名（过滤空映射）
	var targetCols []string
	for _, col := range mapping {
		if strings.TrimSpace(col) != "" {
			targetCols = append(targetCols, strings.TrimSpace(col))
		}
	}
	if len(targetCols) == 0 {
		result.Error = "无有效列映射"
		return result, nil
	}

	// 设计：动态对象名只能来自已读取的元数据——Describe 校验表与列存在
	resolvedCols, err := validateImportTarget(ctx, adapter, schema, tableName, targetCols)
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

	// 在一个事务中按 ImportBatchSize 批处理；任一行失败则全部回滚
	tx, err := adapter.Begin(ctx, false)
	if err != nil {
		result.Error = fmt.Sprintf("开启事务失败: %v", err)
		return result, nil
	}

	imported := 0
	rowNum := 1 // 1-based 含表头；数据行从 2 起
	err = streamImportRows(session, func(row []string, isHeader bool) error {
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
				args = append(args, row[j])
			} else {
				args = append(args, nil)
			}
		}
		if _, err := tx.Exec(ctx, insertSQL, args...); err != nil {
			return &importRowError{row: rowNum, msg: adapter.NormalizeError(err).Message}
		}
		imported++
		// 每批检查上下文取消
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
func validateImportTarget(ctx context.Context, adapter DataSourceAdapter, schema, tableName string, targetCols []string) ([]string, error) {
	obj := MetadataNode{Kind: NodeTable, Schema: schema, Name: tableName}
	structure, err := adapter.Describe(ctx, obj)
	if err != nil {
		return nil, fmt.Errorf("无法读取目标表结构: %w", err)
	}
	if len(structure.Columns) == 0 {
		return nil, fmt.Errorf("目标表不存在或无字段: %s", tableName)
	}
	exact := map[string]string{}  // lower -> actual
	for _, c := range structure.Columns {
		exact[c.Name] = c.Name
		exact[strings.ToLower(c.Name)] = c.Name
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
		return nil, fmt.Errorf("目标表不存在列: %s", col)
	}
	return resolved, nil
}

// streamImportRows 逐行回调文件内容；CSV 流式读取，XLSX 受 excelize 限制整表读入后逐行回调。
func streamImportRows(session *ImportSession, fn func(row []string, isHeader bool) error) error {
	switch session.Format {
	case "csv":
		return streamCSVRows(session.FilePath, fn)
	case "xlsx":
		rows, err := readAllXLSX(session.FilePath, session.Sheet)
		if err != nil {
			return err
		}
		for i, row := range rows {
			if err := fn(row, i == 0); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("不支持的格式")
}

func streamCSVRows(path string, fn func(row []string, isHeader bool) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bom := make([]byte, 3)
	n, _ := f.Read(bom)
	if n < 3 || bom[0] != 0xEF || bom[1] != 0xBB || bom[2] != 0xBF {
		f.Seek(0, io.SeekStart)
	}
	reader := csv.NewReader(f)
	reader.LazyQuotes = true
	reader.ReuseRecord = false
	first := true
	for {
		row, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(row, first); err != nil {
			return err
		}
		first = false
	}
}

func readAllXLSX(path, sheet string) ([][]string, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.GetRows(sheet)
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
	s.importMu.Unlock()
	if !exists || session.ResourceID != id || session.Username != user {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "导入会话不存在")
		return
	}
	s.removeImportSession(importID)
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
	var expired []string
	now := time.Now()
	for id, session := range s.importSessions {
		if now.Sub(session.CreatedAt) > ImportTempTimeout {
			expired = append(expired, id)
		}
	}
	s.importMu.Unlock()
	for _, id := range expired {
		s.removeImportSession(id)
	}
}
