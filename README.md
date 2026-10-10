# 血染钟楼 · 暗流涌动 — 线上桌游平台

基于《血染钟楼·暗流涌动》（Blood on the Clocktower: Trouble Brewing）的多人在线社交推理桌游，支持真人玩家与大模型 AI 玩家混合对局、纯 AI 电子斗蛐蛐观战。

## 项目结构

```
├── botc_server/          Go 游戏主服务（Gin + gorilla/websocket + GORM/SQLite）
│   ├── internal/
│   │   ├── engine/       游戏引擎：暗流涌动 22 角色 + 官方规则（纯内存无 IO）
│   │   ├── room/         房间 Actor：每房间一个 goroutine，事件 channel 串行驱动
│   │   ├── aiclient/     Python AI 服务 HTTP 客户端
│   │   ├── handler/      HTTP（认证/建房）+ WS 指令路由 + 好友服务
│   │   ├── ws/           WebSocket Hub + Client
│   │   ├── model/        GORM 模型（用户/房间/好友/对局日志）
│   │   └── config/       环境变量配置
│   ├── static/           前端（Vue3 + Element Plus，本地 vendor，纯 WS 通信）
│   └── data/             运行时数据（arena_keys.example.json 为配置模板）
├── ai_service/           Python AI 推理服务（Flask）
│   └── main.py           AI 发言/决策/笔记/人格/战术体系，HTTP 解耦
└── README.md             本文件
```

## 快速开始

```bash
# 1. 启动 Go 游戏主服务（端口 8741）
botc_server/start_server.bat

# 2. 启动 Python AI 推理服务（端口 8742，大模型 AI 必需）
ai_service/start_ai.bat

# 3. 浏览器打开 http://127.0.0.1:8741
```

## 核心特性

- **暗流涌动完整规则**：22 角色、官方人数板子、夜晚行动顺序、中毒/酒鬼伪装、恶魔自杀传位、红唇女郎继承、镇长弹刀、管家跟随投票、守鸦人死后查验、猎手开枪、贞洁者触发、圣徒禁票、死亡票
- **大模型 AI 玩家**：Prompt 分层注入（阵营目标/角色规范/场景规则/上下文）、持久笔记（时间线日志/怀疑/信任）、先推理后决策、沉默权、复读检测、私聊自然收尾、邪恶方战术体系（恶魔分配伪装身份、间谍报魔典、爪牙服从指令）
- **电子斗蛐蛐**：房主不占座的纯 AI 观战，上帝视角看私聊会话/说书人记录/真实角色（酒鬼显示真身），赛后完整对局记录一键导出 Word
- **社交系统**：注册登录（验证码）、好友、房间邀请、断线重连、历史对局回放

## 配置

| 变量 | 默认 | 说明 |
|------|------|------|
| BOTC_ADDR | 127.0.0.1:8741 | 监听地址 |
| BOTC_AI_URL | http://127.0.0.1:8742 | AI 服务地址 |
| BOTC_HUMAN_CHOICE | false | 真人夜晚交互选择（占卜师/管家挂起手动选择） |
| DEEPSEEK_API_KEY | 空 | AI 服务默认模型 Key |

斗蛐蛐「一键入座 8 智谱 AI」需配置 `botc_server/data/arena_keys.json`（模板见 `arena_keys.example.json`，该文件已 gitignore）。

## 测试

```bash
cd botc_server
go test ./...        # 引擎规则 / 房间事件循环 / 好友 / Hub 全量单测
```

更多细节见 [botc_server/README.md](botc_server/README.md)。
