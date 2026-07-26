// 资源、授权、传输的 DTO 与内部模型。
// 明确不包含 password / private_key 等任何凭据字段。
package serverops

// Host key 状态（对外）。
const (
	HostKeyStatusUnconfirmed = "unconfirmed" // 尚未确认指纹
	HostKeyStatusTrusted     = "trusted"     // 已确认
	// 连接时比对失败不在此字段持久化，而是即时返回 HOST_KEY_MISMATCH。
)

// AccessMode 第一版只有「可运维」一种真实授权；admin 在 DTO 中显示为 admin。
const (
	AccessOperate = "operate"
	AccessAdmin   = "admin"
)

// 传输方向与状态。
const (
	DirectionUpload   = "upload"
	DirectionDownload = "download"

	TransferUploading      = "uploading"
	TransferCompleting     = "completing"
	TransferCompleted      = "completed"
	TransferCancelled      = "cancelled"
	TransferFailed         = "failed"
	TransferCleanupPending = "cleanup_pending"
	TransferExpired        = "expired"
)

// ServerResource 是库内完整形态（无密码）。
type ServerResource struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	Username         string `json:"username"`
	HostKeyAlgorithm string `json:"hostKeyAlgorithm"`
	HostKeySHA256    string `json:"hostKeySha256"`
	CreatedBy        string `json:"createdBy"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
}

// ResourceOut 是对外摘要：含当前用户权限与 host key 状态。
type ResourceOut struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	Username         string `json:"username"`
	HostKeyStatus    string `json:"hostKeyStatus"`
	HostKeyAlgorithm string `json:"hostKeyAlgorithm"`
	HostKeySHA256    string `json:"hostKeySha256"`
	AccessMode       string `json:"accessMode"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
}

// ResourceInput 是创建/更新请求体。
type ResourceInput struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}

// ServerResourceGrant 是用户管理中的服务器运维授权。
// 第一版无 accessMode 细分，有行即「可运维」。
type ServerResourceGrant struct {
	ResourceID string `json:"resourceId"`
	Username   string `json:"username,omitempty"`
	GrantedBy  string `json:"grantedBy,omitempty"`
	GrantedAt  int64  `json:"grantedAt,omitempty"`
}

// FileTransfer 是不含凭据的传输元数据（支持断点续传与精确清理）。
type FileTransfer struct {
	ID              string `json:"id"`
	ResourceID      string `json:"resourceId"`
	Username        string `json:"username"`
	Direction       string `json:"direction"`
	RemotePath      string `json:"remotePath"`
	RemoteTempPath  string `json:"remoteTempPath"`
	ExpectedSize    int64  `json:"expectedSize"`
	TransferredSize int64  `json:"transferredSize"`
	State           string `json:"state"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	ExpiresAt       int64  `json:"expiresAt"`
}

// HostKeyProbeResult 是探测草稿指纹的响应（不写库）。
type HostKeyProbeResult struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Algorithm string `json:"algorithm"`
	SHA256    string `json:"sha256"`
}

// SessionCreateResponse 是创建 SSH 会话成功响应。
// sessionId 仅作运行时句柄，不写 URL/localStorage。
type SessionCreateResponse struct {
	SessionID  string `json:"sessionId"`
	ResourceID string `json:"resourceId"`
	ExpiresAt  int64  `json:"expiresAt"`
}

// FileEntry 是 SFTP 目录项。
type FileEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Type       string `json:"type"` // file | directory | symlink | other
	Size       int64  `json:"size"`
	Mode       string `json:"mode"`
	ModifiedAt int64  `json:"modifiedAt"`
}

// FileListResponse 是列目录响应。
type FileListResponse struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}

// UploadInitResponse 是分块上传初始化响应。
type UploadInitResponse struct {
	TransferID string `json:"transferId"`
	ChunkSize  int    `json:"chunkSize"`
	NextOffset int64  `json:"nextOffset"`
	ExpiresAt  int64  `json:"expiresAt"`
}

func (r ServerResource) hostKeyStatus() string {
	if r.HostKeySHA256 == "" {
		return HostKeyStatusUnconfirmed
	}
	return HostKeyStatusTrusted
}

func toResourceOut(r ServerResource, accessMode string) ResourceOut {
	return ResourceOut{
		ID:               r.ID,
		Name:             r.Name,
		Host:             r.Host,
		Port:             r.Port,
		Username:         r.Username,
		HostKeyStatus:    r.hostKeyStatus(),
		HostKeyAlgorithm: r.HostKeyAlgorithm,
		HostKeySHA256:    r.HostKeySHA256,
		AccessMode:       accessMode,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}
