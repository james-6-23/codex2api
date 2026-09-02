package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	proxyBatchTestConcurrency = 4
	proxyBatchTestMaxIDs      = 100
	proxyBatchTestMaxBody     = 64 << 10
	proxyProbeMaxBody         = 1 << 20
	proxyProbeIPAPIFields     = "status,message,country,regionName,city,isp,query"
)

var (
	proxyProbeIPv4URLFn      = defaultProxyProbeIPv4URL
	proxyProbeIPv6EchoURLsFn = defaultProxyProbeIPv6EchoURLs
	proxyProbeLookupGeoFn    = lookupProxyExitGeo
	proxyProbeGeoHTTPClient  = &http.Client{Timeout: 5 * time.Second}
)

type proxyProbeResult struct {
	Success    bool   `json:"success"`
	Conclusive bool   `json:"conclusive"`
	IP         string `json:"ip,omitempty"`
	Country    string `json:"country,omitempty"`
	Region     string `json:"region,omitempty"`
	City       string `json:"city,omitempty"`
	ISP        string `json:"isp,omitempty"`
	LatencyMs  int    `json:"latency_ms,omitempty"`
	Location   string `json:"location,omitempty"`
	Error      string `json:"error,omitempty"`
}

type proxyProbeConnectionState struct {
	ConnectedToProxyEndpoint  bool
	StartedSOCKSTargetConnect bool
	GotTransportConnection    bool
}

type proxyBatchTestEvent struct {
	Type    string            `json:"type"`
	ProxyID int64             `json:"proxy_id,omitempty"`
	Current int               `json:"current,omitempty"`
	Total   int               `json:"total,omitempty"`
	Success int               `json:"success,omitempty"`
	Failed  int               `json:"failed,omitempty"`
	Error   string            `json:"error,omitempty"`
	Result  *proxyProbeResult `json:"result,omitempty"`
}

func (h *Handler) runProxyProbe(ctx context.Context, proxyURL, lang string) proxyProbeResult {
	if h.proxyProbe != nil {
		return h.proxyProbe(ctx, proxyURL, lang)
	}
	return probeProxy(ctx, proxyURL, lang)
}

func probeProxy(ctx context.Context, proxyURL, lang string) proxyProbeResult {
	return probeProxyWithTimeout(ctx, proxyURL, lang, 15*time.Second)
}

func probeProxyWithTimeout(
	ctx context.Context,
	proxyURL string,
	lang string,
	clientTimeout time.Duration,
) proxyProbeResult {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return proxyProbeResult{
			Conclusive: true,
			Error:      "代理 URL 不能为空",
		}
	}
	proxyScheme := ""
	if separator := strings.Index(proxyURL, "://"); separator > 0 {
		proxyScheme = strings.ToLower(strings.TrimSpace(proxyURL[:separator]))
	}
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = baseDialer.DialContext
	var connectedToProxyEndpoint atomic.Bool
	var startedSOCKSTargetConnect atomic.Bool
	if err := auth.ConfigureTransportProxyWithObserver(
		transport,
		proxyURL,
		baseDialer,
		auth.ProxyTransportObserver{
			OnProxyConnect: func() {
				connectedToProxyEndpoint.Store(true)
			},
			OnSOCKSTargetConnectStart: func() {
				startedSOCKSTargetConnect.Store(true)
			},
		},
	); err != nil {
		return proxyProbeResult{
			Conclusive: true,
			Error:      fmt.Sprintf("代理 URL 格式错误: %v", err),
		}
	}

	lang = boundedProxyProbeField(lang, 16)
	if lang == "" {
		lang = "en"
	}

	client := &http.Client{Transport: transport, Timeout: clientTimeout}
	var gotTransportConnection atomic.Bool
	connectionState := func() proxyProbeConnectionState {
		return proxyProbeConnectionState{
			ConnectedToProxyEndpoint:  connectedToProxyEndpoint.Load(),
			StartedSOCKSTargetConnect: startedSOCKSTargetConnect.Load(),
			GotTransportConnection:    gotTransportConnection.Load(),
		}
	}
	primary := doProxyProbeRequest(
		ctx,
		client,
		&gotTransportConnection,
		proxyScheme,
		connectionState,
		proxyProbeIPv4URLFn(lang),
		parseIPAPIProbeBody,
	)
	if primary.Success || ctx.Err() != nil || !shouldFallbackToIPv6Probe(primary) {
		return primary
	}

	var ipv6Result proxyProbeResult
	for _, echoURL := range proxyProbeIPv6EchoURLsFn() {
		if ctx.Err() != nil {
			return primary
		}
		ipv6Result = doProxyProbeRequest(
			ctx,
			client,
			&gotTransportConnection,
			proxyScheme,
			connectionState,
			echoURL,
			parseProxyProbeEchoBody,
		)
		if ipv6Result.Success {
			break
		}
	}
	if !ipv6Result.Success {
		if primary.Error != "" {
			primary.Error += "；IPv4/IPv6 检测目标都不可达"
		}
		return primary
	}

	country, region, city, isp := proxyProbeLookupGeoFn(ctx, ipv6Result.IP, lang)
	ipv6Result.Country = country
	ipv6Result.Region = region
	ipv6Result.City = city
	ipv6Result.ISP = isp
	ipv6Result.Location = joinProxyProbeLocation(country, region, city)
	return ipv6Result
}

func defaultProxyProbeIPv4URL(lang string) string {
	return fmt.Sprintf(
		"http://ip-api.com/json/?lang=%s&fields=%s",
		url.QueryEscape(lang),
		proxyProbeIPAPIFields,
	)
}

func defaultProxyProbeIPv6EchoURLs() []string {
	return []string{
		"https://api6.ipify.org?format=json",
		"https://ipv6.icanhazip.com",
	}
}

func doProxyProbeRequest(
	ctx context.Context,
	client *http.Client,
	gotTransportConnection *atomic.Bool,
	proxyScheme string,
	connectionState func() proxyProbeConnectionState,
	probeURL string,
	parseBody func([]byte, int) proxyProbeResult,
) proxyProbeResult {
	gotTransportConnection.Store(false)
	trace := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			gotTransportConnection.Store(true)
		},
	}
	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(ctx, trace),
		http.MethodGet,
		probeURL,
		nil,
	)
	if err != nil {
		return proxyProbeResult{Error: fmt.Sprintf("创建代理检测请求失败: %v", err)}
	}

	start := time.Now()
	resp, err := client.Do(req)
	latencyMs := int(time.Since(start).Milliseconds())
	if err != nil {
		return proxyProbeResult{
			Conclusive: proxyProbeErrorIsConclusive(ctx, err, connectionState(), proxyScheme),
			LatencyMs:  latencyMs,
			Error:      fmt.Sprintf("连接失败: %v", err),
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusProxyAuthRequired {
		return proxyProbeResult{
			Conclusive: true,
			LatencyMs:  latencyMs,
			Error:      "代理认证失败 (HTTP 407)",
		}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return proxyProbeResult{
			LatencyMs: latencyMs,
			Error:     fmt.Sprintf("代理检测服务暂时不可用 (HTTP %d)", resp.StatusCode),
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, proxyProbeMaxBody+1))
	if err != nil {
		return proxyProbeResult{
			LatencyMs: latencyMs,
			Error:     fmt.Sprintf("读取代理检测服务响应失败: %v", err),
		}
	}
	if len(body) > proxyProbeMaxBody {
		return proxyProbeResult{
			LatencyMs: latencyMs,
			Error:     "代理检测服务响应过大",
		}
	}
	return parseBody(body, latencyMs)
}

func parseIPAPIProbeBody(body []byte, latencyMs int) proxyProbeResult {
	if !gjson.ValidBytes(body) {
		return proxyProbeResult{
			LatencyMs: latencyMs,
			Error:     "代理检测服务返回了无效响应",
		}
	}
	result := gjson.ParseBytes(body)
	if result.Get("status").String() != "success" {
		message := boundedProxyProbeField(result.Get("message").String(), 256)
		if message == "" {
			message = "代理检测服务暂时不可用，请稍后重试"
		} else {
			message = "代理检测服务未能完成检测: " + message
		}
		return proxyProbeResult{LatencyMs: latencyMs, Error: message}
	}
	ip := strings.TrimSpace(result.Get("query").String())
	if net.ParseIP(ip) == nil {
		return proxyProbeResult{
			LatencyMs: latencyMs,
			Error:     "代理检测服务响应缺少有效的出口 IP",
		}
	}
	country, region, city, isp := parseIPAPIGeoFields(result)
	return proxyProbeResult{
		Success:    true,
		Conclusive: true,
		IP:         ip,
		Country:    country,
		Region:     region,
		City:       city,
		ISP:        isp,
		LatencyMs:  latencyMs,
		Location:   joinProxyProbeLocation(country, region, city),
	}
}

func parseProxyProbeEchoBody(body []byte, latencyMs int) proxyProbeResult {
	ip, err := parseProxyProbeExitIP(body)
	if err != nil {
		return proxyProbeResult{
			LatencyMs: latencyMs,
			Error:     "代理检测服务响应缺少有效的出口 IP",
		}
	}
	return proxyProbeResult{
		Success:    true,
		Conclusive: true,
		IP:         ip,
		LatencyMs:  latencyMs,
	}
}

func parseProxyProbeExitIP(body []byte) (string, error) {
	if gjson.ValidBytes(body) {
		parsed := gjson.ParseBytes(body)
		for _, key := range []string{"ip", "query"} {
			ip := strings.TrimSpace(parsed.Get(key).String())
			if net.ParseIP(ip) != nil {
				return ip, nil
			}
		}
	}
	text := strings.TrimSpace(string(body))
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "ip="); ok {
			ip := strings.TrimSpace(after)
			if net.ParseIP(ip) != nil {
				return ip, nil
			}
		}
	}
	if ip := net.ParseIP(text); ip != nil {
		return ip.String(), nil
	}
	return "", errors.New("missing exit ip")
}

func parseIPAPIGeoFields(result gjson.Result) (country, region, city, isp string) {
	return boundedProxyProbeField(result.Get("country").String(), 80),
		boundedProxyProbeField(result.Get("regionName").String(), 80),
		boundedProxyProbeField(result.Get("city").String(), 80),
		boundedProxyProbeField(result.Get("isp").String(), 160)
}

func parseIPWhoisGeoFields(result gjson.Result) (country, region, city, isp string) {
	return boundedProxyProbeField(result.Get("country").String(), 80),
		boundedProxyProbeField(result.Get("region").String(), 80),
		boundedProxyProbeField(result.Get("city").String(), 80),
		boundedProxyProbeField(result.Get("connection.isp").String(), 160)
}

func joinProxyProbeLocation(country, region, city string) string {
	if country == "" && region == "" && city == "" {
		return ""
	}
	return country + "·" + region + "·" + city
}

func shouldFallbackToIPv6Probe(result proxyProbeResult) bool {
	if result.Success || result.Conclusive {
		return false
	}
	return proxyProbeErrorLooksLikeTargetUnreachable(result.Error)
}

func proxyProbeErrorLooksLikeTargetUnreachable(message string) bool {
	message = strings.ToLower(message)
	for _, fragment := range []string{
		"unknown error network unreachable",
		"unknown error host unreachable",
		"connect: network is unreachable",
		"connect: host unreachable",
		"no route to host",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func lookupProxyExitGeo(ctx context.Context, ip, lang string) (country, region, city, isp string) {
	if net.ParseIP(ip) == nil {
		return "", "", "", ""
	}
	if country, region, city, isp, ok := fetchIPAPIGeo(ctx, ip, lang); ok {
		return country, region, city, isp
	}
	return fetchIPWhoisGeo(ctx, ip)
}

func fetchIPAPIGeo(ctx context.Context, ip, lang string) (country, region, city, isp string, ok bool) {
	probeURL := fmt.Sprintf(
		"http://ip-api.com/json/%s?lang=%s&fields=%s",
		url.PathEscape(ip),
		url.QueryEscape(lang),
		proxyProbeIPAPIFields,
	)
	body, err := fetchProxyProbeGeoBody(ctx, probeURL)
	if err != nil || !gjson.ValidBytes(body) {
		return "", "", "", "", false
	}
	result := gjson.ParseBytes(body)
	if result.Get("status").String() != "success" {
		return "", "", "", "", false
	}
	country, region, city, isp = parseIPAPIGeoFields(result)
	return country, region, city, isp, true
}

func fetchIPWhoisGeo(ctx context.Context, ip string) (country, region, city, isp string) {
	probeURL := "https://ipwho.is/" + url.PathEscape(ip)
	body, err := fetchProxyProbeGeoBody(ctx, probeURL)
	if err != nil || !gjson.ValidBytes(body) {
		return "", "", "", ""
	}
	result := gjson.ParseBytes(body)
	if !result.Get("success").Bool() {
		return "", "", "", ""
	}
	return parseIPWhoisGeoFields(result)
}

func fetchProxyProbeGeoBody(ctx context.Context, probeURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := proxyProbeGeoHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("geo lookup HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, proxyProbeMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > proxyProbeMaxBody {
		return nil, errors.New("geo lookup response too large")
	}
	return body, nil
}

func boundedProxyProbeField(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}

func proxyProbeErrorIsConclusive(
	ctx context.Context,
	err error,
	state proxyProbeConnectionState,
	proxyScheme string,
) bool {
	if ctx.Err() != nil || state.GotTransportConnection {
		return false
	}
	if !state.ConnectedToProxyEndpoint {
		return true
	}
	if proxyScheme == "socks5" || proxyScheme == "socks5h" {
		if !state.StartedSOCKSTargetConnect {
			return true
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return false
		}
		message := strings.ToLower(err.Error())
		for _, targetFailure := range []string{
			"unknown error network unreachable",
			"unknown error host unreachable",
			"unknown error connection refused",
			"unknown error ttl expired",
		} {
			if strings.Contains(message, targetFailure) {
				return false
			}
		}
	}
	return true
}

func (h *Handler) saveProxyTestResult(ctx context.Context, id int64, expectedURL string, result proxyProbeResult) error {
	if id <= 0 || !result.Conclusive {
		return nil
	}
	status := database.ProxyTestStatusError
	if result.Success {
		status = database.ProxyTestStatusSuccess
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := h.db.UpdateProxyTestResult(
		saveCtx,
		id,
		expectedURL,
		status,
		result.IP,
		result.Location,
		result.LatencyMs,
	); err != nil {
		return err
	}
	if !result.Success {
		h.removeProxyURLsFromRuntime([]string{expectedURL})
	}
	return nil
}

func (h *Handler) reloadProxyPool() error {
	if h.reloadProxyPoolFn != nil {
		return h.reloadProxyPoolFn()
	}
	if h.store == nil {
		return nil
	}
	return h.store.ReloadProxyPool()
}

func (h *Handler) removeProxyURLsFromRuntime(proxyURLs []string) {
	if h.store != nil {
		h.store.RemoveProxyURLs(proxyURLs)
	}
}

func (h *Handler) applyRetiredProxiesToRuntime(proxyURLs []string, unboundAccountIDs []int64) error {
	if h.store != nil {
		for _, accountID := range unboundAccountIDs {
			h.store.ClearAccountProxyURLIfMatches(accountID, proxyURLs)
		}
		h.removeProxyURLsFromRuntime(proxyURLs)
	}
	return h.reloadProxyPool()
}

func (h *Handler) sendProxyBatchTestEvent(c *gin.Context, event proxyBatchTestEvent) bool {
	if h.proxyBatchEventSender != nil {
		return h.proxyBatchEventSender(c, event)
	}
	return sendSSEJSON(c, event)
}

func (h *Handler) TestAllProxies(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, proxyBatchTestMaxBody)
	var req struct {
		IDs  []int64 `json:"ids"`
		Lang string  `json:"lang"`
	}
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误或请求体过大")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, "请求只能包含一个 JSON 对象")
		return
	}
	if len(req.IDs) == 0 {
		writeError(c, http.StatusBadRequest, "请提供要测试的代理 ID")
		return
	}
	if len(req.IDs) > proxyBatchTestMaxIDs {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("单次最多测试 %d 个代理", proxyBatchTestMaxIDs))
		return
	}
	for _, id := range req.IDs {
		if id <= 0 {
			writeError(c, http.StatusBadRequest, "代理 ID 必须为正整数")
			return
		}
	}
	if !h.proxyBatchTestMu.TryLock() {
		writeError(c, http.StatusConflict, "已有批量代理测试正在运行")
		return
	}
	var unlockBatchOnce sync.Once
	unlockBatch := func() {
		unlockBatchOnce.Do(h.proxyBatchTestMu.Unlock)
	}
	defer unlockBatch()

	rows, err := h.db.ListProxiesByIDs(c.Request.Context(), req.IDs)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "获取代理列表失败")
		return
	}
	rowsByID := make(map[int64]*database.ProxyRow, len(rows))
	for _, row := range rows {
		rowsByID[row.ID] = row
	}

	seen := make(map[int64]struct{}, len(req.IDs))
	selected := make([]*database.ProxyRow, 0, len(req.IDs))
	missing := make([]int64, 0)
	for _, id := range req.IDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if row := rowsByID[id]; row != nil {
			selected = append(selected, row)
		} else {
			missing = append(missing, id)
		}
	}

	total := len(selected) + len(missing)
	setupSSE(c)
	if !h.sendProxyBatchTestEvent(c, proxyBatchTestEvent{Type: "start", Total: total}) {
		return
	}
	if total == 0 {
		h.sendProxyBatchTestEvent(c, proxyBatchTestEvent{Type: "complete"})
		return
	}

	batches := (len(selected) + proxyBatchTestConcurrency - 1) / proxyBatchTestConcurrency
	workTimeout := time.Duration(batches)*20*time.Second + 30*time.Second
	workCtx, cancel := context.WithTimeout(c.Request.Context(), workTimeout)
	defer cancel()

	jobs := make(chan *database.ProxyRow)
	events := make(chan proxyBatchTestEvent, len(selected))
	var (
		wg       sync.WaitGroup
		didWrite atomic.Bool
	)
	workers := min(proxyBatchTestConcurrency, len(selected))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range jobs {
				result := h.runProxyProbe(workCtx, row.URL, req.Lang)
				if result.Conclusive {
					if err := h.saveProxyTestResult(workCtx, row.ID, row.URL, result); err != nil {
						if errors.Is(err, database.ErrProxyTestTargetChanged) {
							result = proxyProbeResult{Error: "代理在测试期间已被修改，请重新测试"}
						} else {
							result = proxyProbeResult{Error: "保存代理测试结果失败: " + err.Error()}
						}
					} else {
						didWrite.Store(true)
					}
				}
				events <- proxyBatchTestEvent{
					Type:    "progress",
					ProxyID: row.ID,
					Result:  &result,
				}
			}
		}()
	}
	reloadResult := make(chan string, 1)
	go func() {
	feedLoop:
		for _, row := range selected {
			select {
			case jobs <- row:
			case <-workCtx.Done():
				break feedLoop
			}
		}
		close(jobs)
		wg.Wait()

		reloadErr := ""
		if didWrite.Load() {
			if err := h.reloadProxyPool(); err != nil {
				reloadErr = "代理测试结果已保存，但代理池刷新失败: " + err.Error()
			}
		}
		unlockBatch()
		reloadResult <- reloadErr
		close(events)
	}()

	current := 0
	successCount := 0
	failedCount := 0
	streamOpen := true
	for _, id := range missing {
		current++
		failedCount++
		result := proxyProbeResult{Error: "代理不存在或已被删除"}
		if streamOpen && !h.sendProxyBatchTestEvent(c, proxyBatchTestEvent{
			Type:    "progress",
			ProxyID: id,
			Current: current,
			Total:   total,
			Failed:  failedCount,
			Result:  &result,
		}) {
			streamOpen = false
			cancel()
		}
	}
	for event := range events {
		current++
		if event.Result != nil && event.Result.Success {
			successCount++
		} else {
			failedCount++
		}
		event.Current = current
		event.Total = total
		event.Success = successCount
		event.Failed = failedCount
		if streamOpen && !h.sendProxyBatchTestEvent(c, event) {
			streamOpen = false
			cancel()
		}
	}

	reloadErr := <-reloadResult
	if !streamOpen {
		return
	}
	h.sendProxyBatchTestEvent(c, proxyBatchTestEvent{
		Type:    "complete",
		Current: current,
		Total:   total,
		Success: successCount,
		Failed:  failedCount,
		Error:   reloadErr,
	})
}
