package engine

import (
	"errors"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// StoryMsg 说书人消息（仅玩家自己可见）
type StoryMsg struct {
	PlayerNumber int    `json:"player_number"`
	Content      string `json:"content"`
}

// NightActionTimeout 真人夜晚行动等待超时（秒）
const NightActionTimeout = 45

// 选择方式标记（归档到对局日志 GameLog.Actions）
const (
	ChoiceManual  = "manual"  // 玩家手动选择
	ChoiceTimeout = "timeout" // 超时系统代选
	ChoiceAI      = "ai"      // AI 自动选择
)

// NightPendingChoice 夜间挂起的目标选择（通用框架）
// 当前落地：占卜师/管家；预留复用：僧侣/投毒者/小恶魔
type NightPendingChoice struct {
	PlayerID     string `json:"player_id"`     // 玩家 pid
	Number       int    `json:"number"`        // 匿名编号
	RoleKey      string `json:"role"`          // 角色 key
	RoleName     string `json:"role_name"`     // 角色中文名（重连恢复面板标题用）
	ValidTargets []int  `json:"valid_targets"` // 合法目标编号
	MaxTargets   int    `json:"max_targets"`   // 需要选择的目标数
	StepIdx      int    `json:"step_idx"`
	Total        int    `json:"total"`
	Deadline     int64  `json:"deadline"` // unix 秒
	TipID        string `json:"tip_id"`   // 挂起任务唯一标识（幂等去重用）
	TipSent      bool   `json:"-"`        // 提示卡片是否已下发（防止重复广播）
}

// PlayerState 单个玩家对局状态
type PlayerState struct {
	UserID   uint   `json:"user_id"`
	Username string `json:"username"`
	Number   int    `json:"number"`
	Seat     int    `json:"seat"`
	IsAI     bool   `json:"is_ai"`
	AIType   string `json:"ai_type"`
	AIModel  string `json:"ai_model"` // api 类型 AI 调用的大模型名（对局中对外显示）
	ChatFrame string `json:"chat_frame,omitempty"` // 聊天框样式（真人玩家）
	LLMConf  string `json:"-"` // 大模型配置 JSON（api_key/base_url/model，仅 api 类型 AI）
	Alive    bool   `json:"alive"`

	Role     string `json:"role"`
	Team     string `json:"team"`
	RoleName string `json:"role_name"`

	// 中毒状态仅服务端内部保存：任何快照/事件序列化都不外发（json:"-"），玩家无感知
	Poisoned  bool `json:"-"`
	Drunk     bool `json:"drunk"`
	Protected bool `json:"protected"`

	SlayerUsed      bool   `json:"slayer_used"`
	VirginTriggered bool   `json:"virgin_triggered"`
	RavenkeeperUsed bool   `json:"ravenkeeper_used"`
	MonkLast        string `json:"monk_last"`
	ButlerMaster    string `json:"-"` // 管家主人 pid：主人未投票前管家不能投票；主人死亡后限制解除

	FlagTargeted bool `json:"flag_targeted"`
	FlagGood     bool `json:"flag_good"`
	FlagEvil     bool `json:"flag_evil"`

	FakeRole string `json:"fake_role"` // 酒鬼伪装成的镇民角色 key

	DeadVote bool `json:"dead_vote"` // 死亡票：死亡时获得，可投最后一次票，使用后没收

	InfoMinionNums []int    `json:"info_minion_nums"` // 恶魔得知爪牙编号
	InfoDemonNum   int      `json:"info_demon_num"`   // 爪牙得知恶魔编号
	InfoNotInPlay  []string `json:"info_not_in_play"` // 恶魔得知的不在场镇民（伪装用）
	InfoClues      []string `json:"info_clues"`       // 信息位角色的线索文本
}

// GameData 对局运行时状态（整体 JSON 归档到对局日志）
type GameData struct {
	Phase      string                  `json:"phase"`
	Day        int                     `json:"day"`
	FirstNight bool                    `json:"first_night"`
	NightIdx   int                     `json:"night_idx"`
	Players    map[string]*PlayerState `json:"players"`

	StorytellerMsgs []StoryMsg `json:"-"`
	StoryHistory    []StoryMsg `json:"story_history,omitempty"` // 永久历史（AI 推理用，drain 后仍可读）

	DiedToday bool `json:"died_today"`
	Executed  bool `json:"executed"`

	// 今晚死亡的玩家（黎明前对外隐藏死亡状态，天亮统一公布）
	NightDeaths map[int]bool `json:"night_deaths"`

	LastExecutedPID  string `json:"last_executed_pid"`
	LastExecutedRole string `json:"last_executed_role"`

	// 提名冷却窗口截止时间（unix 秒）：未处决的提名结束后 30 秒内禁止新提名
	NomCooldownUntil int64 `json:"nom_cooldown_until"`

	// 均衡配板：斗蛐蛐观战模式预设角色组合（5 组专业配板轮转），为空则随机发牌
	ForceRoles []string `json:"-"`


	// 占卜师的红鲱鱼：一名非恶魔玩家始终被占卜师当作恶魔
	RedHerringPID string `json:"red_herring_pid"`

	// 等待真人操作的挂起夜晚行动（占卜师选择目标）
	PendingChoice *NightPendingChoice `json:"pending_choice"`

	// 提名投票状态机
	Nominations []*NominationRecord `json:"nominations"` // 本轮全部提名记录
	ActiveVote  *VoteState          `json:"active_vote"` // 当前投票中的提名

	// 夜晚选择方式日志（manual/timeout/ai），对局结束归档到 GameLog.Actions
	ActionLog []map[string]any `json:"action_log"`

	LastNomination map[string]any `json:"last_nomination"`
}

// NominationRecord 一条提名记录（含投票结果）
type NominationRecord struct {
	FromNumber   int `json:"from"`
	TargetNumber int `json:"target"`
	Yes          int `json:"yes"`
	No           int `json:"no"`
	Abstain      int `json:"abstain"`
}

// VoteState 当前投票中的提名（辩论期 → 同时投票倒计时，可反复切换投票/取消）
type VoteState struct {
	FromNumber     int            `json:"from"`
	TargetNumber   int            `json:"target"`
	DebateDeadline int64          `json:"debate_deadline"` // 辩论截止（unix 秒）
	VoteDeadline   int64          `json:"vote_deadline"`   // 投票截止（unix 秒，0=尚未开始投票）
	Votes          map[int]string `json:"votes"`           // 玩家编号 → yes（取消投票则移除）
	Stated         map[int]bool   `json:"-"`               // 已表态玩家（投过票或弃权），管家主人限制用
}

// VoteSeconds 投票倒计时时长（期间可反复切换投票/取消）
const VoteSeconds = 20

// DebateSeconds 提名后辩论时长
const DebateSeconds = 20

// NomCooldownSeconds 提名结束后的提名冷却时长（未处决时）
const NomCooldownSeconds = 60

// MyStories 该玩家的说书人私密消息（角色查验结果/首夜线索等，注入 AI 推理用）
// 从永久历史读取：静态关键信息（身份/技能/编号/首夜线索）全部保留，动态消息取最近 3 条
func (e *Engine) MyStories(num int) []string {
	static := []string{}
	dynamic := []string{}
	for _, m := range e.GD.StoryHistory {
		if m.PlayerNumber != num {
			continue
		}
		c := m.Content
		if strings.HasPrefix(c, "🎭") || strings.HasPrefix(c, "📋") ||
			strings.Contains(c, "恶魔编号") || strings.Contains(c, "爪牙编号") ||
			strings.Contains(c, "不在场") || strings.HasPrefix(c, "🔮") ||
			strings.HasPrefix(c, "🔍") || strings.HasPrefix(c, "🧺") ||
			strings.HasPrefix(c, "📚") || strings.HasPrefix(c, "👨‍🍳") ||
			strings.HasPrefix(c, "🕶️") || strings.HasPrefix(c, "💉") ||
			strings.HasPrefix(c, "🛡️") || strings.HasPrefix(c, "🗡️") {
			static = append(static, c)
		} else {
			dynamic = append(dynamic, c)
		}
	}
	out := append(append([]string{}, static...), dynamic...)
	if len(out) > 12 {
		out = append(append([]string{}, static...), dynamic[len(dynamic)-min(3, len(dynamic)):]...)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// AIDecider AI 夜晚行动决策接口（由 aiclient 实现，nil 时随机回退）
type AIDecider interface {
	Decide(roleName, ability string, aliveNums []int, selfNum int) (int, bool)
	DecideWithConf(roleName, ability string, aliveNums []int, selfNum int, llmConf string) (int, bool)
	DecideWithClues(roleName, ability string, aliveNums []int, selfNum int, llmConf string, clues []string) (int, bool)
	DecideWithCluesRoom(roomCode, roleName, ability string, aliveNums []int, selfNum int, llmConf string, clues []string) (int, bool)
	DecideVote(roomCode, roleName string, fromNum, targetNum int, aliveNums []int, selfNum int, llmConf string, clues []string) (bool, bool)
}

// Engine 游戏引擎
type Engine struct {
	GD       *GameData
	RoomCode string
	Emit     func(event string, data any)
	AI       AIDecider
	// HumanChoice 真人夜晚交互选择开关（默认 false：夜晚全自动推进）
	// true：真人拿到主动选择型角色时挂起等待手动选择
	HumanChoice bool
}

// NewEngine 创建引擎
func NewEngine(gd *GameData, roomCode string, emit func(event string, data any)) *Engine {
	if gd.Players == nil {
		gd.Players = map[string]*PlayerState{}
	}
	return &Engine{GD: gd, RoomCode: roomCode, Emit: emit}
}

// PlayerByNum 按匿名编号查找玩家
func (e *Engine) PlayerByNum(num int) *PlayerState {
	for _, p := range e.GD.Players {
		if p.Number == num {
			return p
		}
	}
	return nil
}

// Alive 返回存活的玩家
func (e *Engine) Alive() map[string]*PlayerState {
	out := map[string]*PlayerState{}
	for pid, p := range e.GD.Players {
		if p.Alive {
			out[pid] = p
		}
	}
	return out
}

// Notify 记录说书人消息（仅该玩家可见）；同时写入永久历史（drainStories 清空待发队列后 AI 仍能读取）
func (e *Engine) Notify(num int, msg string) {
	sm := StoryMsg{PlayerNumber: num, Content: msg}
	e.GD.StorytellerMsgs = append(e.GD.StorytellerMsgs, sm)
	e.GD.StoryHistory = append(e.GD.StoryHistory, sm)
}

// NotifyAll 向所有玩家记录消息
func (e *Engine) NotifyAll(msg string) {
	for _, p := range e.GD.Players {
		e.Notify(p.Number, msg)
	}
}

// Sync 广播游戏状态更新
func (e *Engine) Sync() {
	if e.Emit != nil {
		e.Emit("game.state_update", map[string]any{
			"phase": e.GD.Phase, "day": e.GD.Day, "players": e.GD.Players,
		})
	}
}

// AliveNums 存活玩家编号列表（排除自己）
func (e *Engine) AliveNums(excludeNum int) []int {
	out := []int{}
	for _, p := range e.GD.Players {
		if p.Alive && p.Number != excludeNum {
			out = append(out, p.Number)
		}
	}
	return out
}

// pickTarget 随机选择一个存活目标（可选排除自己），返回 pid
func (e *Engine) pickTarget(selfPID string, excludeSelf bool) string {
	pool := []string{}
	for pid, p := range e.GD.Players {
		if p.Alive {
			if excludeSelf && pid == selfPID {
				continue
			}
			pool = append(pool, pid)
		}
	}
	if len(pool) == 0 {
		return ""
	}
	return pool[rand.Intn(len(pool))]
}

// pickTargetByAI AI 决策选择目标（注入说书人线索，带随机回退）
func (e *Engine) pickTargetByAI(selfPID, roleKey string, excludeSelf bool) string {
	self := e.GD.Players[selfPID]
	aliveNums := e.AliveNums(self.Number)
	if self.AIType == "api" && e.AI != nil && len(aliveNums) > 0 {
		ri := Roles[roleKey]
		if choice, ok := e.AI.DecideWithCluesRoom(e.RoomCode, self.RoleName, ri.Ability, aliveNums, self.Number, self.LLMConf, e.MyStories(self.Number)); ok {
			if p := e.PlayerByNum(choice); p != nil && p.Alive {
				return pidOf(e.GD, p.Number)
			}
		}
	}
	return e.pickTarget(selfPID, excludeSelf)
}

func pidOf(gd *GameData, num int) string {
	for pid, p := range gd.Players {
		if p.Number == num {
			return pid
		}
	}
	return ""
}

// ==============================
// 公开接口
// ==============================

// StartGame 开局：分配角色 + 首夜信息
func (e *Engine) StartGame() {
	e.AssignRoles()
	e.GD.Phase = "night"
	e.GD.Day = 0
	e.GD.FirstNight = true
	e.GD.NightIdx = 0
	// 重置投票/提名状态（防止上一局残留导致提前弹投票面板/死锁）
	e.GD.ActiveVote = nil
	e.GD.Nominations = nil
	e.GD.NomCooldownUntil = 0
	e.GD.StorytellerMsgs = nil
	e.GD.StoryHistory = nil
	e.GenFirstNightInfo()
}

// AdvancePhase 推进阶段状态机
// 相位顺序：夜晚 → 天亮(day) → 私聊(private_chat) → 公聊(public_chat) → 提名(nomination) → 夜晚
func (e *Engine) AdvancePhase() map[string]any {
	switch e.GD.Phase {
	case "waiting":
		e.StartGame()
		return map[string]any{"phase": "night", "data": e.GD}
	case "night":
		return e.StepNight()
	case "day":
		e.GD.Phase = "private_chat"
		return map[string]any{"phase": "private_chat"}
	case "private_chat":
		e.GD.Phase = "public_chat"
		return map[string]any{"phase": "public_chat"}
	case "public_chat":
		e.GD.Phase = "nomination"
		return map[string]any{"phase": "nomination"}
	case "nomination":
		e.GD.Phase = "night"
		e.GD.Day++
		e.GD.DiedToday = false
		e.GD.NightIdx = 0
		e.CleanTurn()
		return map[string]any{"phase": "night"}
	}
	return map[string]any{"phase": e.GD.Phase}
}

// StepNight 夜晚逐步执行
func (e *Engine) StepNight() map[string]any {
	order := NightOrderOther
	if e.GD.FirstNight {
		order = NightOrderFirst
	}
	idx := e.GD.NightIdx

	if idx >= len(order) {
		if e.GD.FirstNight {
			e.GD.FirstNight = false
		}
		// 收集昨晚死亡名单（ApplyDeaths 会清空 NightDeaths，先记录）
		deadNums := []int{}
		for n := range e.GD.NightDeaths {
			deadNums = append(deadNums, n)
		}
		sort.Ints(deadNums)
		e.ApplyDeaths()
		e.GD.NightIdx = 0
		e.GD.Phase = "day"
		e.NotifyAll("🌅 天亮了")
		// 天亮播报：昨晚死亡名单 + 场上存活数量
		aliveNums := []int{}
		for _, p := range e.GD.Players {
			if p.Alive {
				aliveNums = append(aliveNums, p.Number)
			}
		}
		sort.Ints(aliveNums)
		if len(deadNums) > 0 {
			e.NotifyAll("第" + itoa(e.GD.Day+1) + "天天亮：昨晚死亡 " + joinNums(deadNums) + "。场上存活 " + itoa(len(aliveNums)) + " 人：" + joinNums(aliveNums))
		} else {
			e.NotifyAll("第" + itoa(e.GD.Day+1) + "天天亮：昨晚无人死亡。场上存活 " + itoa(len(aliveNums)) + " 人：" + joinNums(aliveNums))
		}
		return map[string]any{"phase": "day", "dawn_dead": deadNums, "dawn_alive": aliveNums}
	}

	roleKey := order[idx]
	ri := Roles[roleKey]
	msgs := []map[string]any{}

	for pid, p := range e.GD.Players {
		// 死亡玩家跳过；但守鸦人例外——死亡当晚（NightDeaths 已标记）或死亡后仍会被唤醒主动查验
		if !p.Alive && p.Role != "ravenkeeper" {
			continue
		}
		if p.Role != roleKey {
			continue
		}
		isFirst := e.GD.FirstNight
		if isFirst && !ri.FirstNight {
			continue
		}
		if !isFirst && !ri.OtherNights {
			continue
		}
		// 中毒：角色照常行动（挂起选择/提交/超时完整走完），仅技能产出错误或效果失效；不通知、不拦截

		// —— 真人主动选择型角色：交互开关开启时挂起等待手动选择（45 秒超时兜底）——
		// 守鸦人只有死亡后才会挂起查验（活着时无需选择）
		if e.HumanChoice && ri.NeedsChoice && !p.IsAI && (roleKey != "ravenkeeper" || !p.Alive) {
			e.GD.PendingChoice = &NightPendingChoice{
				PlayerID:     pid,
				Number:       p.Number,
				RoleKey:      roleKey,
				RoleName:     ri.Name,
				ValidTargets: e.ChoiceTargets(pid, roleKey),
				MaxTargets:   e.ChoiceCount(roleKey),
				StepIdx:      idx,
				Total:        len(order),
				Deadline:     time.Now().Add(NightActionTimeout).Unix(),
				// 幂等唯一标识：天-步骤-角色-玩家（同一挂起任务全链路只下发一次提示）
				TipID: itoa(e.GD.Day) + "-" + itoa(idx) + "-" + roleKey + "-" + itoa(p.Number),
			}
			return map[string]any{
				"phase": "night", "waiting": true,
				"player_number": p.Number,
				"role":          roleKey,
				"deadline":      e.GD.PendingChoice.Deadline,
				"valid_targets": e.GD.PendingChoice.ValidTargets,
				"max_targets":   e.ChoiceCount(roleKey),
				"step":          idx + 1, "total": len(order),
				"role_name": ri.Name, "skill": ri.Ability,
			}
		}

		action := e.DoAction(pid, roleKey, isFirst)
		msg := ""
		if len(action) > 0 {
			msg = e.GenStorytellerMsg(pid, roleKey, action)
			msgs = append(msgs, map[string]any{"num": p.Number, "role": ri.Name, "msg": msg})
			e.Notify(p.Number, msg)
		}
		// AI 主动选择型角色的自动选择记录（通用框架：ChoiceAI）
		if ri.NeedsChoice && p.IsAI {
			e.GD.ActionLog = append(e.GD.ActionLog, map[string]any{
				"number": p.Number, "role": roleKey, "choice": ChoiceAI,
				"targets": actionTargets(action), "day": e.GD.Day,
			})
		}
	}

	e.GD.NightIdx = idx + 1
	return map[string]any{
		"phase": "night", "step": idx + 1, "total": len(order),
		"role_name": ri.Name, "role_key": roleKey, "msgs": msgs,
		"skill": ri.Ability,
	}
}

// GenStorytellerMsg 生成说书人消息文本
func (e *Engine) GenStorytellerMsg(pid, roleKey string, action map[string]any) string {
	ri := Roles[roleKey]
	switch roleKey {
	case "poisoner":
		if t, ok := action["target"].(int); ok {
			return "🧪 你毒了 #" + itoa(t)
		}
	case "monk":
		if t, ok := action["target"].(int); ok {
			return "🧘 你保护了 #" + itoa(t)
		}
	case "imp":
		if t, ok := action["target"].(int); ok {
			return "😈 你杀死了 #" + itoa(t)
		}
	case "fortune_teller":
		if ts, ok := intSlice(action["targets"]); ok {
			if found, _ := action["found"].(bool); found {
				return "🔮 你占卜的 " + joinNums(ts) + " 中有恶魔！"
			}
			return "🔮 你占卜的 " + joinNums(ts) + " 中没有恶魔"
		}
	case "empath":
		if n, ok := action["evil"].(int); ok {
			// 带具体邻居编号：弱模型自己算邻居经常算错，导致共情者乱报"#1和#5有邪恶"
			p := e.GD.Players[pid]
			l, r := e.Neighbors(p.Number)
			return "💗 你相邻的存活玩家（#"+itoa(l)+" 与 #"+itoa(r)+"）中有 " + itoa(n) + " 人是邪恶的"
		}
	case "undertaker":
		if info, ok := action["info"].(string); ok {
			return info
		}
		return "⚰️ 昨天没有人被处决"
	case "ravenkeeper":
		if t, ok := action["target"].(int); ok {
			if r, ok := action["role"].(string); ok {
				return "🐦‍⬛ 你得知 #" + itoa(t) + " 是【" + r + "】"
			}
		}
	case "butler":
		if t, ok := action["target"].(int); ok {
			return "🫖 你的主人是 #" + itoa(t)
		}
	case "spy":
		return "🕶️ 你查看了魔典"
	}
	return ri.Name + " 行动完成"
}

// ==============================
// 夜晚行动
// ==============================

// DoAction 执行角色夜晚行动（AI/随机）
func (e *Engine) DoAction(pid, roleKey string, isFirst bool) map[string]any {
	p := e.GD.Players[pid]
	alive := e.Alive()
	delete(alive, pid)

	switch roleKey {
	case "poisoner":
		if t := e.pickTargetByAI(pid, roleKey, true); t != "" {
			// 投毒者中毒：毒不生效；恶魔免疫投毒（官方规则）
			if !p.Poisoned && e.GD.Players[t].Role != "imp" {
				e.GD.Players[t].Poisoned = true
			}
			e.GD.Players[t].FlagTargeted = true
			return map[string]any{"target": e.GD.Players[t].Number}
		}
	case "monk":
		valid := []string{}
		for k := range alive {
			if k != p.MonkLast {
				valid = append(valid, k)
			}
		}
		if len(valid) > 0 {
			t := valid[rand.Intn(len(valid))]
			p.MonkLast = t
			// 中毒僧侣：流程完整跑完，但保护效果不写入对局状态（目标仍可被杀死）
			if !p.Poisoned {
				e.GD.Players[t].Protected = true
			}
			e.GD.Players[t].FlagTargeted = true
			return map[string]any{"target": e.GD.Players[t].Number}
		}
	case "imp":
		// 首夜禁刀：恶魔首夜绝对不能杀人（官方规则），第二夜起正常行动
		if isFirst {
			return map[string]any{}
		}
		// 小概率自杀传位：一名爪牙成为新的小恶魔
		aliveMinions := []string{}
		for k := range alive {
			if e.GD.Players[k].Team == TeamMinion {
				aliveMinions = append(aliveMinions, k)
			}
		}
		if len(aliveMinions) > 0 && rand.Float64() < 0.05 {
			p.Alive = false
			p.DeadVote = true // 死亡票
			e.Notify(p.Number, "☠️ 你在夜晚死亡。希望你的牺牲能为邪恶方带来胜利。")
			e.markNightDeath(p.Number)
			e.becomeImp(e.GD.Players[aliveMinions[rand.Intn(len(aliveMinions))]])
			return map[string]any{"target": p.Number, "suicide": true}
		}
		// 恶魔可以选中士兵但杀不死（官方规则）
		valid := []string{}
		for k := range alive {
			valid = append(valid, k)
		}
		if len(valid) > 0 {
			t := valid[rand.Intn(len(valid))]
			if e.GD.Players[t].Role == "soldier" {
				return map[string]any{"target": e.GD.Players[t].Number, "blocked": true}
			}
			// 镇长弹刀（官方魔典）：恶魔攻击镇长时，说书人可让另一名玩家代替死亡
			if e.GD.Players[t].Role == "mayor" && e.mayorBounce(t) {
				return map[string]any{"target": e.GD.Players[t].Number, "bounced": true}
			}
			e.GD.Players[t].Alive = false
			e.Notify(e.GD.Players[t].Number, "☠️ 你在夜晚死亡")
			e.markNightDeath(e.GD.Players[t].Number)
			return map[string]any{"target": e.GD.Players[t].Number}
		}
	case "fortune_teller":
		// AI/兜底路径：随机选择两名玩家
		return e.doFortuneTeller(pid, nil)
	case "empath":
		l, r := e.Neighbors(p.Number)
		evil := 0
		if lp := e.PlayerByNum(l); lp != nil && lp.FlagEvil {
			evil++
		}
		if rp := e.PlayerByNum(r); rp != nil && rp.FlagEvil {
			evil++
		}
		// 中毒共情者：照常行动，返回虚假数字（0-2 且不等于真实值）
		if p.Poisoned {
			evil = fakeCount(evil, 3)
		}
		return map[string]any{"evil": evil}
	case "undertaker":
		if e.GD.LastExecutedPID != "" {
			if dead := e.GD.Players[e.GD.LastExecutedPID]; dead != nil {
				name := dead.RoleName
				if dead.Role == "drunk" {
					name = "酒鬼" // 酒鬼被处决后真身暴露
				}
				// 中毒送葬者：返回虚假角色名
				if p.Poisoned {
					name = e.fakeRoleName(name)
				}
				return map[string]any{"info": "⚰️ 昨天被处决的是【" + name + "】"}
			}
		}
		return map[string]any{}
	case "ravenkeeper":
		// 守鸦人死当晚（NightDeaths 已标记，Alive 尚未更新）或死亡后被唤醒查验；
		// 中毒时仍会被唤醒，但得到错误角色名
		// AI 守鸦人：用 LLM 决策选择查验目标（基于生前怀疑），失败兜底随机
		if (!p.Alive || e.GD.NightDeaths[p.Number]) && !p.RavenkeeperUsed && len(alive) > 0 {
			t := e.pickTargetByAI(pid, "ravenkeeper", true)
			if t == "" {
				t = e.pickTarget(pid, true)
			}
			p.RavenkeeperUsed = true
			role := e.GD.Players[t].RoleName
			// 中毒守鸦人：返回虚假角色名
			if p.Poisoned {
				role = e.fakeRoleName(role)
			}
			return map[string]any{"target": e.GD.Players[t].Number, "role": role}
		}
	case "butler":
		if t := e.pickTarget(pid, true); t != "" {
			p.ButlerMaster = t // 记录主人（投票限制：主人未投票管家不能投）
			return map[string]any{"target": e.GD.Players[t].Number}
		}
	case "spy":
		return e.infoClueNotify(pid, "spy")
	case "demon_minion_meet":
		// 恶魔爪牙互认：只互知对方编号（minion_info/demon_info 步骤已发放编号）。
		// 爪牙知道恶魔就是小恶魔（唯一恶魔角色）；恶魔不知道爪牙的具体角色（爪牙有 4 种）。
		// 本步骤为顺序占位，无额外信息。
		return map[string]any{}
	case "chef":
		return e.infoClueNotify(pid, "chef")
	case "washerwoman":
		return e.infoClueNotify(pid, "washerwoman")
	case "librarian":
		return e.infoClueNotify(pid, "librarian")
	case "investigator":
		return e.infoClueNotify(pid, "investigator")
	}
	return nil
}

// infoClueNotify 首夜信息位角色的夜晚行动：发放真实线索（中毒时伪造假线索），
// 消息在函数内直接 Notify，返回空 action（StepNight 不再重复通知）
func (e *Engine) infoClueNotify(pid, roleKey string) map[string]any {
	p := e.GD.Players[pid]
	if p == nil || !p.Alive {
		return map[string]any{}
	}
	clue := ""
	if len(p.InfoClues) > 0 {
		clue = p.InfoClues[len(p.InfoClues)-1]
	}
	if p.Poisoned {
		clue = e.fakeClue(roleKey)
	}
	if clue != "" {
		e.Notify(p.Number, clue)
	}
	return map[string]any{}
}

// fakeClue 中毒信息位角色：生成格式一致但内容虚假的线索（玩家无感知）
func (e *Engine) fakeClue(roleKey string) string {
	// 随机两名存活玩家编号（信息指向的"玩家对"）
	nums := []int{}
	for _, q := range e.GD.Players {
		if q.Alive {
			nums = append(nums, q.Number)
		}
	}
	rand.Shuffle(len(nums), func(i, j int) { nums[i], nums[j] = nums[j], nums[i] })
	pair := nums
	if len(pair) > 2 {
		pair = pair[:2]
	}
	if len(pair) < 2 {
		pair = append(pair, nums[0]) // 极端情况：仅一名存活玩家
	}
	switch roleKey {
	case "chef":
		// 假对数 = 真实对数以外的随机值（中毒信息必然错误且格式正确）
		realPairs := 0
		allNums := []int{}
		for _, q := range e.GD.Players {
			allNums = append(allNums, q.Number)
		}
		sort.Ints(allNums)
		for i := range allNums {
			a := e.PlayerByNum(allNums[i])
			b := e.PlayerByNum(allNums[(i+1)%len(allNums)])
			if a != nil && b != nil && a.FlagEvil && b.FlagEvil {
				realPairs++
			}
		}
		return "👨‍🍳 你得知有 " + itoa(fakeCount(realPairs, 3)) + " 对相邻的玩家是邪恶阵营"
	case "washerwoman":
		// 假线索排除自身角色，优先给不在场镇民（"#A与#B中有一人是洗衣妇"这种
		// 自相矛盾的信息会让人一眼看穿洗衣妇中毒）
		onBoard := map[string]bool{}
		for _, q := range e.GD.Players {
			onBoard[q.Role] = true
		}
		pool := []string{}
		for _, rk := range GetRolesByTeam(TeamTownsfolk) {
			if rk == "washerwoman" || onBoard[rk] {
				continue
			}
			pool = append(pool, rk)
		}
		if len(pool) == 0 {
			for _, rk := range GetRolesByTeam(TeamTownsfolk) {
				if rk != "washerwoman" {
					pool = append(pool, rk)
				}
			}
		}
		return "🧺 你得知 " + joinNums(pair) + " 中有一人是【" + Roles[pool[rand.Intn(len(pool))]].Name + "】"
	case "librarian":
		// 与真实情况相反：场上有外来者就说"没有"，没有就说"有"（必然错误且合理）
		hasOutsider := false
		for _, q := range e.GD.Players {
			if q.Team == TeamOutsider {
				hasOutsider = true
				break
			}
		}
		if hasOutsider {
			return "📚 本局没有外来者"
		}
		return "📚 你得知 " + joinNums(pair) + " 中有一人是外来者"
	case "investigator":
		// 优先给不在场爪牙（指向不在场爪牙必然错误且合理）
		minions := GetRolesByTeam(TeamMinion)
		onBoard := map[string]bool{}
		for _, q := range e.GD.Players {
			onBoard[q.Role] = true
		}
		pool := []string{}
		for _, rk := range minions {
			if !onBoard[rk] {
				pool = append(pool, rk)
			}
		}
		if len(pool) == 0 {
			pool = minions
		}
		return "🔍 你得知 " + joinNums(pair) + " 中有一人是【" + Roles[pool[rand.Intn(len(pool))]].Name + "】"
	case "spy":
		lines := []string{}
		allNums := []int{}
		for _, q := range e.GD.Players {
			allNums = append(allNums, q.Number)
		}
		sort.Ints(allNums)
		for _, n := range allNums {
			q := e.PlayerByNum(n)
			lines = append(lines, "#"+itoa(n)+" = "+e.fakeRoleName(q.RoleName))
		}
		return "🕶️ 魔典: " + join(lines, "，")
	}
	return ""
}

// fakeRoleName 中毒信息位返回虚假角色名：优先给不在场角色（指向不在场角色必然错误且合理，
// 避免"被处决的#X是恶魔"这种与场上局势自相矛盾的离谱信息）
func (e *Engine) fakeRoleName(realName string) string {
	onBoard := map[string]bool{}
	for _, q := range e.GD.Players {
		onBoard[q.Role] = true
	}
	off := []string{}
	for _, r := range Roles {
		if r.Key == "drunk" || r.Name == realName || onBoard[r.Key] {
			continue
		}
		off = append(off, r.Name)
	}
	if len(off) > 0 {
		return off[rand.Intn(len(off))]
	}
	keys := []string{}
	for k := range Roles {
		if k == "drunk" || Roles[k].Name == realName {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return realName
	}
	return Roles[keys[rand.Intn(len(keys))]].Name
}

// fakeCount 随机返回 [0,max) 内不等于 real 的数字（虚假信息用）
func fakeCount(real, max int) int {
	v := rand.Intn(max)
	for v == real && max > 1 {
		v = rand.Intn(max)
	}
	return v
}

// ==============================
// 真人交互夜晚行动（占卜师主动选择）
// ==============================

// doFortuneTeller 占卜师行动：用指定目标（真人提交）或随机目标（AI/超时兜底）
// 红鲱鱼：指定的非恶魔玩家始终被当作恶魔
func (e *Engine) doFortuneTeller(pid string, targets []int) map[string]any {
	var ts []int
	if len(targets) >= 2 {
		ts = targets[:2]
	} else {
		allNums := []int{}
		for _, pp := range e.GD.Players {
			if pp.Alive {
				allNums = append(allNums, pp.Number)
			}
		}
		if len(allNums) < 2 {
			return map[string]any{}
		}
		rand.Shuffle(len(allNums), func(i, j int) { allNums[i], allNums[j] = allNums[j], allNums[i] })
		ts = allNums[:2]
	}
	herringNum := 0
	if e.GD.RedHerringPID != "" {
		if hp := e.GD.Players[e.GD.RedHerringPID]; hp != nil {
			herringNum = hp.Number
		}
	}
	found := false
	for _, n := range ts {
		if n == herringNum {
			found = true
			break
		}
		if pp := e.PlayerByNum(n); pp != nil && pp.Role == "imp" {
			found = true
			break
		}
	}
	// 中毒占卜师：选择有效、流程照常，但返回虚假查验结果（反转真实结果，玩家无感知）
	if p := e.GD.Players[pid]; p != nil && p.Poisoned {
		found = !found
	}
	return map[string]any{"targets": ts, "found": found}
}

// ResolveChoice 结算挂起的真人选择（手动提交/超时兜底），并推进夜晚步骤
// choiceType: ChoiceManual / ChoiceTimeout（超时路径 targets 传 nil 由引擎随机合法目标）
func (e *Engine) ResolveChoice(pid, roleKey string, targets []int, choiceType string) (map[string]any, error) {
	pc := e.GD.PendingChoice
	if pc == nil {
		return nil, errors.New("没有等待中的操作")
	}
	p := e.GD.Players[pid]
	ri := Roles[roleKey]
	action := map[string]any{}
	msg := ""
	if p != nil {
		action = e.executeChoice(pid, roleKey, targets)
		if len(action) > 0 {
			msg = e.GenStorytellerMsg(pid, roleKey, action)
			e.Notify(p.Number, msg)
		}
	}
	// 二次挂起（小恶魔自杀后的传位选择）：保留 executeChoice 新建的挂起，夜晚继续等待
	if e.GD.PendingChoice != nil && e.GD.PendingChoice.RoleKey == "imp_inherit" {
		ih := e.GD.PendingChoice
		return map[string]any{"phase": "night", "waiting": true,
			"player_number": ih.Number, "role": "imp_inherit",
			"deadline": ih.Deadline, "valid_targets": ih.ValidTargets,
			"max_targets": 1, "step": pc.StepIdx + 1, "total": pc.Total,
			"role_name": "小恶魔传位", "skill": "选择一名爪牙继承你的恶魔身份",
		}, nil
	}
	// 选择方式日志（手动/超时代选），对局结束归档 GameLog
	e.GD.ActionLog = append(e.GD.ActionLog, map[string]any{
		"number": pc.Number, "role": roleKey, "choice": choiceType,
		"targets": actionTargets(action), "day": e.GD.Day,
	})
	e.GD.PendingChoice = nil
	e.GD.NightIdx = pc.StepIdx + 1
	return map[string]any{
		"phase": "night", "step": pc.StepIdx + 1, "total": pc.Total,
		"role_name": ri.Name, "role_key": roleKey,
		"tip_id": pc.TipID, // 提交事件携带 tip_id：前端据此更新卡片为"已提交"
		"msgs": []map[string]any{{"num": func() int {
			if p != nil {
				return p.Number
			}
			return pc.Number
		}(), "role": ri.Name, "msg": msg}},
		"skill": ri.Ability,
	}, nil
}

// TimeoutChoice 真人夜晚行动超时兜底：AI 随机执行该角色行动并推进夜晚
func (e *Engine) TimeoutChoice() map[string]any {
	pc := e.GD.PendingChoice
	if pc == nil {
		return nil
	}
	p := e.GD.Players[pc.PlayerID]
	ri := Roles[pc.RoleKey]
	action := map[string]any{}
	msg := ""
	if p != nil && !p.IsAI {
		action = e.DoAction(pc.PlayerID, pc.RoleKey, e.GD.FirstNight)
		if len(action) > 0 {
			msg = e.GenStorytellerMsg(pc.PlayerID, pc.RoleKey, action)
			e.Notify(p.Number, msg)
		}
	}
	e.GD.ActionLog = append(e.GD.ActionLog, map[string]any{
		"number": pc.Number, "role": pc.RoleKey, "choice": "timeout",
		"targets": actionTargets(action), "day": e.GD.Day,
	})
	e.GD.PendingChoice = nil
	e.GD.NightIdx = pc.StepIdx + 1
	return map[string]any{
		"phase": "night", "step": pc.StepIdx + 1, "total": pc.Total,
		"role_name": ri.Name, "role_key": pc.RoleKey,
		"tip_id":  pc.TipID, // 超时事件携带 tip_id：前端据此更新卡片为"已超时"
		"timeout": true,
		"msgs":    []map[string]any{{"num": pc.Number, "role": ri.Name, "msg": msg}},
		"skill":   ri.Ability,
	}
}

// executeChoice 执行指定角色的主动选择（通用框架：按官方规则扩展）
func (e *Engine) executeChoice(pid, roleKey string, targets []int) map[string]any {
	p := e.GD.Players[pid]
	switch roleKey {
	case "fortune_teller":
		return e.doFortuneTeller(pid, targets)
	case "butler":
		// 管家：指定主人（主人未投票前管家不能投票；主人死亡后限制解除）
		if len(targets) >= 1 {
			if tp := e.PlayerByNum(targets[0]); tp != nil && tp.Alive {
				p.ButlerMaster = pidOf(e.GD, targets[0])
				return map[string]any{"target": targets[0]}
			}
		}
		return e.DoAction(pid, roleKey, e.GD.FirstNight)
	case "ravenkeeper":
		// 真人守鸦人死后提交查验目标（中毒时给假角色名）
		if len(targets) >= 1 {
			if tp := e.PlayerByNum(targets[0]); tp != nil && tp.Alive {
				p.RavenkeeperUsed = true
				role := tp.RoleName
				if p.Poisoned {
					role = e.fakeRoleName(role)
				}
				return map[string]any{"target": targets[0], "role": role}
			}
		}
		return e.DoAction(pid, roleKey, e.GD.FirstNight)
	case "poisoner":
		// 投毒者：指定下毒目标（投毒者中毒时毒不生效；恶魔免疫投毒）
		if len(targets) >= 1 {
			if tp := e.PlayerByNum(targets[0]); tp != nil && tp.Alive {
				if !p.Poisoned && tp.Role != "imp" {
					tp.Poisoned = true
				}
				tp.FlagTargeted = true
				return map[string]any{"target": targets[0]}
			}
		}
		return e.DoAction(pid, roleKey, e.GD.FirstNight)
	case "monk":
		// 僧侣：指定保护对象（不能连续同一人）；中毒时流程照常但保护效果不生效
		if len(targets) >= 1 {
			if tp := e.PlayerByNum(targets[0]); tp != nil && tp.Alive {
				if !p.Poisoned {
					tp.Protected = true
				}
				tp.FlagTargeted = true
				p.MonkLast = pidOf(e.GD, targets[0])
				return map[string]any{"target": targets[0]}
			}
		}
		return e.DoAction(pid, roleKey, e.GD.FirstNight)
	case "imp":
		// 小恶魔：选择自己 = 自杀传位（官方：自杀必传位，恶魔选择一名爪牙继承）
		if len(targets) >= 1 && targets[0] == p.Number {
			p.Alive = false
			p.DeadVote = true // 死亡票
			e.Notify(p.Number, "☠️ 你在夜晚死亡。希望你的牺牲能为邪恶方带来胜利。")
			e.markNightDeath(p.Number)
			aliveMinions := []string{}
			for k := range e.GD.Players {
				if e.GD.Players[k].Alive && e.GD.Players[k].Team == TeamMinion {
					aliveMinions = append(aliveMinions, k)
				}
			}
			if len(aliveMinions) == 0 {
				return map[string]any{"target": p.Number, "suicide": true, "no_minion": true}
			}
			if len(aliveMinions) == 1 {
				e.becomeImp(e.GD.Players[aliveMinions[0]])
				return map[string]any{"target": p.Number, "suicide": true, "heir": e.GD.Players[aliveMinions[0]].Number}
			}
			// 多名爪牙：真人恶魔二次挂起选择继承者（官方：恶魔选择一名爪牙传位）
			if !p.IsAI {
				heirNums := []int{}
				for _, k := range aliveMinions {
					heirNums = append(heirNums, e.GD.Players[k].Number)
				}
				e.GD.PendingChoice = &NightPendingChoice{
					PlayerID:     pid,
					Number:       p.Number,
					RoleKey:      "imp_inherit",
					RoleName:     "小恶魔传位",
					ValidTargets: heirNums,
					MaxTargets:   1,
					StepIdx:      e.GD.NightIdx,
					Total:        len(NightOrderOther),
					Deadline:     time.Now().Add(NightActionTimeout).Unix(),
				}
				return map[string]any{"phase": "night", "waiting": true,
					"player_number": p.Number, "role": "imp_inherit",
					"deadline":      e.GD.PendingChoice.Deadline,
					"valid_targets": heirNums, "max_targets": 1,
					"step": e.GD.NightIdx + 1, "total": len(NightOrderOther),
					"role_name": "小恶魔传位", "skill": "选择一名爪牙继承你的恶魔身份",
				}
			}
			e.becomeImp(e.GD.Players[aliveMinions[rand.Intn(len(aliveMinions))]])
			return map[string]any{"target": p.Number, "suicide": true, "heir": 0}
		}
		// 士兵：恶魔可以选择士兵，但杀不死（官方规则）
		if len(targets) >= 1 {
			if tp := e.PlayerByNum(targets[0]); tp != nil && tp.Alive {
				if tp.Role == "soldier" {
					e.Notify(p.Number, "🪖 你选择了士兵，但他毫发无伤（士兵免疫恶魔）")
					return map[string]any{"target": targets[0], "blocked": true}
				}
			// 镇长弹刀（官方魔典）：攻击镇长时可能转移给另一名玩家
			if tp.Role == "mayor" && e.mayorBounce(pidOf(e.GD, tp.Number)) {
				return map[string]any{"target": targets[0], "bounced": true}
			}
			tp.Alive = false
			tp.DeadVote = true // 死亡票
			e.Notify(tp.Number, "☠️ 你在夜晚死亡")
			e.markNightDeath(tp.Number)
			return map[string]any{"target": targets[0]}
			}
		}
		return e.DoAction(pid, roleKey, e.GD.FirstNight)
	case "imp_inherit":
		// 小恶魔自杀后的传位选择：选定一名爪牙继承恶魔身份
		if len(targets) >= 1 {
			if heir := e.PlayerByNum(targets[0]); heir != nil && heir.Alive && heir.Team == TeamMinion {
				e.becomeImp(heir)
				return map[string]any{"target": targets[0], "inherit": true}
			}
		}
		return map[string]any{}
	}
	// 回退：原 DoAction 随机路径
	return e.DoAction(pid, roleKey, e.GD.FirstNight)
}

// ChoiceTargets 返回角色可选目标集合（通用框架：按官方规则扩展）
func (e *Engine) ChoiceTargets(pid, roleKey string) []int {
	p := e.GD.Players[pid]
	switch roleKey {
	case "fortune_teller":
		return e.AliveNums(-1) // 全部存活玩家（占卜师可含自己）
	case "imp":
		// 小恶魔：除自己外的存活玩家 + 自己（自杀传位选项）
		nums := e.AliveNums(p.Number)
		nums = append(nums, p.Number)
		return nums
	case "monk":
		// 僧侣：除自己外的存活玩家，且不能连续保护同一人
		nums := e.AliveNums(p.Number)
		out := []int{}
		for _, n := range nums {
			if pidOf(e.GD, n) != p.MonkLast {
				out = append(out, n)
			}
		}
		return out
	}
	// 默认：除自己外的存活玩家（管家/投毒者等）
	return e.AliveNums(e.GD.Players[pid].Number)
}

// ChoiceCount 返回角色需要选择的目标数量（通用框架：按官方规则扩展）
func (e *Engine) ChoiceCount(roleKey string) int {
	switch roleKey {
	case "fortune_teller":
		return 2
	}
	return 1
}

// actionTargets 从行动结果提取目标编号（用于选择日志）
func actionTargets(action map[string]any) []int {
	switch v := action["targets"].(type) {
	case []int:
		return v
	case []any:
		out := []int{}
		for _, x := range v {
			switch n := x.(type) {
			case int:
				out = append(out, n)
			case float64:
				out = append(out, int(n))
			}
		}
		return out
	}
	if t, ok := action["target"].(int); ok {
		return []int{t}
	}
	if t, ok := action["target"].(float64); ok {
		return []int{int(t)}
	}
	return nil
}

// ==============================
// markNightDeath 记录今晚死亡（黎明前对外隐藏死亡状态）
func (e *Engine) markNightDeath(num int) {
	if e.GD.NightDeaths == nil {
		e.GD.NightDeaths = map[int]bool{}
	}
	e.GD.NightDeaths[num] = true
}

// mayorBounce 镇长弹刀：说书人根据双方优劣决定弹刀概率
// 邪恶方人数少于善良方一半 → 弹刀 50%；邪恶方少于善良方 1/4 → 弹刀仅 10%
// 返回 true 表示弹刀成功（镇长存活），false 表示镇长死亡
func (e *Engine) mayorBounce(mayorPID string) bool {
	evilAlive, goodAlive := 0, 0
	for _, p := range e.GD.Players {
		if !p.Alive {
			continue
		}
		if p.Team == TeamMinion || p.Team == TeamDemon {
			evilAlive++
		} else {
			goodAlive++
		}
	}
	chance := 0.5
	if evilAlive*4 < goodAlive {
		chance = 0.1 // 邪恶方远少于善良方：弹刀概率仅 10%
	} else if evilAlive*2 < goodAlive {
		chance = 0.5 // 邪恶方少于善良方一半：弹刀 50%
	}
	if rand.Float64() > chance {
		return false
	}
	others := []string{}
	for pid, p := range e.GD.Players {
		if p.Alive && pid != mayorPID && p.Role != "soldier" {
			others = append(others, pid)
		}
	}
	if len(others) == 0 {
		return false
	}
	victim := e.GD.Players[others[rand.Intn(len(others))]]
	victim.Alive = false
	victim.DeadVote = true // 死亡票
	e.Notify(victim.Number, "☠️ 你在夜晚死亡")
	e.markNightDeath(victim.Number)
	return true
}

// 死亡处理
// ==============================

// ApplyDeaths 结算夜晚死亡（僧侣保护 / 守鸦人遗言 / 恶魔继承）
// SlayerShoot 猎手开枪声明判定：命中恶魔则其立即死亡，否则无事发生；无论是否命中技能消耗
// 返回 (hit, targetRoleName, ok)；ok=false 表示无效声明（非猎手/已用过/目标已死）
func (e *Engine) SlayerShoot(num, targetNum int) (bool, string, bool) {
	p := e.PlayerByNum(num)
	t := e.PlayerByNum(targetNum)
	if p == nil || t == nil || p.Role != "slayer" || p.SlayerUsed || !t.Alive {
		return false, "", false
	}
	p.SlayerUsed = true
	if t.Team == TeamDemon {
		t.Alive = false
		t.DeadVote = true // 死亡票（虽然恶魔死亡通常游戏结束，保持一致）
		e.maybeInheritImp()
		return true, t.RoleName, true
	}
	return false, t.RoleName, true
}

// ApplyDeaths 夜晚死亡结算
func (e *Engine) ApplyDeaths() {
	for _, p := range e.GD.Players {
		if p.Alive || !p.Protected {
			continue
		}
		p.Alive = true // 恶魔被僧侣挡下
		e.Notify(p.Number, "🧘 僧侣保护了你，你没有死亡")
		p.Protected = false
	}
	for _, p := range e.GD.Players {
		p.Protected = false
	}
	// 恶魔死亡 → 红唇女郎继承（唯一继承途径；爪牙仅在小恶魔自杀时继承）
	e.maybeInheritImp()
	// 黎明：公布死亡（清空 NightDeaths 隐藏标记，死亡状态对所有人可见）
	e.GD.NightDeaths = nil
	e.GD.DiedToday = false
	for _, p := range e.GD.Players {
		if !p.Alive {
			e.GD.DiedToday = true
			break
		}
	}
}

// maybeInheritImp 恶魔死亡后的继承：仅红唇女郎可继承（存活≥5人）
// 爪牙变成恶魔只发生在小恶魔自杀时（见 imp 夜晚行动）
func (e *Engine) maybeInheritImp() {
	for _, p := range e.GD.Players {
		if p.Role == "imp" && p.Alive {
			return
		}
	}
	aliveCount := 0
	for _, p := range e.GD.Players {
		if p.Alive {
			aliveCount++
		}
	}
	if aliveCount < 5 {
		return
	}
	for _, p := range e.GD.Players {
		if p.Alive && p.Role == "scarlet_woman" {
			e.becomeImp(p)
			return
		}
	}
}

// becomeImp 玩家转变为小恶魔（获得爪牙与伪装信息）
func (e *Engine) becomeImp(heir *PlayerState) {
	heir.Role = "imp"
	heir.Team = TeamDemon
	heir.RoleName = "小恶魔"
	heir.FlagEvil = true
	heir.FlagGood = false
	minions := []int{}
	for _, p := range e.GD.Players {
		if p.Team == TeamMinion {
			minions = append(minions, p.Number)
		}
	}
	heir.InfoMinionNums = minions
	heir.InfoNotInPlay = e.notInPlayTownsfolk(nil)
	// 其他爪牙的"恶魔编号"同步更新为新恶魔（否则爪牙私聊会找已死的旧恶魔）
	for _, p := range e.GD.Players {
		if p.Team == TeamMinion {
			p.InfoDemonNum = heir.Number
		}
	}
	e.Notify(heir.Number, "😈 你成为了新的小恶魔！")
	if len(minions) > 0 {
		e.Notify(heir.Number, "你的爪牙编号: "+joinNums(minions))
	}
	if len(heir.InfoNotInPlay) > 0 {
		e.Notify(heir.Number, "不在场的镇民(可伪装): "+join(heir.InfoNotInPlay, ", "))
	}
}

// ==============================
// 处决提名
// ==============================

// Nominate 提名处决（含贞洁者/猎手/圣徒特殊规则；死者不可提名）
// Nominate 发起提名：创建投票状态，等待所有存活玩家投票（AI 自动投，真人经 UI 投票）
// 规则：允许提名死人（无实际效果）；同一玩家本轮只能被提名一次；猎手/贞洁者特判保留
func (e *Engine) Nominate(fromNum, targetNum int) map[string]any {
	targetPID := pidOf(e.GD, targetNum)
	if targetPID == "" {
		return map[string]any{"success": false, "message": "目标无效"}
	}
	fromPID := pidOf(e.GD, fromNum)
	if fromPID == "" || !e.GD.Players[fromPID].Alive {
		return map[string]any{"success": false, "message": "死亡玩家不能提名"}
	}
	target := e.GD.Players[targetPID]

	// 已有投票进行中：拒绝新提名（先完成当前投票）
	if e.GD.ActiveVote != nil {
		return map[string]any{"success": false, "message": "已有提名正在投票中"}
	}
	// 提名者本轮只能提名一次：提名权用过即收回
	for _, rec := range e.GD.Nominations {
		if rec.FromNumber == fromNum {
			return map[string]any{"success": false, "message": "你本轮已经提名过了，不能再提名"}
		}
	}
	// 被提名过的玩家无法再次提名
	for _, rec := range e.GD.Nominations {
		if rec.TargetNumber == targetNum {
			return map[string]any{"success": false, "message": "该玩家已被提名过，不能再提名"}
		}
	}

	// —— 猎手：每局一次，指认恶魔立即击杀（目标须存活） ——
	if fromPID != "" && target.Alive {
		slayer := e.GD.Players[fromPID]
		if slayer.Role == "slayer" && !slayer.SlayerUsed {
			slayer.SlayerUsed = true
			if target.Role == "imp" {
				target.Alive = false
				target.DeadVote = true // 死亡票
				e.GD.DiedToday = true
				e.maybeInheritImp()
				result := map[string]any{
					"target": targetNum, "message": "🏹 猎手射杀了 #" + itoa(targetNum) + "！他是小恶魔！",
					"special": "slayer", "yes": 0, "total": 0,
				}
				if win := e.CheckWin(); win != "" {
					result["game_over"] = true
					result["winner"] = win
				}
				e.GD.LastNomination = result
				return result
			}
			return map[string]any{
				"target": targetNum, "message": "🏹 猎手射偏了，#" + itoa(targetNum) + " 不是恶魔",
				"special": "slayer", "yes": 0, "total": 0,
			}
		}
		// —— 贞洁者：首次被镇民提名时，提名者立即被处决 ——
		if target.Role == "virgin" && !target.VirginTriggered && slayer.Team == TeamTownsfolk {
			target.VirginTriggered = true
			slayer.Alive = false
			slayer.DeadVote = true // 死亡票
			e.Notify(slayer.Number, "🌸 贞洁者触发：你提名了贞洁者，被立即处决死亡")
			e.GD.DiedToday = true
			e.maybeInheritImp()
			result := map[string]any{
				"target": targetNum, "message": "🌸 贞洁者触发！#" + itoa(fromNum) + " 被立即处决",
				"special": "virgin", "yes": 0, "total": 0,
			}
			if win := e.CheckWin(); win != "" {
				result["game_over"] = true
				result["winner"] = win
			}
			e.GD.LastNomination = result
			return result
		}
	}

	// 创建投票状态：先进入辩论期（20 秒，仅提名双方可发言），辩论结束后开始同时投票
	e.GD.ActiveVote = &VoteState{
		FromNumber:     fromNum,
		TargetNumber:   targetNum,
		DebateDeadline: time.Now().Add(DebateSeconds * time.Second).Unix(),
		Votes:          map[int]string{},
		Stated:         map[int]bool{},
	}
	// 新提名重置提名周期倒计时：投票结束后若 60 秒内无新提名则统一比对处决
	e.GD.NomCooldownUntil = time.Now().Add(NomCooldownSeconds * time.Second).Unix()
	return map[string]any{
		"nominated":       true,
		"from":            fromNum,
		"target":          targetNum,
		"debate":          true,
		"debate_deadline": e.GD.ActiveVote.DebateDeadline,
	}
}

// StartVoting 辩论结束，开始同时投票倒计时
// AI 投票不再在此同步执行（避免 8 个 LLM 决策阻塞事件循环导致投票期被挤掉），
// 由房间层异步决策后经 ai_vote_resolve 事件回投写票
func (e *Engine) StartVoting() (map[string]any, bool) {
	v := e.GD.ActiveVote
	if v == nil {
		return nil, false
	}
	if v.VoteDeadline != 0 {
		return nil, false // 已在投票中
	}
	v.VoteDeadline = time.Now().Add(VoteSeconds * time.Second).Unix()
	return map[string]any{
		"vote_start": true,
		"from":       v.FromNumber,
		"target":     v.TargetNumber,
		"deadline":   v.VoteDeadline,
	}, false
}

// AI 投票玩家池：存活 AI + 有死亡票的死亡 AI
func (e *Engine) AIVoters() []*PlayerState {
	out := []*PlayerState{}
	for _, p := range e.GD.Players {
		if p.IsAI && (p.Alive || p.DeadVote) {
			out = append(out, p)
		}
	}
	return out
}

// ApplyAIVote 房间层回投 AI 投票结果（事件循环内执行，无竞态）
func (e *Engine) ApplyAIVote(num int, voteYes bool) {
	v := e.GD.ActiveVote
	if v == nil || v.VoteDeadline == 0 {
		return
	}
	if time.Now().Unix() >= v.VoteDeadline {
		return
	}
	if v.Votes == nil {
		v.Votes = map[int]string{}
	}
	if v.Stated == nil {
		v.Stated = map[int]bool{}
	}
	p := e.PlayerByNum(num)
	if p == nil {
		return
	}
	if voteYes {
		v.Votes[num] = "yes"
		if !p.Alive {
			p.DeadVote = false // 死亡票：投出后没收（仅此一次）
		}
	} else {
		delete(v.Votes, num)
	}
	v.Stated[num] = true
}

// aiVoteChoice 已废弃：AI 投票改为房间层异步决策（DecideVote + ai_vote_resolve 事件回投），
// 避免 8 个 LLM 决策同步阻塞事件循环

// CastVote 投票倒计时内投票/取消投票（可反复切换意向）
// choice="yes" 投赞成；choice="abstain" 取消投票（移除自己的票）
// 规则：提名者与被提名者（辩论双方）不能投票；死亡玩家可用死亡票投最后一次
func (e *Engine) CastVote(num int, choice string) (map[string]any, bool, error) {
	v := e.GD.ActiveVote
	if v == nil {
		return nil, false, errors.New("当前没有进行中的投票")
	}
	if v.VoteDeadline == 0 {
		return nil, false, errors.New("辩论尚未结束，还不能投票")
	}
	if time.Now().Unix() >= v.VoteDeadline {
		return nil, false, errors.New("投票已结束")
	}
	if choice != "yes" && choice != "no" && choice != "abstain" {
		return nil, false, errors.New("投票选项无效")
	}
	// 提名者和被提名者也可以投票（官方规则：所有人都能投票，包括提名自己）
	p := e.PlayerByNum(num)
	if p == nil {
		return nil, false, errors.New("玩家不存在")
	}
	// 管家投票限制（官方魔典）：主人未表态时管家的投票不计入统计；管家中毒则技能失效（不受限制）
	// 注意：管家仍可以投出票（界面不拦截），仅在 FinishVote 统计时跳过未获主人表态的管家票
	switch choice {
	case "abstain", "no":
		// 取消投票：移除自己的票（可反复切换意向，无需投票资格）
		delete(v.Votes, num)
		if v.Stated == nil {
			v.Stated = map[int]bool{}
		}
		v.Stated[num] = true // 弃权也算表态（管家主人限制）
	case "yes":
		if !p.Alive {
			if !p.DeadVote {
				return nil, false, errors.New("你没有死亡票，无法投票")
			}
			p.DeadVote = false // 使用死亡票：没收投票权利
		}
		v.Votes[num] = "yes"
		if v.Stated == nil {
			v.Stated = map[int]bool{}
		}
		v.Stated[num] = true
	}
	return map[string]any{
		"voted":    true,
		"choice":   choice,
		"from":     v.FromNumber,
		"target":   v.TargetNumber,
		"deadline": v.VoteDeadline,
	}, false, nil
}

// FinishVote 投票倒计时结束：统计票数 → 记录该被提名者票数 → 开启 30 秒提名冷却窗口
// 处决判定延迟到冷却到期统一比对（ResolveNominationPeriod）
// 投票参与者 = 存活玩家 + 有死亡票的死亡玩家，排除提名双方（辩论双方不能投票）
func (e *Engine) FinishVote() map[string]any {
	v := e.GD.ActiveVote
	if v == nil {
		return nil
	}
	yes, abstain := 0, 0
	yesNums := []int{}
	votersNums := []int{}
	unvotedNums := []int{}
	for _, p := range e.GD.Players {
		if p.Alive || p.DeadVote {
			votersNums = append(votersNums, p.Number)
			if _, voted := v.Votes[p.Number]; !voted {
				unvotedNums = append(unvotedNums, p.Number)
			}
		}
	}
	sort.Ints(votersNums)
	sort.Ints(unvotedNums)
	for num, choice := range v.Votes {
		if choice == "yes" {
			// 管家票不计入：主人存活且未投赞成时，管家的赞成票跳过（中毒管家不受限）
			// 主人必须实际投出赞成票，管家票才计入统计（"主人没举手，管家不能算票"）
			if bp := e.PlayerByNum(num); bp != nil && bp.Role == "butler" && !bp.Poisoned && bp.ButlerMaster != "" {
				if master := e.GD.Players[bp.ButlerMaster]; master != nil && master.Alive {
					if v.Votes[master.Number] != "yes" {
						continue
					}
				}
			}
			yes++
			yesNums = append(yesNums, num)
		} else {
			abstain++
		}
	}
	sort.Ints(yesNums)
	rec := &NominationRecord{FromNumber: v.FromNumber, TargetNumber: v.TargetNumber, Yes: yes, Abstain: abstain}
	e.GD.Nominations = append(e.GD.Nominations, rec)
	unvoted := len(unvotedNums)
	e.GD.ActiveVote = nil
	// 开启 30 秒提名冷却窗口：冷却期内不再接受新提名，到期后统一比对处决
	e.GD.NomCooldownUntil = time.Now().Add(NomCooldownSeconds * time.Second).Unix()
	return map[string]any{
		"vote_done":    true,
		"from":         v.FromNumber,
		"target":       v.TargetNumber,
		"yes":          yes,
		"abstain":      abstain,
		"voted":        len(votersNums) - unvoted,
		"unvoted":      unvoted,
		"voters_nums":  votersNums,
		"unvoted_nums": unvotedNums,
		"yes_nums":     yesNums,
		"votes":        v.Votes,
		"executed":     false,
		"cooldown":     NomCooldownSeconds,
		"message":      "#" + itoa(v.TargetNumber) + " 得 " + itoa(yes) + " 票，等待其他提名…",
	}
}

// ResolveNominationPeriod 提名周期结束（30 秒冷却到期）：比对全部被提名者票数
// 规则：唯一最高票 且 > 存活玩家半数 → 处决死亡；并列最高或未过半 → 无人处决
func (e *Engine) ResolveNominationPeriod() map[string]any {
	if len(e.GD.Nominations) == 0 {
		e.GD.NomCooldownUntil = 0
		return nil
	}
	var best *NominationRecord
	tie := false
	for _, rec := range e.GD.Nominations {
		if best == nil || rec.Yes > best.Yes {
			best = rec
			tie = false
		} else if rec.Yes == best.Yes && rec.TargetNumber != best.TargetNumber {
			tie = true
		}
	}
	result := map[string]any{
		"nominations": len(e.GD.Nominations),
		"executed":    false,
	}
	// 处决规则：唯一最高票 且 > 存活玩家半数 → 处决死亡；平票、0 票或未过半 → 无人处决
	aliveCount := 0
	for _, p := range e.GD.Players {
		if p.Alive {
			aliveCount++
		}
	}
	if best == nil || tie || best.Yes == 0 || best.Yes <= aliveCount/2 {
		if tie {
			result["message"] = "票数并列最高，无人处决"
		} else if best == nil {
			result["message"] = "没有有效提名，无人处决"
		} else if best.Yes <= aliveCount/2 {
			result["message"] = "#" + itoa(best.TargetNumber) + " 得 " + itoa(best.Yes) + " 票，未超过存活玩家半数，无人处决"
		} else {
			result["message"] = "无人获得赞成票，无人处决"
		}
	} else if targetPID := pidOf(e.GD, best.TargetNumber); targetPID != "" {
		if target := e.GD.Players[targetPID]; target != nil && target.Alive {
			target.Alive = false
			target.DeadVote = true // 死亡票
			e.Notify(target.Number, "⚖️ 你在白天被投票处决死亡")
			e.GD.Executed = true
			e.GD.DiedToday = true
			e.GD.LastExecutedPID = targetPID
			e.GD.LastExecutedRole = target.Role
			e.maybeInheritImp()
			result["executed"] = true
			result["target"] = best.TargetNumber
			result["message"] = "#" + itoa(best.TargetNumber) + " 得票最高（" + itoa(best.Yes) + " 票），被处决"
			if win := e.CheckWin(); win != "" {
				result["game_over"] = true
				result["winner"] = win
			}
		} else {
			result["message"] = "#" + itoa(best.TargetNumber) + " 已死亡，处决无效果"
		}
	}
	// 周期结束：清空提名记录，允许开启下一轮提名；强制清理活跃投票防残留死锁
	e.GD.Nominations = nil
	e.GD.ActiveVote = nil
	e.GD.NomCooldownUntil = 0
	return result
}

// EndNomination 结束提名阶段：多个提名中「票数过半且得票最多」者被处决
func (e *Engine) EndNomination() map[string]any {
	// 若有未完成投票，先放弃（不计入）
	e.GD.ActiveVote = nil

	aliveCount := 0
	for _, p := range e.GD.Players {
		if p.Alive {
			aliveCount++
		}
	}
	// 找出过半且得票最多的提名
	var best *NominationRecord
	for _, rec := range e.GD.Nominations {
		if rec.Yes > aliveCount/2 {
			if best == nil || rec.Yes > best.Yes {
				best = rec
			}
		}
	}
	result := map[string]any{
		"nominations": len(e.GD.Nominations),
		"executed":    false,
	}
	if best == nil {
		result["message"] = "没有提名获得过半票数，平安日"
	} else {
		targetPID := pidOf(e.GD, best.TargetNumber)
		target := e.GD.Players[targetPID]
		// 提名死人：处决无实际效果（不产生新死亡）
		if !target.Alive {
			result["message"] = "#" + itoa(best.TargetNumber) + " 已死亡，处决无效果"
			result["executed"] = true
			result["target"] = best.TargetNumber
		} else if target.Role == "saint" {
			target.Alive = false
			target.DeadVote = true // 死亡票
			e.Notify(target.Number, "⚖️ 你在白天被投票处决死亡（圣徒被处决，善良阵营落败）")
			e.GD.Executed = true
			e.GD.DiedToday = true
			e.GD.LastExecutedPID = targetPID
			e.GD.LastExecutedRole = "saint"
			result["message"] = "#" + itoa(best.TargetNumber) + " 被处决，圣徒死亡，善良阵营落败！"
			result["executed"] = true
			result["target"] = best.TargetNumber
			result["game_over"] = true
			result["winner"] = "evil"
		} else {
			target.Alive = false
			target.DeadVote = true // 死亡票
			e.Notify(target.Number, "⚖️ 你在白天被投票处决死亡")
			e.GD.Executed = true
			e.GD.DiedToday = true
			e.GD.LastExecutedPID = targetPID
			e.GD.LastExecutedRole = target.Role
			e.maybeInheritImp()
			result["message"] = "#" + itoa(best.TargetNumber) + " 被处决（" + itoa(best.Yes) + " 票）"
			result["executed"] = true
			result["target"] = best.TargetNumber
			if win := e.CheckWin(); win != "" {
				result["game_over"] = true
				result["winner"] = win
			}
		}
	}
	e.GD.LastNomination = result
	return result
}

// ==============================
// 首夜信息 & 角色分配
// ==============================

// GenFirstNightInfo 生成首夜信息
func (e *Engine) GenFirstNightInfo() {
	// 首夜信息位（洗衣妇/图书管理员/调查员/厨师/间谍）：线索不在此发放，
	// 延迟到夜晚对应步骤（投毒者行动之后）发放，保证"被毒 → 收到假线索"的官方时序。
	// 酒鬼的编造线索照常开局发放（其 Role 为 drunk，不在延迟集合内）。
	delayedClues := map[string]bool{
		"washerwoman": true, "librarian": true, "investigator": true, "chef": true, "spy": true,
	}
	for _, p := range e.GD.Players {
		ri := Roles[p.Role]
		// 酒鬼看到伪装镇民的身份与能力
		if p.Role == "drunk" {
			if fake, ok := Roles[p.FakeRole]; ok {
				ri = fake
			}
		}
		msgs := []string{"🎭 你的身份是: " + ri.Name, "📋 " + ri.Ability}
		if len(p.InfoMinionNums) > 0 {
			msgs = append(msgs, "你的爪牙编号: "+joinNums(p.InfoMinionNums))
		}
		if p.InfoDemonNum != 0 {
			msgs = append(msgs, "你的恶魔编号: #"+itoa(p.InfoDemonNum))
		}
		if len(p.InfoNotInPlay) > 0 {
			msgs = append(msgs, "不在场的镇民(可伪装): "+join(p.InfoNotInPlay, ", "))
		}
		if !delayedClues[p.Role] {
			for _, clue := range p.InfoClues {
				msgs = append(msgs, clue)
			}
		}
		for _, m := range msgs {
			e.Notify(p.Number, m)
		}
	}
}

// notInPlayTownsfolk 返回不在场镇民名（排除 exclude 角色）
func (e *Engine) notInPlayTownsfolk(exclude map[string]bool) []string {
	inPlay := map[string]bool{}
	for _, p := range e.GD.Players {
		if p.Role != "" {
			inPlay[p.Role] = true
		}
		if p.FakeRole != "" {
			inPlay[p.FakeRole] = true
		}
	}
	out := []string{}
	for _, key := range GetRolesByTeam(TeamTownsfolk) {
		if !inPlay[key] && !exclude[key] {
			out = append(out, Roles[key].Name)
		}
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// AssignRoles 按暗流涌动官方人数表分配角色
func (e *Engine) AssignRoles() {
	players := e.GD.Players
	total := len(players)
	pidList := keysOf(players)
	rand.Shuffle(len(pidList), func(i, j int) { pidList[i], pidList[j] = pidList[j], pidList[i] })

	// 暗流涌动官方人数配置：(镇民, 外来者, 爪牙, 恶魔)，13-15 人局含旅行者（暂不支持）
	roleTable := map[int][4]int{
		5: {3, 0, 1, 1}, 6: {3, 1, 1, 1}, 7: {5, 0, 1, 1}, 8: {5, 1, 1, 1},
		9: {5, 2, 1, 1}, 10: {7, 0, 2, 1}, 11: {7, 1, 2, 1}, 12: {7, 2, 2, 1},
	}
	t := total
	if t < 5 {
		t = 5
	}
	if t > 12 {
		t = 12 // 上游应已拦截（旅行者机制未实现）
	}
	cfg := roleTable[t]
	tfC, outC, minC := cfg[0], cfg[1], cfg[2]

	minions := sample(GetRolesByTeam(TeamMinion), minC)
	// 男爵：场上多两名外来者（移除两名镇民）
	for _, m := range minions {
		if m == "baron" {
			outC += 2
			tfC -= 2
		}
	}
	var outsiders []string
	if outC > 0 {
		outsiders = sample(GetRolesByTeam(TeamOutsider), outC)
	}
	townsfolk := sample(GetRolesByTeam(TeamTownsfolk), tfC)
	pool := append([]string{"imp"}, append(append(minions, outsiders...), townsfolk...)...)
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	// 均衡配板：斗蛐蛐观战模式可预设角色组合（5 组专业配板轮转），保证对局均衡可比
	if len(e.GD.ForceRoles) == len(pool) {
		pool = append([]string{}, e.GD.ForceRoles...)
		rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	}

	// 体验优化：真人玩家优先拿主动选择型角色（占卜师/管家），提升真人参与感
	// 角色池重排：NeedsChoice 角色移到前部；玩家序列重排：真人移到前部
	// 效果：只要有真人在场，主动角色优先分配给真人
	choiceRoles := []string{}
	normalRoles := []string{}
	for _, rk := range pool {
		if Roles[rk].NeedsChoice {
			choiceRoles = append(choiceRoles, rk)
		} else {
			normalRoles = append(normalRoles, rk)
		}
	}
	rand.Shuffle(len(choiceRoles), func(i, j int) { choiceRoles[i], choiceRoles[j] = choiceRoles[j], choiceRoles[i] })
	rand.Shuffle(len(normalRoles), func(i, j int) { normalRoles[i], normalRoles[j] = normalRoles[j], normalRoles[i] })
	pool = append(choiceRoles, normalRoles...)

	humanPIDs := []string{}
	aiPIDs := []string{}
	for _, pid := range pidList {
		if players[pid].IsAI {
			aiPIDs = append(aiPIDs, pid)
		} else {
			humanPIDs = append(humanPIDs, pid)
		}
	}
	rand.Shuffle(len(humanPIDs), func(i, j int) { humanPIDs[i], humanPIDs[j] = humanPIDs[j], humanPIDs[i] })
	rand.Shuffle(len(aiPIDs), func(i, j int) { aiPIDs[i], aiPIDs[j] = aiPIDs[j], aiPIDs[i] })
	pidList = append(humanPIDs, aiPIDs...)

	demonPID := ""
	minionNums := []int{}
	for i, pid := range pidList {
		rk := pool[i]
		ri := Roles[rk]
		p := players[pid]
		p.Role = rk
		p.Team = ri.Team
		p.RoleName = ri.Name
		p.Alive = true
		p.Poisoned = false
		p.Drunk = false
		p.Protected = false
		p.SlayerUsed = false
		p.VirginTriggered = false
		p.RavenkeeperUsed = false
		p.FlagTargeted = false
		p.FlagEvil = ri.Team == TeamMinion || ri.Team == TeamDemon
		p.FlagGood = !p.FlagEvil
		// 隐士可能被当作邪恶阵营
		if rk == "recluse" && rand.Float64() < 0.5 {
			p.FlagEvil = true
		}
		if ri.Team == TeamDemon {
			demonPID = pid
		}
		if ri.Team == TeamMinion {
			minionNums = append(minionNums, p.Number)
		}
	}

	// 恶魔得知爪牙 + 三个不在场镇民
	if demonPID != "" {
		players[demonPID].InfoMinionNums = minionNums
		players[demonPID].InfoNotInPlay = e.notInPlayTownsfolk(nil)
	}
	// 爪牙得知恶魔
	for _, p := range players {
		if p.Team == TeamMinion && demonPID != "" {
			p.InfoDemonNum = players[demonPID].Number
		}
	}

	// 占卜师的红鲱鱼：随机一名非恶魔玩家始终被当作恶魔
	herringPool := []string{}
	for pid := range players {
		if pid != demonPID {
			herringPool = append(herringPool, pid)
		}
	}
	if len(herringPool) > 0 {
		e.GD.RedHerringPID = herringPool[rand.Intn(len(herringPool))]
	}

	// 酒鬼伪装成一个不在场的镇民
	for _, p := range players {
		if p.Role == "drunk" {
			used := map[string]bool{}
			for _, q := range players {
				if q.Role != "" {
					used[q.Role] = true
				}
			}
			fakePool := []string{}
			for _, key := range GetRolesByTeam(TeamTownsfolk) {
				if !used[key] {
					fakePool = append(fakePool, key)
				}
			}
			if len(fakePool) == 0 {
				fakePool = GetRolesByTeam(TeamTownsfolk)
			}
			p.FakeRole = fakePool[rand.Intn(len(fakePool))]
			p.RoleName = Roles[p.FakeRole].Name
			// 酒鬼伪装成信息位时，说书人编造一条错误信息
			if clue := e.fakeDrunkClue(p); clue != "" {
				p.InfoClues = append(p.InfoClues, clue)
			}
		}
	}

	// 信息位角色首夜线索
	e.assignInfoClues(players, townsfolk, outsiders, minions)
}

// fakeDrunkClue 酒鬼伪装成信息位角色时，说书人编造的错误线索
// 注意：线索格式与真实线索完全一致（不带任何标记）——酒鬼玩家不知道自己是酒鬼
func (e *Engine) fakeDrunkClue(drunk *PlayerState) string {
	otherNums := []int{}
	for _, q := range e.GD.Players {
		if q.Number != drunk.Number {
			otherNums = append(otherNums, q.Number)
		}
	}
	if len(otherNums) == 0 {
		return ""
	}
	rand.Shuffle(len(otherNums), func(i, j int) { otherNums[i], otherNums[j] = otherNums[j], otherNums[i] })
	pair := otherNums[:minInt(2, len(otherNums))]
	switch drunk.FakeRole {
	case "chef":
		// 假对数 = 真实对数以外的随机值
		realPairs := 0
		allNums := []int{}
		for _, q := range e.GD.Players {
			allNums = append(allNums, q.Number)
		}
		sort.Ints(allNums)
		for i := range allNums {
			a := e.PlayerByNum(allNums[i])
			b := e.PlayerByNum(allNums[(i+1)%len(allNums)])
			if a != nil && b != nil && a.FlagEvil && b.FlagEvil {
				realPairs++
			}
		}
		return "👨‍🍳 你得知有 " + itoa(fakeCount(realPairs, 3)) + " 对相邻的玩家是邪恶阵营"
	case "empath":
		l, r := e.Neighbors(drunk.Number)
		return "💗 你相邻的存活玩家（#" + itoa(l) + " 与 #" + itoa(r) + "）中有 " + itoa(rand.Intn(3)) + " 人是邪恶的"
	case "fortune_teller":
		verdict := "没有恶魔"
		if rand.Float64() < 0.5 {
			verdict = "有恶魔！"
		}
		return "🔮 你占卜的 " + joinNums(pair) + " 中" + verdict
	case "washerwoman":
		// 酒鬼假线索同样排除自身伪装角色，优先给不在场镇民
		onBoard := map[string]bool{}
		for _, q := range e.GD.Players {
			onBoard[q.Role] = true
		}
		pool := []string{}
		for _, rk := range GetRolesByTeam(TeamTownsfolk) {
			if rk == drunk.FakeRole || onBoard[rk] {
				continue
			}
			pool = append(pool, rk)
		}
		if len(pool) == 0 {
			for _, rk := range GetRolesByTeam(TeamTownsfolk) {
				if rk != drunk.FakeRole {
					pool = append(pool, rk)
				}
			}
		}
		rn := Roles[pool[rand.Intn(len(pool))]].Name
		return "🧺 你得知 " + joinNums(pair) + " 中有一人是【" + rn + "】"
	case "librarian":
		// 与真实情况相反：场上有外来者就说"没有"，没有就说"有"
		hasOutsider := false
		for _, q := range e.GD.Players {
			if q.Team == TeamOutsider {
				hasOutsider = true
				break
			}
		}
		if hasOutsider {
			return "📚 本局没有外来者"
		}
		return "📚 你得知 " + joinNums(pair) + " 中有一人是外来者"
	case "investigator":
		// 优先给不在场爪牙（指向不在场爪牙必然错误且合理）
		minions := GetRolesByTeam(TeamMinion)
		onBoard := map[string]bool{}
		for _, q := range e.GD.Players {
			onBoard[q.Role] = true
		}
		pool := []string{}
		for _, rk := range minions {
			if !onBoard[rk] {
				pool = append(pool, rk)
			}
		}
		if len(pool) == 0 {
			pool = minions
		}
		return "🔍 你得知 " + joinNums(pair) + " 中有一人是【" + Roles[pool[rand.Intn(len(pool))]].Name + "】"
	}
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// assignInfoClues 洗衣妇/图书管理员/调查员/厨师/间谍的首夜线索
func (e *Engine) assignInfoClues(players map[string]*PlayerState, townsfolk, outsiders, minions []string) {
	allPIDs := keysOf(players)
	rand.Shuffle(len(allPIDs), func(i, j int) { allPIDs[i], allPIDs[j] = allPIDs[j], allPIDs[i] })

	// 间谍：查看魔典（全量角色）
	for _, p := range players {
		if p.Role == "spy" {
			nums := []int{}
			for _, q := range players {
				nums = append(nums, q.Number)
			}
			sort.Ints(nums)
			lines := []string{}
			for _, n := range nums {
				q := e.PlayerByNum(n)
				name := q.RoleName
				if q.Role == "drunk" {
					name = "酒鬼" // 间谍看到真身
				}
				lines = append(lines, "#"+itoa(n)+" = "+name)
			}
			p.InfoClues = append(p.InfoClues, "🕶️ 魔典: "+join(lines, "，"))
		}
	}

	// 洗衣妇：两名玩家之一是指定的镇民
	for _, p := range players {
		if p.Role != "washerwoman" {
			continue
		}
		// 找一个在场上且不是信息位自己的镇民
		holderPID := ""
		roleKey := ""
		for pid, q := range players {
			for _, rk := range townsfolk {
				if q.Role == rk && rk != "washerwoman" {
					holderPID = pid
					roleKey = rk
					break
				}
			}
			if holderPID != "" {
				break
			}
		}
		if holderPID == "" {
			break
		}
		other := ""
		for _, pid := range allPIDs {
			if pid != holderPID {
				other = pid
				break
			}
		}
		p.InfoClues = append(p.InfoClues,
			"🧺 你得知 #"+itoa(players[holderPID].Number)+" 与 #"+itoa(players[other].Number)+
				" 中有一人是【"+Roles[roleKey].Name+"】")
	}

	// 图书管理员：两名玩家之一（或没有）外来者
	for _, p := range players {
		if p.Role != "librarian" {
			continue
		}
		if len(outsiders) == 0 {
			p.InfoClues = append(p.InfoClues, "📚 本局没有外来者")
			continue
		}
		outPID, otherPID := "", ""
		for pid, q := range players {
			if q.Team == TeamOutsider && outPID == "" {
				outPID = pid
			} else if otherPID == "" && pid != outPID {
				otherPID = pid
			}
		}
		if otherPID == "" {
			for _, pid := range allPIDs {
				if pid != outPID {
					otherPID = pid
					break
				}
			}
		}
		if outPID != "" && otherPID != "" {
			p.InfoClues = append(p.InfoClues,
				"📚 你得知 #"+itoa(players[outPID].Number)+" 与 #"+itoa(players[otherPID].Number)+" 中有一人是外来者")
		}
	}

	// 调查员：两名玩家之一是爪牙
	for _, p := range players {
		if p.Role != "investigator" {
			continue
		}
		minPID, otherPID := "", ""
		for pid, q := range players {
			if q.Team == TeamMinion && minPID == "" {
				minPID = pid
			} else if otherPID == "" && pid != minPID {
				otherPID = pid
			}
		}
		if otherPID == "" {
			for _, pid := range allPIDs {
				if pid != minPID {
					otherPID = pid
					break
				}
			}
		}
		if minPID != "" && otherPID != "" {
			p.InfoClues = append(p.InfoClues,
				"🔍 你得知 #"+itoa(players[minPID].Number)+" 与 #"+itoa(players[otherPID].Number)+" 中有一人是【"+players[minPID].RoleName+"】")
		}
	}

	// 厨师：相邻邪恶对数
	for _, p := range players {
		if p.Role != "chef" {
			continue
		}
		nums := []int{}
		for _, q := range players {
			nums = append(nums, q.Number)
		}
		sort.Ints(nums)
		pairs := 0
		for i := range nums {
			a := e.PlayerByNum(nums[i])
			b := e.PlayerByNum(nums[(i+1)%len(nums)])
			if a != nil && b != nil && a.FlagEvil && b.FlagEvil {
				pairs++
			}
		}
		p.InfoClues = append(p.InfoClues, "👨‍🍳 你得知有 "+itoa(pairs)+" 对相邻的玩家是邪恶阵营")
	}
}

// ==============================
// 胜负 & 辅助
// ==============================

// CheckWin 判定胜负：good / evil / 空
func (e *Engine) CheckWin() string {
	aliveDemons := 0
	aliveCount := 0
	aliveGood := 0
	mayorValid := false // 镇长存活、未中毒、非酒鬼
	for _, p := range e.GD.Players {
		if !p.Alive {
			continue
		}
		aliveCount++
		if p.Role == "imp" {
			aliveDemons++
		}
		if p.Role == "mayor" && !p.Poisoned && !p.Drunk {
			mayorValid = true
		}
		if p.Team == TeamTownsfolk || p.Team == TeamOutsider {
			aliveGood++
		}
	}
	if aliveDemons == 0 {
		return "good"
	}
	// 天亮后场上仅存活邪恶阵营玩家 → 邪恶胜
	if aliveGood == 0 {
		return "evil"
	}
	// 镇长日：仅剩 3 人存活、镇长健康（未中毒非酒鬼）、当天无人被处决 → 善良胜
	if aliveCount == 3 && mayorValid && !e.GD.Executed {
		return "good"
	}
	// 天亮后场上存活两名玩家且恶魔存活 → 邪恶胜
	if aliveCount <= 2 {
		return "evil"
	}
	return ""
}

// Neighbors 存活玩家的左右邻居编号
func (e *Engine) Neighbors(num int) (int, int) {
	nums := []int{}
	for _, p := range e.GD.Players {
		if p.Alive {
			nums = append(nums, p.Number)
		}
	}
	sort.Ints(nums)
	for i, n := range nums {
		if n == num {
			return nums[(i-1+len(nums))%len(nums)], nums[(i+1)%len(nums)]
		}
	}
	return 0, 0
}

// CleanTurn 回合清理（中毒/保护持续到黄昏，回合末清除）
func (e *Engine) CleanTurn() {
	for _, p := range e.GD.Players {
		p.Drunk = false
		p.Protected = false
		p.Poisoned = false
	}
}

// ==============================
// 工具函数
// ==============================

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sample(list []string, n int) []string {
	if n <= 0 || len(list) == 0 {
		return nil
	}
	if n > len(list) {
		n = len(list)
	}
	cp := append([]string{}, list...)
	rand.Shuffle(len(cp), func(i, j int) { cp[i], cp[j] = cp[j], cp[i] })
	return cp[:n]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func join(parts []string, sep string) string {
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

func joinNums(nums []int) string {
	parts := make([]string, 0, len(nums))
	for _, n := range nums {
		parts = append(parts, "#"+itoa(n))
	}
	return join(parts, ", ")
}

func intSlice(v any) ([]int, bool) {
	switch arr := v.(type) {
	case []int:
		return arr, true
	case []any:
		out := make([]int, 0, len(arr))
		for _, x := range arr {
			switch n := x.(type) {
			case int:
				out = append(out, n)
			case float64:
				out = append(out, int(n))
			default:
				return nil, false
			}
		}
		return out, true
	}
	return nil, false
}
