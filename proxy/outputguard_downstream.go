package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	// GuardWriteDeadlineTimeout 是已提交响应头后写入 guard 终态帧的有界超时。
	// 它只约束下游尽力通知，绝不参与、也不阻塞上游取消。
	GuardWriteDeadlineTimeout = 5 * time.Second
)

// ApplyDownstreamWriteDeadline 通过 http.ResponseController 给下游写施加
// 有界超时。timeout<=0 表示清除 deadline。底层不支持时返回
// http.ErrNotSupported，调用方必须放弃写而不是退化为可能无界阻塞的同步写。
func ApplyDownstreamWriteDeadline(w http.ResponseWriter, timeout time.Duration) error {
	if w == nil {
		return errors.New("nil response writer")
	}
	rc := http.NewResponseController(w)
	if rc == nil {
		return http.ErrNotSupported
	}
	if timeout <= 0 {
		return rc.SetWriteDeadline(time.Time{})
	}
	return rc.SetWriteDeadline(time.Now().Add(timeout))
}

// FormatGuardTerminalEnvelope 生成各协议脱敏后的 guard 终态帧。
// 返回的 status 仅在响应头未提交时使用（502）；已提交时忽略。
// 所有协议都不携带内部触发 reason。
func FormatGuardTerminalEnvelope(protocol string, streamID string) ([]byte, int, error) {
	normProto := strings.ToLower(strings.TrimSpace(protocol))
	switch normProto {
	case "responses":
		payload, err := json.Marshal(gin.H{
			"type": "response.failed",
			"response": gin.H{
				"created_at": time.Now().Unix(),
				"status":     "failed",
				"error": gin.H{
					"message": GuardTriggeredPublicMessage,
					"type":    api.ErrorTypeUpstream,
					"code":    GuardTriggeredPublicCode,
				},
			},
		})
		if err != nil {
			return nil, 0, err
		}
		return []byte("data: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "chat":
		payload, err := json.Marshal(gin.H{
			"error": gin.H{
				"message": GuardTriggeredPublicMessage,
				"type":    api.ErrorTypeUpstream,
				"code":    GuardTriggeredPublicCode,
			},
		})
		if err != nil {
			return nil, 0, err
		}
		return []byte("data: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "anthropic":
		payload, err := json.Marshal(gin.H{
			"type":  "error",
			"error": gin.H{"type": "api_error", "message": GuardTriggeredPublicMessage},
		})
		if err != nil {
			return nil, 0, err
		}
		return []byte("event: error\ndata: " + string(payload) + "\n\n"), http.StatusOK, nil

	case "gemini":
		// Native Gemini SDK 不解析 SSE；错误必须是无 data: 前缀的裸 JSON。
		payload, err := json.Marshal(gin.H{
			"error": gin.H{"code": http.StatusBadGateway, "message": GuardTriggeredPublicMessage, "status": "UNAVAILABLE"},
		})
		if err != nil {
			return nil, 0, err
		}
		return payload, http.StatusOK, nil

	case "websocket":
		payload, err := json.Marshal(struct {
			Type     string        `json:"type"`
			StreamID string        `json:"stream_id,omitempty"`
			Status   int           `json:"status"`
			Error    *api.APIError `json:"error"`
		}{
			Type:     "error",
			StreamID: streamID,
			Status:   http.StatusBadGateway,
			Error:    api.NewAPIError(GuardTriggeredPublicCode, GuardTriggeredPublicMessage, api.ErrorTypeUpstream),
		})
		if err != nil {
			return nil, 0, err
		}
		return payload, http.StatusOK, nil

	default:
		payload, err := json.Marshal(gin.H{
			"error": gin.H{"message": GuardTriggeredPublicMessage, "type": api.ErrorTypeUpstream, "code": GuardTriggeredPublicCode},
		})
		if err != nil {
			return nil, 0, err
		}
		return payload, http.StatusBadGateway, nil
	}
}

// SendDownstreamGuardTerminalHTTP 尽力向下游 HTTP 客户端发送 guard 终态。
//
// 未提交响应头：直接返回 502 JSON 错误。
// 已提交流式响应头：通过 TryLock 非阻塞获取下游写锁，锁内用 ResponseController
// 施加 5s 写超时后写出协议终态帧；锁被占用、deadline 不受支持、连接已断开或
// 写入失败时立即放弃（clientGone=true），绝不启动孤儿 goroutine 拼死写入。
//
// 无论成功与否，该函数都不会阻塞或延迟上游取消：调用方必须先执行
// GuardTerminationCoordinator.HandleTripped。
func SendDownstreamGuardTerminalHTTP(c *gin.Context, mu *sync.Mutex, clientGone *bool, protocol string, reason string) error {
	if c == nil || c.Writer == nil {
		if clientGone != nil {
			*clientGone = true
		}
		return errors.New("nil context or writer")
	}

	if !c.Writer.Written() {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusBadGateway, gin.H{
			"error": gin.H{"message": GuardTriggeredPublicMessage, "type": api.ErrorTypeUpstream, "code": GuardTriggeredPublicCode},
		})
		if clientGone != nil {
			*clientGone = true
		}
		return nil
	}

	if mu != nil {
		if !mu.TryLock() {
			if clientGone != nil {
				*clientGone = true
			}
			return errors.New("downstream write lock busy; dropped guard terminal notification")
		}
		defer mu.Unlock()
	}

	if c.Request != nil {
		if err := c.Request.Context().Err(); err != nil {
			if clientGone != nil {
				*clientGone = true
			}
			return err
		}
	}

	if err := ApplyDownstreamWriteDeadline(c.Writer, GuardWriteDeadlineTimeout); err != nil {
		if clientGone != nil {
			*clientGone = true
		}
		return fmt.Errorf("set write deadline failed (%w); safely dropped write", err)
	}
	defer func() { _ = ApplyDownstreamWriteDeadline(c.Writer, 0) }()

	envelope, _, err := FormatGuardTerminalEnvelope(protocol, "")
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(envelope); err != nil {
		if clientGone != nil {
			*clientGone = true
		}
		return err
	}
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
	if clientGone != nil {
		*clientGone = true
	}
	return nil
}

// SendDownstreamGuardTerminalWS 向 Responses WebSocket 连接写入带 stream_id 的
// 脱敏 error 帧。写超时有界；调用方负责后续关闭连接。
//
// 并发写契约：gorilla/websocket 禁止同一 conn 上出现并发写（并发写会 panic）。
// 调用方必须保证同一 conn 上无并发写（含流式转发 goroutine）；本函数不做任何
// 连接级写串行化，也不隐式等待转发侧写入结束。后续 wsrelay 接线时须由调用方
// 用同一把写锁/单写 goroutine 保证互斥。
func SendDownstreamGuardTerminalWS(conn *websocket.Conn, streamID string, reason string) error {
	if conn == nil {
		return errors.New("nil websocket conn")
	}
	envelope, _, err := FormatGuardTerminalEnvelope("websocket", streamID)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(GuardWriteDeadlineTimeout))
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	return conn.WriteMessage(websocket.TextMessage, envelope)
}
