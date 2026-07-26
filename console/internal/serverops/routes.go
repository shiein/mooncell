// 模块内统一注册路由。仅当 enabled 时由 consoleapp 调用。
package serverops

import "net/http"

// RegisterRoutes 将服务器运维 API 挂到 mux。
// auth 须已注入 Principal（WithUser）；admin 校验在 handler 内完成。
func (s *Service) RegisterRoutes(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	if wrap == nil {
		wrap = func(h http.HandlerFunc) http.HandlerFunc { return h }
	}

	// 资源管理
	mux.HandleFunc("GET /api/server-resources", wrap(s.ListResources))
	mux.HandleFunc("POST /api/server-resources", wrap(s.CreateResource))
	mux.HandleFunc("GET /api/server-resources/{id}", wrap(s.GetResource))
	mux.HandleFunc("PUT /api/server-resources/{id}", wrap(s.UpdateResource))
	mux.HandleFunc("DELETE /api/server-resources/{id}", wrap(s.DeleteResource))
	mux.HandleFunc("POST /api/server-resources/host-key/probe", wrap(s.ProbeHostKey))
	mux.HandleFunc("POST /api/server-resources/{id}/host-key/confirm", wrap(s.ConfirmHostKey))

	// 会话
	mux.HandleFunc("POST /api/server-resources/{id}/sessions", wrap(s.CreateSession))
	mux.HandleFunc("DELETE /api/server-resources/{id}/sessions/{sid}", wrap(s.DeleteSession))
	mux.HandleFunc("GET /api/server-resources/{id}/sessions/{sid}/terminal", wrap(s.TerminalWS))

	// SFTP
	mux.HandleFunc("GET /api/server-resources/{id}/sessions/{sid}/files", wrap(s.ListFiles))
	mux.HandleFunc("GET /api/server-resources/{id}/sessions/{sid}/download", wrap(s.DownloadFile))
	mux.HandleFunc("POST /api/server-resources/{id}/sessions/{sid}/uploads", wrap(s.InitUpload))
	mux.HandleFunc("PUT /api/server-resources/{id}/sessions/{sid}/uploads/{tid}", wrap(s.UploadChunk))
	mux.HandleFunc("GET /api/server-resources/{id}/uploads/{tid}", wrap(s.GetUploadStatus))
	mux.HandleFunc("POST /api/server-resources/{id}/sessions/{sid}/uploads/{tid}/resume", wrap(s.ResumeUpload))
	mux.HandleFunc("POST /api/server-resources/{id}/sessions/{sid}/uploads/{tid}/complete", wrap(s.CompleteUpload))
	mux.HandleFunc("DELETE /api/server-resources/{id}/sessions/{sid}/uploads/{tid}", wrap(s.CancelUpload))
}
