package lovart

import "encoding/json"

// Artifact 是任务产物。content 是可直接下载的 URL（a.lovart.ai）。
type Artifact struct {
	Type     string          `json:"type"`
	Content  string          `json:"content"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// Task 对应 lgw 的 generator task。
type Task struct {
	GeneratorTaskID string          `json:"generator_task_id"`
	GeneratorName   string          `json:"generator_name"`
	Status          string          `json:"status"` // pending / running / completed / failed
	Error           string          `json:"error,omitempty"`
	ErrorCode       string          `json:"error_code,omitempty"`
	OriginalInput   json.RawMessage `json:"original_input_args,omitempty"`
	Artifacts       []Artifact      `json:"artifacts,omitempty"`
	CreatedAt       int64           `json:"created_at,omitempty"`
	EndedAt         int64           `json:"ended_at,omitempty"`
}

// Done 任务是否终态。
func (t *Task) Done() bool {
	switch t.Status {
	case "completed", "succeeded", "success", "failed", "error", "canceled", "cancelled":
		return true
	}
	return false
}

// Failed 任务是否失败终态。
func (t *Task) Failed() bool {
	switch t.Status {
	case "failed", "error", "canceled", "cancelled":
		return true
	}
	return false
}

// ImageURLs 抽取产物里的图片地址。
func (t *Task) ImageURLs() []string {
	out := make([]string, 0, len(t.Artifacts))
	for _, a := range t.Artifacts {
		if a.Content == "" {
			continue
		}
		if a.Type == "" || a.Type == "image" {
			out = append(out, a.Content)
		}
	}
	return out
}

// Generator 是 /generators/list 的一项。
type Generator struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"` // image / image-modify / video / video-modify
	Description string `json:"description"`
	IsVIP       bool   `json:"is_vip"`
	Index       int    `json:"index"`
	Icon        string `json:"icon"`
}

// generatorListResp 对应 {"code":0,"data":{"items":[…]}}。
type generatorListResp struct {
	Items []Generator `json:"items"`
}

// Project 是 /project/projectList 的一项（只取网关关心的字段）。
type Project struct {
	ProjectID   string   `json:"projectId"`
	ProjectName string   `json:"projectName"`
	PicCount    int      `json:"picCount"`
	CoverList   []string `json:"projectCoverList"`
	UpdateTime  int64    `json:"updateTime"`
}

type projectListResp struct {
	Total int       `json:"total"`
	Data  []Project `json:"data"`
}

// Thread 是 /agent/agentThreadList 的一项。
type Thread struct {
	ID            int64  `json:"id"`
	ProjectID     string `json:"projectId"`
	AgentThreadID string `json:"agentThreadId"`
	Text          string `json:"text"`
	UpdateTime    int64  `json:"updateTimestamp"`
}

type threadListResp struct {
	Total int      `json:"total"`
	Data  []Thread `json:"data"`
}

// Kit 是素材库。
type Kit struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	KitType    string `json:"kit_type"`
	Permission string `json:"permission"`
	VFSPath    string `json:"vfs_path"`
}

// Presign 是上传预签名。
type Presign struct {
	UploadURL     string `json:"upload_url"`
	OSSKey        string `json:"oss_key"`
	CallbackToken string `json:"callback_token"`
	ExpiresAt     int64  `json:"expires_at"`
	MaxSize       int64  `json:"max_size"`
}

// UploadedFile 是 confirm 之后的可访问文件。
type UploadedFile struct {
	SubType     string `json:"sub_type"`
	FileName    string `json:"file_name"`
	FileSize    int64  `json:"file_size"`
	ContentType string `json:"content_type"`
	OSSURL      string `json:"oss_url"`
	VFSPath     string `json:"vfs_path"`
}

// UploadedAsset 是 confirm 返回的 asset。
type UploadedAsset struct {
	ID        string         `json:"id"`
	KitID     string         `json:"kit_id"`
	AssetType string         `json:"asset_type"`
	Files     []UploadedFile `json:"files"`
}

type confirmResp struct {
	Asset UploadedAsset `json:"asset"`
}
