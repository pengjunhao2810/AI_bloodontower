package aiclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Client Python AI 推理服务客户端（Go 主服务通过 HTTP 调用，解耦）
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// dramaRooms：房间级玩法标记（创建房间后可在设置里开关）
	mu         sync.Mutex
	dramaRooms map[string]bool
}

// SetRoomDrama 标记/取消剧本演绎房间（开局时调用；房间销毁时清除）
func (c *Client) SetRoomDrama(roomCode string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dramaRooms == nil {
		c.dramaRooms = map[string]bool{}
	}
	if on {
		c.dramaRooms[roomCode] = true
	} else {
		delete(c.dramaRooms, roomCode)
	}
}

func (c *Client) dramaOf(roomCode string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dramaRooms[roomCode]
}

// New 创建 AI 客户端
// HTTP 超时 40 秒：AI 调用全部在独立 goroutine 异步回投，不阻塞房间事件循环；
// Python 侧有并发信号量（免费 key 组织级并发限制），排队请求需要更长等待窗口
// Transport{Proxy: nil}：AI 服务在本地 127.0.0.1，强制直连——系统开启代理时
// Go 默认读环境变量会把本地请求也转发给代理软件导致卡死
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP: &http.Client{
			Timeout:   40 * time.Second,
			Transport: &http.Transport{Proxy: nil},
		},
	}
}

// SpeakReq AI 发言请求：携带对局上下文（轮次/阶段/存活玩家/最近发言）+ 座位级 LLM 配置 + 私密线索 + 阵营
type SpeakReq struct {
	RoomCode     string // 房间码（Python 侧 AI 推理笔记隔离键）
	RoleName     string
	Team         string // 阵营：townsfolk/outsider=善良，minion/demon=邪恶（发言立场约束）
	Number       int
	Phase        string
	Day          int
	AliveNums    []int
	TotalPlayers int // 本局总人数（用于注入官方板子配置，盘角色数量）
	Context      []map[string]any
	MyClues      []string // 该 AI 的说书人私密信息（查验结果等），供推理/分享
	LLMConf      string   // 座位级大模型配置 JSON（api_key/base_url/model）
	OtherNum     int      // 私聊场景：对话对方编号（0=非私聊）
}

// Speak 生成 AI 玩家公聊发言（POST Python AI 服务）
// 返回 (reply, ok)；endChat 表示私聊中模型自然收尾（此后该 AI 对此会话保持沉默）
func (c *Client) Speak(req SpeakReq) (string, bool) {
	payload := map[string]any{
		"room_code":     req.RoomCode,
		"role_name":     req.RoleName,
		"team":          req.Team,
		"number":        req.Number,
		"phase":         req.Phase,
		"day":           req.Day,
		"alive_nums":    req.AliveNums,
		"total_players": req.TotalPlayers,
		"context":       req.Context,
		"my_clues":      req.MyClues,
		"llm_conf":      req.LLMConf,
		"other_num":     req.OtherNum,
		"drama":         c.dramaOf(req.RoomCode),
	}
	var resp struct {
		Reply   string `json:"reply"`
		EndChat bool   `json:"end_chat"`
	}
	if err := c.post("/api/speak", payload, &resp); err != nil {
		return "", false
	}
	return resp.Reply, resp.Reply != ""
}

// SpeakWithEnd 同 Speak，额外返回私聊收尾标记
func (c *Client) SpeakWithEnd(req SpeakReq) (string, bool, bool) {
	payload := map[string]any{
		"room_code":     req.RoomCode,
		"role_name":     req.RoleName,
		"team":          req.Team,
		"number":        req.Number,
		"phase":         req.Phase,
		"day":           req.Day,
		"alive_nums":    req.AliveNums,
		"total_players": req.TotalPlayers,
		"context":       req.Context,
		"my_clues":      req.MyClues,
		"llm_conf":      req.LLMConf,
		"other_num":     req.OtherNum,
	}
	var resp struct {
		Reply   string `json:"reply"`
		EndChat bool   `json:"end_chat"`
	}
	if err := c.post("/api/speak", payload, &resp); err != nil {
		return "", false, false
	}
	return resp.Reply, resp.Reply != "", resp.EndChat
}

// Decide 生成 AI 夜晚行动目标决策，返回目标编号
func (c *Client) Decide(roleName, ability string, aliveNums []int, selfNum int) (int, bool) {
	return c.DecideWithConf(roleName, ability, aliveNums, selfNum, "")
}

// DecideWithConf 携带座位级 LLM 配置与私密线索的目标决策
func (c *Client) DecideWithConf(roleName, ability string, aliveNums []int, selfNum int, llmConf string) (int, bool) {
	return c.decide("", roleName, ability, aliveNums, selfNum, llmConf, nil)
}

// DecideWithClues 携带私密线索（说书人查验结果）的目标决策——AI 基于真实信息推理
func (c *Client) DecideWithClues(roleName, ability string, aliveNums []int, selfNum int, llmConf string, clues []string) (int, bool) {
	return c.decide("", roleName, ability, aliveNums, selfNum, llmConf, clues)
}

// DecideWithCluesRoom 携带房间码（Python 侧推理笔记隔离键）+ 私密线索的目标决策
func (c *Client) DecideWithCluesRoom(roomCode, roleName, ability string, aliveNums []int, selfNum int, llmConf string, clues []string) (int, bool) {
	return c.decide(roomCode, roleName, ability, aliveNums, selfNum, llmConf, clues)
}

func (c *Client) decide(roomCode, roleName, ability string, aliveNums []int, selfNum int, llmConf string, clues []string) (int, bool) {
	payload := map[string]any{
		"room_code":  roomCode,
		"role_name":  roleName,
		"ability":    ability,
		"alive_nums": aliveNums,
		"self_num":   selfNum,
		"my_clues":   clues,
		"llm_conf":   llmConf,
		"drama":      c.dramaOf(roomCode),
	}
	var resp struct {
		Choice *int `json:"choice"`
	}
	if err := c.post("/api/decide", payload, &resp); err != nil {
		return 0, false
	}
	if resp.Choice == nil {
		return 0, false
	}
	return *resp.Choice, true
}

// DecidePrivateAccept AI 判断是否接受私聊邀请（让 AI 自己想）：返回 (接受, ok)；ok=false 兜底接受
func (c *Client) DecidePrivateAccept(roomCode, roleName string, fromNum int, aliveNums []int, selfNum int, llmConf string, clues []string) (bool, bool) {
	payload := map[string]any{
		"room_code":     roomCode,
		"role_name":     roleName,
		"alive_nums":    aliveNums,
		"self_num":      selfNum,
		"my_clues":      clues,
		"llm_conf":      llmConf,
		"decision_type": "private_accept",
		"invite_from":   fromNum,
	}
	var resp struct {
		Accept bool `json:"accept"`
	}
	if err := c.post("/api/decide", payload, &resp); err != nil {
		return false, false
	}
	return resp.Accept, true
}

// Recap 赛后复盘：说书人视角回顾整局（角色分布+公聊节选）
func (c *Client) Recap(roomCode, winner string, players []map[string]any, msgs []map[string]any, llmConf string) (string, bool) {
	payload := map[string]any{
		"room_code": roomCode,
		"winner":    winner,
		"players":   players,
		"messages":  msgs,
		"llm_conf":  llmConf,
	}
	var resp struct {
		Recap string `json:"recap"`
	}
	if err := c.post("/api/recap", payload, &resp); err != nil {
		return "", false
	}
	return resp.Recap, resp.Recap != ""
}

// Ping 连通性测试：用给定 LLM 配置调一次模型，返回结果详情（添加 AI 前校验 Key）
func (c *Client) Ping(llmConf string) (map[string]any, bool) {
	payload := map[string]any{"llm_conf": llmConf}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Model string `json:"model"`
	}
	if err := c.post("/api/ping", payload, &resp); err != nil {
		return map[string]any{"ok": false, "error": "AI 服务不可用"}, false
	}
	return map[string]any{"ok": resp.OK, "error": resp.Error, "model": resp.Model}, resp.OK
}

// DecideVote AI 投票决策（先推理后决策）：返回 (投赞成, ok)
func (c *Client) DecideVote(roomCode, roleName string, fromNum, targetNum int, aliveNums []int, selfNum int, llmConf string, clues []string) (bool, bool) {
	payload := map[string]any{
		"room_code":   roomCode,
		"role_name":   roleName,
		"alive_nums":  aliveNums,
		"self_num":    selfNum,
		"my_clues":    clues,
		"llm_conf":    llmConf,
		"decision_type": "vote",
		"vote_from":   fromNum,
		"vote_target": targetNum,
		"drama":       c.dramaOf(roomCode),
	}
	var resp struct {
		VoteYes bool `json:"vote_yes"`
	}
	if err := c.post("/api/decide", payload, &resp); err != nil {
		return false, false
	}
	return resp.VoteYes, true
}

// Usage 查询大模型 Token 用量统计（全局+按房间+按模型）
func (c *Client) Usage() map[string]any {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/api/usage", nil)
	if err != nil {
		return nil
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return out
}

// SessionReset 对局生命周期重置：清空该房间 AI 笔记（开局/结束/踢出时调用）
// fire-and-forget：异步执行，绝不阻塞房间事件循环（AI 服务慢时曾冻结整个房间 40 秒）
func (c *Client) SessionReset(roomCode string) {
	go func() {
		defer func() { _ = recover() }()
		payload := map[string]any{"room_code": roomCode}
		var out map[string]any
		_ = c.post("/api/session/reset", payload, &out)
	}()
}

// SessionPhase 阶段切换通知：房主推进阶段时调用，告知该房间 AI 当前阶段（异步，不阻塞）
func (c *Client) SessionPhase(roomCode, phase string) {
	go func() {
		defer func() { _ = recover() }()
		payload := map[string]any{"room_code": roomCode, "phase": phase}
		var out map[string]any
		_ = c.post("/api/session/phase", payload, &out)
	}()
}

// SessionEvent 对局事件通知：提名/辩论/投票/处决发生时写入该房间 AI 笔记（异步，不阻塞）
func (c *Client) SessionEvent(roomCode, event string) {
	go func() {
		defer func() { _ = recover() }()
		payload := map[string]any{"room_code": roomCode, "event": event}
		var out map[string]any
		_ = c.post("/api/session/event", payload, &out)
	}()
}

func (c *Client) post(path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ai service status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
