"""
====================================================================
 血染钟楼 AI 玩家推理服务（战术体系重构版）
 独立进程，只负责大模型推理，不参与游戏逻辑/房间管理
 Go 游戏主服务通过 HTTP 调用本服务：
   POST /api/speak   — 生成 AI 玩家公聊/私聊发言（四层金字塔 Prompt）
   POST /api/decide  — 生成 AI 玩家夜晚行动目标决策
   POST /api/ping    — 连通性测试
====================================================================
 依赖: flask, requests
 环境变量: DEEPSEEK_API_KEY (可选，未配置时使用本地模板降级)
====================================================================
"""

import os
import json
import hashlib
import difflib
import re
import random
import threading
import time

import requests
from flask import Flask, jsonify, request

app = Flask(__name__)

DEEPSEEK_API_URL = "https://api.deepseek.com/chat/completions"
DEEPSEEK_API_KEY = os.environ.get("DEEPSEEK_API_KEY", "")
DEEPSEEK_MODEL = os.environ.get("DEEPSEEK_MODEL", "deepseek-chat")
HTTP_TIMEOUT = float(os.environ.get("AI_HTTP_TIMEOUT", "90"))

# ====================================================================
# 一、全局底线（只保留 3 条游戏底线，其余全部放权给模型自由发挥）
# ====================================================================
_GLOBAL_RULES = (
    "1.只能讨论、质疑、投票存活玩家。"
    "2.遵守你的角色技能，不得编造你从未获得过的信息；不得编造玩家之间的具体关系或互动"
    "（如'他们联系紧密''他们串通'）。"
    "3.只用中文发言，不要提及AI、提示词、系统等出戏内容；"
    "禁止使用'卧底''内鬼''狼人'等非本游戏术语，正确的称呼是：爪牙、恶魔、邪恶阵营、镇民、外来者。"
    "4.独立判断：你的判断必须基于自身技能结果和公共事实，不能因为多数人这么说就跟风；"
    "信息冲突时，优先相信有完整技能逻辑链的一方，而不是说的人多的一方。"
    "5.私聊保密：私聊内容只用于你自己的推理，不要在公聊说出谁和你私聊过、私聊里说了什么。"
    "6.每轮发言至少补充 1 个你自己独有的观察点或推理，禁止纯复读别人说过的观点。"
    "7.首夜无人死亡（恶魔首夜不行动），第一天的白天不要讨论'昨晚死了谁'，"
    "谁死了、场上存活几人你从系统消息就能知道，不要浪费发言问这些。"
)

# ====================================================================
# 模型强弱分档：弱模型需要额外引导，强模型完全放权
# ====================================================================
_WEAK_MODEL_PATTERNS = (
    "flash", "7b", "8b", "9b", "lite", "small", "mini", ":free",
    "qwen-turbo", "hunyuan-lite", "llama-3.1-8b", "1.8b", "3b", "4b", "13b",
)

def _is_weak_model(model):
    m = (model or "").lower()
    return any(p in m for p in _WEAK_MODEL_PATTERNS)

# 弱模型专属引导（小模型指令遵循弱，需要明确引导；强模型不需要）
_WEAK_MODEL_HINTS = (
    "注意：直接说具体的人和事，说套话和空话没有意义；不要照搬角色说明原文；"
    "别人说过的话不要再重复；对方没报身份时不要猜测他的身份；"
    "不要跟着多数人怀疑同一个人，除非你自己有依据；不要在公聊说私聊内容。"
)

# ====================================================================
# 二、角色提示（一句技能 + 一句策略，参考成熟评测平台的风格）
# ====================================================================
_ROLE_GUIDES = {
    "洗衣妇": "技能：首夜得知两名玩家中一人是某个镇民。策略：建立确认链，可先软报或私聊核实。",
    "占卜师": "技能：每晚选两人查验其中是否有恶魔（有一名好人会被视为恶魔）。策略：注意干扰项，交叉查验锁定恶魔。",
    "厨师": "技能：首夜得知相邻邪恶玩家的对数。策略：数字0/1/2用于座位推理，注意陌客/间谍带来的偏差。",
    "共情者": "技能：每晚得知左右相邻存活玩家中的邪恶人数。策略：可逐步处决邻座扩展探测范围，易成为投毒目标。",
    "调查员": "技能：首夜得知两名玩家中一人是某个爪牙。策略：优先处理高危爪牙，信息公开后可考虑接受处决。",
    "图书管理员": "技能：首夜得知两名玩家中一人是外来者，或没有外来者。策略：关注外来者数量与男爵可能，可用来定位酒鬼。",
    "僧侣": "技能：每晚保护一名玩家免受恶魔杀害。策略：优先保护强信息位，平安夜不一定等于守对。",
    "镇长": "技能：投票多一票；仅剩3人且无人处决时善良获胜。策略：可推进三人局获胜，替死可能暴露身份。",
    "士兵": "技能：不会死于恶魔的刀。策略：可诱导恶魔空刀，也常被恶魔假跳。",
    "送葬者": "技能：每晚得知白天被处决者的角色。策略：用于验证别人跳的身份与酒鬼，尽量保持存活。",
    "猎手": "技能：每局一次，白天公开射杀一人，若为恶魔则其死亡。策略：锁定高嫌疑目标后，在公聊直接说'我是猎手，我要开枪杀#X'来开枪验证。",
    "守鸦人": "技能：若夜晚死亡，可查验一名玩家角色。策略：可伪装高威胁善良身份骗刀。",
    "贞洁者": "技能：首次被镇民提名时，提名者被处决。策略：找白板镇民提名自证，代价高需择机。",
    "圣徒": "技能：若被处决，善良阵营立刻失败。策略：尽早声明避免被误处决。",
    "酒鬼": "技能：你以为自己是某个镇民，但实际不是；你的信息可能错误，但你坚信。策略：信息冲突时考虑自己是酒鬼。",
    "管家": "技能：每晚选主人，次日只能跟随主人投票。策略：选择可信主人保持票权。",
    "陌客": "技能：你可能被当作爪牙或恶魔。策略：污染信息，可考虑尽早被处决减少干扰。",
    "小恶魔": "技能：每晚杀一人；必要时可自杀传刀给爪牙。策略：优先击杀强信息位。",
    "红唇女郎": "技能：存活人数不少于5时恶魔死亡，你成为小恶魔。策略：保持低调，随时准备接刀。",
    "男爵": "技能：场上多出2名外来者（替换2名镇民）。策略：扰乱外来者数量，可伪装外来者。",
    "下毒者": "技能：每晚毒一人，其技能失效信息可能出错。策略：重点毒强信息位。",
    "间谍": "技能：可查看魔典（知道全场每个玩家的真实角色）。策略：私聊把魔典完整告诉恶魔——谁在什么位置、谁是威胁最大的信息位，让恶魔据此定刀人顺序；自己伪装强信息位、帮恶魔伪装镇长等功能位。",
}

def get_role_guide(role_name):
    return _ROLE_GUIDES.get(role_name, "")

# ====================================================================
# 三、邪恶阵营：指挥执行体系（压缩版）
# ====================================================================
_EVIL_PLAYBOOK = {
    "小恶魔": "你是邪恶阵营指挥官：每晚杀一人，优先击杀强信息位。",
    "红唇女郎": "恶魔死亡后你接任小恶魔：前期低调接刀。",
    "男爵": "场上多出2个外来者：可利用数量差搅局。",
    "下毒者": "每晚毒一人：重点毒强信息位。",
}

# 恶魔指挥义务（最高优先级）
_DEMON_COMMAND = (
    "你是邪恶阵营指挥官：每个白天私聊阶段必须主动找每个爪牙私聊，"
    "必须给每个爪牙分配一个互不重复的伪装身份（优先用不在场镇民名单），"
    "必须下达明确指令（本轮抗推谁/谁带队/谁隐身）；刀人后同步意图。你的指令是最高优先级，爪牙必须执行。",
)

# 爪牙服从规则
_MINION_OBEY = (
    "恶魔是你的老大，100%服从：确认伪装身份、汇报观察、报告技能使用、请示指令。"
    "每个白天私聊阶段主动找恶魔私聊，向老大索要伪装身份和本轮指令。"
    "当恶魔或爪牙队友被好人集火时，你必须主动转移焦点——抛出新的怀疑目标、指出其他好人的疑点，"
    "禁止跟着好人一起踩自己的队友。",
)

# 邪恶阵营通用准则
_EVIL_TEAM_RULES = (
    "邪恶阵营共同编织一套不互斥的虚假身份网，避免硬撞身份；"
    "目标：保护恶魔，制造信息冲突误导好人，必要时牺牲爪牙保护恶魔；"
    "不要向好人自曝邪恶阵营或暴露队友。",
)

# 按角色专属战术
_EVIL_ROLE_TACTICS = {
    "男爵": "可暗示外来者数量不对来引导好人内耗。每轮公聊至少主动提及一次外来者数量问题，主动制造新的争议点，禁止全程跟着好人的话题走。",
    "下毒者": "毒真信息位后，伪装同身份报相反信息反问对方是否被下毒。",
}

# 对好人的伪装技巧
_EVIL_FRAUD_TACTICS = (
    "伪装技巧：先听对方报什么再决定自己身份；假信息半真半假带具体编号；被质疑时装委屈。",
)

# ====================================================================
# 四、好人阵营协作逻辑（压缩版）
# ====================================================================
_GOOD_TEAM_LOGIC = (
    "好人不必全盘托出，避免被恶魔夜刀；信息矛盾时考虑醉酒/中毒；票型异常可作为线索。"
    "信息冲突时，优先相信有完整技能逻辑链的一方，而不是说的人多的一方；"
    "禁止仅凭'过早明牌可疑'这类主观理由投票，必须绑定具体的信息矛盾点。"
)

_INFO_ROLE_GUIDE = (
    "你是信息角色：可较早报身份与信息，但可保留不确定性。"
)

def get_good_team_logic(role_name):
    info_roles = ("洗衣妇", "厨师", "调查员", "图书管理员", "共情者", "占卜师", "守鸦人", "送葬者")
    logic = _GOOD_TEAM_LOGIC
    if role_name in info_roles:
        logic += _INFO_ROLE_GUIDE
    return logic

def get_evil_playbook(team, role_name):
    if team not in ("minion", "demon"):
        return ""
    book = _EVIL_PLAYBOOK.get(role_name, "")
    if role_name == "小恶魔":
        book += "".join(_DEMON_COMMAND)
    if team == "minion":
        book += "".join(_MINION_OBEY)
    book += "".join(_EVIL_TEAM_RULES) + _EVIL_ROLE_TACTICS.get(role_name, "") + "".join(_EVIL_FRAUD_TACTICS)
    return book

# ====================================================================
# 六、结构化推理笔记（公开身份/怀疑/信任/战术目标）
# ====================================================================
_NOTES_LOCK = threading.Lock()
_AI_NOTES = {}  # (room, number) -> {"identity": str, "suspect": str, "trust": str, "tactic": str, "log": [str]}

def _get_notes(room, number):
    key = (str(room), str(number))
    with _NOTES_LOCK:
        if key not in _AI_NOTES:
            _AI_NOTES[key] = {"identity": "", "suspect": "", "trust": "", "tactic": "", "log": []}
        return _AI_NOTES[key]

def _add_log(room, number, note, day=None):
    if not note:
        return
    if day is not None:
        note = "[D" + str(int(day) + 1) + "] " + note
    n = _get_notes(room, number)
    with _NOTES_LOCK:
        if note not in n["log"]:
            n["log"].append(note)
            if len(n["log"]) > 20:
                del n["log"][: len(n["log"]) - 20]

def _record_observations(room, number, context, my_clues, day=None):
    for m in context[-8:]:
        num = m.get("num")
        content = m.get("content", "")
        if not content:
            continue
        if num == 0 or str(num) == "0":
            _add_log(room, number, f"系统事件：{content}", day)
        else:
            _add_log(room, number, f"听到 #{num} 说：{content}", day)
    for c in my_clues[-6:]:
        _add_log(room, number, f"说书人告诉我：{c}", day)

def _notes_text(room, number):
    n = _get_notes(room, number)
    with _NOTES_LOCK:
        identity = n["identity"]
        suspect = n["suspect"]
        trust = n["trust"]
        tactic = n["tactic"]
        recent = list(n["log"][-8:])
    parts = []
    if identity:
        parts.append("你的公开身份：" + identity)
    if suspect:
        parts.append("你怀疑的人：" + suspect)
    if trust:
        parts.append("你信任的人：" + trust)
    if tactic:
        parts.append("你的战术目标：" + tactic)
    if recent:
        parts.append("最近记录：" + "；".join(recent))
    if not parts:
        return ""
    return "【你的记忆】" + "；".join(parts)

# 从发言中提取声称身份，更新结构化笔记
def _extract_identity_update(room, number, reply):
    idents = ["洗衣妇", "占卜师", "厨师", "共情者", "调查员", "图书管理员", "僧侣", "镇长",
              "士兵", "送葬者", "猎手", "守鸦人", "贞洁者", "圣徒", "管家", "陌客",
              "小恶魔", "红唇女郎", "男爵", "下毒者", "酒鬼"]
    found = None
    for ident in idents:
        if ident in reply:
            found = ident
            break
    if found:
        n = _get_notes(room, number)
        with _NOTES_LOCK:
            if not n["identity"]:
                n["identity"] = found

# ====================================================================
# 结构化判断记忆：发言后提炼"怀疑谁/信任谁"持续写回笔记（Samuel 建议的核心缺口）
# ====================================================================
def _update_structured_notes(room, number, reply, context, llm_conf):
    """后台线程：从刚说的话+场上对话提炼当前怀疑/信任对象，覆盖式写回结构化字段"""
    msgs = [m for m in context[-8:] if m.get("content")]
    hist = "；".join(f"#{m.get('num')}: {m.get('content')}" for m in msgs)
    prompt = (
        f"你是玩家#{number}，你刚说：{reply}。"
        f"最近场上对话：{hist if hist else '无'}。"
        f"根据你现在的判断，用一句话分别说明：1)你当前最怀疑谁（编号+简短理由，没有就写'无'）；"
        f"2)你当前最信任谁（编号+简短理由，没有就写'无'）。"
        f"严格按这个格式输出，不要多余内容：\n怀疑：...\n信任：..."
    )
    result = _ask_deepseek([
        {"role": "system", "content": "只输出两行：怀疑：xxx 和 信任：xxx。"},
        {"role": "user", "content": prompt},
    ], llm_conf, temperature=0.3, max_tokens=120, room_code=room)
    if not result:
        return
    suspect = trust = ""
    for line in result.splitlines():
        line = line.strip()
        if line.startswith("怀疑") or line.startswith("怀疑："):
            suspect = line.split("：", 1)[-1].strip() if "：" in line else line[2:].strip()
        elif line.startswith("信任") or line.startswith("信任："):
            trust = line.split("：", 1)[-1].strip() if "：" in line else line[2:].strip()
    if not suspect and not trust:
        return
    n = _get_notes(room, number)
    with _NOTES_LOCK:
        if suspect and suspect != "无":
            n["suspect"] = suspect
        elif suspect == "无":
            n["suspect"] = ""
        if trust and trust != "无":
            n["trust"] = trust
        elif trust == "无":
            n["trust"] = ""

# ====================================================================
# 私聊自动总结记忆：私聊对话后异步提炼要点写入笔记，供后续引用
# ====================================================================
def _summarize_private(room, number, other_num, context, llm_conf):
    """后台线程：用一句话总结本次私聊（对方身份/关键信息/约定），存入结构化笔记"""
    if not other_num:
        return
    msgs = [m for m in context[-8:] if m.get("content")]
    if not msgs:
        return
    hist = "；".join(f"#{m.get('num')}: {m.get('content')}" for m in msgs)
    prompt = (
        f"你是玩家#{number}，刚和 #{other_num} 私聊。对话记录：{hist}。"
        f"用一句话（40字内）总结这次私聊对你最重要的信息："
        f"对方声称的身份、对方提供的关键信息（含编号数字）、你们的约定或战术。"
        f"只输出总结本身，用第一人称。"
    )
    summary = _ask_deepseek([
        {"role": "system", "content": "只输出一句话总结。"},
        {"role": "user", "content": prompt},
    ], llm_conf, temperature=0.3, max_tokens=120, room_code=room)
    if not summary:
        return
    n = _get_notes(room, number)
    with _NOTES_LOCK:
        n.setdefault("summaries", {})
        arr = n["summaries"].setdefault(str(other_num), [])
        if summary not in arr:
            arr.append(summary)
        if len(arr) > 4:
            del arr[: len(arr) - 4]

def _chat_summary_text(room, number, other_num=None):
    """注入用：私聊时给当前对象的总结；公聊时给所有对象的最新总结"""
    n = _get_notes(room, number)
    with _NOTES_LOCK:
        summaries = n.get("summaries", {})
        if other_num:
            arr = summaries.get(str(other_num), [])
            if arr:
                return "【你和 #%s 的私聊记忆】%s" % (other_num, "；".join(arr[-3:]))
        else:
            parts = []
            for k, arr in list(summaries.items())[-4:]:
                if arr:
                    parts.append("与#" + k + "：" + arr[-1])
            if parts:
                return "【你的私聊记忆·仅内部参考，不得公开说出】" + "；".join(parts)
    return ""

# ====================================================================
# 七、LLM 调用（温度分级 + max_tokens 220 + token 用量统计）
# ====================================================================
_USAGE_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "usage.json")
_USAGE_LOCK = threading.Lock()
_USAGE = {}  # room_code -> {model: {"prompt": n, "completion": n, "total": n, "calls": n}}

def _load_usage():
    global _USAGE
    try:
        with open(_USAGE_FILE, "r", encoding="utf-8") as f:
            _USAGE = json.load(f) or {}
    except Exception:
        _USAGE = {}

def _save_usage():
    with _USAGE_LOCK:
        try:
            with open(_USAGE_FILE, "w", encoding="utf-8") as f:
                json.dump(_USAGE, f, ensure_ascii=False)
        except Exception:
            pass

def _record_usage(room_code, info):
    if not info:
        return
    prompt = int(info.get("prompt_tokens", 0) or 0)
    completion = int(info.get("completion_tokens", 0) or 0)
    model = info.get("model", "unknown")
    with _USAGE_LOCK:
        room = _USAGE.setdefault(str(room_code), {})
        m = room.setdefault(model, {"prompt": 0, "completion": 0, "total": 0, "calls": 0})
        m["prompt"] += prompt
        m["completion"] += completion
        m["total"] += prompt + completion
        m["calls"] += 1
    _save_usage()

def _resolve_llm(llm_conf):
    key, base, model = "", "", ""
    if llm_conf:
        try:
            cfg = json.loads(llm_conf or "{}")
            key = cfg.get("api_key", "")
            base = cfg.get("base_url", "")
            model = cfg.get("model", "")
        except Exception:
            pass
    if not base:
        base = os.environ.get("LLM_BASE_URL", "")
    if not model:
        model = os.environ.get("LLM_MODEL", "")
    if not key:
        key = DEEPSEEK_API_KEY
    if not base:
        base = DEEPSEEK_API_URL
    if not model:
        model = DEEPSEEK_MODEL
    if not key:
        return None, None, None
    return _chat_url(base), key, model

def _chat_url(base):
    base = (base or "").rstrip("/")
    if base.endswith("/chat/completions"):
        return base
    if base.endswith("/v1"):
        return base + "/chat/completions"
    return base + "/chat/completions"

# ====================================================================
# Token 用量统计（全局 + 按房间 + 按模型，线程安全）
# ====================================================================
_USAGE_LOCK = threading.Lock()
_USAGE = {
    "total": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "calls": 0},
    "rooms": {},   # room_code -> 同结构
    "models": {},  # model -> 同结构
}

def _record_usage(room_code, model, usage):
    def add(target):
        target["prompt_tokens"] += usage.get("prompt_tokens", 0) or 0
        target["completion_tokens"] += usage.get("completion_tokens", 0) or 0
        target["total_tokens"] += usage.get("total_tokens", 0) or 0
        target["calls"] += 1
    with _USAGE_LOCK:
        add(_USAGE["total"])
        if model:
            _USAGE["models"].setdefault(model, {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "calls": 0})
            add(_USAGE["models"][model])
        if room_code:
            _USAGE["rooms"].setdefault(str(room_code), {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "calls": 0})
            add(_USAGE["rooms"][str(room_code)])

def _usage_snapshot():
    with _USAGE_LOCK:
        return json.loads(json.dumps(_USAGE, ensure_ascii=False))

# ====================================================================
# LLM 调用全局限流：免费 key 有组织级并发限制（约 5-6 并发），
# 私聊回复/总结线程/公聊发言同时调用时超限会被服务端挂起导致 Go 超时
# ====================================================================
_LLM_SEM = threading.Semaphore(6)

def _ask_deepseek(messages, llm_conf="", temperature=0.8, max_tokens=220, room_code=""):
    url, key, model = _resolve_llm(llm_conf)
    if not url or not key:
        return None
    if not _LLM_SEM.acquire(timeout=40):
        return None
    try:
        return _ask_deepseek_locked(messages, url, key, model, temperature, max_tokens, room_code)
    finally:
        _LLM_SEM.release()

def _ask_deepseek_locked(messages, url, key, model, temperature, max_tokens, room_code):
    headers = {"Authorization": f"Bearer {key}", "Content-Type": "application/json"}
    # 尝试序列：默认参数 → 强制 temperature=1（部分推理模型只接受 1）→ 加大输出上限（推理模型思考消耗大）
    attempts = [(temperature, max_tokens)]
    if abs(temperature - 1.0) > 0.01:
        attempts.append((1.0, max_tokens))
    attempts.append((1.0, max_tokens * 3))
    seen = set()
    for temp, mt in attempts:
        if (temp, mt) in seen:
            continue
        seen.add((temp, mt))
        try:
            resp = requests.post(
                url,
                headers=headers,
                json={
                    "model": model,
                    "messages": messages,
                    "temperature": temp,
                    "max_tokens": mt,
                },
                timeout=HTTP_TIMEOUT,
            )
            data = resp.json()
            if "choices" in data and data["choices"]:
                content = data["choices"][0]["message"]["content"].strip()
                if content:
                    _record_usage(room_code, model, data.get("usage") or {})
                    return content
                continue  # 内容为空（推理模型 token 耗尽）→ 下一轮更大上限
            err_body = json.dumps(data, ensure_ascii=False)
            if "temperature" in err_body and temp != 1.0:
                continue  # 该模型只接受 temperature=1 → 下一轮重试
            try:
                with open("ai_err.log", "a", encoding="utf-8") as f:
                    f.write(f"{time.strftime('%H:%M:%S')} model={model} status={resp.status_code} body={err_body[:400]}\n")
            except Exception:
                pass
            return None
        except Exception as e:
            try:
                with open("ai_err.log", "a", encoding="utf-8") as f:
                    f.write(f"{time.strftime('%H:%M:%S')} model={model} err={str(e)[:300]}\n")
            except Exception:
                pass
            return None
    return None

def _real_clues(my_clues):
    """过滤身份/技能通知，只保留真正的查验线索"""
    out = []
    for c in my_clues:
        s = str(c)
        if s.startswith("🎭") or s.startswith("📋"):
            continue
        out.append(s)
    return out

def _dedup_context(context, limit=6):
    """对话上下文去重：完全重复的发言只注入一次，防止重复谎言效应淹没 AI 判断"""
    seen = set()
    out = []
    for m in context[-12:]:
        c = str(m.get("content", "")).strip()
        if not c or c in seen:
            continue
        seen.add(c)
        out.append(m)
    return out[-limit:]

# 暗流涌动官方板子配置（总人数 -> 镇民/外来者/爪牙/恶魔）；男爵会 +2 外来者 -2 镇民
_PLAYER_TABLE = {
    5: (3, 0, 1, 1), 6: (3, 1, 1, 1), 7: (5, 0, 1, 1), 8: (5, 1, 1, 1),
    9: (5, 2, 1, 1), 10: (7, 0, 2, 1), 11: (7, 1, 2, 1), 12: (7, 2, 2, 1),
}

def _board_note(total):
    if total not in _PLAYER_TABLE:
        return ""
    tf, out, mn, dm = _PLAYER_TABLE[total]
    return (
        f"本局共{total}人，官方标准配置：镇民{tf}人、外来者{out}人、爪牙{mn}人、恶魔{dm}人"
        f"（若场上声称外来者明显多于{out}个、或镇民身份明显超出{tf}个，说明有人在说谎；"
        f"男爵在场会使外来者+2、镇民-2）。"
    )

# 私聊收尾词：模型说完信息/分析完对方发言后自然结束对话的语句
_CHAT_END_WORDS = (
    "聊到这里", "就到这", "到此为止", "先这样", "暂时相信你", "暂时信你",
    "回头再聊", "没什么要说的", "没别的要讲", "信息就这么多", "就这些",
    "先聊到这", "不多说", "差不多这样", "先这样吧", "就到这里",
)

def _is_chat_end(text):
    return any(w in text for w in _CHAT_END_WORDS)

def _sanitize_reply(reply, team="", phase=""):
    """后处理清洗：弱模型偶发泄露游戏外词汇/固定幻觉，直接替换为玩家口吻"""
    if not reply:
        return reply
    r = reply
    r = r.replace("说书人告诉我的", "我得到的信息是").replace("说书人告诉我", "我得到的信息是")
    r = r.replace("说书人提示", "规则提示").replace("说书人", "规则")
    r = r.replace("推理笔记", "记忆").replace("笔记", "记忆")
    r = r.replace("Prompt", "规则").replace("prompt", "规则")
    r = re.sub(r"(战术|策略)(编号)?\d{0,4}", "", r)  # 含纯"战术编号"
    # 非游戏术语强制替换（弱模型发明"卧底/内鬼"等词）
    r = r.replace("卧底", "爪牙").replace("内鬼", "爪牙").replace("狼人", "爪牙")
    return r

# 邪恶方公聊自爆关键词（弱模型把角色说明/阵营背到公聊）
_EVIL_EXPOSE_WORDS = [
    "红唇女郎", "小恶魔", "投毒者", "男爵", "间谍", "接刀", "成为小恶魔", "魔典",
    "恶魔编号", "爪牙编号", "我的老大", "我的爪牙", "保护恶魔", "恶魔出事",
    "我的队友", "我们的队友", "我是邪恶", "邪恶阵营的我",
    "我给#", "我的指令", "我们的计划", "今晚就杀", "削弱好人", "伪装身份",
]

def _evil_self_expose(reply, team, phase):
    if team not in ("minion", "demon") or phase not in ("public_chat", "nomination"):
        return False
    for w in _EVIL_EXPOSE_WORDS:
        if w in reply:
            return True
    return False

_FALLBACK_PUBLIC_EVIL = [
    "我暂时没更多线索，先听听大家的看法。",
    "我还在观察，信息还不太够，大家继续。",
    "先看看大家怎么分析，我稍后补充。",
]

# ====================================================================
# 八、兜底话术池（LLM 失败时按阵营返回符合人设的话，绝不沉默）
# ====================================================================
_FALLBACK_PUBLIC = [
    "我暂时没太多线索，先听听大家的。",
    "今天信息有点乱，我再理理。",
    "先看投票吧，我保持观望。",
]
_FALLBACK_PRIVATE_OPEN = [
    "你是什么身份？",
    "这局你怎么看？",
    "一起对下信息？",
]
_FALLBACK_PRIVATE = [
    "你说的我再想想。",
    "这个我暂时不太好说。",
    "嗯，你继续说。",
]

# ====================================================================
# 健康检查 / 连通测试
# ====================================================================
@app.route("/health")
def health():
    _, key, _ = _resolve_llm("")
    return jsonify({"status": "ok", "llm": bool(key)})

# Token 用量查询：GET /api/usage 返回全局+按房间+按模型统计
@app.route("/api/usage")
def api_usage():
    snap = _usage_snapshot()
    return jsonify({
        "total": snap["total"],
        "rooms": snap["rooms"],
        "models": snap["models"],
    })

# 会话生命周期重置：对局开局/结束/踢出时调用，清空该房间全部 AI 笔记，
# 防止 LLM 把上一局的状态（怀疑/信任/发言）带进新对局
@app.route("/api/session/reset", methods=["POST"])
def api_session_reset():
    d = request.get_json(force=True, silent=True) or {}
    room = str(d.get("room_code", "")).strip()
    if room:
        with _NOTES_LOCK:
            keys = [k for k in _AI_NOTES if k[0] == room]
            for k in keys:
                del _AI_NOTES[k]
    return jsonify({"ok": True})

# 提名/投票/处决事件通知：给该房间所有 AI 笔记写入对局事件（AI 据此回应提名动态）
@app.route("/api/session/event", methods=["POST"])
def api_session_event():
    d = request.get_json(force=True, silent=True) or {}
    room = str(d.get("room_code", "")).strip()
    evt = str(d.get("event", "")).strip()
    if room and evt:
        with _NOTES_LOCK:
            for key in _AI_NOTES:
                if key[0] == room:
                    n = _AI_NOTES[key]
                    note = f"系统事件：{evt}"
                    if not n["log"] or n["log"][-1] != note:
                        n["log"].append(note)
                        if len(n["log"]) > 20:
                            del n["log"][: len(n["log"]) - 20]
    return jsonify({"ok": True})

# 阶段切换通知：房主推进阶段时调用，给该房间所有 AI 笔记写入"当前阶段"事件
_PHASE_NAMES = {"private_chat": "私聊", "public_chat": "公聊", "nomination": "提名", "night": "黑夜", "day": "黎明"}

@app.route("/api/session/phase", methods=["POST"])
def api_session_phase():
    d = request.get_json(force=True, silent=True) or {}
    room = str(d.get("room_code", "")).strip()
    phase = str(d.get("phase", "")).strip()
    if room and phase:
        name = _PHASE_NAMES.get(phase, phase)
        with _NOTES_LOCK:
            for key in _AI_NOTES:
                if key[0] == room:
                    n = _AI_NOTES[key]
                    evt = f"系统事件：现在是{name}阶段"
                    if not n["log"] or n["log"][-1] != evt:
                        n["log"].append(evt)
                        if len(n["log"]) > 20:
                            del n["log"][: len(n["log"]) - 20]
    return jsonify({"ok": True})

@app.route("/api/ping", methods=["POST"])
def api_ping():
    d = request.get_json(force=True, silent=True) or {}
    llm_conf = d.get("llm_conf", "")
    url, key, model = _resolve_llm(llm_conf)
    if not url or not key:
        return jsonify({"ok": False, "error": "未提供 API Key（本地 Ollama 除外）"})
    try:
        resp = requests.post(
            url,
            headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
            json={"model": model, "messages": [{"role": "user", "content": "回复两个字：你好"}], "max_tokens": 200},
            timeout=30,
        )
        if resp.status_code != 200:
            try:
                detail = resp.json()
                msg = str(detail.get("error", {}).get("message", "")) or detail.get("error", "")
            except Exception:
                msg = ""
            return jsonify({"ok": False, "error": f"服务商返回 {resp.status_code} {str(msg)[:200]}"})
        data = resp.json()
        content = data["choices"][0]["message"]["content"].strip()
        return jsonify({"ok": True, "model": model, "sample": content[:50]})
    except Exception as e:
        return jsonify({"ok": False, "error": f"无法连接服务地址：{str(e)[:200]}"})

# ====================================================================
# 发言生成（四层金字塔 Prompt）
# ====================================================================
@app.route("/api/speak", methods=["POST"])
def api_speak():
    d = request.get_json(force=True, silent=True) or {}
    try:
        with open("req_debug.log", "a", encoding="utf-8") as f:
            f.write(f"{time.strftime('%H:%M:%S')} speak room={d.get('room_code')} role={d.get('role_name')} phase={d.get('phase')}\n")
    except Exception:
        pass
    try:
        with open("req_debug.log", "a", encoding="utf-8") as f:
            f.write(f"{time.strftime('%H:%M:%S')} speak room={d.get('room_code')} role={d.get('role_name')} phase={d.get('phase')} other={d.get('other_num')}\n")
    except Exception:
        pass
    role_name = d.get("role_name", "玩家")
    team = d.get("team", "")
    number = d.get("number", "?")
    phase = d.get("phase", "public_chat")
    day = d.get("day", 0)
    alive_nums = d.get("alive_nums", [])
    context = d.get("context", []) or []
    my_clues = d.get("my_clues", []) or []
    llm_conf = d.get("llm_conf", "")
    room_code = d.get("room_code", "default")

    _record_observations(room_code, number, context, my_clues, day)
    notes_str = _notes_text(room_code, number)

    alive_str = ",".join(str(n) for n in alive_nums)
    real_clues = _real_clues(my_clues)
    clue_str = "；".join(real_clues)
    if clue_str:
        clue_str = (
            "【你的关键私密信息·必须牢记】" + clue_str + "。"
            "当别人问起你的信息或查验结果时，直接、完整、准确地复述以上内容（包括具体编号），"
            "绝对禁止说'记不清''不记得''不能透露''信息安全'。"
        )
    else:
        clue_str = (
            "【你的关键私密信息：无】你的角色没有任何夜晚查验信息。"
            "当别人问起你的信息时，你唯一允许的回答是'我没有信息'。"
            "绝对禁止编造任何观察、任何'我注意到/我看到/我觉得XX异常'。"
        )
    ctx = _dedup_context(context, 6)
    ctx_str = "；".join(f"#{m.get('num')}: {m.get('content', '')}" for m in ctx)
    if ctx_str:
        ctx_str = "最近对话（已去重）：" + ctx_str

    # ——— 第一层：阵营核心目标（最高优先级） ———
    if team in ("townsfolk", "outsider"):
        core = f"【最高优先级·阵营目标】你是{role_name}，善良阵营，目标是找出并消灭恶魔与爪牙，保护好人。"
        team_logic = get_good_team_logic(role_name)
    elif team in ("minion", "demon"):
        core = f"【最高优先级·阵营目标】你是{role_name}，邪恶阵营，目标是隐藏身份活到最后，杀光好人。你必须服从邪恶方协同作战体系。"
        team_logic = get_evil_playbook(team, role_name)
    else:
        core = f"你是{role_name}。"
        team_logic = ""

    # ——— 第二层：角色规范 ———
    guide = get_role_guide(role_name)
    if guide:
        guide = "你的角色规范：" + guide

    # ——— 第三层：场景规则 ———
    if phase == "private_chat":
        other_num = d.get("other_num") or 0
        other_note = f"你正在和 #{other_num} 一对一私聊。" if other_num else ""
        # 私聊历史整理：num=0 是系统开场消息（刚建立私聊）
        history_msgs = [m for m in context if m.get("num") != 0 and m.get("content")]
        first_time = len(history_msgs) == 0
        hist_str = "；".join(f"#{m.get('num')}: {m.get('content')}" for m in history_msgs[-8:])
        if first_time:
            hist_note = (
                "此前你们没有私聊对话，不要假装认识。直接说事：问身份、报身份、分享信息、谈战术。"
            )
        else:
            hist_note = (
                f"【你们之前的私聊记录】{hist_str}。"
                "只基于以上记录回应；如果对方指出你说错了或记错了什么，立即承认并停止坚持。"
            )
        if team in ("minion", "demon"):
            # 确定性队友判断：从私密信息中解析队友编号，直接告诉模型对方是谁（不靠模型自行比对）
            demon_num = 0
            minion_nums = set()
            for c in my_clues:
                cs = str(c)
                m = re.search(r"你的恶魔编号[:：]\s*#?(\d+)", cs)
                if m:
                    demon_num = int(m.group(1))
                m = re.search(r"你的爪牙编号[:：]\s*(.+)", cs)
                if m:
                    for n in re.findall(r"#?(\d+)", m.group(1)):
                        minion_nums.add(int(n))
            is_mate = (team == "demon" and other_num in minion_nums) or \
                      (team == "minion" and demon_num and other_num == demon_num)
            if is_mate:
                # 队友模式：互斥注入——只给战术沟通规则，不给伪装欺诈规则
                guide = re.sub(r"【禁止红线】[^。]*。", "", guide)
                if team == "demon":
                    mate_note = (
                        f"对方 #{other_num} 是你的爪牙，自己人。直接谈事："
                        f"必须先给他分配一个伪装身份（用你掌握的不在场镇民名单，确保不与其他人撞车），"
                        f"然后下达本轮指令（抗推谁/谁带队/谁隐身）、同步刀人计划。"
                    )
                else:
                    mate_note = (
                        f"对方 #{other_num} 是你的恶魔老大，自己人。直接谈事："
                        f"主动向老大索要你的伪装身份和本轮指令，汇报你的观察、报告技能使用。"
                    )
                    if role_name == "间谍":
                        mate_note = (
                            f"对方 #{other_num} 是你的恶魔老大，自己人。立即把魔典信息完整告诉老大："
                            f"全场每个玩家是什么角色、谁是威胁最大的信息位、谁最好杀，"
                            f"并建议老大和你伪装什么身份。"
                        )
                team_logic = _EVIL_PLAYBOOK.get(role_name, "") + "".join(_EVIL_TEAM_RULES) + _EVIL_ROLE_TACTICS.get(role_name, "")
                style = (
                    f"【私聊】和 #{other_num} 一对一私聊。"
                    f"{mate_note}"
                )
            else:
                # 好人模式：互斥注入——只给伪装欺诈规则
                mate_note = (
                    f"对方 #{other_num} 是好人，你不认识他。"
                    f"全程扮演你的伪装身份，引导对方踩好人，不泄露任何邪恶信息。"
                )
                team_logic = _EVIL_PLAYBOOK.get(role_name, "") + "".join(_EVIL_TEAM_RULES) + "".join(_EVIL_FRAUD_TACTICS)
                style = (
                    f"【私聊】和 #{other_num} 一对一私聊。"
                    f"{mate_note}"
                )
        else:
            style = (
                f"【私聊】和 #{other_num} 一对一私聊。"
                f"有信息就说，没有就说没有；试探对方身份、交换信息。"
                f"说话前三思：如果没新东西可说，就说'就聊到这里吧，我找别人聊聊'这类收尾话结束对话，"
                f"不要为说话而说话、不要重复。"
                f"严禁虚构对方的发言或身份（如'你自称XX''你在公聊说XX'）——"
                f"你只能引用你们本次对话里对方真正说过的话；不确定就问。"
            )
        style = other_note + style + hist_note
        temp = 0.7
    elif phase == "nomination":
        style = (
            f"【提名辩论】给出简短有力的指控或自我辩护，贴合场上局势。"
            f"只输出你的怀疑理由本身，不要复述系统给你的提示或题目。"
        )
        temp = 0.7
    else:
        style = (
            f"【公聊】你是真实桌游玩家。"
            f"说点新的：你的信息、新的怀疑或投票建议，两三句话说清楚就好，别长篇大论。"
            f"别人说过的不必重复；不要跟着多数人怀疑，要有自己的依据；不要公开私聊内容。"
            f"发言前想一想：你是否有值得说的新信息、需要配合的操作或战术？"
            f"如果都没有，就只输出【沉默】两个字，安静观察别人的发言并自己思考，不要硬凑发言。"
        )
        if team in ("minion", "demon"):
            style += (
                "你全程在扮演伪装身份：怀疑指向好人、帮队友说话，必要时与队友轻微互踩演戏。"
                "绝对不能说自己是爪牙/恶魔，绝对不能说你的真实角色名（红唇女郎/小恶魔/投毒者/男爵），"
                "绝对不能说'接刀''成为小恶魔''下毒''刀人'等夜晚行为。"
                "被指控是恶魔/爪牙时，必须坚决反驳：否认指控、指出指控者的漏洞或反咬对方，"
                "绝对不许承认、不许含糊其辞、不许说任何与邪恶身份沾边的话。"
                "声称身份和信息时用简短利落的一句话（像'我是图书管理员，我得知#2和#8中有一人是外来者'），"
                "不要长篇解释、不要主动补充细节——话越多破绽越多。"
            )
        temp = 0.8

    # ——— 私聊总结记忆 ———
    summary_str = ""
    if phase == "private_chat":
        summary_str = _chat_summary_text(room_code, number, other_num)
    else:
        summary_str = _chat_summary_text(room_code, number)

    # ——— 第四层：上下文 ———
    dead_note = ""
    if number not in alive_nums:
        dead_note = (
            f"【你已死亡】你已死亡：不能提名、夜晚不再行动，但你持有 1 张死亡票——"
            f"处决投票时你仍可投出这 1 票，投完即作废。"
            f"你仍然可以自由发言、私聊和推理——把你掌握的信息（尤其是死后技能获得的信息）说出来帮助队友。"
        )
    board_note = _board_note(d.get("total_players") or 0)
    prompt = (
        f"你正在玩多人社交推理桌游《血染钟楼·暗流涌动》。"
        f"当前第{day + 1}天，阶段：{phase}。"
        f"你的身份是{role_name}，匿名编号#{number}。"
        f"场上存活玩家: #{alive_str}。座位围成一圈，相邻关系为环形（最大编号的下一位是 1 号）。"
        f"夜晚规则：首夜恶魔不行动，第一晚绝不会有人死亡；"
        f"之后每晚恶魔选择一名玩家杀死（若目标被僧侣保护或是士兵则不会死亡，可能无人死亡）。"
        f"{board_note}"
        f"{dead_note}"
        f"{core}"
        f"{clue_str}"
        f"{guide}"
        f"{team_logic}"
        f"{style}"
        f"{summary_str}"
        f"{notes_str}"
        f"{ctx_str}"
        f"发言要体现你的记忆与战术目标；如果之前有人说过与现在矛盾的话，指出来。"
        f"你永远不会说'我记不清''我不记得''我忘了'——你的私密信息就在上面，被问到时准确回答。"
    )
    _, _, model_name = _resolve_llm(llm_conf)
    model_hints = _WEAK_MODEL_HINTS if _is_weak_model(model_name) else ""
    reply = _ask_deepseek([
        {"role": "system", "content": "你是血染钟楼真实线下桌游玩家。输出自然口语化，不要格式列表、不要markdown，不要机械。" + _GLOBAL_RULES + model_hints},
        {"role": "user", "content": prompt},
    ], llm_conf, temperature=temp, max_tokens=500 if phase == "private_chat" else 280, room_code=room_code)

    # 兜底：LLM 失败不沉默，返回符合人设的话
    if not reply:
        if phase == "private_chat":
            # 第一句（对方只有系统开场消息）用开场式兜底，避免"你说的我再想想"当开场白
            real_msgs = [m for m in context if m.get("num") != 0 and m.get("content")]
            if not real_msgs:
                reply = random.choice(_FALLBACK_PRIVATE_OPEN)
            else:
                reply = random.choice(_FALLBACK_PRIVATE)
        else:
            reply = random.choice(_FALLBACK_PUBLIC)
        return jsonify({"reply": reply[:80]})

    # 身份一致性校验：提取声称身份写入笔记（下次生成时约束一致）
    reply = _sanitize_reply(reply, team, phase)
    # 公聊沉默权：模型输出【沉默】时不发言（Go 侧收到空回复不广播）
    if phase == "public_chat":
        stripped = reply.strip().strip("。！!.,， ")
        if stripped in ("沉默", "【沉默】", "我沉默", "我选择沉默", "先沉默", "我保持沉默", "暂时沉默"):
            return jsonify({"reply": "", "silent": True})
    # 重复发言检测（公聊+私聊通用）：
    # 1) 与本人最近 3 条发言相似 >0.55 则沉默（禁止复读自己的车轱辘话）
    # 2) 与对方最近的消息相似 >0.7 则沉默（禁止复读别人刚说的话，弱模型常把对方的话原样背一遍）
    n = _get_notes(room_code, number)
    my_saids = [log_line[len("我说过："):] for log_line in reversed(n["log"])
                if log_line.startswith("我说过：")][:3]
    for last_said in my_saids:
        sim = difflib.SequenceMatcher(None, reply, last_said).ratio()
        if sim > 0.55:
            return jsonify({"reply": "", "silent": True, "duplicate": True})
    recent_others = [str(m.get("content", "")) for m in context[-4:]
                     if str(m.get("num")) not in ("0", str(number)) and m.get("content")]
    for oc in recent_others:
        if len(oc) > 10 and difflib.SequenceMatcher(None, reply, oc).ratio() > 0.7:
            return jsonify({"reply": "", "silent": True, "duplicate": True})
    # 邪恶方公聊自爆兜底：检测到真实身份/阵营/战术泄露，整条换成安全发言
    if _evil_self_expose(reply, team, phase):
        reply = random.choice(_FALLBACK_PUBLIC_EVIL)
    _extract_identity_update(room_code, number, reply)
    _add_log(room_code, number, f"我说过：{reply[:150]}", day)
    # 异步结构化记忆：提炼"怀疑/信任"对象写回笔记（不阻塞回复返回）
    snap_ctx = [dict(m) for m in context[-8:]]
    threading.Thread(
        target=_update_structured_notes,
        args=(room_code, number, reply, snap_ctx, llm_conf),
        daemon=True,
    ).start()
    # 私聊后异步总结记忆（不阻塞回复返回）
    if phase == "private_chat":
        other_n = d.get("other_num") or 0
        if other_n:
            snap = [dict(m) for m in context[-8:]]
            threading.Thread(
                target=_summarize_private,
                args=(room_code, number, other_n, snap, llm_conf),
                daemon=True,
            ).start()
    # 私聊收尾检测：模型说完自己想说的、分析完对方的话后自然收尾（如"就聊到这里""暂时相信你"）
    # 带 end_chat 标记 → Go 侧该 AI 对此会话进入沉默权（不再回复，仅思考）
    end_chat = False
    if phase == "private_chat":
        end_chat = _is_chat_end(reply)
    return jsonify({"reply": reply, "end_chat": end_chat})

# ====================================================================
# 决策生成（温度 0.6，稳定执行战术）
# ====================================================================
@app.route("/api/decide", methods=["POST"])
def api_decide():
    d = request.get_json(force=True, silent=True) or {}
    role_name = d.get("role_name", "玩家")
    ability = d.get("ability", "")
    alive_nums = [int(n) for n in d.get("alive_nums", [])]
    self_num = d.get("self_num")
    my_clues = d.get("my_clues", []) or []
    llm_conf = d.get("llm_conf", "")
    room_code = d.get("room_code", "default")
    decision_type = d.get("decision_type", "target")  # target=选目标 / vote=投赞成或弃权

    clue_str = "；".join(str(c) for c in my_clues[-6:])
    if clue_str:
        clue_str = "你掌握的私密信息：" + clue_str
    guide = get_role_guide(role_name)
    if guide:
        guide = "你的角色规范：" + guide
    notes_str = _notes_text(room_code, self_num)

    # —— 投票决策：先推理后输出 1(赞成)/2(弃权) ——
    if decision_type == "vote":
        vote_from = d.get("vote_from", 0)
        vote_target = d.get("vote_target", 0)
        dead_note = ""
        if self_num not in alive_nums:
            dead_note = (
                f"注意：你已死亡，本票是你的死亡票，只有这一张，用完即作废。"
            )
        prompt = (
            f"你是《血染钟楼·暗流涌动》里的{role_name}。"
            f"当前正在投票：#{vote_from} 提名了 #{vote_target}。"
            f"{dead_note}"
            f"{guide}"
            f"{clue_str}"
            f"{notes_str}"
            f"请先简要写出你的推理（两到三句：这个人该不该被处决，"
            f"结合你的阵营立场和已有信息），然后最后一行只输出一个数字：1=投赞成，2=弃权。"
        )
        reply = _ask_deepseek([
            {"role": "system", "content": "先输出简短推理，最后一行只输出 1 或 2。"},
            {"role": "user", "content": prompt},
        ], llm_conf, temperature=0.6, max_tokens=300, room_code=room_code)
        if not reply:
            return jsonify({"vote_yes": random.random() < 0.5})
        for line in reversed(reply.splitlines()):
            m = re.match(r"^#?\s*([12])\s*$", line.strip())
            if m:
                vote_yes = m.group(1) == "1"
                _add_log(room_code, str(self_num), f"我投票{'赞成' if vote_yes else '弃权'} #{vote_target}，推理：{reply[:80]}")
                return jsonify({"vote_yes": vote_yes, "reasoning": reply[:200]})
        for n in reversed(re.findall(r"\d+", reply)):
            if n in ("1", "2"):
                vote_yes = n == "1"
                return jsonify({"vote_yes": vote_yes, "reasoning": reply[:200]})
        return jsonify({"vote_yes": random.random() < 0.5})

    # —— 私聊邀请判断：让 AI 自己决定接受还是拒绝 ——
    if decision_type == "private_accept":
        invite_from = d.get("invite_from", 0)
        prompt = (
            f"你是《血染钟楼·暗流涌动》里的{role_name}。"
            f"#{invite_from} 邀请你私聊。"
            f"请根据你的笔记（身份、怀疑对象、信任对象、阵营立场）自己决定："
            f"接受私聊可以获取信息或打探对方；如果你怀疑对方、正在专心自己的计划、"
            f"或者不想暴露自己，可以拒绝。"
            f"{clue_str}"
            f"{notes_str}"
            f"先写一句理由，最后一行只输出一个数字：1=接受私聊，2=拒绝。"
        )
        reply = _ask_deepseek([
            {"role": "system", "content": "你是真实玩家，独立判断。最后一行只输出 1 或 2。"},
            {"role": "user", "content": prompt},
        ], llm_conf, temperature=0.6, max_tokens=200, room_code=room_code)
        if not reply:
            return jsonify({"accept": True})  # LLM 失败兜底接受
        for line in reversed(reply.splitlines()):
            m = re.match(r"^#?\s*([12])\s*$", line.strip())
            if m:
                accept = m.group(1) == "1"
                _add_log(room_code, str(self_num), f"我{'接受' if accept else '拒绝'}了 #{invite_from} 的私聊邀请，理由：{reply[:60]}")
                return jsonify({"accept": accept, "reasoning": reply[:150]})
        for n in reversed(re.findall(r"\d+", reply)):
            if n in ("1", "2"):
                return jsonify({"accept": n == "1", "reasoning": reply[:150]})
        return jsonify({"accept": True})

    # —— 目标决策：先推理后输出编号 ——
    candidates = [n for n in alive_nums if n != self_num]
    if not candidates:
        return jsonify({"choice": None})

    prompt = (
        f"你是《血染钟楼·暗流涌动》里的{role_name}，能力：{ability}。"
        f"{guide}"
        f"{clue_str}"
        f"{notes_str}"
        f"结合身份立场仔细思考多层逻辑：好人优先指向高嫌疑目标；"
        f"邪恶方选择击杀/下毒目标时优先杀害高嫌疑好人，绝对不要选择邪恶队友。"
        f"候选玩家编号: {candidates}。"
        f"请先简要写出你的推理（两到三句：为什么选这个目标、为什么排除其他人），"
        f"然后最后一行只输出一个数字作为你的最终选择。"
    )
    reply = _ask_deepseek([
        {"role": "system", "content": "先输出简短推理，最后一行只输出一个整数编号。"},
        {"role": "user", "content": prompt},
    ], llm_conf, temperature=0.6, max_tokens=400, room_code=room_code)

    # 兜底：LLM 失败走简单规则（不返回 None，避免玩家离线感）
    if not reply:
        return jsonify({"choice": random.choice(candidates)})
    # 解析：优先取最后一行纯数字；否则取全文最后一个在候选内的数字
    choice = 0
    for line in reversed(reply.splitlines()):
        line = line.strip()
        m = re.match(r"^#?\s*(\d+)\s*$", line)
        if m:
            choice = int(m.group(1))
            break
    if choice not in candidates:
        for n in reversed(re.findall(r"\d+", reply)):
            if int(n) in candidates:
                choice = int(n)
                break
    if choice in candidates:
        # 推理写进笔记（供后续发言/决策引用）
        _add_log(room_code, str(self_num), f"我选择 #{choice}，推理：{reply[:80]}")
        return jsonify({"choice": choice, "reasoning": reply[:200]})
    return jsonify({"choice": random.choice(candidates)})

if __name__ == "__main__":
    # threaded=True 关键：游戏里 5 个 AI 同时请求（私聊/公聊/夜晚决策），
    # Flask 默认单线程会排队导致 Go 侧 25 秒超时、AI 公聊静默
    app.run(host="127.0.0.1", port=8742, debug=False, threaded=True)
