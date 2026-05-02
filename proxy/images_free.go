package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// FreeAccountImageGenerator 实现 Free 账号的生图逻辑
// 使用 ChatGPT 对话接口 + system_hints: ["picture_v2"]
type FreeAccountImageGenerator struct {
	handler *Handler
}

// GenerateImage 使用 Free 账号生图
func (f *FreeAccountImageGenerator) GenerateImage(c *gin.Context, account *auth.Account, prompt string, size string, n int) ([]string, error) {
	log.Printf("[FreeAccountImageGenerator] Starting generation for account=%d prompt=%q size=%s n=%d", account.ID(), prompt, size, n)
	
	// 1. 准备请求参数
	convID := generateUUID()
	messageID := generateUUID()
	parentMsgID := generateUUID()

	// 2. 构建 /backend-api/f/conversation 请求
	payload := map[string]interface{}{
		"action": "next",
		"messages": []map[string]interface{}{
			{
				"id":     messageID,
				"author": map[string]string{"role": "user"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []string{prompt},
				},
			},
		},
		"conversation_id":               convID,
		"parent_message_id":             parentMsgID,
		"model":                         "auto", // 关键：使用 auto 而不是 gpt-5-3
		"timezone_offset_min":           -480,
		"system_hints":                  []string{"picture_v2"}, // 关键：标识图片生成
		"history_and_training_disabled": false,
	}

	body, _ := json.Marshal(payload)
	
	log.Printf("[FreeAccountImageGenerator] Request payload: %s", string(body))

	// 3. 直接发送到 /backend-api/f/conversation（不使用 ExecuteRequest，因为它硬编码了 /responses）
	account.Mu().RLock()
	accessToken := account.AccessToken
	account.Mu().RUnlock()
	
	if accessToken == "" {
		return nil, fmt.Errorf("no access token")
	}
	
	// 构建请求
	endpoint := "https://chatgpt.com/backend-api/f/conversation"
	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	
	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	
	// 使用代理
	proxyURL := f.handler.store.ResolveProxyForAccount(account)
	client := createHTTPClient(proxyURL, 120*time.Second)
	
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[FreeAccountImageGenerator] Request failed for account=%d: %v", account.ID(), err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("[FreeAccountImageGenerator] Upstream error for account=%d: status=%d, body=%s", account.ID(), resp.StatusCode, string(bodyBytes))
		return nil, fmt.Errorf("upstream error: status=%d, body=%s", resp.StatusCode, string(bodyBytes))
	}
	
	log.Printf("[FreeAccountImageGenerator] Got response for account=%d, parsing SSE...", account.ID())

	// 5. 解析 SSE 响应，提取图片引用
	imageRefs := []string{}
	scanner := NewSSEScanner(resp.Body)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		// 解析 JSON
		result := gjson.Parse(data)

		// 查找 file-service:// 或 sediment 引用
		message := result.Get("message")
		if message.Exists() {
			content := message.Get("content")
			if content.Exists() {
				parts := content.Get("parts")
				if parts.IsArray() {
					for _, part := range parts.Array() {
						partStr := part.String()
						// 提取 file-service:// 引用
						if strings.Contains(partStr, "file-service://") {
							// 提取文件ID
							if fid := extractFileID(partStr); fid != "" {
								imageRefs = append(imageRefs, fid)
							}
						}
					}
				}
			}
		}

		// 如果已经获得足够的图片，提前返回
		if len(imageRefs) >= n {
			break
		}
	}

	// 6. 如果 SSE 没有获得足够的图片，进行轮询
	if len(imageRefs) < n {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
		defer cancel()

		additionalRefs, _ := f.pollConversation(ctx, account, convID, n-len(imageRefs))
		imageRefs = append(imageRefs, additionalRefs...)
	}

	return imageRefs, nil
}

// pollConversation 轮询会话获取图片
func (f *FreeAccountImageGenerator) pollConversation(ctx context.Context, account *auth.Account, convID string, needed int) ([]string, error) {
	imageRefs := []string{}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return imageRefs, ctx.Err()
		case <-ticker.C:
			// 请求会话详情
			account.Mu().RLock()
			accessToken := account.AccessToken
			account.Mu().RUnlock()

			req, _ := http.NewRequestWithContext(ctx, "GET",
				fmt.Sprintf("https://chatgpt.com/backend-api/conversation/%s", convID),
				nil)
			req.Header.Set("Authorization", "Bearer "+accessToken)

			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}

			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// 解析响应查找图片引用
			result := gjson.ParseBytes(bodyBytes)
			mapping := result.Get("mapping")
			if mapping.Exists() {
				mapping.ForEach(func(key, value gjson.Result) bool {
					message := value.Get("message")
					if message.Exists() {
						content := message.Get("content")
						if content.Exists() {
							parts := content.Get("parts")
							if parts.IsArray() {
								for _, part := range parts.Array() {
									partStr := part.String()
									if strings.Contains(partStr, "file-service://") {
										if fid := extractFileID(partStr); fid != "" {
											// 避免重复
											found := false
											for _, ref := range imageRefs {
												if ref == fid {
													found = true
													break
												}
											}
											if !found {
												imageRefs = append(imageRefs, fid)
											}
										}
									}
								}
							}
						}
					}
					return len(imageRefs) < needed
				})
			}

			if len(imageRefs) >= needed {
				return imageRefs, nil
			}
		}
	}
}

// extractFileID 从文本中提取文件ID
func extractFileID(text string) string {
	// 查找 file-service://file-xxx 格式
	if idx := strings.Index(text, "file-service://"); idx >= 0 {
		start := idx + len("file-service://")
		end := start
		for end < len(text) && (text[end] != ' ' && text[end] != ')' && text[end] != ']' && text[end] != '\n') {
			end++
		}
		return text[start:end]
	}
	return ""
}

// generateUUID 生成简单的UUID
func generateUUID() string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		time.Now().UnixNano()&0xffffffff,
		time.Now().UnixNano()>>32&0xffff,
		0x4000|time.Now().UnixNano()>>48&0x0fff,
		0x8000|time.Now().UnixNano()&0x3fff,
		time.Now().UnixNano()&0xffffffffffff)
}

// SSEScanner 简单的 SSE 扫描器
type SSEScanner struct {
	reader  io.Reader
	buffer  []byte
	scanner *bytes.Buffer
}

func NewSSEScanner(r io.Reader) *SSEScanner {
	return &SSEScanner{
		reader:  r,
		buffer:  make([]byte, 4096),
		scanner: &bytes.Buffer{},
	}
}

func (s *SSEScanner) Scan() bool {
	n, err := s.reader.Read(s.buffer)
	if err != nil {
		return false
	}
	s.scanner.Write(s.buffer[:n])
	return true
}

func (s *SSEScanner) Text() string {
	text := s.scanner.String()
	s.scanner.Reset()
	return text
}

// DownloadImageAsBase64 下载图片并转换为 base64
func (f *FreeAccountImageGenerator) DownloadImageAsBase64(ctx context.Context, account *auth.Account, fileID string) (string, error) {
	// 构建下载 URL
	downloadURL := fmt.Sprintf("https://chatgpt.com/backend-api/files/%s/download", fileID)

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return "", err
	}

	account.Mu().RLock()
	accessToken := account.AccessToken
	account.Mu().RUnlock()

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: status=%d", resp.StatusCode)
	}

	// 读取图片数据
	imageData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// 转换为 base64
	return base64.StdEncoding.EncodeToString(imageData), nil
}

// createHTTPClient 创建带代理的 HTTP 客户端
func createHTTPClient(proxyURL string, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}
	
	if proxyURL != "" {
		if proxy, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(proxy)
		}
	}
	
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}
