package engine

// 对抗性压力测试套件（源自王伟三压力场景：极端发言密度/长时记忆负荷/高欺骗性环境
// + 谢斌随机扰动思想：异常输入与状态机乱序调用下引擎不崩溃、状态一致）

import (
	"testing"
)

// newStressEngine 构造全 AI 对局引擎（8 人固定配板，含均衡的邪恶组合）
func newStressEngine(t *testing.T, board []string) *Engine {
	t.Helper()
	gd := &GameData{
		Players:    map[string]*PlayerState{},
		ForceRoles: board,
	}
	for i := 0; i < len(board); i++ {
		pid := "p" + itoa(i+1)
		gd.Players[pid] = &PlayerState{Number: i + 1, IsAI: true, Alive: true}
	}
	e := NewEngine(gd, "stress", func(event string, data any) {})
	e.StartGame()
	if e.GD.Phase != "night" {
		t.Fatalf("StartGame 后应为 night，实际 %s", e.GD.Phase)
	}
	return e
}

// runNights 把夜晚逐步推完（StepNight 直到天亮 day）
func runNights(e *Engine) {
	for {
		r := e.AdvancePhase()
		if r["phase"] != "night" {
			return
		}
	}
}

// runDay 推进到提名阶段：day→private_chat→public_chat→nomination
func runDay(e *Engine) {
	e.AdvancePhase() // day -> private
	e.AdvancePhase() // private -> public
	e.AdvancePhase() // public -> nomination
}

// TestStress_LongMemory 长时记忆负荷：连续推进 20 轮，验证引擎状态一致、无死锁残留
func TestStress_LongMemory(t *testing.T) {
	board := []string{"imp", "poisoner", "recluse", "washerwoman", "chef", "empath", "monk", "soldier"}
	e := newStressEngine(t, board)
	for day := 0; day < 20; day++ {
		runNights(e)
		if e.GD.Phase != "day" {
			t.Fatalf("第%d轮：夜晚推进后应为 day，实际 %s", day+1, e.GD.Phase)
		}
		runDay(e)
		if e.GD.Phase != "nomination" {
			t.Fatalf("第%d轮：白天推进后应为 nomination，实际 %s", day+1, e.GD.Phase)
		}
		res := e.ResolveNominationPeriod()
		if res["game_over"] == true {
			t.Logf("第%d轮对局正常结束：%v", day+1, res["winner"])
			return
		}
		e.AdvancePhase() // nomination -> night
		if e.GD.Phase != "night" {
			t.Fatalf("第%d轮：提名结束应入夜，实际 %s", day+1, e.GD.Phase)
		}
		if e.GD.ActiveVote != nil {
			t.Fatalf("第%d轮：入夜后残留活跃投票（死锁风险）", day+1)
		}
		if e.GD.PendingChoice != nil {
			t.Fatalf("第%d轮：入夜后残留挂起选择（死锁风险）", day+1)
		}
	}
	t.Logf("20 轮长时推进通过：当前第 %d 天，存活 %d 人", e.GD.Day, len(e.Alive()))
}

// TestStress_HighDeception 高欺骗性环境：4 邪恶 + 4 干扰角色极端配板，引擎不崩溃且胜负判定合法
func TestStress_HighDeception(t *testing.T) {
	// 男爵+间谍+投毒者+红唇女郎+恶魔 = 5 邪恶？配板必须含 1 恶魔（imp 或 scarlet_woman 由 AssignRoles 决定）
	// 此处用 4 邪恶极端板：imp + 3 爪牙 + 外来者干扰
	board := []string{"imp", "spy", "baron", "poisoner", "drunk", "recluse", "saint", "chef"}
	e := newStressEngine(t, board)
	evil := 0
	for _, p := range e.GD.Players {
		if p.Team == "minion" || p.Team == "demon" {
			evil++
		}
	}
	if evil != 4 {
		t.Fatalf("高欺骗配板应有 4 名邪恶方，实际 %d", evil)
	}
	for day := 0; day < 8; day++ {
		runNights(e)
		if win := e.CheckWin(); win != "" {
			if win != "good" && win != "evil" {
				t.Fatalf("非法胜负判定：%s", win)
			}
			t.Logf("高欺骗环境第%d轮终局：%s", day+1, win)
			return
		}
		runDay(e)
		res := e.ResolveNominationPeriod()
		if res["game_over"] == true {
			if w, _ := res["winner"].(string); w != "good" && w != "evil" {
				t.Fatalf("非法胜负判定：%v", res["winner"])
			}
			t.Logf("高欺骗环境第%d轮终局：%v", day+1, res["winner"])
			return
		}
		e.AdvancePhase()
	}
	t.Logf("高欺骗环境 8 轮推进通过，无崩溃")
}

// TestStress_VoteStateMachine 投票状态机压力：反复切换/取消/重复提名/非法提名
func TestStress_VoteStateMachine(t *testing.T) {
	board := []string{"imp", "poisoner", "recluse", "washerwoman", "chef", "empath", "monk", "soldier"}
	e := newStressEngine(t, board)
	runNights(e)
	runDay(e)
	alive := e.AliveNums(0)
	target := alive[1]

	// 1. 合法提名：成功且进入辩论期
	r := e.Nominate(alive[0], target)
	if r["nominated"] != true {
		t.Fatalf("合法提名失败：%v", r["message"])
	}
	// 1.5 辩论期结束，启动投票
	e.GD.ActiveVote.DebateDeadline = 0
	if res, _ := e.StartVoting(); res == nil || res["vote_start"] != true {
		t.Fatalf("启动投票失败：%v", res)
	}
	// 2. 投票进行中新提名：应被拒绝
	r = e.Nominate(alive[3], alive[4])
	if r["success"] == true {
		t.Fatalf("投票期间新提名应被拒绝")
	}
	// 3. 投票反复切换：yes → no → yes → 取消
	if _, _, err := e.CastVote(alive[2], "yes"); err != nil {
		t.Fatalf("投票失败：%v", err)
	}
	if _, _, err := e.CastVote(alive[2], "no"); err != nil {
		t.Fatalf("切换弃权票失败：%v", err)
	}
	if _, _, err := e.CastVote(alive[2], "yes"); err != nil {
		t.Fatalf("切换赞成票失败：%v", err)
	}
	if _, _, err := e.CastVote(alive[2], "abstain"); err != nil {
		t.Fatalf("取消投票失败：%v", err)
	}
	// 4. 乱序结束投票：不崩溃
	if res := e.FinishVote(); res == nil {
		t.Fatalf("FinishVote 返回 nil")
	}
	// 5. 同一天内：提名者重复提名应失败
	r = e.Nominate(alive[0], alive[4])
	if r["success"] == true {
		t.Fatalf("同一天重复提名应失败")
	}
	// 6. 被提名过的目标再被提名应失败
	r = e.Nominate(alive[3], target)
	if r["success"] == true {
		t.Fatalf("被提名过的目标应无法再次被提名")
	}
	// 7. 结束提名期（含处决判定）
	res := e.ResolveNominationPeriod()
	if res == nil {
		t.Fatalf("ResolveNominationPeriod 返回 nil")
	}
	if res["game_over"] == true {
		t.Logf("投票压力测试中对局提前结束（正常）：%v", res["winner"])
		return
	}
	// 8. 入夜 → 第二天：提名已死玩家路径
	e.AdvancePhase() // 入夜
	runNights(e)
	// 强制制造一名死亡玩家
	deadNum := -1
	for _, p := range e.GD.Players {
		if !p.Alive {
			deadNum = p.Number
			break
		}
	}
	if deadNum == -1 {
		for _, n := range e.AliveNums(0) {
			e.markNightDeath(n)
			deadNum = n
			break
		}
		e.ApplyDeaths()
	}
	runDay(e)
	fromNum := -1
	for _, n := range e.AliveNums(0) {
		fromNum = n
		break
	}
	if fromNum != -1 {
		r = e.Nominate(fromNum, deadNum)
		// 提名死人不崩溃即可（成功与否取决于引擎语义）
		if r["game_over"] == true {
			t.Logf("提名死人路径触发终局：%v", r["winner"])
		} else {
			res = e.ResolveNominationPeriod()
			if res["game_over"] == true {
				t.Logf("提名死人处决期终局：%v", res["winner"])
			}
		}
	}
	t.Logf("投票状态机压力测试通过")
}

// TestStress_ExtremeBoardRotation 极端配板轮转：全部 5 组均衡板 + 全随机板各跑 3 天
func TestStress_ExtremeBoardRotation(t *testing.T) {
	boards := [][]string{
		{"imp", "poisoner", "recluse", "washerwoman", "chef", "empath", "monk", "soldier"},
		{"imp", "spy", "saint", "librarian", "fortune_teller", "mayor", "butler", "slayer"},
		{"imp", "scarlet_woman", "drunk", "investigator", "ravenkeeper", "virgin", "soldier", "chef"},
		{"imp", "baron", "recluse", "saint", "drunk", "washerwoman", "chef", "soldier"},
		{"imp", "spy", "saint", "chef", "empath", "mayor", "virgin", "slayer"},
	}
	for i, board := range boards {
		e := newStressEngine(t, board)
		for day := 0; day < 3; day++ {
			runNights(e)
			if win := e.CheckWin(); win != "" {
				break
			}
			runDay(e)
			res := e.ResolveNominationPeriod()
			if res["game_over"] == true {
				break
			}
			e.AdvancePhase()
		}
		t.Logf("配板 %d 推进完成（存活 %d 人）", i+1, len(e.Alive()))
	}
}
