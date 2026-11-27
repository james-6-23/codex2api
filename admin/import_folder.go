package admin

import (
	"mime/multipart"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const (
	accountImportFolderRoute   = "/accounts/folder/import"
	accountImportPhaseHeader   = "X-Account-Import-Phase"
	accountImportSessionHeader = "X-Account-Import-Session"
	accountImportBatchHeader   = "X-Account-Import-Batch"
	accountImportOwnerKey      = "accountImportAdminDigest"

	folderImportTTL         = 5 * time.Minute
	folderImportMaxSessions = 4
	folderImportMaxBatches  = 100
	folderImportMaxFiles    = 100
	folderImportBatchBytes  = 150 << 20
	// Shared by all live sessions; includes upload bytes and a charge for parsed records.
	folderImportMaxBytes = 2 * security.DefaultMaxImportBodySize
)

// Only the authenticated import session can consume its staged batches. The
// store is local to the handler; restart/expiry fails closed and requires retry.
type accountImportFolderStore struct {
	sync.Mutex
	folders map[string]*accountImportFolder
	bytes   int64
}

type accountImportFolder struct {
	// Guarded by the store lock. All remaining fields belong to the busy request.
	owner   [32]byte
	busy    bool
	bytes   int64
	expires time.Time
	timer   *time.Timer

	settings   importSettings
	groupIDs   []int64
	preferAT   bool
	sealed     bool
	prepared   int
	committed  int
	tokenCount int
	batches    [][]importToken
	identities *importOAuthIdentityGuard
}

func (s *accountImportFolderStore) acquire(id string, owner [32]byte, create bool) (string, *accountImportFolder, int) {
	s.Lock()
	defer s.Unlock()
	if create {
		if len(s.folders) >= folderImportMaxSessions {
			return "", nil, http.StatusTooManyRequests
		}
		var err error
		id, err = oauthRandomHex(32)
		if err != nil {
			return "", nil, http.StatusInternalServerError
		}
		if s.folders == nil {
			s.folders = make(map[string]*accountImportFolder)
		}
		folder := &accountImportFolder{owner: owner, expires: time.Now().Add(folderImportTTL)}
		folder.timer = time.AfterFunc(folderImportTTL, func() { s.expire(id) })
		s.folders[id] = folder
	}
	folder := s.folders[id]
	if folder == nil || folder.owner != owner || folder.busy {
		return "", nil, http.StatusConflict
	}
	if !time.Now().Before(folder.expires) {
		s.remove(id, folder)
		return "", nil, http.StatusConflict
	}
	folder.busy = true
	return id, folder, 0
}

func (s *accountImportFolderStore) expire(id string) {
	s.Lock()
	defer s.Unlock()
	folder := s.folders[id]
	if folder == nil {
		return
	}
	if folder.busy {
		folder.timer.Reset(folderImportTTL)
	} else if remaining := time.Until(folder.expires); remaining > 0 {
		folder.timer.Reset(remaining)
	} else {
		s.remove(id, folder)
	}
}

func (s *accountImportFolderStore) remove(id string, folder *accountImportFolder) {
	delete(s.folders, id)
	s.bytes -= folder.bytes
	folder.timer.Stop()
}

func (s *accountImportFolderStore) release(id string, folder *accountImportFolder, keep bool) {
	s.Lock()
	defer s.Unlock()
	if !keep {
		s.remove(id, folder)
		return
	}
	folder.busy = false
	folder.expires = time.Now().Add(folderImportTTL)
	folder.timer.Reset(folderImportTTL)
}

func (s *accountImportFolderStore) reserve(folder *accountImportFolder, bytes int64) bool {
	s.Lock()
	defer s.Unlock()
	if bytes > folderImportMaxBytes-s.bytes {
		return false
	}
	s.bytes += bytes
	folder.bytes += bytes
	return true
}

func (h *Handler) importAccountFolder(c *gin.Context) {
	phase := c.GetHeader(accountImportPhaseHeader)
	value, _ := c.Get(accountImportOwnerKey)
	owner, ok := value.([32]byte)
	if !ok {
		writeError(c, http.StatusUnauthorized, "管理密钥无效或缺失")
		return
	}
	stage := phase == "stage" || phase == "stage-last"
	if !stage && phase != "prepare" && phase != "commit" && phase != "cancel" {
		writeError(c, http.StatusBadRequest, "无效的文件夹导入阶段")
		return
	}
	index, err := strconv.Atoi(c.GetHeader(accountImportBatchHeader))
	if phase != "cancel" && (err != nil || index < 0 || index >= folderImportMaxBatches) {
		writeError(c, http.StatusBadRequest, "无效的文件夹导入批次，最多 100 批")
		return
	}
	id := c.GetHeader(accountImportSessionHeader)
	s := &h.accountImportFolders
	id, folder, status := s.acquire(id, owner, stage && id == "" && index == 0)
	if folder == nil {
		writeError(c, status, "文件夹导入会话不可用，请重新导入")
		return
	}
	keep := true
	defer func() { s.release(id, folder, keep) }()

	switch phase {
	case "stage", "stage-last":
		if folder.sealed || index != len(folder.batches) || (index == folderImportMaxBatches-1 && phase != "stage-last") {
			break
		}
		keep = h.stageAccountImportBatch(c, folder, phase == "stage-last")
		if keep {
			c.Header(accountImportSessionHeader, id)
			c.Status(http.StatusNoContent)
		}
		return
	case "prepare":
		if !folder.sealed || index != folder.prepared || index >= len(folder.batches) {
			break
		}
		// Preflight uses the same local identity resolution as the write path. Proxy
		// registration remains part of commit; preflight never dials or registers proxies.
		folder.identities.collect(folder.batches[index])
		if c.Request.Context().Err() != nil {
			keep = false
			return
		}
		folder.prepared++
		c.Status(http.StatusNoContent)
		return
	case "commit":
		if !folder.sealed || folder.prepared != len(folder.batches) || index != folder.committed || index >= len(folder.batches) {
			break
		}
		if c.Request.Context().Err() != nil {
			keep = false
			return
		}
		c.Set(importGroupIDsContextKey, folder.groupIDs)
		if !h.commitAccountImportBatch(c, folder.batches[index], folder.settings, folder.identities) {
			keep = false
			return
		}
		folder.batches[index] = nil
		folder.committed++
		keep = folder.committed < len(folder.batches)
		return
	case "cancel":
		keep = false
		c.Status(http.StatusNoContent)
		return
	}
	writeError(c, http.StatusConflict, "文件夹导入批次未就绪或已处理")
}

func (h *Handler) stageAccountImportBatch(c *gin.Context, folder *accountImportFolder, last bool) bool {
	files, err := jsonImportFiles(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return false
	}
	defer c.Request.MultipartForm.RemoveAll()
	if len(folder.batches) == 0 {
		format, settings, ok := h.accountImportOptions(c)
		if !ok {
			return false
		}
		if format != "json" && format != "json_at" {
			writeError(c, http.StatusBadRequest, "文件夹导入会话只接受 JSON")
			return false
		}
		folder.preferAT = format == "json_at"
		folder.settings = settings
		folder.groupIDs = importGroupIDsFromContext(c)
	}
	fileBytes, status := folderImportBatchSize(files)
	if status == http.StatusBadRequest {
		writeError(c, status, "每个文件夹导入批次最多 100 个文件")
		return false
	}
	if status == http.StatusRequestEntityTooLarge {
		writeError(c, status, "多个文件的导入批次不得超过 150 MiB")
		return false
	}
	s := &h.accountImportFolders
	if !s.reserve(folder, fileBytes) {
		writeError(c, http.StatusRequestEntityTooLarge, "文件夹导入暂存空间不足")
		return false
	}
	tokens, err := parseJSONImportFiles(files, folder.preferAT)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return false
	}
	// Budget per-token structures as well as file bytes, including tiny records.
	if !s.reserve(folder, int64(len(tokens))*1024) {
		writeError(c, http.StatusRequestEntityTooLarge, "文件夹导入暂存空间不足")
		return false
	}
	folder.tokenCount += len(tokens)
	if last && folder.tokenCount == 0 {
		writeError(c, http.StatusBadRequest, "JSON 文件中未找到有效的账号凭据")
		return false
	}
	folder.batches = append(folder.batches, tokens)
	if last {
		folder.sealed = true
		folder.identities = newImportOAuthIdentityGuard(conflictingImportChatGPTIDs(folder.batches...), folder.settings.customHeaders)
	}
	return true
}

func folderImportBatchSize(files []*multipart.FileHeader) (int64, int) {
	if len(files) > folderImportMaxFiles {
		return 0, http.StatusBadRequest
	}
	var fileBytes int64
	for _, file := range files {
		fileBytes += file.Size
	}
	if len(files) > 1 && fileBytes > folderImportBatchBytes {
		return 0, http.StatusRequestEntityTooLarge
	}
	return fileBytes, 0
}
