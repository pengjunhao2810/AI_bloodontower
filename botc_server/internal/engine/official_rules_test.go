package engine

import (
	"testing"
	"time"
)

// 管家投票限制（官方魔典）：主人未表态 → 管家票不计入统计；主人表态后/管家中毒 → 计入
func TestButlerVoteRestriction(t *testing.T) {
	e := makePoisonGame(t)
	bt := e.GD.Players["2"]
	bt.Role = "butler"
	bt.RoleName = "管家"
	bt.IsAI = false
	bt.AIType = ""
	bt.ButlerMaster = "3" // 主人 #3
	// 主人也设为真人（避免 StartVoting 时 AI 自动投导致"主人已投"）
	e.GD.Players["3"].IsAI = false
	e.GD.Players["3"].AIType = ""
	e.Nominate(1, 5)
	e.StartVoting()

	// 主人未表态：管家可以投出票，但统计时不计数
	if _, _, err := e.CastVote(3, "yes"); err != nil {
		t.Fatalf("管家应能投出票（仅不计入统计）: %v", err)
	}
	// 结算统计：主人未表态 → 管家的票不在 yes_nums 里
	e.GD.ActiveVote.VoteDeadline = time.Now().Unix() - 10
	res := e.FinishVote()
	for _, n := range res["yes_nums"].([]int) {
		if n == bt.Number {
			t.Fatalf("主人未表态时管家票不应计数, yes_nums=%v", res["yes_nums"])
		}
	}
	// 主人先表态：管家可投且计入
	e2 := makePoisonGame(t)
	bt2 := e2.GD.Players["2"]
	bt2.Role = "butler"
	bt2.RoleName = "管家"
	bt2.IsAI = false
	bt2.AIType = ""
	bt2.ButlerMaster = "3"
	e2.GD.Players["3"].IsAI = false
	e2.GD.Players["3"].AIType = ""
	e2.Nominate(1, 5)
	e2.StartVoting()
	if _, _, err := e2.CastVote(4, "yes"); err != nil { // 主人 #4 先投
		t.Fatalf("主人投票应成功: %v", err)
	}
	if _, _, err := e2.CastVote(3, "yes"); err != nil {
		t.Fatalf("主人已投票后管家应可投: %v", err)
	}
	// 主人死亡：限制解除
	e.FinishVote()
	e.GD.NomCooldownUntil = 0
	e.Nominate(2, 4) // #4 未被提名过（此前提名的是 #5）
	e.StartVoting()
	e.GD.Players["3"].Alive = false // 主人死亡
	e.GD.Players["3"].DeadVote = true
	if _, _, err := e.CastVote(3, "yes"); err != nil {
		t.Fatalf("主人死亡后管家应可自由投票: %v", err)
	}
}

// 镇长弹刀：恶魔攻击镇长 → 50% 概率另一名玩家代替死亡（镇长存活）
func TestMayorBounce(t *testing.T) {
	bounced := 0
	for i := 0; i < 50; i++ {
		e := makePoisonGame(t)
		mayor := e.GD.Players["4"]
		mayor.Role = "mayor"
		mayor.RoleName = "镇长"
		// 恶魔攻击镇长
		action := e.DoAction("0", "imp", false)
		_ = action
		if mayor.Alive {
			bounced++
			// 弹刀必须有替死鬼死亡
			dead := 0
			for _, p := range e.GD.Players {
				if !p.Alive {
					dead++
				}
			}
			if dead == 0 {
				t.Fatal("弹刀后应有另一名玩家死亡")
			}
		}
	}
	// 50 次中弹刀概率应显著存在（0.5^50 几乎不可能全失败）
	if bounced == 0 {
		t.Fatal("50 次攻击从未弹刀，概率异常")
	}
	if bounced == 50 {
		t.Fatal("50 次攻击全部弹刀，概率异常")
	}
}
