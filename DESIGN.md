# pico (pi-coordinator)

> 将 pi 接入 Telegram 的协调服务。用户在私聊发起任务，在群聊 Topic 中跟踪执行全过程。

## 1. 核心目标

**一句话**：通过 Telegram Bot 远程操控 pi，让 pi 的执行过程和结果完整呈现在群聊 Topic 中。

**负责范围**：
- **Coordinator**：用户消息 → pi（管理进程、路由消息、维护映射）
- **pi-trace**：pi 事件 → Telegram Topic（已有能力，coordinator 传递 topic 参数）

**不负责**（未来扩展）：
- 审批流程（pending，将通过插件实现）
- 多编排（pending，将通过插件 + coordinator API 实现）
- 远程运行时抽象（pending）

---

## 2. 参与角色

```
┌──────────┐     ┌───────────────┐     ┌──────────┐     ┌───────────────┐
│  User     │────▶│  Coordinator  │────▶│   pi     │────▶│  pi-trace     │
│ (Telegram)│     │  (Go Service) │     │ (RPC)    │     │  (Extension)  │
└──────────┘     └───────────────┘     └──────────┘     └───────────────┘
     │                  │                   │                   │
     │  ① 私聊命令       │                   │                   │
     │  /workspace       │                   │                   │
     │  /new             │                   │                   │
     │  /sync            │                   │                   │
     │                  │                   │                   │
     │  ② 群聊 Topic 中的  │                   │                   │
     │  跟进消息          │                   │                   │
     │                  │                   │                   │
     │                  │  ③ spawn pi       │                   │
     │                  │  --mode rpc       │                   │
     │                  │  --topic <id>     │                   │
     │                  │                   │                   │
     │                  │  ④ stdin JSONL    │                   │
     │                  │  prompt/steer     │                   │
     │                  │                   │                   │
     │                  │                   │  ⑤ stdout events   │
     │                  │                   │  (pi-trace hooks)  │
     │                  │                   │                   │
     │                  │                   │                   │  ⑥ Telegram API
     │                  │                   │                   │  thinking/tool/
     │  ⑦ 看到进展 ◀────┼───────────────────┼───────────────────┤  assistant/summary
     │                  │                   │                   │
```

**角色分工**：

| 角色 | 职责 |
|------|------|
| **User** | 私聊发起任务、群聊跟进 |
| **Coordinator** | Bot 接入、Workspace/Session 管理、pi 进程生命周期、消息路由 |
| **pi (RPC)** | 执行 AI agent，通过 stdin/stdout 接收命令和输出事件 |
| **pi-trace** | 挂载在 pi 的 extension hooks 上，将执行进度发送到 Telegram Topic |

---

## 3. 业务流程

### 3.1 核心流程：/workspace → 选 Session → 开始对话

```
User 私聊发送 /workspace
  │
  ▼
Coordinator 返回 workspace 列表（Inline Keyboard）
  ├── 来源 1：coordinator 数据库中已记录的 workspace（之前 spawn 过的）
  └── 来源 2：通过 /sync 扫描 ~/.pi/agent/sessions/ 发现的 workspace
  │
  ▼
User 点击某个 workspace
  │
  ▼
Coordinator 返回该 workspace 下的 session 列表（Inline Keyboard）
  ├── 每个 session 显示：标题 + 时间
  ├── 标题来源：优先使用数据库中的 name，其次用 session JSONL 中第一条 user message 的第一行
  │     （sync 时 name 为空，后续可扩展为大模型生成）
  └── 额外选项：[+ New Session]
  │
  ▼
User 选择一个已有 session 或 [+ New Session]
  │
  ▼ （若是新 session，等待用户输入第一条消息）
  │
  ▼
Coordinator：
  1. 在群聊中创建 Topic（标题 = 用户消息第一行）
  2. 将用户原始消息在 Topic 中置顶
  3. 发送 Keyboard：[🆕 New] [📋 Sessions] [📌 Pin]
  4. Spawn pi 进程（--mode rpc --topic <thread_id>）
  5. 通过 stdin 发送 prompt
  │
  ▼
pi 开始执行 → pi-trace 将进展发送到 Topic
```

### 3.2 快捷流程：/new

```
User 私聊发送 /new 帮我修复登录页面的 500 错误
  │
  ▼ （跳过 session 选择，直接新建）
  │
  ▼
Coordinator 返回 workspace 列表（Inline Keyboard）
  │
  ▼
User 点击 workspace
  │
  ▼
Coordinator：
  1～5 同 3.1（直接创建新 session，不需再选 session）
```

```
User 私聊发送 /new（无参数）
  │
  ▼
Coordinator 返回 workspace 列表
  │
  ▼
User 点击 workspace
  │
  ▼
Coordinator 提示"请输入任务描述"
  │
  ▼
User 发送消息
  │
  ▼
Coordinator：同 3.1 的 1～5
```

### 3.3 Pin 流程

```
User 在已有任务的 Keyboard 中点击 [📌 Pin]
  │
  ▼
Coordinator 记录 pinned workspace（内存状态）
发送置顶消息：
  📌 已固定工作目录：/Users/xiaot/projects/myapp
  后续所有消息将在此目录下直接创建新任务。
  /unpin 或重新 /workspace 可取消。
  │
  ▼
User 发送任意文本（非命令）：
  "帮我重构 user service"
  │
  ▼
Coordinator：
  等价于 /new 帮我重构 user service（跳过选 workspace）
```

**Pin 状态规则**：
- 纯内存状态，重启丢失
- `/workspace` 选择新目录 → 覆盖
- `/new` → 不覆盖（用户已确认目录，简化流程）
- `/unpin` → 清除

### 3.4 群聊 Topic 中的跟进

```
Session 正在执行（pi 进程运行中，isStreaming = true）
  │
  ▼
User 在 Topic 中发送消息：
  "也检查一下注册流程"
  │
  ▼
Coordinator 监听到新消息（非 bot 发送、来自授权用户）
  │
  ▼
Coordinator 查表：topic_id → session_id → pi 进程
  │
  ▼
通过 stdin 发送 steer：
  {"type": "steer", "message": "也检查一下注册流程"}
```

```
Session 已结束（pi 进程空闲或已退出）
  │
  ▼
User 在 Topic 中发送消息
  │
  ▼
Coordinator：
  若进程已退出 → 重新 spawn pi，switch_session 恢复
  发送 prompt：
  {"type": "prompt", "message": "也检查一下注册流程"}
```

### 3.5 /model 命令

```
User 私聊发送 /model
  │
  ▼
Coordinator 返回级别选择 Keyboard：
  [🌐 Global] [📁 Workspace] [💬 Session]
  │
  ├── Global：修改配置文件，对所有 workspace 生效
  ├── Workspace：写入数据库 workspace 记录，对该 workspace 下新 session 生效
  └── Session：写入数据库 session 记录，仅对当前 session 生效
  │
  ▼
Coordinator 返回模型列表 Keyboard（通过 pi RPC `get_available_models` 获取）：
  [Anthropic / Claude Sonnet 4]
  [Anthropic / Claude Opus 4]
  [OpenAI / GPT-4o]
  [Google / Gemini 2.5 Pro]
  [✖ Cancel]
  │
  ▼
User 选择模型
  │
  ▼
Coordinator 发送二次确认：
  "将 Workspace /myapp 的模型设为 Claude Opus 4？"
  [✅ Confirm] [✖ Cancel]
  │
  ▼
User 确认
  │
  ▼
Coordinator：
  ├── Global → 更新配置文件
  ├── Workspace → UPDATE workspaces SET model = ...
  └── Session → UPDATE sessions SET model = ...
  返回："✅ 已更新"
```

**模型优先级（spawn pi 时确定）**：
`Session.model` > `Workspace.model` > `Global.model` > pi 默认模型

### 3.6 /sync 命令

```
User 私聊发送 /sync
  │
  ▼
Coordinator 扫描 ~/.pi/agent/sessions/
  │
  ├── 解析目录名 → 还原 cwd
  ├── 过滤临时目录（如 --private-tmp--）
  ├── 读取每个 .jsonl 文件第一行 → 获取 sessionId、timestamp
  ├── 读取第一个 user message → 获取标题
  └── 将 workspace+session 写入 SQLite
  │
  ▼
返回统计："同步完成：发现 3 个新 workspace，12 个 session"
```

**无法解析 cwd 的目录 → 直接跳过**。

---

## 4. 整体架构

### 4.1 模块划分

```
pico (Go)
│
├── cmd/pico/                # 入口
│
├── internal/
│   ├── bot/                 # Telegram Bot 接入
│   │   ├── handler.go       # 消息路由（命令 vs 文本 vs callback）
│   │   ├── keyboard.go      # Inline Keyboard 构建
│   │   └── commands.go      # /workspace, /new, /sync, /unpin
│   │
│   ├── session/             # Session 扫描与管理
│   │   ├── scanner.go       # 扫描 session JSONL，提取 cwd + metadata
│   │   └── validator.go     # 校验 cwd 目录是否存在
│   │
│   ├── runner/              # pi 进程管理
│   │   ├── manager.go       # 进程生命周期（spawn, kill, idle timeout）
│   │   ├── process.go       # 单个 pi 进程的 stdin/stdout 通信
│   │   └── rpc.go           # RPC 命令构造
│   │
│   ├── store/               # 持久化（SQLite）
│   │   ├── db.go            # 连接与迁移
│   │   ├── workspace.go     # workspace CRUD
│   │   └── mapping.go       # session ↔ topic ↔ process 映射
│   │
│   └── config/              # 配置
│       └── config.go        # Bot Token, Group Chat ID, 超时等
```

### 4.2 数据模型（SQLite）

```sql
-- 工作区
CREATE TABLE workspaces (
    id        INTEGER PRIMARY KEY,
    path      TEXT NOT NULL UNIQUE,    -- 绝对路径，如 /Users/xiaot/projects/myapp
    name      TEXT,                    -- 显示名，默认取路径最后一段
    model     TEXT,                    -- workspace 级别模型（provider/modelId）
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 会话（一个 session = 一个群聊 Topic）
CREATE TABLE sessions (
    id              TEXT PRIMARY KEY,     -- session UUID
    workspace_id    INTEGER NOT NULL REFERENCES workspaces(id),
    file_path       TEXT NOT NULL,        -- JSONL 文件绝对路径
    name            TEXT,                 -- 人工或大模型生成的话题名称
    title           TEXT,                 -- 首条用户消息第一行（sync 自动提取）
    model           TEXT,                 -- session 级别模型（provider/modelId）
    topic_id        INTEGER,              -- 关联的 Telegram message_thread_id
    goal_message_id INTEGER,              -- 置顶的 Goal 消息 ID
    created_at      DATETIME,
    updated_at      DATETIME
);
```

### 4.3 内存状态（pin）

```go
type PinState struct {
    mu        sync.RWMutex
    pinned    map[int64]string  // telegramUserId → workspacePath
}
```

---

## 5. 关键设计决策

### 5.1 进程模型

```
一个 pi 进程 = 一个活跃 session
同一 workspace 可同时运行多个 pi 进程（各自独立 session）
```

- **启动**：有任务时 spawn `pi --mode rpc`
- **复用**：同一 session 下次发消息时重新 spawn，通过 `switch_session` 恢复
- **空闲回收**：`agent_end` 后若 N 分钟无新消息 → kill

### 5.2 pi 启动参数

```bash
pi --mode rpc \
   --session-dir ~/.pi/agent/sessions \
   --name "<session title>" \
   --model <resolved_model> \
   --topic <message_thread_id>
```

其中 `<resolved_model>` 按优先级解析：`Session.model` > `Workspace.model` > `Global配置` > 不传（pi 默认）。
格式为 `provider/modelId`，如 `anthropic/claude-sonnet-4-20250514`。

环境变量（由 coordinator 设置）：
```
PI_TRACE_TELEGRAM_BOT_TOKEN=<token>
PI_TRACE_TELEGRAM_CHAT_IDS=<group_chat_id>
```

### 5.3 pi-trace 的角色

pi-trace 继续直接调用 Telegram API 发送执行进展。Coordinator 不转发这些消息。

优势：
- 复用 pi-trace 已有的完整实现（thinking/tool/assistant/summary、分块、HTML 渲染）
- Coordinator 职责单一（用户输入 → pi）
- 两者通过 `--topic` flag 共享 topic ID 即可协作

### 5.4 Topic 命名与 Goal 标记

- **Topic 标题**：用户消息第一行（截断到 Telegram 限制 128 字符）
- **Goal 置顶**：coordinator 将用户原始消息在 Topic 中 `pinChatMessage`，前缀 `🎯 `（不带 "Goal:" 文字）
- 消息置顶而非话题描述，因为 Topic 描述不可见且不醒目

### 5.5 群聊消息过滤

Coordinator 监听群聊消息时，仅处理：
1. 非 bot 自身的消息
2. 消息的 `message_thread_id` 匹配已知活跃 session 的 topic_id
3. 发送者是授权用户（通过 Telegram user ID 白名单）

### 5.6 模型列表来源

模型列表不通过 pico 配置维护，而是运行时从 pi 查询：
- pico spawn 第一个 pi 进程时，先发送 `get_available_models`，缓存结果
- `/model` 命令展示缓存的模型列表
- 若缓存为空（首次启动且无活跃进程），临时 spawn `pi --mode rpc --no-session` 查询后立即退出
- 提供 `/model --refresh` 强制刷新缓存

Global/Workspace/Session 级别存储的是选中的模型标识（`provider/modelId`），不属于模型列表配置。

### 5.7 Session 标题生成

Session 列表展示时，标题优先级：
1. 数据库 `name` 字段（人工设置或大模型生成）—— 当前预留，sync 时为空
2. 数据库 `title` 字段（sync 时自动提取）

`title` 提取逻辑，参考 pi-trace 的 `title.ts`：
1. 读 session JSONL 第一行 → `{ type: "session", cwd, id, timestamp }`
2. 遍历后续行，找第一条 `message.role === "user"` 的记录
3. 取 `content[0].text` 的第一行作为标题

后续扩展：通过大模型根据 session 内容生成更精炼的 `name`。

---

## 6. 交互协议汇总

### 6.1 私聊命令

| 命令 | 行为 |
|------|------|
| `/workspace` | 列出所有 workspace → 选 session → 开始对话 |
| `/add` | 从 `~` 开始浏览目录 → 确认当前目录 → 添加 workspace |
| `/new [描述]` | 带描述：选 workspace → 直接创建 session；无描述：选 workspace → 提示输入 |
| `/open` | 在 Session Topic 内打开该 session 对应 workspace |
| `/model` | 设置模型：选级别（Global/Workspace/Session）→ 选模型 → 确认 |
| `/sync` | 扫描 sessions 目录，同步到数据库 |
| `/unpin` | 清除 pin 状态 |
| 任意文本（pin 状态下） | 等同 `/new <文本>` |

### 6.2 Keyboard 交互

**任务发起后**：
```
[🆕 New] [📋 Sessions] [📌 Pin]
```

- **New**：在当前 workspace 下创建新 session，等待用户输入
- **Sessions**：列出当前 workspace 的所有 session
- **Pin**：固定当前 workspace，后续消息自动创建新任务

**Pin 状态下**：
```
[🆕 New] [📋 Sessions] [📍 Unpin]
```

### 6.3 pi RPC 命令使用

| 场景 | RPC 命令 |
|------|---------|
| 用户发起新任务 | `new_session` → `prompt` |
| 用户恢复已有 session | `switch_session` → `prompt` |
| 运行中追加消息 | `steer` |
| 停止后追加消息 | `prompt` |
| 运行时切换模型 | `set_model` |

---

## 7. 配置

```yaml
# coordinator 配置文件
telegram:
  bot_token: "123456:ABC-DEF1234"
  group_chat_id: "-1001234567890"   # 任务跟踪群聊
  allowed_users: [123456789]         # 授权用户 ID 白名单

runner:
  idle_timeout: 5m                   # pi 进程空闲超时
  session_dir: "~/.pi/agent/sessions"

global_model: ""                     # Global 级别默认模型（为空则用 pi 默认）

store:
  # 数据库路径由 PICO_ENV 决定，不在配置文件里指定
```

### 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `PICO_ENV` | 运行环境，设为 `dev` 时数据目录指向 `./dev_assets/` | 空（生产） |

## 8. 开发命令

项目使用 npm scripts 封装 Go 命令：

```bash
# 开发运行（设置开发环境变量）
npm run dev

# 生产构建
npm run build       # → go build -o pico ./cmd/pico

# 类型检查 + lint
npm run check       # → go vet ./... && staticcheck ./...
```

`npm run dev` 内部等价于：
```bash
PICO_ENV=dev go run ./cmd/pico
```

环境路径规则：
| 环境 | 数据库 | 配置文件 |
|------|--------|----------|
| `PICO_ENV=dev` | `./dev_assets/pico.db` | `./dev_assets/pico.config.yaml` |
| 生产（默认） | `~/.mypi/pico/pico.db` | `~/.mypi/pico/config.yaml` |

---

## 9. 范围界定

### 当前范围（v1）

- [x] `/workspace` 选择 workspace 和 session
- [x] `/new` 快速创建新任务
- [x] `/model` 多级模型设置（Global/Workspace/Session）
- [x] Pin 模式
- [x] 群聊 Topic 中的进度跟踪（via pi-trace）
- [x] 群聊 Topic 中的消息跟进与自动转发
- [x] Goal 消息置顶
- [x] `/sync` 扫描历史 session
- [x] SQLite 持久化
- [x] Keyboard 交互（New/Sessions/Pin）

### 明确不在此版本

- ❌ Extension UI Protocol 处理（审批流）
- ❌ 多编排
- ❌ 远程运行时
- ❌ 多群聊支持
- ❌ 多用户并发（先支持单用户白名单）

---

## 10. 依赖关系

```
pico 依赖：
├── pi （RPC mode，由 coordinator spawn）
├── pi-trace（pi extension，输出到 Telegram）
├── Telegram Bot API
└── SQLite

pi-trace 依赖：
├── pi（作为 extension 运行）
└── Telegram Bot API（直连）
```

Coordinator 和 pi-trace **不直接通信**，通过共享的 `--topic` flag 和 Telegram Topic 协作。
