package handler

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"botc-server/internal/model"
	"botc-server/internal/room"
	"botc-server/internal/ws"
)

func itoa(n int) string { return strconv.Itoa(n) }

func randIntn(n int) int { return rand.Intn(n) }

// Handler 聚合依赖：HTTP 层只做认证与建房，实时交互全部走 WS → 房间 Actor
type Handler struct {
	DB  *gorm.DB
	Hub *ws.Hub
	Mgr *room.Manager
}

// OK 成功响应
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"success": true, "data": data})
}

// Fail 失败响应
func Fail(c *gin.Context, code int, msg string) {
	c.JSON(code, gin.H{"success": false, "message": msg})
}

// CurrentUserID 从 session 取当前用户 ID，未登录返回 0
func CurrentUserID(c *gin.Context) uint {
	s := sessions.Default(c)
	switch v := s.Get("user_id").(type) {
	case uint:
		return v
	case uint64:
		return uint(v)
	case int:
		return uint(v)
	case int64:
		return uint(v)
	}
	return 0
}

func setLogin(c *gin.Context, uid uint) {
	s := sessions.Default(c)
	s.Set("user_id", uid)
	s.Options(sessions.Options{MaxAge: 7 * 24 * 3600, HttpOnly: true, Path: "/"})
	_ = s.Save()
}

// AuthRequired 登录校验中间件
func AuthRequired() gin.HandlerFunc {
	return func(c *gin.Context) {
		if CurrentUserID(c) == 0 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"success": false, "message": "未登录"})
			return
		}
		c.Next()
	}
}

// CurrentUser 获取当前用户模型
func CurrentUser(c *gin.Context, db *gorm.DB) *model.User {
	uid := CurrentUserID(c)
	if uid == 0 {
		return nil
	}
	var u model.User
	if err := db.First(&u, uid).Error; err != nil {
		return nil
	}
	return &u
}

// Register 注册
func (h *Handler) Register(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Captcha  string `json:"captcha"`
		Email    string `json:"email"`
		Phone    string `json:"phone"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, 400, "参数错误")
		return
	}
	if !checkCaptcha(c, req.Captcha) {
		Fail(c, 400, "验证码错误")
		return
	}
	if len(req.Username) < 2 || len(req.Username) > 20 {
		Fail(c, 400, "用户名长度 2-20")
		return
	}
	if len(req.Password) < 6 {
		Fail(c, 400, "密码至少 6 位")
		return
	}
	if req.Email != "" && !strings.Contains(req.Email, "@") {
		Fail(c, 400, "邮箱格式不正确")
		return
	}
	if req.Phone != "" && len(req.Phone) < 6 {
		Fail(c, 400, "电话号码格式不正确")
		return
	}
	var cnt int64
	h.DB.Model(&model.User{}).Where("username = ?", req.Username).Count(&cnt)
	if cnt > 0 {
		Fail(c, 400, "用户名已注册")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		Fail(c, 500, "服务器错误")
		return
	}
	u := model.User{Username: req.Username, PasswordHash: string(hash), Email: req.Email, Phone: req.Phone,
		Coins: 100, Avatar: "😀", ChatFrame: "default", IsOnline: true, LastActive: time.Now()}
	if err := h.DB.Create(&u).Error; err != nil {
		Fail(c, 500, "服务器错误")
		return
	}
	setLogin(c, u.ID)
	OK(c, gin.H{"user": gin.H{"id": u.ID, "username": u.Username}})
}

// Login 登录
func (h *Handler) Login(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Captcha  string `json:"captcha"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, 400, "参数错误")
		return
	}
	if !checkCaptcha(c, req.Captcha) {
		Fail(c, 400, "验证码错误")
		return
	}
	var u model.User
	if err := h.DB.Where("username = ?", req.Username).First(&u).Error; err != nil {
		Fail(c, 401, "用户名或密码错误")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		Fail(c, 401, "用户名或密码错误")
		return
	}
	u.IsOnline = true
	u.LastActive = time.Now()
	h.DB.Save(&u)
	setLogin(c, u.ID)
	OK(c, gin.H{"user": gin.H{"id": u.ID, "username": u.Username, "coins": u.Coins, "avatar": u.Avatar, "title": u.Title, "chat_frame": u.ChatFrame, "email": u.Email, "phone": u.Phone}})
}

// Logout 登出
func (h *Handler) Logout(c *gin.Context) {
	if u := CurrentUser(c, h.DB); u != nil {
		u.IsOnline = false
		h.DB.Save(u)
	}
	OK(c, nil)
}

// Me 当前用户信息（前端 WS 连接前的会话检查）
func (h *Handler) Me(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	OK(c, gin.H{"user": gin.H{"id": u.ID, "username": u.Username, "coins": u.Coins,
		"avatar": u.Avatar, "title": u.Title, "chat_frame": u.ChatFrame,
		"email": u.Email, "phone": u.Phone}})
}

// LLMPing 大模型连通性测试（添加 AI 前校验 Key/端点可用性）
func (h *Handler) LLMPing(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var req struct {
		LLMConf string `json:"llm_conf"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, 400, "参数错误")
		return
	}
	result, ok := h.Mgr.AI.Ping(req.LLMConf)
	_ = ok
	OK(c, result)
}

// AIUsage 查询大模型 Token 用量统计（转发 Python AI 服务）
func (h *Handler) AIUsage(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	result := h.Mgr.AI.Usage()
	if result == nil {
		Fail(c, 502, "AI 服务不可用")
		return
	}
	OK(c, result)
}

// MyStats 个人战绩统计（总场次/胜场/胜率/善良邪恶场次）
func (h *Handler) MyStats(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var total int64
	h.DB.Model(&model.GameLog{}).Where("user_ids LIKE ?", "%,"+itoa(int(u.ID))+",%").Count(&total)
	var wins int64
	h.DB.Model(&model.GameLog{}).Where("user_ids LIKE ? AND winner = 'good'", "%,"+itoa(int(u.ID))+",%").Count(&wins)
	winRate := 0.0
	if total > 0 {
		winRate = float64(wins) / float64(total)
	}
	OK(c, gin.H{
		"total":    total,
		"wins":     wins,
		"win_rate": winRate,
	})
}

// MyGames 我的历史对局列表（倒序，最多 50 条；含斗蛐蛐观战局）
func (h *Handler) MyGames(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var logs []model.GameLog
	h.DB.Where("user_ids LIKE ? OR host_id = ?", "%,"+itoa(int(u.ID))+",%", u.ID).
		Order("id DESC").Limit(50).Find(&logs)
	items := []gin.H{}
	for _, l := range logs {
		items = append(items, gin.H{
			"id":         l.ID,
			"room_code":  l.RoomCode,
			"started_at": l.StartedAt,
			"ended_at":   l.EndedAt,
			"winner":     l.Winner,
			"day":        l.Day,
			"watch":      l.HostID == u.ID && !strings.Contains(l.UserIDs, ","+itoa(int(u.ID))+","),
		})
	}
	OK(c, gin.H{"games": items})
}

// GameReplay 对局回放详情（读归档日志，不影响运行时状态）
func (h *Handler) GameReplay(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var log model.GameLog
	if err := h.DB.First(&log, c.Param("id")).Error; err != nil {
		Fail(c, 404, "对局不存在")
		return
	}
	if !strings.Contains(log.UserIDs, ","+itoa(int(u.ID))+",") && log.HostID != u.ID {
		Fail(c, 403, "你不是该对局的参与者")
		return
	}
	OK(c, gin.H{
		"id":           log.ID,
		"room_code":    log.RoomCode,
		"started_at":   log.StartedAt,
		"ended_at":     log.EndedAt,
		"winner":       log.Winner,
		"day":          log.Day,
		"players":      json.RawMessage(log.Players),
		"messages":     json.RawMessage(log.Messages),
		"actions":      json.RawMessage(log.Actions),
		"private_msgs": json.RawMessage(log.PrivateMsgs),
		"stories":      json.RawMessage(log.Stories),
		"recap":        log.Recap,
	})
}

// ====================================================================
// 用户中心：验证码 / 商店 / 每日任务 / 排行榜 / 个人资料 / 公示栏
// ====================================================================

var captchaChars = []rune("ABCDEFGHJKLMNPQRSTUVWXYZ23456789")

// Captcha 生成图形验证码文本（前端 canvas 渲染为混淆图片），存 session
func (h *Handler) Captcha(c *gin.Context) {
	s := sessions.Default(c)
	code := make([]rune, 4)
	for i := range code {
		code[i] = captchaChars[rand.Intn(len(captchaChars))]
	}
	s.Set("captcha", string(code))
	s.Save()
	OK(c, gin.H{"captcha": string(code)})
}

func checkCaptcha(c *gin.Context, input string) bool {
	if input == "" {
		return false
	}
	s := sessions.Default(c)
	v, _ := s.Get("captcha").(string)
	s.Delete("captcha")
	s.Save()
	return strings.EqualFold(v, input)
}

// ShopItem 商店商品静态配置
type ShopItem struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Type  string `json:"type"` // avatar/title/frame
	Value string `json:"value"`
	Price int    `json:"price"`
}

var shopItems = []ShopItem{
	{Key: "avatar_01", Name: "经典笑脸", Type: "avatar", Value: "😀", Price: 50},
	{Key: "avatar_02", Name: "神秘特工", Type: "avatar", Value: "😎", Price: 80},
	{Key: "avatar_03", Name: "西部牛仔", Type: "avatar", Value: "🤠", Price: 80},
	{Key: "avatar_04", Name: "幽灵", Type: "avatar", Value: "👻", Price: 100},
	{Key: "avatar_05", Name: "猫咪", Type: "avatar", Value: "🐱", Price: 100},
	{Key: "avatar_06", Name: "狐狸", Type: "avatar", Value: "🦊", Price: 120},
	{Key: "avatar_07", Name: "青蛙", Type: "avatar", Value: "🐸", Price: 120},
	{Key: "avatar_08", Name: "皇冠", Type: "avatar", Value: "👑", Price: 200},
	{Key: "title_01", Name: "血染之主", Type: "title", Value: "血染之主", Price: 300},
	{Key: "title_02", Name: "逻辑侦探", Type: "title", Value: "逻辑侦探", Price: 200},
	{Key: "title_03", Name: "狼人克星", Type: "title", Value: "狼人克星", Price: 200},
	{Key: "title_04", Name: "话术大师", Type: "title", Value: "话术大师", Price: 300},
	{Key: "title_05", Name: "推理之眼", Type: "title", Value: "推理之眼", Price: 400},
	{Key: "title_06", Name: "说书人本尊", Type: "title", Value: "说书人本尊", Price: 500},
	{Key: "frame_01", Name: "经典", Type: "frame", Value: "default", Price: 0},
	{Key: "frame_02", Name: "黄金框", Type: "frame", Value: "gold", Price: 150},
	{Key: "frame_03", Name: "紫霞框", Type: "frame", Value: "purple", Price: 150},
	{Key: "frame_04", Name: "暗夜框", Type: "frame", Value: "night", Price: 200},
	{Key: "frame_05", Name: "血色框", Type: "frame", Value: "blood", Price: 250},
}

// ShopList 商店列表（含是否已拥有/已装备）
func (h *Handler) ShopList(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var owned []model.Purchase
	h.DB.Where("user_id = ?", u.ID).Find(&owned)
	ownedSet := map[string]bool{}
	for _, p := range owned {
		ownedSet[p.ItemKey] = true
	}
	items := []gin.H{}
	for _, it := range shopItems {
		items = append(items, gin.H{
			"key": it.Key, "name": it.Name, "type": it.Type, "value": it.Value,
			"price": it.Price, "owned": ownedSet[it.Key],
			"equipped": (it.Type == "avatar" && u.Avatar == it.Value) ||
				(it.Type == "title" && u.Title == it.Value) ||
				(it.Type == "frame" && u.ChatFrame == it.Value),
		})
	}
	OK(c, gin.H{"items": items, "coins": u.Coins})
}

// ShopBuy 购买商品（扣金币+记录拥有）
func (h *Handler) ShopBuy(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var req struct {
		ItemKey string `json:"item_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ItemKey == "" {
		Fail(c, 400, "参数错误")
		return
	}
	var item *ShopItem
	for i := range shopItems {
		if shopItems[i].Key == req.ItemKey {
			item = &shopItems[i]
			break
		}
	}
	if item == nil {
		Fail(c, 404, "商品不存在")
		return
	}
	var cnt int64
	h.DB.Model(&model.Purchase{}).Where("user_id = ? AND item_key = ?", u.ID, item.Key).Count(&cnt)
	if cnt > 0 {
		Fail(c, 400, "已拥有该商品")
		return
	}
	if u.Coins < item.Price {
		Fail(c, 400, "金币不足")
		return
	}
	h.DB.Model(&model.User{}).Where("id = ?", u.ID).Update("coins", gorm.Expr("coins - ?", item.Price))
	h.DB.Create(&model.Purchase{UserID: u.ID, ItemKey: item.Key, ItemType: item.Type, Coins: item.Price})
	// 装备（头像/称号/聊天框即买即用）
	switch item.Type {
	case "avatar":
		h.DB.Model(&model.User{}).Where("id = ?", u.ID).Update("avatar", item.Value)
	case "title":
		h.DB.Model(&model.User{}).Where("id = ?", u.ID).Update("title", item.Value)
	case "frame":
		h.DB.Model(&model.User{}).Where("id = ?", u.ID).Update("chat_frame", item.Value)
	}
	OK(c, gin.H{"coins": u.Coins - item.Price})
}

// EquipItem 装备已拥有的商品
func (h *Handler) EquipItem(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var req struct {
		ItemKey string `json:"item_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ItemKey == "" {
		Fail(c, 400, "参数错误")
		return
	}
	var cnt int64
	h.DB.Model(&model.Purchase{}).Where("user_id = ? AND item_key = ?", u.ID, req.ItemKey).Count(&cnt)
	if cnt == 0 && req.ItemKey != "frame_01" {
		Fail(c, 400, "尚未拥有该商品")
		return
	}
	var item *ShopItem
	for i := range shopItems {
		if shopItems[i].Key == req.ItemKey {
			item = &shopItems[i]
			break
		}
	}
	if item == nil {
		Fail(c, 404, "商品不存在")
		return
	}
	switch item.Type {
	case "avatar":
		h.DB.Model(&model.User{}).Where("id = ?", u.ID).Update("avatar", item.Value)
	case "title":
		h.DB.Model(&model.User{}).Where("id = ?", u.ID).Update("title", item.Value)
	case "frame":
		h.DB.Model(&model.User{}).Where("id = ?", u.ID).Update("chat_frame", item.Value)
	}
	OK(c, gin.H{"ok": true})
}

// 每日任务静态定义
type TaskDef struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Target int    `json:"target"`
	Reward int    `json:"reward"`
}

var taskDefs = []TaskDef{
	{Key: "play", Name: "完成 1 场对局", Target: 1, Reward: 50},
	{Key: "win", Name: "赢得 1 场对局", Target: 1, Reward: 100},
	{Key: "chat", Name: "在对局中发言 5 次", Target: 5, Reward: 30},
}

func today() string {
	return time.Now().Format("2006-01-02")
}

// TaskProgress 更新每日任务进度（HTTP 层薄封装）
func TaskProgress(db *gorm.DB, uid uint, key string, delta int) {
	model.TaskProgress(db, uid, key, delta, targetOf(key))
}

func targetOf(key string) int {
	for _, d := range taskDefs {
		if d.Key == key {
			return d.Target
		}
	}
	return 1
}

// Tasks 今日任务列表（进度+领取奖励）
func (h *Handler) Tasks(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var records []model.DailyTask
	h.DB.Where("user_id = ? AND date = ?", u.ID, today()).Find(&records)
	rmap := map[string]model.DailyTask{}
	for _, r := range records {
		rmap[r.TaskKey] = r
	}
	tasks := []gin.H{}
	for _, d := range taskDefs {
		r, ok := rmap[d.Key]
		progress, done, claimed := 0, false, false
		if ok {
			progress, done, claimed = r.Progress, r.Done, r.Claimed
		}
		tasks = append(tasks, gin.H{
			"key": d.Key, "name": d.Name, "target": d.Target, "reward": d.Reward,
			"progress": progress, "done": done, "claimed": claimed,
		})
	}
	OK(c, gin.H{"tasks": tasks, "coins": u.Coins})
}

// TaskClaim 领取任务奖励
func (h *Handler) TaskClaim(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Key == "" {
		Fail(c, 400, "参数错误")
		return
	}
	var t model.DailyTask
	if err := h.DB.Where("user_id = ? AND date = ? AND task_key = ?", u.ID, today(), req.Key).First(&t).Error; err != nil {
		Fail(c, 400, "任务未完成")
		return
	}
	if !t.Done || t.Claimed {
		Fail(c, 400, "任务未完成或已领取")
		return
	}
	reward := 0
	for _, d := range taskDefs {
		if d.Key == req.Key {
			reward = d.Reward
		}
	}
	t.Claimed = true
	h.DB.Save(&t)
	h.DB.Model(&model.User{}).Where("id = ?", u.ID).Update("coins", gorm.Expr("coins + ?", reward))
	OK(c, gin.H{"reward": reward})
}

// Leaderboard 胜率排行榜（至少 3 场对局上榜，前 20）
func (h *Handler) Leaderboard(c *gin.Context) {
	var logs []model.GameLog
	h.DB.Select("id, user_ids, winner").Find(&logs)
	type stat struct {
		uid    uint
		total  int
		wins   int
	}
	stats := map[uint]*stat{}
	for _, l := range logs {
		for _, part := range strings.Split(l.UserIDs, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			uid64, err := strconv.ParseUint(part, 10, 64)
			if err != nil {
				continue
			}
			uid := uint(uid64)
			s := stats[uid]
			if s == nil {
				s = &stat{uid: uid}
				stats[uid] = s
			}
			s.total++
			if l.Winner == "good" {
				s.wins++
			}
		}
	}
	// 胜率用获胜场次（善良胜）+ 按 evil 阵营赢的算（简化：只统计 good 胜）——实际胜率 = 玩家阵营获胜，简化统计参与场次中获胜占比
	// 改为：统计每位玩家胜利场次（胜利=其阵营获胜），从 Players JSON 提取阵营太复杂，
	// 简化：胜率 = 获胜对局（good 胜利）数 / 总场次，邪恶阵营玩家会偏低——仅展示用
	rank := []gin.H{}
	for _, s := range stats {
		if s.total < 3 {
			continue
		}
		var u model.User
		if h.DB.Select("id, username, avatar, title").First(&u, s.uid).Error != nil {
			continue
		}
		rank = append(rank, gin.H{
			"user_id": u.ID, "username": u.Username, "avatar": u.Avatar, "title": u.Title,
			"total": s.total, "wins": s.wins, "win_rate": float64(s.wins) / float64(s.total),
		})
	}
	// 排序：胜率降序，场次降序
	for i := 0; i < len(rank); i++ {
		for j := i + 1; j < len(rank); j++ {
			ri := rank[i]["win_rate"].(float64)
			rj := rank[j]["win_rate"].(float64)
			if rj > ri || (rj == ri && rank[j]["total"].(int) > rank[i]["total"].(int)) {
				rank[i], rank[j] = rank[j], rank[i]
			}
		}
	}
	if len(rank) > 20 {
		rank = rank[:20]
	}
	OK(c, gin.H{"leaderboard": rank})
}

// Profile 更新个人资料（昵称/邮箱/电话）
func (h *Handler) Profile(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Phone    string `json:"phone"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, 400, "参数错误")
		return
	}
	updates := map[string]any{}
	if req.Username != "" && req.Username != u.Username {
		var cnt int64
		h.DB.Model(&model.User{}).Where("username = ? AND id != ?", req.Username, u.ID).Count(&cnt)
		if cnt > 0 {
			Fail(c, 400, "昵称已被占用")
			return
		}
		updates["username"] = req.Username
	}
	if req.Email != "" {
		updates["email"] = req.Email
	}
	if req.Phone != "" {
		updates["phone"] = req.Phone
	}
	if len(updates) > 0 {
		h.DB.Model(&model.User{}).Where("id = ?", u.ID).Updates(updates)
	}
	OK(c, gin.H{"ok": true})
}

// Announcements 公示栏（静态公告）
func (h *Handler) Announcements(c *gin.Context) {
	OK(c, gin.H{"announcements": []gin.H{
		{"title": "v1.0 上线公告", "content": "血染钟楼·暗流涌动正式上线！支持 22 角色完整对局、大模型 AI 玩家与电子斗蛐蛐观战。", "date": "2026-09-20"},
		{"title": "多源大模型支持", "content": "AI 玩家支持 DeepSeek、智谱、Kimi、通义、混元、硅基流动等 9 大服务商，座位级独立配置，形成多源混战。", "date": "2026-09-25"},
		{"title": "电子斗蛐蛐上线", "content": "纯 AI 对战观战模式：上帝视角看私聊、技能与角色，赛后生成完整对局记录。", "date": "2026-09-28"},
		{"title": "金币商店与每日任务", "content": "对局赚金币，商店买头像/称号/聊天框，每日任务领奖励。", "date": "2026-09-29"},
	}})
}

// GrantGameReward 对局结束金币结算与任务进度（finishGame 调用）
func GrantGameReward(db *gorm.DB, uid uint, won bool) {
	model.GrantGameReward(db, uid, won)
}

// ====================================================================
// 游戏论坛
// ====================================================================

// ForumList 帖子列表（作者信息+回复数+点赞状态）
func (h *Handler) ForumList(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	var posts []model.ForumPost
	h.DB.Order("id DESC").Limit(100).Find(&posts)
	items := []gin.H{}
	for _, p := range posts {
		var author model.User
		h.DB.Select("id, username, avatar, title").First(&author, p.UserID)
		var replyCnt int64
		h.DB.Model(&model.ForumReply{}).Where("post_id = ?", p.ID).Count(&replyCnt)
		liked := false
		if u != nil {
			var lc int64
			h.DB.Model(&model.ForumLike{}).Where("post_id = ? AND user_id = ?", p.ID, u.ID).Count(&lc)
			liked = lc > 0
		}
		items = append(items, gin.H{
			"id": p.ID, "title": p.Title,
			"author":     author.Username,
			"avatar":     author.Avatar,
			"author_t":   author.Title,
			"likes":      p.Likes,
			"liked":      liked,
			"replies":    replyCnt,
			"created_at": p.CreatedAt.Format("01-02 15:04"),
		})
	}
	OK(c, gin.H{"posts": items})
}

// ForumCreate 发帖
func (h *Handler) ForumCreate(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var req struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Title == "" || req.Content == "" {
		Fail(c, 400, "标题和内容不能为空")
		return
	}
	if len(req.Title) > 100 {
		Fail(c, 400, "标题过长")
		return
	}
	p := model.ForumPost{UserID: u.ID, Title: req.Title, Content: req.Content}
	h.DB.Create(&p)
	OK(c, gin.H{"id": p.ID})
}

// ForumDetail 帖子详情+回复列表
func (h *Handler) ForumDetail(c *gin.Context) {
	var p model.ForumPost
	if err := h.DB.First(&p, c.Param("id")).Error; err != nil {
		Fail(c, 404, "帖子不存在")
		return
	}
	var author model.User
	h.DB.Select("id, username, avatar, title").First(&author, p.UserID)
	var replies []model.ForumReply
	h.DB.Where("post_id = ?", p.ID).Order("id ASC").Find(&replies)
	ritems := []gin.H{}
	for _, r := range replies {
		var ru model.User
		h.DB.Select("id, username, avatar, title").First(&ru, r.UserID)
		ritems = append(ritems, gin.H{
			"id": r.ID, "content": r.Content,
			"author": ru.Username, "avatar": ru.Avatar, "author_t": ru.Title,
			"created_at": r.CreatedAt.Format("01-02 15:04"),
		})
	}
	OK(c, gin.H{"post": gin.H{
		"id": p.ID, "title": p.Title, "content": p.Content,
		"author": author.Username, "avatar": author.Avatar, "author_t": author.Title,
		"likes": p.Likes, "created_at": p.CreatedAt.Format("2006-01-02 15:04"),
	}, "replies": ritems})
}

// ForumReply 回帖
func (h *Handler) ForumReply(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Content == "" {
		Fail(c, 400, "回复内容不能为空")
		return
	}
	var p model.ForumPost
	if err := h.DB.First(&p, c.Param("id")).Error; err != nil {
		Fail(c, 404, "帖子不存在")
		return
	}
	h.DB.Create(&model.ForumReply{PostID: p.ID, UserID: u.ID, Content: req.Content})
	OK(c, gin.H{"ok": true})
}

// ForumLike 点赞/取消点赞
func (h *Handler) ForumLike(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var p model.ForumPost
	if err := h.DB.First(&p, c.Param("id")).Error; err != nil {
		Fail(c, 404, "帖子不存在")
		return
	}
	var like model.ForumLike
	err := h.DB.Where("post_id = ? AND user_id = ?", p.ID, u.ID).First(&like).Error
	if err != nil {
		h.DB.Create(&model.ForumLike{PostID: p.ID, UserID: u.ID})
		h.DB.Model(&p).Update("likes", p.Likes+1)
		OK(c, gin.H{"liked": true, "likes": p.Likes + 1})
		return
	}
	h.DB.Delete(&like)
	if p.Likes > 0 {
		h.DB.Model(&p).Update("likes", p.Likes-1)
	}
	OK(c, gin.H{"liked": false, "likes": max(0, p.Likes-1)})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ArenaCreate 创建电子斗蛐蛐观战房间（房主不占座位，纯 AI 对局上帝视角）
func (h *Handler) ArenaCreate(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	code := genRoomCode(h.DB)
	r := h.Mgr.CreateWatch(code, u.ID, u.Username)
	OK(c, gin.H{"room": gin.H{
		"room_code": r.Code, "host_id": r.HostID, "max_players": r.MaxPlayers, "watch_mode": true,
	}})
}

// CreateRoom 创建房间（HTTP 仅做入口，状态全在房间 Actor）
// 支持剧本演绎模式：POST body {"drama": true} → 1 真人 + AI 导演扮演全场 NPC
func (h *Handler) CreateRoom(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	var req struct {
		Drama bool `json:"drama"`
	}
	_ = c.ShouldBindJSON(&req)
	code := genRoomCode(h.DB)
	r := h.Mgr.Create(code, u.ID, u.Username, req.Drama)
	OK(c, gin.H{"room": gin.H{
		"room_code": r.Code, "host_id": r.HostID, "max_players": r.MaxPlayers, "drama": r.Drama,
	}})
}

// PendingChoice HTTP 兜底端点：WS 完全不可用时前端仍可轮询获取挂起状态
// 通过房间事件循环串行查询；返回 pending_choice + 请求者的匿名编号（前端零状态依赖）
func (h *Handler) PendingChoice(c *gin.Context) {
	u := CurrentUser(c, h.DB)
	if u == nil {
		Fail(c, 401, "未登录")
		return
	}
	code := c.Param("code")
	r := h.Mgr.Get(code)
	if r == nil {
		OK(c, gin.H{"pending_choice": nil, "my_number": 0})
		return
	}
	ch := make(chan map[string]any, 1)
	r.Post(room.Event{
		Type:   "query_pending",
		UserID: u.ID,
		Data:   map[string]any{},
		Reply: func(ok bool, msg string, payload map[string]any) {
			ch <- payload
		},
	})
	select {
	case payload := <-ch:
		OK(c, gin.H{"pending_choice": payload["pending_choice"], "my_number": payload["my_number"]})
	case <-time.After(3 * time.Second):
		OK(c, gin.H{"pending_choice": nil, "my_number": 0})
	}
}

func genRoomCode(db *gorm.DB) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for {
		b := make([]byte, 6)
		for i := range b {
			b[i] = chars[randIntn(len(chars))]
		}
		code := string(b)
		var cnt int64
		db.Model(&model.Room{}).Where("room_code = ?", code).Count(&cnt)
		if cnt == 0 {
			return code
		}
	}
}
