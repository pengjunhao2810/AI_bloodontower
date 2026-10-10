package room

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"botc-server/internal/aiclient"
	"botc-server/internal/config"
	"botc-server/internal/engine"
	"botc-server/internal/model"
)

// Room 房间 Actor：所有状态修改都通过 events channel 串执
// Room 房间 Actor
type Room struct {
	Code       string
	HostID     uint
	MaxPlayers int
	Status     string // waiting / playing

	seats    map[int]*Seat
	messages []ChatMsg

	game            *engine.GameData
	eng             *engine.Engine
	pubMsgs         []GameMsg
	privMsgs        []PrivateMsg         // 私聊消息（仅归档，不广播）
	privateInvites  map[int]int          // 待处理私聊邀请：被邀请者编号 -> 邀请者编号
	privateSessions map[int]map[int]bool // 已建立的私聊会话：from -> set(to)
	sessionAt       map[string]int64     // 会话建立时间："a-b" -> unix 秒（超时退出用）
	privateSilence  map[int]map[int]bool // 私聊沉默权：AI 自然收尾后不再回复该会话
	privateTalked   map[string]bool      // 当天已聊过的一对玩家："a-b"（防止无限重开会话）
	stories         map[int][]string     // 玩家编号 → 说书人消息
	startedAt       time.Time

	events           chan Event
	quit             chan struct{}
	mgr              *Manager
	msgSeq           uint64
	lastHumanTrigger time.Time
	abandonedAt      time.Time // 最后一个真人离线的时刻（空置清理用）
	aiBusy           bool      // AI 发言请求在途标记（防止并发与堆积）
	WatchMode        bool      // 电子斗蛐蛐观战模式：房主不占座位，上帝视角看全部私聊/技能/角色
	lastAutoPhase    string    // 斗蛐蛐自动推进：上次记录的阶段
	phaseEnteredAt   time.Time // 斗蛐蛐自动推进：进入当前阶段的时刻
	speakCounts      map[int]int // AI 发言次数计数（防车轱辘刷屏：每阶段每人最多 3 次公聊）
	speakCountsPhase string      // 发言计数对应的阶段
	speakTurn        int         // 公聊轮转指针：轮流给每个 AI 发言机会（轮到后由 AI 自己决定说话或沉默）
	debateSpoken     map[int]bool // 辩论期已发言的 AI（每个提名轮次重置）
}

// PrivateMsg 私聊消息（仅收发双方，GameLog 归档?
type PrivateMsg struct {
	From    int       `json:"from"`
	To      int       `json:"to"`
	Content string    `json:"content"`
	Day     int       `json:"day"`
	At      time.Time `json:"at"`
}

const aiReplyCooldown = 8 * time.Second

// 猎手开枪声明匹配：发言中出现"猎手"+"开枪/枪毙/射杀/射击"+"编号"
var slayerRe = regexp.MustCompile(`猎手[^，。\n]*?(?:开枪|枪毙|射杀|射击)[^，。\n]*?#?\s*(\d+)`)

// Post 投事件（非阻塞，?HTTP/WS 层调甼
func (r *Room) Post(ev Event) {
	select {
	case r.events <- ev:
	case <-r.quit:
		if ev.Reply != nil {
			ev.Reply(false, "房间已解", nil)
		}
	default:
		if ev.Reply != nil {
			ev.Reply(false, "房间繁忙，重试", nil)
		}
	}
}

// Run 房间主循玼事件处理 + AI 发言定时?
func (r *Room) Run() {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("[room %s] panic recovered: %v", r.Code, err)
			r.mgr.Delete(r.Code)
		}
	}()
	aiTicker := time.NewTicker(time.Duration(config.C.AIChateIntervalSec) * time.Second)
	defer aiTicker.Stop()
	// 1 ?ticker：查挂起的真人夜晚行动昐超时
	timeoutTicker := time.NewTicker(time.Second)
	defer timeoutTicker.Stop()
	// 夜晚臊推进 ticker：每 2 秒自动执行一晚骤（挂起时暂停）
	nightTicker := time.NewTicker(2 * time.Second)
	defer nightTicker.Stop()
	// 对局状持久化 ticker：每 5 秒落盘快照（服务重启叁复）
	persistTicker := time.NewTicker(5 * time.Second)
	defer persistTicker.Stop()
	for {
		select {
		case ev := <-r.events:
			r.handle(ev)
		case <-aiTicker.C:
			r.aiAutoChat()
		case <-timeoutTicker.C:
			r.checkPendingTimeout()
			r.checkVoteTimeout()
			r.checkPrivateChatTimeout()
		case <-nightTicker.C:
			r.autoNightAdvance()
		case <-persistTicker.C:
			r.persistGame()
			r.checkAbandoned()
		case <-r.quit:
			return
		}
	}
}

// persistGame 对局状落盘（角色/存活/相位等，重启后恢复）
func (r *Room) persistGame() {
	if r.Status != "playing" || r.game == nil {
		return
	}
	b, err := json.Marshal(r.game)
	if err != nil {
		return
	}
	r.mgr.DB.Model(&model.Room{}).Where("room_code = ?", r.Code).
		Updates(map[string]any{"game_data": string(b), "status": "playing"})
}

// autoNightAdvance 夜晚臊推进：每步一色劼到达白天后自动进入聊阶段；真人挂起时暂?
func (r *Room) autoNightAdvance() {
	if r.Status != "playing" || r.game == nil || r.game.Phase != "night" {
		if r.Status == "playing" && r.game != nil && r.game.Phase == "day" {
			// 天亮：查胜负后臊进入私聊阶
			if win := r.eng.CheckWin(); win != "" {
				r.finishGame(win)
				return
			}
			r.eng.AdvancePhase() // day -> private_chat

			r.enterPrivateChat()
		}
		return
	}
	if r.game.PendingChoice != nil {
		return // 真人选择挂起丼暂停臊推进（由提交/超时恢?
	}
	result := r.eng.AdvancePhase()
	r.drainStories()
	// 挂起：定向推送给真人玩 + 广播相位/必
	if result["waiting"] == true {
		r.broadcastPending(result)
		r.broadcast("game.phase", result)
		r.broadcast("game.state", r.gameSnapshot())
		return
	}
	if result["phase"] == "day" {
		// 天亮播报：公开进公聊时间轴（谁死了、存活几人）；首夜过后不公示（恶魔首夜不行动，无人会死）
		if r.game.Day >= 1 {
			if dead, ok := result["dawn_dead"].([]int); ok {
				alive, _ := result["dawn_alive"].([]int)
				if len(dead) > 0 {
					r.pubSystem("🌅 第" + itoa(r.game.Day+1) + "天天亮：昨晚死亡 " + joinInts(dead) +
						"。场上存活 " + itoa(len(alive)) + " 人：" + joinInts(alive))
				} else {
					r.pubSystem("🌅 第" + itoa(r.game.Day+1) + "天天亮：昨晚无人死亡。场上存活 " +
						itoa(len(alive)) + " 人：" + joinInts(alive))
				}
			}
		}
		if win := r.eng.CheckWin(); win != "" {
			r.finishGame(win)
			return
		}
		r.broadcast("game.phase", result)
		r.broadcast("game.state", r.gameSnapshot())
		return
	}
	r.broadcast("game.phase", result)
	r.broadcast("game.state", r.gameSnapshot())
}

// broadcastPending 挂起通知定向推：叏给需要操作的真人玩（止全广播?// broadcastPending 挂起通知：定向推送有系统提示（仅目标玩家可见，其余玩收不到）
// 幂等：同丌起任务只下发次提示卡片（TipSent 标?
func (r *Room) broadcastPending(result map[string]any) {
	pc := r.game.PendingChoice
	if pc == nil || pc.TipSent {
		return
	}
	pc.TipSent = true
	targetUID := uint(0)
	if p := r.game.Players[pc.PlayerID]; p != nil {
		targetUID = p.UserID
	}
	if targetUID != 0 {
		r.mgr.Hub.SendToUser(targetUID, "game.night_tip", map[string]any{
			"number":        result["player_number"],
			"role":          result["role"],
			"role_name":     result["role_name"],
			"valid_targets": result["valid_targets"],
			"max_targets":   result["max_targets"],
			"deadline":      result["deadline"],
			"step":          result["step"],
			"total":         result["total"],
			"tip_id":        pc.TipID,
		})
	}
}

func (r *Room) broadcast(event string, data any) {
	r.mgr.Hub.BroadcastToRoom(r.Code, event, data)
}

// addSystemMsg 系统消息
func (r *Room) addSystemMsg(content string) {
	r.msgSeq++
	r.messages = append(r.messages, ChatMsg{ID: r.msgSeq, Username: "系统", Content: content, System: true, At: time.Now()})
}

// ==============================
// 事件处理
// ==============================

func (r *Room) handle(ev Event) {
	reply := func(ok bool, msg string, data map[string]any) {
		if ev.Reply != nil {
			ev.Reply(ok, msg, data)
		}
	}
	switch ev.Type {
	case "join":
		r.onJoin(ev, reply)
	case "user_offline":
		r.onUserOffline(ev)
	case "ai_say":
		r.onAISay(ev)
	case "ai_say_seg":
		r.onAISaySegment(toInt(ev.Data["number"]), strOr(ev.Data["content"]))
	case "ai_nominate":
		r.onAINominate(ev)
	case "ai_private_say":
		r.onAIPrivateSay(ev)
	case "ai_private_accept_resolve":
		r.onAIPrivateAcceptResolve(ev)
	case "ai_private_silence":
		r.setSilence(toInt(ev.Data["number"]), toInt(ev.Data["to_number"]))
	case "slayer_shoot":
		r.onSlayerShoot(ev, reply)
	case "ai_busy_done":
		r.aiBusy = false
	case "chat":
		r.onChat(ev, reply)
	case "ready":
		r.onReady(ev, reply)
	case "add_ai":
		r.onAddAI(ev, reply)
	case "remove_ai":
		r.onRemoveAI(ev, reply)
	case "set_seats":
		r.onSetSeats(ev, reply)
	case "start":
		r.onStart(ev, reply)
	case "leave":
		r.onLeave(ev, reply)
	case "kick":
		r.onKick(ev, reply)
	case "fill_bots":
		r.onFillBots(ev, reply)
	case "game_chat":
		r.onGameChat(ev, reply)
	case "next_phase":
		r.onNextPhase(ev, reply)
	case "nominate":
		r.onNominate(ev, reply)
	case "vote":
		r.onVote(ev, reply)
	case "end_nomination":
		r.onEndNomination(ev, reply)
	case "end":
		r.onEnd(ev, reply)
	case "my_info":
		r.onMyInfo(ev, reply)
	case "submit_choice":
		r.onSubmitChoice(ev, reply)
	case "tip_ack":
		r.onTipAck(ev, reply)
	case "query_pending":
		// HTTP 兜底竂：取当前挂起的盠选择 + 请求者的匿名编号（不依赖前状同步）
		payload := map[string]any{"pending_choice": nil, "my_number": 0}
		if r.game != nil {
			if s := r.seatOf(ev.UserID); s != nil {
				if p := r.game.Players[itoa(s.Number)]; p != nil {
					payload["my_number"] = p.Number
				}
			}
			if r.game.PendingChoice != nil {
				payload["pending_choice"] = r.game.PendingChoice
			}
		}
		reply(true, "", payload)
	case "private_chat":
		r.onPrivateChat(ev, reply)
	case "private_invite":
		r.onPrivateInvite(ev, reply)
	case "private_accept":
		r.onPrivateAccept(ev, reply)
	case "private_decline":
		r.onPrivateDecline(ev, reply)
	case "private_exit":
		r.onPrivateExit(ev, reply)
	case "skip_private":
		r.onSkipPrivate(ev, reply)
	}
}

// seatOf 按用户找座位
func (r *Room) seatOf(userID uint) *Seat {
	for _, s := range r.seats {
		if s.UserID == userID {
			return s
		}
	}
	return nil
}

func (r *Room) playerCount() int { return len(r.seats) }

func (r *Room) allReady() bool {
	for _, s := range r.seats {
		if !s.IsAI && s.UserID != r.HostID && !s.IsReady {
			return false
		}
	}
	return true
}

func (r *Room) freeSeat() int {
	for i := 0; i < r.MaxPlayers; i++ {
		if _, ok := r.seats[i]; !ok {
			return i
		}
	}
	return -1
}

// syncSeatsDB 座位变化同?DB（断线恢复镜像）
func (r *Room) syncSeatsDB() {
	var rec model.Room
	if r.mgr.DB.Where("room_code = ?", r.Code).First(&rec).Error != nil {
		return
	}
	// 全表删除座位
	r.mgr.DB.Where("room_id = ?", rec.ID).Delete(&model.RoomSeat{})
	for _, s := range r.seats {
		seat := model.RoomSeat{RoomID: rec.ID, SeatNumber: s.Number, IsAI: s.IsAI, AIType: s.AIType}
		if !s.IsAI {
			uid := s.UserID
			seat.UserID = &uid
		}
		r.mgr.DB.Create(&seat)
	}
}

// ==============================
// 加入 / 离开
// ==============================

func (r *Room) onJoin(ev Event, reply func(bool, string, map[string]any)) {
	// 斗蛐蛐观战模式：房主不占座位，永远以观战者身份进入
	if r.WatchMode && ev.UserID == r.HostID {
		r.mgr.Hub.JoinRoom(r.mgr.Hub.ClientOf(ev.UserID), r.Code)
		payload := map[string]any{"state": r.stateSnapshot(), "seat": 0, "spectator": true, "watch_mode": true}
		if r.Status == "playing" && r.game != nil {
			payload["game"] = r.gameSnapshot()
			payload["stories"] = r.stories
			payload["private_msgs"] = r.privMsgs
		}
		reply(true, "", payload)
		return
	}
	// 已连接：直接回执必（丙带游戏快照）
	if s := r.seatOf(ev.UserID); s != nil {
		r.mgr.Hub.JoinRoom(r.mgr.Hub.ClientOf(ev.UserID), r.Code)
		s.IsOnline = true
		r.abandonedAt = time.Time{} // 有真人回归：取消空置计时
		payload := map[string]any{"state": r.stateSnapshot(), "seat": s.Number}
		if r.Status == "playing" && r.game != nil {
			payload["game"] = r.gameSnapshot()
		}
		reply(true, "", payload)
		return
	}
	// 同一用户禁同时处于多个房间?	// - 在其他房间且在线（在玩）→ 拒绝，需先
	// - 在其他房间但已线（关闭页面/斺残留）→ 清理旧座位后允加入新房?	// - 对局业离线玩袧出后，其帽?AI 接（防止卡?
	for code2, r2 := range r.mgr.rooms {
		if code2 == r.Code {
			continue
		}
		if s := r2.seatOf(ev.UserID); s != nil {
			if s.IsOnline {
				reply(false, "你已在其他房间中，先", nil)
				return
			}
			// 离线残留座位：从旧房间移除（内存+DB），允迁移
			delete(r2.seats, s.Number)
			r2.syncSeatsDB()
			if r2.game != nil {
				if p := r2.game.Players[itoa(s.Number)]; p != nil {
					p.IsAI = true // AI 接帽
					p.AIType = "local"
				}
			}
			r2.broadcast("room.state", r2.stateSnapshot())
		}
	}
	// DB 兜底：删除用户在当前房间之外的残留座位（含孤儿座位—历史脏数据?
	r.mgr.DB.Model(&model.RoomSeat{}).
		Where("user_id = ? AND room_id NOT IN (SELECT id FROM rooms WHERE room_code = ?)", ev.UserID, r.Code).
		Delete(&model.RoomSeat{})
	if r.Status != "waiting" {
		// 对局进丼非座位玩家可作为观众旁（不占座位不影响游戏状）
		r.mgr.Hub.JoinRoom(r.mgr.Hub.ClientOf(ev.UserID), r.Code)
		payload := map[string]any{"state": r.stateSnapshot(), "seat": 0, "spectator": true}
		if r.game != nil {
			payload["game"] = r.gameSnapshot()
		}
		reply(true, "", payload)
		return
	}
	seat := r.freeSeat()
	if seat < 0 {
		reply(false, "房间已满", nil)
		return
	}
	var user model.User
	if r.mgr.DB.First(&user, ev.UserID).Error != nil {
		reply(false, "用户不存", nil)
		return
	}
	r.seats[seat] = &Seat{Number: seat, UserID: ev.UserID, Username: user.Username, IsOnline: true}
	r.syncSeatsDB()
	r.mgr.Hub.JoinRoom(r.mgr.Hub.ClientOf(ev.UserID), r.Code)
	r.addSystemMsg(user.Username + " 加入了房间")
	r.broadcast("room.state", r.stateSnapshot())
	reply(true, "", map[string]any{"state": r.stateSnapshot(), "seat": seat})
}

func (r *Room) onLeave(ev Event, reply func(bool, string, map[string]any)) {
	s := r.seatOf(ev.UserID)
	if s == nil {
		reply(true, "", nil)
		return
	}
	if r.Status == "playing" {
		// 主动离开对局：真正移除座位（席位由 AI 接管防止卡死）
		// 与断线区分：断线保留座位供重连恢复；主动离开 = 放弃对局
		if r.game != nil {
			if p := r.game.Players[itoa(s.Number)]; p != nil {
				p.IsAI = true
				p.AIType = "local"
			}
		}
		delete(r.seats, s.Number)
		r.syncSeatsDB()
		r.markAbandonedIfEmpty()
		r.addSystemMsg(s.Username + " 离开了对局（席位由 AI 接管）")
		r.broadcast("room.state", r.stateSnapshot())
		reply(true, "", nil)
		return
	}
	delete(r.seats, s.Number)
	r.syncSeatsDB()
	if ev.UserID == r.HostID {
		// 房主轧或解?
		next := r.nextHuman()
		if next != nil {
			r.HostID = next.UserID
			r.mgr.DB.Model(&model.Room{}).Where("room_code = ?", r.Code).Update("host_id", r.HostID)
			r.addSystemMsg("房主已转移给 " + next.Username)
		} else {
			r.broadcast("room.disbanded", map[string]any{})
			r.mgr.Delete(r.Code)
			reply(true, "", nil)
			return
		}
	}
	r.markAbandonedIfEmpty()
	r.broadcast("room.state", r.stateSnapshot())
	reply(true, "", nil)
}

// markAbandonedIfEmpty 空置标：无在线真人时录时刻（等待房间立即毁，对局房间宽限后位）
func (r *Room) markAbandonedIfEmpty() {
	for _, s := range r.seats {
		if s != nil && !s.IsAI && s.IsOnline {
			r.abandonedAt = time.Time{}
			return
		}
	}
	if r.abandonedAt.IsZero() {
		r.abandonedAt = time.Now()
	}
	if r.Status == "waiting" {
		// 等待房间空置：立即销?
		r.mgr.Delete(r.Code)
	}
}

// checkAbandoned 空置清理：房间有真人线超?
func (r *Room) checkAbandoned() {
	if r.Status != "playing" || r.game == nil || r.abandonedAt.IsZero() {
		return
	}
	if time.Since(r.abandonedAt) < 3*time.Minute {
		return
	}
	r.resetToWaiting()
}

// resetToWaiting 对局复位：清空游戏状态回到等待房间（座位保留?
func (r *Room) resetToWaiting() {
	r.Status = "waiting"
	r.game = nil
	r.eng = nil
	r.pubMsgs = nil
	r.privMsgs = nil
	r.stories = make(map[int][]string)
	r.abandonedAt = time.Time{}
	r.mgr.DB.Model(&model.Room{}).Where("room_code = ?", r.Code).
		Updates(map[string]any{"status": "waiting", "game_data": ""})
	for _, s := range r.seats {
		s.IsReady = s.IsAI
	}
	r.addSystemMsg("有玩家已离线，臊复位")
	r.broadcast("room.ended", map[string]any{"reason": "abandoned"})
	r.broadcast("room.state", r.stateSnapshot())
	// 复位后仍无真??立即毁（此前复位房间永远滞留为等待房间，堆积?恢 29 中待房??
	r.abandonedAt = time.Now()
	r.markAbandonedIfEmpty()
}

// onKick 房主踢出等待房间业真人玩（丸司?
func (r *Room) onKick(ev Event, reply func(bool, string, map[string]any)) {
	if ev.UserID != r.HostID {
		reply(false, "叜房主能踢", nil)
		return
	}
	if r.Status != "waiting" {
		reply(false, "对局丸能踢", nil)
		return
	}
	targetUID, _ := ev.Data["user_id"].(float64)
	if uint(targetUID) == 0 || uint(targetUID) == r.HostID {
		reply(false, "盠无效", nil)
		return
	}
	for num, s := range r.seats {
		if s != nil && !s.IsAI && s.UserID == uint(targetUID) {
			delete(r.seats, num)
			r.syncSeatsDB()
			r.mgr.Hub.SendToUser(uint(targetUID), "room.kicked", map[string]any{})
			r.addSystemMsg(s.Username + " 袈主移出了房间")
			r.broadcast("room.state", r.stateSnapshot())
			reply(true, "", nil)
			return
		}
	}
	reply(false, "盠不在房间", nil)
}

// onFillBots 房主锡充假人凑人数：将 AI 座位补满到定人数（至少 5 人可?
func (r *Room) onFillBots(ev Event, reply func(bool, string, map[string]any)) {
	if ev.UserID != r.HostID || r.Status != "waiting" {
		reply(false, "叜房主能添加假", nil)
		return
	}
	target := r.MaxPlayers
	if target < 5 {
		target = 5
	}
	added := 0
	for r.freeSeat() >= 0 && len(r.seats) < target {
		seat := r.freeSeat()
		r.seats[seat] = &Seat{Number: seat, Username: "AI-" + itoa(seat), IsAI: true, AIType: "local", IsReady: true}
		added++
	}
	if added == 0 {
		reply(false, "房间已满", nil)
		return
	}
	r.syncSeatsDB()
	r.broadcast("room.state", r.stateSnapshot())
	reply(true, "", map[string]any{"added": added})
}

func (r *Room) nextHuman() *Seat {
	for _, s := range r.seats {
		if !s.IsAI {
			return s
		}
	}
	return nil
}

// ==============================
// 等待阶
// ==============================

func (r *Room) onChat(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "waiting" {
		reply(false, "请在对局丽用游戏聊", nil)
		return
	}
	content, _ := ev.Data["content"].(string)
	if content == "" {
		reply(false, "内为空", nil)
		return
	}
	s := r.seatOf(ev.UserID)
	if s == nil {
		reply(false, "不在房间", nil)
		return
	}
	r.msgSeq++
	msg := ChatMsg{ID: r.msgSeq, Username: s.Username, Content: content, At: time.Now()}
	r.messages = append(r.messages, msg)
	if len(r.messages) > 200 {
		r.messages = r.messages[len(r.messages)-200:]
	}
	r.broadcast("room.message", msg)
	reply(true, "", map[string]any{"message": msg})
}

func (r *Room) onReady(ev Event, reply func(bool, string, map[string]any)) {
	s := r.seatOf(ev.UserID)
	if s == nil {
		reply(false, "不在房间", nil)
		return
	}
	if ev.UserID == r.HostID {
		reply(false, "房主无需准", nil)
		return
	}
	s.IsReady = !s.IsReady
	r.broadcast("room.state", r.stateSnapshot())
	reply(true, "", map[string]any{"is_ready": s.IsReady})
}

func (r *Room) onAddAI(ev Event, reply func(bool, string, map[string]any)) {
	if ev.UserID != r.HostID || r.Status != "waiting" {
		reply(false, "叜房主能添?AI", nil)
		return
	}
	seat := r.freeSeat()
	if seat < 0 {
		reply(false, "房间已满", nil)
		return
	}
	aiType, _ := ev.Data["ai_type"].(string)
	if aiType != "api" {
		aiType = "local"
	}
	// 大模?AI：携带手动输入的 API 配置（key/服务地址/模型名）
	llmJSON, _ := ev.Data["llm_config"].(string)
	if aiType == "api" && llmJSON == "" {
		cfg := map[string]string{
			"api_key":  strOr(ev.Data["api_key"]),
			"base_url": strOr(ev.Data["base_url"]),
			"model":    strOr(ev.Data["model"]),
		}
		// Ollama 本地模型（127.0.0.1:11434）允许无 Key；其他服务商必须填 Key

		if cfg["api_key"] == "" && !strings.Contains(cfg["base_url"], "11434") {
			reply(false, "请填?API Key（或选择无需 Key 的本地模型）", nil)
			return
		}
		if b, err := json.Marshal(cfg); err == nil {
			llmJSON = string(b)
		}
	}
	// api 类型 AI：座位名直接显示所调大模型名；local 假人显示 AI-座位号
	aiName := "AI-" + itoa(seat)
	if aiType == "api" {
		if m := ModelFromLLMConfig(llmJSON); m != "" {
			aiName = m
		}
	}
	r.seats[seat] = &Seat{Number: seat, Username: aiName, IsAI: true, AIType: aiType, LLMConfig: llmJSON, IsReady: true}
	r.syncSeatsDB()
	r.broadcast("room.state", r.stateSnapshot())
	reply(true, "", nil)
}

func (r *Room) onRemoveAI(ev Event, reply func(bool, string, map[string]any)) {
	if ev.UserID != r.HostID {
		reply(false, "叜房主能移?AI", nil)
		return
	}
	seatN, _ := ev.Data["seat"].(float64)
	delete(r.seats, int(seatN))
	r.syncSeatsDB()
	// 移除 AI：清空该房间 AI 笔记（对局生命周期信号）
	r.mgr.AI.SessionReset(r.Code)
	r.broadcast("room.state", r.stateSnapshot())
	reply(true, "", nil)
}

func (r *Room) onSetSeats(ev Event, reply func(bool, string, map[string]any)) {
	if ev.UserID != r.HostID || r.Status != "waiting" {
		reply(false, "叜房主能罺", nil)
		return
	}
	max, _ := ev.Data["max_players"].(float64)
	m := int(max)
	if m > config.C.MaxPlayersPerRoom {
		m = config.C.MaxPlayersPerRoom
	}
	if m < r.playerCount() {
		m = r.playerCount()
	}
	r.MaxPlayers = m
	r.mgr.DB.Model(&model.Room{}).Where("room_code = ?", r.Code).Update("max_players", m)
	r.broadcast("room.state", r.stateSnapshot())
	reply(true, "", map[string]any{"max_players": m})
}

// ==============================
//
// ==============================

func (r *Room) onStart(ev Event, reply func(bool, string, map[string]any)) {
	if ev.UserID != r.HostID {
		reply(false, "叜房主能开始游", nil)
		return
	}
	if r.Status != "waiting" {
		reply(false, "游戏已开", nil)
		return
	}
	if r.playerCount() < config.C.MinPlayersPerRoom {
		reply(false, "至少?"+itoa(config.C.MinPlayersPerRoom)+" 名玩", nil)
		return
	}
	if r.playerCount() > 12 {
		reply(false, "暗流涌动剧本多支?12 名玩家（13-15 人局旅者机制）", nil)
		return
	}
	if !r.allReady() {
		reply(false, "还有玩朇", nil)
		return
	}

	// 匿名编号随机打乱
	seatList := make([]*Seat, 0, len(r.seats))
	for _, s := range r.seats {
		seatList = append(seatList, s)
	}
	sort.Slice(seatList, func(i, j int) bool { return seatList[i].Number < seatList[j].Number })
	nums := make([]int, len(seatList))
	for i := range nums {
		nums[i] = i + 1
	}
	rand.Shuffle(len(nums), func(i, j int) { nums[i], nums[j] = nums[j], nums[i] })

	gd := &engine.GameData{
		Phase:   "night",
		Day:     0,
		Players: map[string]*engine.PlayerState{},
	}
	for i, s := range seatList {
		ps := &engine.PlayerState{
			UserID: s.UserID, Username: s.Username, Number: nums[i], Seat: s.Number,
			IsAI: s.IsAI, AIType: s.AIType, LLMConf: s.LLMConfig, Alive: true,
		}
		if s.IsAI && s.AIType == "api" {
			ps.AIModel = ModelFromLLMConfig(s.LLMConfig)
		}
		// 真人玩家聊天框样式（商店装备）
		if !s.IsAI && s.UserID != 0 {
			var u model.User
			if r.mgr.DB.Select("chat_frame").First(&u, s.UserID).Error == nil {
				ps.ChatFrame = u.ChatFrame
			}
		}
		gd.Players[itoa(s.Number)] = ps
	}

	r.eng = engine.NewEngine(gd, r.Code, func(event string, data any) {
		r.broadcast(event, data)
	})
	r.eng.AI = r.mgr.AI
	r.eng.HumanChoice = config.C.HumanChoiceEnabled
	r.game = gd
	// 私聊状初始化（防 nil map panic：前后绻码路径在 enterPrivateChat 前闼崩溃?
	r.privateInvites = map[int]int{}
	r.privateSessions = map[int]map[int]bool{}
	r.eng.StartGame()
	r.drainStories()

	r.pubMsgs = nil
	r.Status = "playing"
	r.startedAt = time.Now()
	// 对局生命周期信号：开局时清空该房间 AI 笔记，确保 LLM 以"新的一局"状态开始
	r.mgr.AI.SessionReset(r.Code)
	r.mgr.DB.Model(&model.Room{}).Where("room_code = ?", r.Code).Updates(map[string]any{"status": "playing"})

	r.broadcast("game.started", map[string]any{"room_code": r.Code})
	r.broadcast("game.state", r.gameSnapshot())
	reply(true, "", nil)
}

// drainStories 把引擎生成的说书人消恎给应玩家并留存
func (r *Room) drainStories() {
	for _, msg := range r.eng.GD.StorytellerMsgs {
		r.stories[msg.PlayerNumber] = append(r.stories[msg.PlayerNumber], msg.Content)
		if uid := r.userOfNumber(msg.PlayerNumber); uid != 0 {
			r.mgr.Hub.SendToUser(uid, "game.story", map[string]any{"content": msg.Content})
		}
		// 斗蛐蛐观战：推技能与查验结果；身份发放/角色说明类消息不推（左侧列表已能看到角色）
		if r.WatchMode && !strings.Contains(msg.Content, "你的身份是") &&
			!strings.Contains(msg.Content, "你的角色") && !strings.Contains(msg.Content, "天亮了") {
			r.mgr.Hub.SendToUser(r.HostID, "game.watch_story", map[string]any{
				"number": msg.PlayerNumber, "content": msg.Content,
			})
		}
	}
	r.eng.GD.StorytellerMsgs = nil
}

func (r *Room) userOfNumber(num int) uint {
	for _, p := range r.game.Players {
		if p.Number == num {
			return p.UserID
		}
	}
	return 0
}

// ==============================
// 对局阶
// ==============================

// pubSystem 对局系统消息（进公聊流 + 广播）
func (r *Room) pubSystem(content string) {
	r.msgSeq++
	msg := GameMsg{ID: r.msgSeq, Number: 0, Content: content, System: true, At: time.Now()}
	r.pubMsgs = append(r.pubMsgs, msg)
	if !r.WatchMode && len(r.pubMsgs) > 500 {
		r.pubMsgs = r.pubMsgs[len(r.pubMsgs)-500:]
	}
	r.broadcast("game.message", msg)
	// 斗蛐蛐观战：公共事件（提名/投票/处决/天亮）也进说书人记录
	if r.WatchMode {
		r.mgr.Hub.SendToUser(r.HostID, "game.watch_story", map[string]any{
			"number": 0, "content": content,
		})
	}
}

// trySlayerDeclare 检测并结算猎手开枪声明（公聊发言/提名理由）
// 返回 true 表示检测到声明并已结算（无论命中与否）
func (r *Room) trySlayerDeclare(num int, content string) bool {
	if r.Status != "playing" || r.game == nil {
		return false
	}
	m := slayerRe.FindStringSubmatch(content)
	if m == nil {
		return false
	}
	// 否定式排除："不会开枪""不开枪""没有开枪"等只是谈论技能，不是真实开枪声明
	if strings.Contains(m[0], "不会开枪") || strings.Contains(m[0], "不开枪") ||
		strings.Contains(m[0], "没有开枪") || strings.Contains(m[0], "不打算开枪") ||
		strings.Contains(m[0], "不敢开枪") || strings.Contains(m[0], "不开枪") {
		return false
	}
	target, _ := strconv.Atoi(m[1])
	hit, _, ok := r.eng.SlayerShoot(num, target)
	if !ok {
		// 无效声称（非猎手/技能已用/目标已死）：说书人同样回应"无事发生"，
		// 与真猎手射偏的播报一致，不暴露声称者身份
		r.pubSystem("🔫 #" + itoa(num) + " 声称猎手对 #" + itoa(target) + " 开枪。无事发生")
		r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：#"+itoa(num)+" 声称自己是猎手并开枪射 #"+itoa(target)+"……无事发生，对方没死")
		return true
	}
	if hit {
		r.pubSystem("🔫 #" + itoa(num) + " 声称猎手对 #" + itoa(target) + " 开枪。他就是恶魔，当场死亡！")
		r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：猎手 #"+itoa(num)+" 声称猎手对 #"+itoa(target)+"，恶魔死亡，游戏结束")
		if win := r.eng.CheckWin(); win != "" {
			r.finishGame(win)
			return true
		}
	} else {
		r.pubSystem("🔫 #" + itoa(num) + " 声称猎手对 #" + itoa(target) + " 开枪。无事发生")
		r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：#"+itoa(num)+" 声称自己是猎手并开枪射 #"+itoa(target)+"……无事发生，对方没死")
	}
	r.broadcast("game.state", r.gameSnapshot())
	return true
}

func (r *Room) onGameChat(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" {
		reply(false, "对局朼", nil)
		return
	}
	content, _ := ev.Data["content"].(string)
	if content == "" {
		reply(false, "内为空", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	num := r.game.Players[itoa(seat.Number)].Number
	// 黑禁言：黑夜阶段只允系统夜间卡片，普通聊天一律拒?
	if r.game.Phase == "night" {
		reply(false, "黑不能发言，等待天亮", nil)
		return
	}
	// 提名辩规则：投票进行中仅提名和袏名可以在允发言
	if r.game.Phase == "nomination" && r.game.ActiveVote != nil {
		if num != r.game.ActiveVote.FromNumber && num != r.game.ActiveVote.TargetNumber {
			reply(false, "辩丼仅提名和袏名可以发", nil)
			return
		}
	}
	r.msgSeq++
	msg := GameMsg{ID: r.msgSeq, Number: num, Username: seat.Username, Content: content, At: time.Now()}
	if p := r.game.Players[itoa(seat.Number)]; p != nil {
		msg.Frame = p.ChatFrame
	}
	r.pubMsgs = append(r.pubMsgs, msg)
	if !r.WatchMode && len(r.pubMsgs) > 500 {
		r.pubMsgs = r.pubMsgs[len(r.pubMsgs)-500:]
	}
	r.broadcast("game.message", msg)
	// 猎手开枪声明检测（公聊中喊"我是猎手，我要开枪杀X"）
	if !seat.IsAI {
		r.trySlayerDeclare(num, content)
		// 每日任务：对局中发言进度
		model.TaskProgress(r.mgr.DB, ev.UserID, "chat", 1, 5)
	}
	// 真人发言后触?AI 即时回应（冷却期内跳过）
	if !seat.IsAI && time.Since(r.lastHumanTrigger) > aiReplyCooldown {
		r.lastHumanTrigger = time.Now()
		r.aiAutoChat()
	}
	reply(true, "", map[string]any{"message": msg})
}

func (r *Room) onNextPhase(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" {
		reply(false, "对局朼", nil)
		return
	}
	if ev.UserID != r.HostID {
		reply(false, "叜房主能控制流", nil)
		return
	}
	// 真人主动选择挂起丼拒绝推进（等待提交或超时兜底?
	if r.game.PendingChoice != nil {
		reply(false, "等待玩选择盠丼稍后再试", nil)
		return
	}
	result := r.eng.AdvancePhase()
	r.drainStories()
	// 阶段切换通知：告知该房间所有 AI 当前阶段已切换
	if ph, ok := result["phase"].(string); ok {
		r.mgr.AI.SessionPhase(r.Code, ph)
	}

	// 挂起：定向推送给挂起玩（止房间全广播? 相位/必广播
	if result["waiting"] == true {
		r.broadcastPending(result)
		r.broadcast("game.phase", result)
		r.broadcast("game.state", r.gameSnapshot())
		reply(true, "", result)
		return
	}

	if result["phase"] == "day" {
		if win := r.eng.CheckWin(); win != "" {
			r.finishGame(win)
			reply(true, "", map[string]any{"game_over": true, "winner": win})
			return
		}
	}
	// 进入私聊阶：必须走 enterPrivateChat（清空邀?会话并广撼
	if result["phase"] == "private_chat" {
		r.enterPrivateChat()
		reply(true, "", result)
		return
	}
	r.broadcast("game.phase", result)
	r.broadcast("game.state", r.gameSnapshot())
	reply(true, "", result)
}

// onSubmitChoice 真人提交夜间盠选择（用挂起框架?
//
// 数据传参协（前??后）：
//
//	{"event": "game.submit_choice",
//	 "data": {"room_code": "ABC123", "targets": [2, 4]},  // targets = 选中的玩家匿名编?
//	 "id": 42}
//
// 通过后：执行技能 → 销毁挂起 → 夜晚步骤 +1 → 夜晚自动推进恢复
// 失败响应（ack）：{ok:false, message}
func (r *Room) onSubmitChoice(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil {
		reply(false, "对局未开始", nil)
		return
	}
	pc := r.game.PendingChoice
	if pc == nil {
		reply(false, "没有等待中的操作", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局中", nil)
		return
	}
	myNum := r.game.Players[itoa(seat.Number)].Number
	if myNum != pc.Number {
		reply(false, "不是你的操作", nil)
		return
	}
	// 超时校验：已取消（自动代选停用后，玩家随时可以提交择?
	// if time.Now().Unix() > pc.Deadline {
	// 	reply(false, "操作已超时，系统已自动择", nil)
	// 	return
	// }
	// 解析提交的目标（必须全部在合法集合内且玩家存活）
	targets := []int{}
	if arr, ok := ev.Data["targets"].([]any); ok {
		for _, v := range arr {
			if f, ok := v.(float64); ok {
				targets = append(targets, int(f))
			}
		}
	}
	if len(targets) == 0 {
		reply(false, "请择盠", nil)
		return
	}
	need := r.eng.ChoiceCount(pc.RoleKey)
	if len(targets) != need {
		reply(false, "要择 "+itoa(need)+" 丛", nil)
		return
	}
	legal := map[int]bool{}
	for _, n := range pc.ValidTargets {
		legal[n] = true
	}
	for _, n := range targets {
		if !legal[n] {
			reply(false, "盠不在合法列表", nil)
			return
		}
		p := r.game.Players[pidOfNum(r.game, n)]
		if p == nil || !p.Alive {
			reply(false, "盠无效", nil)
			return
		}
	}
	result, err := r.eng.ResolveChoice(pc.PlayerID, pc.RoleKey, targets, engine.ChoiceManual)
	if err != nil {
		reply(false, err.Error(), nil)
		return
	}
	r.drainStories()
	r.broadcast("game.phase", result)
	r.broadcast("game.state", r.gameSnapshot())
	reply(true, "", result)
}

// onAINominate AI 提名回投：先校验提名（失败不发理由、写笔记），成功后才发理由+广播
func (r *Room) onAINominate(ev Event) {
	if r.Status != "playing" || r.game == nil || r.game.Phase != "nomination" {
		return
	}
	num := toInt(ev.Data["number"])
	target := toInt(ev.Data["target"])
	reason, _ := ev.Data["reason"].(string)
	p := r.game.Players[pidOfNum(r.game, num)]
	if p == nil || !p.Alive || !p.IsAI || target == 0 {
		return
	}
	if r.game.ActiveVote != nil {
		return
	}
	// 先校验提名：失败则不发理由（避免 AI 说了"我提名X"但系统拒绝的言行不一）
	result := r.eng.Nominate(p.Number, target)
	if result["success"] == false {
		if msg, ok := result["message"].(string); ok {
			r.mgr.AI.SessionEvent(r.Code, "你的提名无效："+msg)
		}
		return
	}
	// 猎手开枪特判：引擎已执行射击（射偏/命中），播报开枪结果，不走正常提名流程
	if sp, _ := result["special"].(string); sp == "slayer" {
		if result["game_over"] == true {
			r.pubSystem("🔫 #" + itoa(p.Number) + " 声称猎手对 #" + itoa(target) + " 开枪。他就是恶魔，当场死亡！")
			r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：猎手 #"+itoa(p.Number)+" 声称猎手对 #"+itoa(target)+"，恶魔死亡，游戏结束")
			r.broadcast("game.nominate", result)
			r.broadcast("game.state", r.gameSnapshot())
			win, _ := result["winner"].(string)
			r.finishGame(win)
			return
		}
		r.pubSystem("🔫 #" + itoa(p.Number) + " 声称猎手对 #" + itoa(target) + " 开枪。无事发生")
		r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：#"+itoa(p.Number)+" 声称自己是猎手并开枪射 #"+itoa(target)+"……无事发生，对方没死")
		r.broadcast("game.nominate", result)
		r.broadcast("game.state", r.gameSnapshot())
		return
	}
	// 贞洁者触发：提名者被立即处决
	if sp, _ := result["special"].(string); sp == "virgin" {
		if msg, ok := result["message"].(string); ok {
			r.pubSystem("⚡ " + msg)
		}
		r.broadcast("game.nominate", result)
		r.broadcast("game.state", r.gameSnapshot())
		if result["game_over"] == true {
			win, _ := result["winner"].(string)
			r.finishGame(win)
		}
		return
	}
	// 提名成功：发提名理由（公聊）
	if reason != "" {
		r.msgSeq++
		msg := GameMsg{ID: r.msgSeq, Number: p.Number, Username: p.Username, Content: reason, AIModel: p.AIModel, At: time.Now()}
		r.pubMsgs = append(r.pubMsgs, msg)
		if !r.WatchMode && len(r.pubMsgs) > 500 {
			r.pubMsgs = r.pubMsgs[len(r.pubMsgs)-500:]
		}
		r.broadcast("game.message", msg)
	}
	r.debateSpoken = nil // 新提名轮次：重置辩论发言记录
	r.pubSystem("📢 #" + itoa(p.Number) + " 提名了 #" + itoa(target))
	r.broadcast("game.nominate", result)
	r.broadcast("game.state", r.gameSnapshot())
}

// onAIPrivateSay AI 私聊回回投：定向推送给对话双方
func (r *Room) onAIPrivateSay(ev Event) {
	if r.Status != "playing" || r.game == nil {
		return
	}
	num := toInt(ev.Data["number"])
	toNum := toInt(ev.Data["to_number"])
	content, _ := ev.Data["content"].(string)
	if content == "" {
		return
	}
	p := r.game.Players[pidOfNum(r.game, num)]
	target := r.game.Players[pidOfNum(r.game, toNum)]
	if p == nil || target == nil || !p.IsAI {
		return
	}
	if !r.hasSession(p.Number, target.Number) {
		return
	}
	msg := PrivateMsg{From: p.Number, To: target.Number, Content: content, Day: r.game.Day, At: time.Now()}
	r.privMsgs = append(r.privMsgs, msg)
	if len(r.privMsgs) > 500 {
		r.privMsgs = r.privMsgs[len(r.privMsgs)-500:]
	}
	payload := map[string]any{
		"from_number": msg.From,
		"to_number":   msg.To,
		"content":     msg.Content,
	}
	r.mgr.Hub.SendToUser(p.UserID, "game.private_message", payload)
	if !target.IsAI {
		r.mgr.Hub.SendToUser(target.UserID, "game.private_message", payload)
	}
	// 私聊长消息分段推送（观战方与真人收到多条，更自然）
	if r.WatchMode {
		for _, seg := range SplitLongMessage(content, 100) {
			r.mgr.Hub.SendToUser(r.HostID, "game.watch_private", map[string]any{
				"from_number": msg.From, "to_number": msg.To, "content": seg,
			})
		}
	}
	// AI→AI 私聊回复循环：触发接收方 AI 回话；邪恶方队友会话上限更高（战术交流需要更多轮次）
	if target.IsAI && target.AIType == "api" {
		if r.isSilent(target.Number, p.Number) {
			return // 对方 AI 已自然收尾，保持沉默（仅思考不回复）
		}
		cnt := 0
		for _, m := range r.privMsgs {
			if (m.From == p.Number && m.To == target.Number) || (m.From == target.Number && m.To == p.Number) {
				cnt++
			}
		}
		// 条数上限：好人 4 条（交换信息 2 轮足够，再多就是车轱辘话）；
		// 邪恶方 10 条（战术链路需要：分配伪装身份→爪牙确认→汇报观察→指令→确认计划）
		limit := 4
		bothEvil := (p.Team == engine.TeamDemon || p.Team == engine.TeamMinion) &&
			(target.Team == engine.TeamDemon || target.Team == engine.TeamMinion)
		if bothEvil {
			limit = 10
		}
		if cnt < limit {
			r.aiBusy = true
			go func(aiNum int, aiRole string, aiTeam string, aiConf string, clues []string, askerNum int) {
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("[ai-debug] PANIC aiPrivateReplyLoop: %v", rec)
					}
					r.Post(Event{Type: "ai_busy_done"})
				}()
				ctx := []map[string]any{}
				for _, m := range r.privMsgs {
					if (m.From == aiNum && m.To == askerNum) || (m.From == askerNum && m.To == aiNum) {
						ctx = append(ctx, map[string]any{"num": m.From, "content": m.Content})
					}
				}
			if len(ctx) > 10 {
				ctx = ctx[len(ctx)-10:]
			}
			reply, ok, endChat := r.mgr.AI.SpeakWithEnd(aiclient.SpeakReq{
				RoomCode:  r.Code,
				RoleName:  aiRole,
				Team:      aiTeam,
				Number:    aiNum,
				Phase:     "private_chat",
				Day:       r.game.Day,
				AliveNums: r.aliveList(), TotalPlayers: len(r.game.Players),
				Context:   ctx,
				MyClues:   clues,
				LLMConf:   aiConf,
				OtherNum:  askerNum,
			})
			if !ok || reply == "" {
				return
			}
			if endChat {
				r.Post(Event{Type: "ai_private_silence", Data: map[string]any{"number": aiNum, "to_number": askerNum}})
			}
			r.Post(Event{Type: "ai_private_say", Data: map[string]any{
				"number": aiNum, "to_number": askerNum, "content": reply,
			}})
		}(target.Number, target.RoleName, target.Team, target.LLMConf, r.eng.MyStories(target.Number), p.Number)
	} else {
		// 聊满上限：结束该会话（退出私聊，双方可找其他人）
		r.endSession(p.Number, target.Number)
	}
}
}

// endSession 结束一对私聊会话（双向清除），AI 可重新找人私聊
func (r *Room) endSession(a, b int) {
	if r.privateSessions[a] != nil {
		delete(r.privateSessions[a], b)
	}
	if r.privateSessions[b] != nil {
		delete(r.privateSessions[b], a)
	}
	if r.sessionAt != nil {
		delete(r.sessionAt, fmt.Sprintf("%d-%d", a, b))
		delete(r.sessionAt, fmt.Sprintf("%d-%d", b, a))
	}
	if r.privateSilence[a] != nil {
		delete(r.privateSilence[a], b)
	}
	if r.privateSilence[b] != nil {
		delete(r.privateSilence[b], a)
	}
	// 标记已聊过：今天这对不再重开（防止无限重开会话）
	if a < b {
		r.privateTalked[fmt.Sprintf("%d-%d", a, b)] = true
	} else {
		r.privateTalked[fmt.Sprintf("%d-%d", b, a)] = true
	}
}

// hasTalked 今天这对玩家是否已聊过（已结束的会话不再重开）
func (r *Room) hasTalked(a, b int) bool {
	var key string
	if a < b {
		key = fmt.Sprintf("%d-%d", a, b)
	} else {
		key = fmt.Sprintf("%d-%d", b, a)
	}
	return r.privateTalked[key]
}

// setSilence AI 对某会话进入沉默权（自然收尾后不再回复）
func (r *Room) setSilence(ai, other int) {
	if r.privateSilence[ai] == nil {
		r.privateSilence[ai] = map[int]bool{}
	}
	r.privateSilence[ai][other] = true
}

// isSilent AI 是否已对该会话沉默
func (r *Room) isSilent(ai, other int) bool {
	return r.privateSilence[ai] != nil && r.privateSilence[ai][other]
}

// onAIPrivateAcceptResolve AI 私聊邀请决策回投：接受则建立会话+AI 开场白；拒绝则通知邀请者
func (r *Room) onAIPrivateAcceptResolve(ev Event) {
	if r.Status != "playing" || r.game == nil {
		return
	}
	from := toInt(ev.Data["from"])
	target := toInt(ev.Data["target"])
	accept, _ := ev.Data["accept"].(bool)
	fp := r.game.Players[pidOfNum(r.game, from)]
	tp := r.game.Players[pidOfNum(r.game, target)]
	if fp == nil || tp == nil {
		return
	}
	if !accept {
		if fp.UserID != 0 {
			r.mgr.Hub.SendToUser(fp.UserID, "game.private_declined", map[string]any{"by_number": target})
		}
		return
	}
	if r.hasSession(from, target) || len(r.privateSessions[target]) > 0 {
		return
	}
	r.establishSession(from, target)
	if fp.UserID != 0 {
		r.mgr.Hub.SendToUser(fp.UserID, "game.private_accepted", map[string]any{"partner": target})
	}
	if tp.AIType == "api" {
		r.aiPrivateFirstLine(tp, fp)
	}
}

// onSlayerShoot 真人猎手主动开枪（点击弓箭图标选择目标）
func (r *Room) onSlayerShoot(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil {
		reply(false, "对局未开始", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	p := r.game.Players[itoa(seat.Number)]
	if p == nil || !p.Alive {
		reply(false, "你已死亡", nil)
		return
	}
	if p.Role != "slayer" {
		reply(false, "你不是猎手", nil)
		return
	}
	if p.SlayerUsed {
		reply(false, "你的子弹已经用过了", nil)
		return
	}
	target, _ := ev.Data["target_number"].(float64)
	if target == 0 {
		reply(false, "目标无效", nil)
		return
	}
	hit, _, ok := r.eng.SlayerShoot(p.Number, int(target))
	if !ok {
		reply(false, "开枪失败（目标已死或技能已用）", nil)
		return
	}
	if hit {
		r.pubSystem("🔫 #" + itoa(p.Number) + " 声称猎手对 #" + itoa(int(target)) + " 开枪。他就是恶魔，当场死亡！")
		r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：猎手 #"+itoa(p.Number)+" 声称猎手对 #"+itoa(int(target))+"，恶魔死亡，游戏结束")
		if win := r.eng.CheckWin(); win != "" {
			r.finishGame(win)
			reply(true, "", nil)
			return
		}
	} else {
		r.pubSystem("🔫 #" + itoa(p.Number) + " 声称猎手对 #" + itoa(int(target)) + " 开枪。无事发生")
		r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：#"+itoa(p.Number)+" 声称自己是猎手并开枪射 #"+itoa(int(target))+"……无事发生，对方没死")
	}
	r.broadcast("game.state", r.gameSnapshot())
	reply(true, "", nil)
}

// onAISay AI 异发言结果回投：校验玩家仍在且存活后广播（在事件徎内执行，无竞争）
func (r *Room) onAISay(ev Event) {
	if r.Status != "playing" || r.game == nil {
		log.Printf("[ai-debug] onAISay 丢弃: status=%s game=%v", r.Status, r.game == nil)
		return
	}
	if r.game.Phase == "nomination" {
		// 提名辩论期：仅提名者/被提名者的 AI 发言可通过（辩论发言）
		v := r.game.ActiveVote
		if v == nil || v.VoteDeadline != 0 {
			return
		}
		num := toInt(ev.Data["number"])
		if num != v.FromNumber && num != v.TargetNumber {
			return
		}
	} else if r.game.Phase != "public_chat" {
		log.Printf("[ai-debug] onAISay 丢弃: phase=%s", r.game.Phase)
		return
	}
	num := toInt(ev.Data["number"])
	content, _ := ev.Data["content"].(string)
	if content == "" {
		return
	}
	p := r.game.Players[pidOfNum(r.game, int(num))]
	if p == nil || !p.Alive || !p.IsAI {
		return
	}
	// 长发言分段发送：每段间隔 2 秒，像真人打字连续发几条（异步，不阻塞事件循环）
	segs := SplitLongMessage(content, 100)
	if len(segs) == 1 {
		r.onAISaySegment(p.Number, segs[0])
	} else {
		go func(num int, segments []string) {
			for _, seg := range segments {
				r.Post(Event{Type: "ai_say_seg", Data: map[string]any{"number": num, "content": seg}})
				// 模拟真人"发一条 → 思考 → 再发下一条"的节奏（2-4 秒随机思考时长）
				time.Sleep(time.Duration(2000+rand.Intn(2000)) * time.Millisecond)
			}
		}(p.Number, segs)
	}
	// AI 发言同样检测猎手开枪声明
	r.trySlayerDeclare(p.Number, content)
}

// onAISaySegment 分段发言的单条消息回投（在事件循环内执行，线程安全）
func (r *Room) onAISaySegment(num int, content string) {
	if r.Status != "playing" || r.game == nil {
		return
	}
	if r.game.Phase == "nomination" {
		v := r.game.ActiveVote
		if v == nil || v.VoteDeadline != 0 {
			return
		}
		if num != v.FromNumber && num != v.TargetNumber {
			return
		}
	} else if r.game.Phase != "public_chat" {
		return
	}
	p := r.game.Players[pidOfNum(r.game, num)]
	if p == nil || !p.Alive || !p.IsAI || content == "" {
		return
	}
	r.msgSeq++
	msg := GameMsg{ID: r.msgSeq, Number: p.Number, Username: p.Username, Content: content, AIModel: p.AIModel, Frame: p.ChatFrame, At: time.Now()}
	r.pubMsgs = append(r.pubMsgs, msg)
	if !r.WatchMode && len(r.pubMsgs) > 500 {
		r.pubMsgs = r.pubMsgs[len(r.pubMsgs)-500:]
	}
	// 发言计数（车轱辘限制用）
	if r.speakCounts == nil {
		r.speakCounts = map[int]int{}
	}
	r.speakCounts[p.Number]++
	r.broadcast("game.message", msg)
}

// onUserOffline WS 斿：标记座位线（若房间空罈始清理时）
func (r *Room) onUserOffline(ev Event) {
	if s := r.seatOf(ev.UserID); s != nil {
		s.IsOnline = false
		r.markAbandonedIfEmpty()
	}
}

// checkPendingTimeout 真人夜晚行动超时兜底?5 秒）：AI 臊代并继续夜晚，防止卡
func (r *Room) checkPendingTimeout() {
	if r.Status != "playing" || r.game == nil || r.game.PendingChoice == nil {
		return
	}
	pc := r.game.PendingChoice
	if time.Now().Unix() < pc.Deadline {
		return
	}
	result := r.eng.TimeoutChoice()
	if result == nil {
		return
	}
	// 广播超时代结?+ 必（晚自动推?ticker 会继绸步）
	r.broadcast("game.phase", result)
	r.broadcast("game.state", r.gameSnapshot())
	r.drainStories()
}

// onTipAck 玩收到夜间行动卡片：从此刻始重新时（超时以卡片达为起点）
func (r *Room) onTipAck(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil || r.game.PendingChoice == nil {
		reply(true, "", map[string]any{"ok": true})
		return
	}
	pc := r.game.PendingChoice
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(true, "", map[string]any{"ok": true})
		return
	}
	if p := r.game.Players[itoa(seat.Number)]; p != nil && p.Number == pc.Number {
		pc.Deadline = time.Now().Add(45 * time.Second).Unix()
	}
	reply(true, "", map[string]any{"ok": true, "deadline": pc.Deadline})
}

// RolesOf 角色世名（避免重查表?
func RolesOf(roleKey string) string {
	if ri, ok := engine.Roles[roleKey]; ok {
		return ri.Name
	}
	return roleKey
}

func (r *Room) onNominate(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" {
		reply(false, "对局朼", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	if r.game.Phase != "nomination" {
		reply(false, "当前不是提名阶", nil)
		return
	}
	target, _ := ev.Data["target_number"].(float64)
	if target == 0 {
		reply(false, "盠无效", nil)
		return
	}
	myNum := r.game.Players[itoa(seat.Number)].Number
	result := r.eng.Nominate(myNum, int(target))
	if result["success"] == false {
		reply(false, result["message"].(string), nil)
		return
	}
	// 猎手提名 = 对提名对象开枪（官方变体规则：提名对象即开枪对象）
	if p := r.game.Players[itoa(seat.Number)]; p != nil && p.Role == "slayer" && !p.SlayerUsed {
		if hit, _, ok := r.eng.SlayerShoot(myNum, int(target)); ok {
			if hit {
				r.pubSystem("🔫 #" + itoa(myNum) + " 声称猎手对 #" + itoa(int(target)) + " 开枪。他就是恶魔，当场死亡！")
				r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：猎手 #"+itoa(myNum)+" 声称猎手对 #"+itoa(int(target))+"，恶魔死亡，游戏结束")
				if win := r.eng.CheckWin(); win != "" {
					r.finishGame(win)
					reply(true, "", result)
					return
				}
			} else {
				r.pubSystem("🔫 #" + itoa(myNum) + " 声称猎手对 #" + itoa(int(target)) + " 开枪。无事发生")
				r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：#"+itoa(myNum)+" 声称自己是猎手并开枪射 #"+itoa(int(target))+"……无事发生，对方没死")
			}
			r.broadcast("game.state", r.gameSnapshot())
		}
	}
	// 立即效果（猎?贞洁者）
	if result["special"] != nil {
		if msg, ok := result["message"].(string); ok {
			r.pubSystem("⚡ " + msg)
		}
		r.broadcast("game.nominate", result)
		if result["game_over"] == true {
			win, _ := result["winner"].(string)
			r.finishGame(win)
		}
		reply(true, "", result)
		return
	}
	// 常提名：广撾论开始（提名/辩信息由前竳统卡片渲染，不发裸文朶恼
	r.debateSpoken = nil // 新提名轮次：重置辩论发言记录
	r.pubSystem("📢 #" + itoa(myNum) + " 提名了 #" + itoa(int(target)))
	r.broadcast("game.nominate", result)
	r.broadcast("game.state", r.gameSnapshot())
	reply(true, "", result)
}

// checkPrivateChatTimeout 斗蛐蛐观战模式：私聊/公聊阶段限时自动推进（直到一方胜利）
// 私聊 60 秒 → 公聊；公聊 120 秒 → 提名；提名/夜晚沿用原有倒计时机制
func (r *Room) checkPrivateChatTimeout() {
	if !r.WatchMode || r.Status != "playing" || r.game == nil {
		return
	}
	ph := r.game.Phase
	if ph != "private_chat" && ph != "public_chat" {
		r.lastAutoPhase = ""
		return
	}
	if ph != r.lastAutoPhase {
		r.lastAutoPhase = ph
		r.phaseEnteredAt = time.Now()
		return
	}
	limit := 60 * time.Second
	if ph == "public_chat" {
		limit = 120 * time.Second
	}
	if time.Since(r.phaseEnteredAt) >= limit {
		result := r.eng.AdvancePhase()
		r.lastAutoPhase = ""
		if ph2, ok := result["phase"].(string); ok {
			r.mgr.AI.SessionPhase(r.Code, ph2)
		}
		r.broadcast("game.phase", result)
		r.broadcast("game.state", r.gameSnapshot())
		log.Printf("[ai-debug] 斗蛐蛐自动推进: %s -> %v", ph, result["phase"])
	}
}

// enterPrivateChat 进入私聊阶：清空邀请与会话，并广播（无时间限制，房主可手动跳过?
func (r *Room) enterPrivateChat() {
	// 状互斥守卼夜晚朻束或真人选择挂起朧除时，止进入聊（防相位错乱）
	if r.game.PendingChoice != nil || r.game.Phase == "night" {
		return
	}
	r.privateInvites = map[int]int{}
	r.privateSessions = map[int]map[int]bool{}
	r.sessionAt = map[string]int64{}
	r.privateSilence = map[int]map[int]bool{}
	r.privateTalked = map[string]bool{}
	r.broadcast("game.phase", map[string]any{
		"phase": "private_chat",
	})
	r.broadcast("game.state", r.gameSnapshot())
}

// onPrivateInvite 真人请另名存活玩家聊（请制：方接受后才建立会话）
func (r *Room) onPrivateInvite(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil {
		reply(false, "对局朼", nil)
		return
	}
	if r.game.Phase != "private_chat" {
		reply(false, "当前不是私聊阶", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	me := r.game.Players[itoa(seat.Number)]
	if me == nil {
		reply(false, "无效玩家", nil)
		return
	}
	toNum, _ := ev.Data["to_number"].(float64)
	target := r.game.Players[pidOfNum(r.game, int(toNum))]
	if target == nil || !target.Alive {
		reply(false, "盠无效或已死亡", nil)
		return
	}
	if target.Number == me.Number {
		reply(false, "不能请自己", nil)
		return
	}
	if r.hasSession(me.Number, target.Number) {
		reply(false, "你们已在私聊", nil)
		return
	}
	if _, exists := r.privateInvites[target.Number]; exists {
		reply(false, "对方已有私聊请待处理", nil)
		return
	}
	r.privateInvites[target.Number] = me.Number
	// AI 盠：由 AI 自己判断是否接受邀请（LLM 决策），不再用固定概率
	if target.IsAI {
		// 单私聊限制：AI 已有活跃会话时拒绝新邀请（避免同时多人私聊导致记忆错乱）
		if len(r.privateSessions[target.Number]) > 0 {
			delete(r.privateInvites, target.Number)
			reply(false, "#"+itoa(target.Number)+" 正在私聊中，稍后再试", nil)
			return
		}
		delete(r.privateInvites, target.Number)
		if target.AIType != "api" {
			// 本地模板 AI：直接接受
			r.establishSession(me.Number, target.Number)
			r.mgr.Hub.SendToUser(me.UserID, "game.private_accepted", map[string]any{"partner": target.Number})
			reply(true, "", map[string]any{"auto_accept": true, "partner": target.Number})
			return
		}
		// API AI：异步让 AI 自己想是否接受
		r.aiBusy = true
		go func(fromNum int, fromUID uint, ai *engine.PlayerState) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[ai-debug] PANIC DecidePrivateAccept: %v", rec)
				}
				r.Post(Event{Type: "ai_busy_done"})
			}()
			accept, ok := r.mgr.AI.DecidePrivateAccept(r.Code, ai.RoleName, fromNum,
				r.aliveList(), ai.Number, ai.LLMConf, r.eng.MyStories(ai.Number))
			if !ok {
				accept = true
			}
			r.Post(Event{Type: "ai_private_accept_resolve", Data: map[string]any{
				"from": fromNum, "target": ai.Number, "accept": accept,
			}})
		}(me.Number, me.UserID, target)
		reply(true, "", map[string]any{"waiting": true, "partner": target.Number})
		return
	}
	// 真人盠：推送邀?
	r.mgr.Hub.SendToUser(target.UserID, "game.private_invite", map[string]any{
		"from_number": me.Number,
	})
	reply(true, "", map[string]any{"waiting": true, "partner": target.Number})
}

// onPrivateAccept 接受私聊?
func (r *Room) onPrivateAccept(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil || r.game.Phase != "private_chat" {
		reply(false, "当前不是私聊阶", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	me := r.game.Players[itoa(seat.Number)]
	fromNum, exists := r.privateInvites[me.Number]
	if !exists {
		reply(false, "没有待理的私聊", nil)
		return
	}
	delete(r.privateInvites, me.Number)
	r.establishSession(me.Number, fromNum)
	// 双向通知
	from := r.game.Players[pidOfNum(r.game, fromNum)]
	r.mgr.Hub.SendToUser(me.UserID, "game.private_accepted", map[string]any{"partner": fromNum})
	if from != nil {
		r.mgr.Hub.SendToUser(from.UserID, "game.private_accepted", map[string]any{"partner": me.Number})
	}
	// AI 请主动笸句（会话建立?AI 先开口，像真人一样）
	if from != nil && from.IsAI && from.AIType == "api" {
		r.aiPrivateFirstLine(from, me)
	}
	reply(true, "", map[string]any{"partner": fromNum})
}

// aiPrivateFirstLine AI 在聊会话建立后主动发?
func (r *Room) aiPrivateFirstLine(ai, other *engine.PlayerState) {
	log.Printf("[ai-private] 触发笸?ai=#%d other=#%d type=%s", ai.Number, other.Number, ai.AIType)
	go func(aiNum int, aiRole string, aiTeam string, aiConf string, clues []string, otherNum int) {
		defer func() {
			r.Post(Event{Type: "ai_busy_done"})
		}()
		reply, ok, endChat := r.mgr.AI.SpeakWithEnd(aiclient.SpeakReq{
			RoomCode:  r.Code,
			RoleName:  aiRole,
			Team:      aiTeam,
			Number:    aiNum,
			Phase:     "private_chat",
			Day:       r.game.Day,
			AliveNums: r.aliveList(), TotalPlayers: len(r.game.Players),
			Context:   []map[string]any{{"num": 0, "content": "你们刚刚建立了私聊，请主动开启话题"}},
			MyClues:   clues,
			LLMConf:   aiConf,
			OtherNum:  otherNum,
		})
		log.Printf("[ai-private] 笸句生?ok=%v reply=%q", ok, reply)
		if !ok || reply == "" {
			return
		}
		if endChat {
			r.Post(Event{Type: "ai_private_silence", Data: map[string]any{"number": aiNum, "to_number": otherNum}})
		}
		r.Post(Event{Type: "ai_private_say", Data: map[string]any{
			"number": aiNum, "to_number": otherNum, "content": reply,
		}})
	}(ai.Number, ai.RoleName, ai.Team, ai.LLMConf, r.eng.MyStories(ai.Number), other.Number)
}

// onPrivateDecline 拒绝私聊?
func (r *Room) onPrivateDecline(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil {
		reply(false, "对局朼", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	me := r.game.Players[itoa(seat.Number)]
	fromNum, exists := r.privateInvites[me.Number]
	if !exists {
		reply(false, "没有待理的私聊", nil)
		return
	}
	delete(r.privateInvites, me.Number)
	from := r.game.Players[pidOfNum(r.game, fromNum)]
	if from != nil {
		r.mgr.Hub.SendToUser(from.UserID, "game.private_declined", map[string]any{"by_number": me.Number})
	}
	reply(true, "", nil)
}

// establishSession 建立双向私聊会话
func (r *Room) establishSession(a, b int) {
	if r.privateSessions[a] == nil {
		r.privateSessions[a] = map[int]bool{}
	}
	if r.privateSessions[b] == nil {
		r.privateSessions[b] = map[int]bool{}
	}
	r.privateSessions[a][b] = true
	r.privateSessions[b][a] = true
	if r.sessionAt == nil {
		r.sessionAt = map[string]int64{}
	}
	r.sessionAt[fmt.Sprintf("%d-%d", a, b)] = time.Now().Unix()
}

// hasSession 查是否已建立会话
func (r *Room) hasSession(a, b int) bool {
	return r.privateSessions[a] != nil && r.privateSessions[a][b]
}

// expireSessions 清理超时会话（聊了几句就断线的会话自动退出，双方可重新找人）
// 仅清理 AI↔AI 会话：真人参与的会话永不超时（真人想聊多久聊多久）
func (r *Room) expireSessions() {
	if r.sessionAt == nil {
		return
	}
	now := time.Now().Unix()
	for key, at := range r.sessionAt {
		if now-at > 180 {
			var a, b int
			fmt.Sscanf(key, "%d-%d", &a, &b)
			pa := r.game.Players[pidOfNum(r.game, a)]
			pb := r.game.Players[pidOfNum(r.game, b)]
			if pa == nil || pb == nil {
				r.endSession(a, b)
				delete(r.sessionAt, key)
				continue
			}
			if pa.IsAI && pb.IsAI {
				r.endSession(a, b)
				delete(r.sessionAt, key)
			}
		}
	}
}

// onPrivateChat 发聊消恼仅已建立会话的双方）
func (r *Room) onPrivateChat(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil {
		reply(false, "对局朼", nil)
		return
	}
	if r.game.Phase != "private_chat" {
		reply(false, "当前不是私聊阶", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	me := r.game.Players[itoa(seat.Number)]
	if me == nil {
		reply(false, "无效玩家", nil)
		return
	}
	toNum, _ := ev.Data["to_number"].(float64)
	content, _ := ev.Data["content"].(string)
	if int(toNum) == 0 || content == "" {
		reply(false, "参数错", nil)
		return
	}
	target := r.game.Players[pidOfNum(r.game, int(toNum))]
	if target == nil || !target.Alive {
		reply(false, "盠无效或已死亡", nil)
		return
	}
	if !r.hasSession(me.Number, target.Number) {
		reply(false, "你们尚未建立私聊，先邀", nil)
		return
	}
	msg := PrivateMsg{From: me.Number, To: target.Number, Content: content, Day: r.game.Day, At: time.Now()}
	r.privMsgs = append(r.privMsgs, msg)
	if len(r.privMsgs) > 500 {
		r.privMsgs = r.privMsgs[len(r.privMsgs)-500:]
	}
	// 定向推：仅发送方与接收方
	payload := map[string]any{
		"from_number": msg.From,
		"to_number":   msg.To,
		"content":     msg.Content,
	}
	r.mgr.Hub.SendToUser(me.UserID, "game.private_message", payload)
	if !target.IsAI {
		r.mgr.Hub.SendToUser(target.UserID, "game.private_message", payload)
	}
	// AI 私聊回：目标是 API AI ?异 LLM 生成回并定向推?	// 注意：不使用 aiBusy 全局锁（否则允发言时会永久丢弃私聊回），结果回投串处理天然无竞?
	if target.IsAI && target.AIType == "api" {
		if r.isSilent(target.Number, me.Number) {
			return // AI 已自然收尾，保持沉默（仅思考不回复）
		}
		r.aiBusy = true
		go func(aiNum int, aiRole string, aiTeam string, aiConf string, clues []string, askerNum int, askerMsg string) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[ai-debug] PANIC aiPrivateReply: %v", rec)
				}
				r.Post(Event{Type: "ai_busy_done"})
			}()
			// 收集?AI 与方的私聊历史作为上下?
			ctx := []map[string]any{}
			for _, m := range r.privMsgs {
				if (m.From == aiNum && m.To == askerNum) || (m.From == askerNum && m.To == aiNum) {
					ctx = append(ctx, map[string]any{"num": m.From, "content": m.Content})
				}
			}
			if len(ctx) > 10 {
				ctx = ctx[len(ctx)-10:]
			}
			reply, ok, endChat := r.mgr.AI.SpeakWithEnd(aiclient.SpeakReq{
				RoomCode:  r.Code,
				RoleName:  aiRole,
				Team:      aiTeam,
				Number:    aiNum,
				Phase:     "private_chat",
				Day:       r.game.Day,
				AliveNums: r.aliveList(), TotalPlayers: len(r.game.Players),
				Context:   ctx,
				MyClues:   clues,
				LLMConf:   aiConf,
				OtherNum:  askerNum,
			})
			if !ok || reply == "" {
				return
			}
			if endChat {
				r.Post(Event{Type: "ai_private_silence", Data: map[string]any{"number": aiNum, "to_number": askerNum}})
			}
			r.Post(Event{Type: "ai_private_say", Data: map[string]any{
				"number": aiNum, "to_number": askerNum, "content": reply,
			}})
		}(target.Number, target.RoleName, target.Team, target.LLMConf, r.eng.MyStories(target.Number), me.Number, content)
	}
	reply(true, "", map[string]any{"message": payload})
}

// onPrivateExit 真人主动退出私聊：结束与该对象的会话（AI 被释放，可接受他人邀请）
func (r *Room) onPrivateExit(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil {
		reply(false, "对局朼", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	me := r.game.Players[itoa(seat.Number)]
	if me == nil {
		reply(false, "无效玩家", nil)
		return
	}
	toNum, _ := ev.Data["to_number"].(float64)
	target := r.game.Players[pidOfNum(r.game, int(toNum))]
	if target == nil {
		reply(false, "对象无效", nil)
		return
	}
	r.endSession(me.Number, target.Number)
	reply(true, "", nil)
}

// onSkipPrivate 房主跳过私聊阶，直接进入公?
func (r *Room) onSkipPrivate(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil {
		reply(false, "对局朼", nil)
		return
	}
	if ev.UserID != r.HostID {
		reply(false, "叜房主能跳过聊阶", nil)
		return
	}
	if r.game.Phase != "private_chat" {
		log.Printf("[ai-debug] skip_private 失败: phase=%s", r.game.Phase)
		reply(false, "当前不是私聊阶", nil)
		return
	}
	log.Printf("[ai-debug] skip_private 成功")
	result := r.eng.AdvancePhase() // private_chat -> public_chat
	if ph, ok := result["phase"].(string); ok {
		r.mgr.AI.SessionPhase(r.Code, ph)
	}

	r.broadcast("game.phase", result)
	r.broadcast("game.state", r.gameSnapshot())
	reply(true, "", result)
}

// checkVoteTimeout 提名状机：辩论到??投票倒时；投票到期 ?统+冷却窗口；冷却到??比处决
func (r *Room) checkVoteTimeout() {
	if r.Status != "playing" || r.game == nil {
		return
	}
	// 无活跃投祼提名冷却到期 ?统一比全部袏名票数?
	if r.game.ActiveVote == nil {
		if r.game.NomCooldownUntil != 0 && time.Now().Unix() >= r.game.NomCooldownUntil {
			r.resolveNominationPeriod()
		}
		return
	}
	v := r.game.ActiveVote
	// 辩期：到期后启动投祼同时投票倒时，AI 立即投票?
	if v.VoteDeadline == 0 {
		if time.Now().Unix() >= v.DebateDeadline {
			turnResult, _ := r.eng.StartVoting()
			if turnResult == nil {
				return
			}
			r.broadcast("game.vote_start", turnResult)
			r.broadcast("game.state", r.gameSnapshot())
			// 提名事件通知 AI：辩论开始，请聆听
			r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：#"+itoa(v.FromNumber)+" 提名了 #"+itoa(v.TargetNumber)+"，辩论开始，请仔细聆听")
		}
		return
	}
	// 投票期：到期后统计票???30 秒提名冷却（处决判定延迟到冷却到期）
	// 投票统由前?vote_result 系统卡片渲染，不发裸文本
	if time.Now().Unix() >= v.VoteDeadline {
		result := r.eng.FinishVote()
		if result == nil {
			return
		}
		r.broadcast("game.vote_result", result)
		r.broadcast("game.state", r.gameSnapshot())
		// 投票结束事件通知 AI：完整投票明细（谁投了赞成、谁没投）
		yes, _ := result["yes"].(int)
		yesNums, _ := result["yes_nums"].([]int)
		unvotedNums, _ := result["unvoted_nums"].([]int)
		detail := "第" + itoa(r.game.Day+1) + "天：对 #" + itoa(v.TargetNumber) + " 的投票结束：赞成 " + joinInts(yesNums) +
			"（共 " + itoa(yes) + " 票），未投票 " + joinInts(unvotedNums) +
			"；如果还有要提名的请及时公开声称"
		r.mgr.AI.SessionEvent(r.Code, detail)
		r.pubSystem("📢 投票结束：对 #" + itoa(v.TargetNumber) + " 的投票，赞成 " + joinInts(yesNums) +
			"（共 " + itoa(yes) + " 票）")
	}
}

// resolveNominationPeriod 提名周期结束：比对票??处决（入夜）或无人决（吖轏名）
func (r *Room) resolveNominationPeriod() {
	result := r.eng.ResolveNominationPeriod()
	if result == nil {
		return
	}
	// 处决判定结果由前?nominate_result 系统卡片渲染，不发裸文本
	r.broadcast("game.nominate_result", result)
	r.broadcast("game.state", r.gameSnapshot())
	// 处决事件通知 AI + 公屏时间轴播报
	if msg, ok := result["message"].(string); ok {
		r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天提名阶段结果："+msg)
		r.pubSystem("⚖️ " + msg)
	}
	if result["game_over"] == true {
		win, _ := result["winner"].(string)
		r.finishGame(win)
		return
	}
	// 提名周期结束：无论是否处决（平安日也入夜），都进入黑夜阶段
	phaseResult := r.eng.AdvancePhase() // nomination -> night
	r.broadcast("game.phase", phaseResult)
	r.broadcast("game.state", r.gameSnapshot())
}

func strOr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// toInt 兼 float64（JSON 反序列化）与 int（内部事件投递）两数类?
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// voteStatsText 投票统系统消息文本（公聊展示：已投/朊/每人票数 + 30 秒提名窗口提示）
func (r *Room) voteStatsText(result map[string]any) string {
	yes, _ := result["yes"].(int)
	abstain, _ := result["abstain"].(int)
	voted, _ := result["voted"].(int)
	unvoted, _ := result["unvoted"].(int)
	target, _ := result["target"].(int)
	return "📢 投票结束?" + itoa(target) + " 赞成 " + itoa(yes) +
		" 祼弃权 " + itoa(abstain) + " 祼已投?" + itoa(voted) +
		" 人，朊?" + itoa(unvoted) + " 人?" + itoa(target) +
		" ?" + itoa(yes) + " 祼等待其他提名?倒?0秒后无人提名则进入游戏流程下?"
}

// addPubSystemMsg 向公聊追加系统消恹广播（提?投票统等）
func (r *Room) addPubSystemMsg(content string) {
	r.msgSeq++
	msg := GameMsg{ID: r.msgSeq, Number: 0, Content: content, System: true, At: time.Now()}
	r.pubMsgs = append(r.pubMsgs, msg)
	if !r.WatchMode && len(r.pubMsgs) > 500 {
		r.pubMsgs = r.pubMsgs[len(r.pubMsgs)-500:]
	}
	r.broadcast("game.message", msg)
}

// onVote 真人玩投票/取消投票（时内叏复切换意向）
func (r *Room) onVote(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil {
		reply(false, "对局朼", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	choice, _ := ev.Data["choice"].(string)
	myNum := r.game.Players[itoa(seat.Number)].Number
	result, _, err := r.eng.CastVote(myNum, choice)
	if err != nil {
		reply(false, err.Error(), nil)
		return
	}
	reply(true, "", result)
}

// onEndNomination 房主结束提名阶：统计??臊入（晚自动推进接管）
func (r *Room) onEndNomination(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" || r.game == nil {
		reply(false, "对局朼", nil)
		return
	}
	if ev.UserID != r.HostID {
		reply(false, "叜房主能结束提", nil)
		return
	}
	result := r.eng.EndNomination()
	r.broadcast("game.nominate_result", result)
	if msg, ok := result["message"].(string); ok {
		r.pubSystem("⚖️ " + msg)
	}
	if result["game_over"] == true {
		win, _ := result["winner"].(string)
		r.finishGame(win)
		reply(true, "", result)
		return
	}
	// 提名结束臊入：晚自动推进（首禁刀等则由引擎保证?
	phaseResult := r.eng.AdvancePhase()
	r.broadcast("game.phase", phaseResult)
	r.broadcast("game.state", r.gameSnapshot())
	reply(true, "", result)
}

func (r *Room) onMyInfo(ev Event, reply func(bool, string, map[string]any)) {
	if r.Status != "playing" {
		reply(false, "对局朼", nil)
		return
	}
	seat := r.seatOf(ev.UserID)
	if seat == nil {
		reply(false, "不在对局", nil)
		return
	}
	p := r.game.Players[itoa(seat.Number)]
	infos := r.stories[p.Number]
	payload := map[string]any{
		"infos":     infos,
		"my_role":   p.RoleName,
		"my_team":   p.Team,
		"my_number": p.Number,
	}
	// 兜底：附带挂起中的目标择（WS 事件丢失时前竻叼面板?
	if r.game.PendingChoice != nil {
		payload["pending_choice"] = r.game.PendingChoice
	}
	reply(true, "", payload)
}

func (r *Room) onEnd(ev Event, reply func(bool, string, map[string]any)) {
	if ev.UserID != r.HostID {
		reply(false, "叜房主能结束", nil)
		return
	}
	if r.Status != "playing" {
		reply(false, "对局朼", nil)
		return
	}
	r.finishGame("")
	reply(true, "", nil)
}

// finishGame 对局结束：归档日??复位座位 ?广播
func (r *Room) finishGame(winner string) {
	if r.game == nil {
		return
	}
	playersJSON, _ := json.Marshal(r.game.Players)
	msgsJSON, _ := json.Marshal(r.pubMsgs)
	// 归档说书人记录：过滤身份发放/技能说明/天亮播报——复盘只保留"获得了什么信息、用了什么技能"
	archStories := map[int][]string{}
	for num, list := range r.stories {
		for _, s := range list {
			if strings.Contains(s, "你的身份是") || strings.Contains(s, "你的角色") ||
				strings.Contains(s, "天亮了") || strings.HasPrefix(s, "📋") {
				continue
			}
			archStories[num] = append(archStories[num], s)
		}
	}
	storiesJSON, _ := json.Marshal(archStories)
	actionsJSON, _ := json.Marshal(r.game.ActionLog)
	privJSON, _ := json.Marshal(r.privMsgs)
	// 参与玩 uid 列表?1,2, 格式，战绩统?LIKE 查甼
	uidSet := map[uint]bool{}
	for _, p := range r.game.Players {
		if p.UserID != 0 {
			uidSet[p.UserID] = true
		}
	}
	uids := ","
	for uid := range uidSet {
		uids += itoa(int(uid)) + ","
	}
	r.mgr.DB.Create(&model.GameLog{
		RoomCode:    r.Code,
		HostID:      r.HostID,
		StartedAt:   r.startedAt,
		EndedAt:     time.Now(),
		Winner:      winner,
		Day:         r.game.Day + 1,
		UserIDs:     uids,
		Players:     string(playersJSON),
		Messages:    string(msgsJSON),
		Stories:     string(storiesJSON),
		Actions:     string(actionsJSON), // 夜晚选择方式日志：manual/timeout/ai
		PrivateMsgs: string(privJSON),    // 私聊日志（仅归档，不对玩家广撼
	})

	// 金币结算与每日任务进度：真人玩家按胜负发放（AI 座位 UserID=0 跳过）
	for _, p := range r.game.Players {
		if p.UserID == 0 {
			continue
		}
		won := (winner == "good" && (p.Team == "townsfolk" || p.Team == "outsider")) ||
			(winner == "evil" && (p.Team == "minion" || p.Team == "demon"))
		model.GrantGameReward(r.mgr.DB, p.UserID, won)
	}

	r.broadcast("game.over", map[string]any{"winner": winner})
	// 对局生命周期信号：结束时清空 AI 笔记，让 AI"停止扮演"
	r.mgr.AI.SessionReset(r.Code)

	// 复位为等待状?
	r.Status = "waiting"
	r.game = nil
	r.eng = nil
	r.pubMsgs = nil
	r.privMsgs = nil
	r.stories = make(map[int][]string)
	r.mgr.DB.Model(&model.Room{}).Where("room_code = ?", r.Code).
		Updates(map[string]any{"status": "waiting", "game_data": ""})
	for _, s := range r.seats {
		s.IsReady = s.IsAI
	}
	r.addSystemMsg("对局结束，返回等待状态")
	r.broadcast("room.ended", map[string]any{"winner": winner})
	r.broadcast("room.state", r.stateSnapshot())
}

// ==============================
// AI 臊发言（公聊阶段定时触?+ 真人发言触发?
// ==============================

// aiDebate 提名辩论期：提名者/被提名者的 AI 主动发言（辩护或指控）
// 每个提名轮次双方各发言一次，投票开始后不再触发
func (r *Room) aiDebate() {
	if r.Status != "playing" || r.game == nil || r.aiBusy {
		return
	}
	v := r.game.ActiveVote
	if v == nil || v.VoteDeadline != 0 {
		return
	}
	if r.debateSpoken == nil {
		r.debateSpoken = map[int]bool{}
	}
	// 轮流让双方发言（被提名者优先辩护）
	for _, num := range []int{v.TargetNumber, v.FromNumber} {
		if r.debateSpoken[num] {
			continue
		}
		p := r.game.Players[pidOfNum(r.game, num)]
		if p == nil || !p.Alive || !p.IsAI || p.AIType != "api" {
			r.debateSpoken[num] = true // 非 API AI 直接标记跳过
			continue
		}
		r.debateSpoken[num] = true
		r.aiBusy = true
		role := "被提名者"
		if num == v.FromNumber {
			role = "提名者"
		}
		go func(aiNum int, aiRole, aiTeam, aiConf, aiRoleName, dbRole string, clues []string) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[ai-debug] PANIC aiDebate: %v", rec)
				}
				r.Post(Event{Type: "ai_busy_done"})
			}()
			ctx := []map[string]any{}
			recent := r.pubMsgs
			if len(recent) > 10 {
				recent = recent[len(recent)-10:]
			}
			for _, m := range recent {
				ctx = append(ctx, map[string]any{"num": m.Number, "content": m.Content})
			}
			aliveNums := r.aliveList()
			reply, ok := r.mgr.AI.Speak(aiclient.SpeakReq{
				RoomCode:  r.Code,
				RoleName:  aiRole,
				Team:      aiTeam,
				Number:    aiNum,
				Phase:     "nomination",
				Day:       r.game.Day,
				AliveNums: aliveNums, TotalPlayers: len(r.game.Players),
				Context:   ctx,
				MyClues:   clues,
				LLMConf:   aiConf,
			})
			if !ok || reply == "" {
				return
			}
			r.Post(Event{Type: "ai_say", Data: map[string]any{"number": aiNum, "content": reply}})
		}(p.Number, p.RoleName, p.Team, p.LLMConf, p.RoleName, role, r.eng.MyStories(p.Number))
		return
	}
}

// aiAutoChat 让存活的大模?AI 玩以率发
// 上下?= 当前(day) + 阶 + 存活玩 + ?10 条公?// 性能关键：AI HTTP 调用在独?goroutine 三行（3 秒超时），完成后通过事件回投?// 绝不阻房间 Actor 事件徎（前同步调用在 AI 服务慢时会卡死整丈间）
func (r *Room) aiAutoChat() {
	if r.Status != "playing" || r.game == nil {
		return
	}
	if r.aiBusy {
		log.Printf("[ai-debug] room=%s phase=%s aiBusy=true 跳过", r.Code, r.game.Phase)
		return
	}
	ph := r.game.Phase
	log.Printf("[ai-debug] room=%s phase=%s", r.Code, ph)
	// 发言次数计数按阶段重置
	if r.speakCountsPhase != ph {
		r.speakCountsPhase = ph
		r.speakCounts = map[int]int{}
	}
	if ph == "private_chat" {
		// 私聊阶：AI 以率向随机存活玩发起私聊请（纜地随机，?HTTP?
		r.aiAutoPrivate()
		return
	}
	if ph == "nomination" {
		// 提名阶：AI 提名疑玩家并给理由（LLM 推理?
		r.aiNominate()
		// 辩论期：提名者/被提名者的 AI 主动辩论发言
		r.aiDebate()
		return
	}
	if ph != "public_chat" {
		return
	}
	// 选出发言?API AI 玩 + 必上下文（都在事件徎内安全完成）
	ctx := make([]map[string]any, 0, 10)
	recent := r.pubMsgs
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}
	for _, m := range recent {
		ctx = append(ctx, map[string]any{"num": m.Number, "content": m.Content})
	}
	aliveNums := []int{}
	for _, p := range r.game.Players {
		if p.Alive {
			aliveNums = append(aliveNums, p.Number)
		}
	}
	// 轮转制选择发言人：收集全部 AI（含死亡玩家——死人可以继续公聊发言，
	// 守鸦人等死后技能角色必须能把信息说出来；死亡 AI 说或沉默自行决定）
	aliveAIs := []*engine.PlayerState{}
	deadAIs := []*engine.PlayerState{}
	for _, p := range r.game.Players {
		if p.IsAI && p.AIType == "api" {
			if p.Alive {
				aliveAIs = append(aliveAIs, p)
			} else {
				deadAIs = append(deadAIs, p)
			}
		}
	}
	sort.Slice(aliveAIs, func(i, j int) bool { return aliveAIs[i].Number < aliveAIs[j].Number })
	sort.Slice(deadAIs, func(i, j int) bool { return deadAIs[i].Number < deadAIs[j].Number })
	allAIs := append(aliveAIs, deadAIs...)
	if len(allAIs) == 0 {
		return
	}
	// 轮转最多一圈找未达上限的 AI
	var speaker *engine.PlayerState
	for i := 0; i < len(allAIs); i++ {
		cand := allAIs[r.speakTurn%len(allAIs)]
		r.speakTurn++
		if r.speakCounts[cand.Number] < 3 {
			speaker = cand
			break
		}
	}
	if speaker == nil {
		log.Printf("[ai-debug] room=%s 公聊无AI被选中", r.Code)
		return
	}
	log.Printf("[ai-debug] room=%s 选中发言人 #%d 发起Speak", r.Code, speaker.Number)
	// 异调用 AI：结果过 ai_say 事件回投房间徎（避免数捫争与事件徎阻?
	r.aiBusy = true
	go func(num int, roleName string, team string, day int, llmConf string, clues []string, ctxCopy []map[string]any) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[ai-debug] PANIC aiAutoChat: %v", rec)
			}
			r.Post(Event{Type: "ai_busy_done"})
		}()
		reply, ok := r.mgr.AI.Speak(aiclient.SpeakReq{
			RoomCode:  r.Code,
			RoleName:  roleName,
			Team:      team,
			Number:    num,
			Phase:     "public_chat",
			Day:       day,
			AliveNums: aliveNums, TotalPlayers: len(r.game.Players),
			Context:   ctxCopy,
			MyClues:   clues,
			LLMConf:   llmConf,
		})
		log.Printf("[ai-debug] Speak返回 #%d ok=%v replylen=%d", num, ok, len(reply))
		if !ok || reply == "" {
			return
		}
		r.Post(Event{Type: "ai_say", Data: map[string]any{"number": num, "content": reply}})
	}(speaker.Number, speaker.RoleName, speaker.Team, r.game.Day, speaker.LLMConf, r.eng.MyStories(speaker.Number), ctx)
}

// aiNominate 提名阶：AI 玩以率提名其疑的玩并发表理由（LLM 推理?
func (r *Room) aiNominate() {
	if r.Status != "playing" || r.game == nil || r.aiBusy || r.game.Phase != "nomination" {
		return
	}
	if r.game.ActiveVote != nil {
		return
	}
	var nominator *engine.PlayerState
	for _, p := range r.game.Players {
		if !p.Alive || !p.IsAI || p.AIType != "api" {
			continue
		}
		if rand.Float64() > 0.5 {
			continue
		}
		nominator = p
		break
	}
	if nominator == nil {
		return
	}
	aliveNums := []int{}
	for _, p := range r.game.Players {
		if p.Alive && p.Number != nominator.Number {
			aliveNums = append(aliveNums, p.Number)
		}
	}
	if len(aliveNums) == 0 {
		return
	}
	// AI 猎手：优先做开枪决策（不消耗提名冷却；射偏技能作废并暴露身份）
	if nominator.Role == "slayer" && !nominator.SlayerUsed {
		r.aiBusy = true
		go func(num int, llmConf string, clues []string, alive []int) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[ai-debug] PANIC aiSlayerShoot: %v", rec)
				}
				r.Post(Event{Type: "ai_busy_done"})
			}()
			target, ok := r.mgr.AI.DecideWithCluesRoom(r.Code, "猎手",
				"猎手开枪决策：你是猎手，开枪射中恶魔则恶魔立即死亡、善良获胜；射偏则技能作废并暴露你是猎手。"+
					"基于你的怀疑选一个开枪目标编号；如果还没把握就输出 0 放弃开枪。",
				alive, num, llmConf, clues)
			if !ok || target == 0 {
				return
			}
			hit, _, ok := r.eng.SlayerShoot(num, target)
			if !ok {
				return
			}
			if hit {
				r.pubSystem("🔫 #" + itoa(num) + " 声称猎手对 #" + itoa(target) + " 开枪。他就是恶魔，当场死亡！")
				r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：猎手 #"+itoa(num)+" 声称猎手对 #"+itoa(target)+"，恶魔死亡，游戏结束")
				if win := r.eng.CheckWin(); win != "" {
					r.finishGame(win)
					return
				}
			} else {
				r.pubSystem("🔫 #" + itoa(num) + " 声称猎手对 #" + itoa(target) + " 开枪。无事发生")
				r.mgr.AI.SessionEvent(r.Code, "第"+itoa(r.game.Day+1)+"天：#"+itoa(num)+" 声称自己是猎手并开枪射 #"+itoa(target)+"……无事发生，对方没死")
			}
			r.broadcast("game.state", r.gameSnapshot())
		}(nominator.Number, nominator.LLMConf, r.eng.MyStories(nominator.Number), aliveNums)
		return
	}
	r.aiBusy = true
	go func(num int, roleName string, team string, llmConf string, clues []string) {
		defer func() {
			r.Post(Event{Type: "ai_busy_done"})
		}()
		// 今天已被提名过的玩家：不能再提名，从候选里剔除并告知 AI
		excluded := []int{}
		for _, rec := range r.game.Nominations {
			excluded = append(excluded, rec.TargetNumber)
		}
		cands := []int{}
		for _, n := range aliveNums {
			skip := false
			for _, x := range excluded {
				if x == n {
					skip = true
					break
				}
			}
			if !skip {
				cands = append(cands, n)
			}
		}
		if len(cands) == 0 {
			return
		}
		prompt := "白天提名决策：提名的动机可以是处决嫌疑目标、逼沉默玩家表态、自证清白开视野。选择你最有理由提名的人（不一定要投死对方，投票由全体玩家决定）"
		if len(excluded) > 0 {
			prompt += "。今天已被提名过、不能再提名的玩家：" + joinInts(excluded)
		}
		// 选怀疑目标（LLM 基于说书人线索+角色立场推理）
		target, ok := r.mgr.AI.DecideWithCluesRoom(r.Code, roleName,
			prompt, cands, num, llmConf, clues)
		if !ok {
			return
		}
		// ?生成提名理由
		reason, _ := r.mgr.AI.Speak(aiclient.SpeakReq{
			RoomCode:  r.Code,
			RoleName:  roleName,
			Team:      team,
			Number:    num,
			Phase:     "nomination",
			Day:       r.game.Day,
			AliveNums: aliveNums, TotalPlayers: len(r.game.Players),
			Context:   []map[string]any{{"num": 0, "content": "你决定提名 #" + itoa(target) + "，请给出你的怀疑理由"}},
			MyClues:   clues,
			LLMConf:   llmConf,
		})
		r.Post(Event{Type: "ai_nominate", Data: map[string]any{"number": num, "target": target, "reason": reason}})
	}(nominator.Number, nominator.RoleName, nominator.Team, nominator.LLMConf, r.eng.MyStories(nominator.Number))
}

// aiAutoPrivate 私聊阶：AI 玩以率向随机存活玩发起私聊?
func (r *Room) aiAutoPrivate() {
	r.expireSessions()
	alive := []*engine.PlayerState{}
	for _, p := range r.game.Players {
		if p.Alive {
			alive = append(alive, p)
		}
	}
	for _, p := range r.game.Players {
		if !p.IsAI || p.AIType != "api" {
			continue
		}
		// 死亡 AI 低概率发起私聊（死人可以私聊，但主动性降低）
		if !p.Alive && rand.Float64() > 0.15 {
			continue
		}
		// 单私聊限制：AI 已有活跃会话时不再发起新邀请（一次只和一人私聊，聊完退出才能找下一位）
		isEvil := p.Team == engine.TeamDemon || p.Team == engine.TeamMinion
		if len(r.privateSessions[p.Number]) > 0 {
			continue
		}
		if isEvil {
			// 邪恶方：每 tick 必找队友（提高概率，保证战术交流发生）
			if rand.Float64() > 0.7 {
				continue
			}
		} else {
			if rand.Float64() > 0.3 {
				continue
			}
		}
		if len(alive) < 2 {
			return
		}
		var target *engine.PlayerState
		// 邪恶方 AI 优先私聊已知队友（恶魔→爪牙、爪牙→恶魔），保证战术沟通及时
		if isEvil {
			mates := []int{}
			if p.Team == engine.TeamDemon {
				mates = p.InfoMinionNums
			} else if p.InfoDemonNum != 0 {
				mates = []int{p.InfoDemonNum}
			}
			for _, mnum := range mates {
				if t := r.game.Players[pidOfNum(r.game, mnum)]; t != nil && t.Alive &&
					t.Number != p.Number && !r.hasSession(p.Number, t.Number) && !r.hasTalked(p.Number, t.Number) {
					target = t
					break
				}
			}
		}
		// 无队友可找（或好人 AI）：随机其他玩家（存活优先，死人也可私聊）
		if target == nil {
			for i := 0; i < 5; i++ {
				t := alive[rand.Intn(len(alive))]
				if t.Number != p.Number && !r.hasSession(p.Number, t.Number) && !r.hasTalked(p.Number, t.Number) {
					target = t
					break
				}
			}
		}
		if target == nil {
			continue
		}
		if _, exists := r.privateInvites[target.Number]; exists {
			continue
		}
		r.privateInvites[target.Number] = p.Number
		if target.IsAI {
			// AI 盠：自动接?
			r.establishSession(p.Number, target.Number)
			delete(r.privateInvites, target.Number)
			// 发起邀请的 AI 主动开口（开场白），否则双方会话建立后全程沉默
			r.aiPrivateFirstLine(p, target)
		} else {
			// 真人盠：推送邀?
			r.mgr.Hub.SendToUser(target.UserID, "game.private_invite", map[string]any{"from_number": p.Number})
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// aliveList 当前存活玩家编号列表（AI 上下文用）
func (r *Room) aliveList() []int {
	aliveNums := []int{}
	if r.game == nil {
		return aliveNums
	}
	for _, p := range r.game.Players {
		if p.Alive {
			aliveNums = append(aliveNums, p.Number)
		}
	}
	return aliveNums
}

// aiPrivateContent AI 私聊内（大模型生成，失败时朜模板降级?
func (r *Room) aiPrivateContent(from, to *engine.PlayerState) string {
	reply, ok := r.mgr.AI.Speak(aiclient.SpeakReq{
		RoleName:  from.RoleName,
		Number:    from.Number,
		Phase:     "private_chat",
		Day:       r.game.Day,
		AliveNums: r.aliveList(), TotalPlayers: len(r.game.Players),
		OtherNum:  to.Number,
	})
	if !ok || reply == "" {
		lines := []string{
			"我得我仏以合作，你么看？",
			"你昨晚得到什么信恺吗？",
			"小心躾的人，我觉得有人相你了",
			"关于躻，我下交捸下情报？",
		}
		reply = lines[rand.Intn(len(lines))]
	}
	return reply
}

// ==============================
// 必序列?
// ==============================

func (r *Room) stateSnapshot() map[string]any {
	players := make([]map[string]any, 0, len(r.seats))
	seatNums := make([]int, 0, len(r.seats))
	for n := range r.seats {
		seatNums = append(seatNums, n)
	}
	sort.Ints(seatNums)
	allReady := r.allReady()
	for _, n := range seatNums {
		s := r.seats[n]
		aimodel := ""
		if s.IsAI && s.AIType == "api" {
			aimodel = ModelFromLLMConfig(s.LLMConfig)
		}
		players = append(players, map[string]any{
			"seat_number": s.Number,
			"user_id":     s.UserID,
			"username":    s.Username,
			"is_ai":       s.IsAI,
			"ai_type":     s.AIType,
			"ai_model":    aimodel,
			"is_ready":    s.IsReady,
			"is_online":   s.IsOnline,
		})
	}
	msgs := r.messages
	if len(msgs) > 100 {
		msgs = msgs[len(msgs)-100:]
	}
	return map[string]any{
		"room": map[string]any{
			"room_code":    r.Code,
			"host_id":      r.HostID,
			"status":       r.Status,
			"max_players":  r.MaxPlayers,
			"player_count": len(r.seats),
			"all_ready":    allReady,
			"players":      players,
		},
		"messages": msgs,
	}
}

func (r *Room) gameSnapshot() map[string]any {
	if r.game == nil {
		return map[string]any{}
	}
	// 视图剜：晚阶段今晚死亡的玩家隐藏亡状态（黎明典?
	playersView := r.game.Players
	if r.game.Phase == "night" && len(r.game.NightDeaths) > 0 {
		playersView = map[string]*engine.PlayerState{}
		for pid, p := range r.game.Players {
			if r.game.NightDeaths[p.Number] {
				cp := *p
				cp.Alive = true
				playersView[pid] = &cp
			} else {
				playersView[pid] = p
			}
		}
	}
	// 非观战房间：脱敏下发——真人玩家绝不能从快照里看到其他玩家的角色/阵营/私密信息
	if !r.WatchMode {
		safe := map[string]*engine.PlayerState{}
		for pid, p := range playersView {
			cp := *p
			cp.Role = ""
			cp.Team = ""
			cp.RoleName = ""
			cp.FakeRole = ""
			cp.InfoMinionNums = nil
			cp.InfoDemonNum = 0
			cp.InfoNotInPlay = nil
			cp.InfoClues = nil
			cp.FlagGood = false
			cp.FlagEvil = false
			safe[pid] = &cp
		}
		playersView = safe
	}
	return map[string]any{
		"game": map[string]any{
			"phase":          r.game.Phase,
			"day":            r.game.Day,
			"players":        playersView,
			"pending_choice": r.game.PendingChoice, // 挂起状随必下发（重连恢?防面板诅?
			"active_vote":    r.game.ActiveVote,
			"nominations":    r.game.Nominations,
		},
		"messages": r.pubMsgs,
	}
}
