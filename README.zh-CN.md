# pico (pi-coordinator)

> 通过 Telegram 远程操控 [pi](https://github.com/xiaot623/pi) AI 编程助手。在私聊发起任务，在群聊 Forum Topic 中实时跟踪完整执行过程。

## 工作原理

```
私聊                         pico (协调器)              pi (AI Agent)
────                        ──────────────             ─────────────
  /new "修复 bug"  ──────▶  选择工作区           ──────▶  启动 pi 进程
                            创建论坛话题                     执行任务
                            通过 stdin 发送提示词              流式输出进度
                                                                 │
  群聊话题 ◀────────────────────────────────────────────────────┘
  （通过 pi-trace 扩展实时展示进度）
```

在私聊中向 Bot 发送命令和任务描述，Bot 会在配置好的群聊中创建论坛话题并启动 pi 进程，你可以在话题中实时跟踪每一个步骤 —— 思考过程、工具调用、助手回复和总结。

## 功能特性

- **远程任务执行** — 从 Telegram 启动、引导和恢复 AI 编程会话
- **实时进度跟踪** — 思考过程、工具调用和输出实时流式展示在群聊话题中
- **工作区与会话管理** — 浏览、同步和组织历史会话
- **多模式运行** — Local（直接编辑）、Worktree（Git 隔离分支）、Docker（完全沙箱）
- **多级模型选择** — 在 Global / Workspace / Session 三个级别设置 AI 模型
- **Git 集成** — 直接在 Telegram 中执行 rebase、commit 和查看 HTML Diff
- **待办管理** — 跨工作区保存和组织任务想法

## 命令列表

| 命令 | 描述 |
|------|------|
| `/help` | 显示可用命令 |
| `/workspace` | 浏览工作区并选择会话 |
| `/add` | 从文件系统添加工作区 |
| `/new [描述]` | 在工作区中创建新任务 |
| `/todo` | 管理跨工作区的待办事项 |
| `/cron` | 管理跨工作区的定时任务 |
| `/status` | 列出所有活跃会话及详情 |
| `/detail` | 显示工作区路径、运行模式、模型及 Git 摘要 |
| `/stop` | 强制停止当前会话 |
| `/open` | 在本地终端打开工作区（iTerm2 / xdg-open） |
| `/rebase` | 将 worktree/docker 会话变基到主分支 |
| `/commit <消息>` | 将 worktree/docker 会话的更改提交回主分支 |
| `/diff [参数]` | 生成当前更改的 HTML Diff 视图 |
| `/sync` | 从磁盘导入历史会话到数据库 |
| `/pin [查询]` | 固定工作区；之后发送的任意消息将自动创建新任务 |
| `/unpin` | 取消固定工作区 |
| `/model` | 在 Global / Workspace / Session 级别配置模型 |
| `/bots` | 显示托管的 Telegram Bot 账号 |

### 会话话题内命令

在会话的论坛话题中，你可以随时发送跟进消息，Bot 会将其转发到正在运行（或重新启动）的 pi 进程以继续任务。以下命令也可在话题内使用：

| 命令 | 使用场景 |
|------|----------|
| `/stop` | 强制停止当前话题的会话 |
| `/open` | 在本地打开当前会话的工作区 |
| `/rebase` | 将会话的 worktree 变基（worktree/docker 模式） |
| `/commit <msg>` | 提交并推送更改（worktree/docker 模式） |
| `/diff [args]` | 生成带语法高亮的 HTML Diff |

## 运行模式

启动新任务时，可以选择三种运行模式之一：

| 模式 | 工作目录 | 隔离级别 |
|------|----------|----------|
| **Local** | 原始工作区 | 无隔离 — 直接编辑文件 |
| **Worktree** | 每会话独立 Git worktree | 文件更改隔离在单独分支上 |
| **Docker** | 容器中的独立 Git worktree | 完整的文件系统 + 环境沙箱 |

Worktree 和 Docker 模式要求工作区为 Git 仓库。

## 快速开始

### 前置条件

- [Go](https://go.dev/dl/) 1.25+
- [Node.js](https://nodejs.org/) 18+（npm 脚本需要）
- 一个 Telegram Bot Token（通过 [@BotFather](https://t.me/BotFather) 获取）
- 一个启用了论坛的 Telegram 群组，Bot 需要是管理员

### 1. 克隆并安装

```bash
git clone https://github.com/xiaot623/pi-coordinator.git
cd pi-coordinator
npm install
```

### 2. 配置

首次运行时会自动创建配置模板，请填写必填项：

```bash
npm run dev
# → 创建 dev_assets/pico.config.yaml
```

编辑 `dev_assets/pico.config.yaml`（生产环境使用 `~/.mypi/pico/config.yaml`）：

```yaml
telegram:
  bot_token: "123456:ABC-DEF1234"
  group_chat_id: -1001234567890
  allowed_users:
    - 123456789
```

### 3. 运行

```bash
# 开发模式（使用 dev_assets/ 目录）
npm run dev

# 生产构建
go build -o pico ./cmd/pico
./pico
```

### 4. Docker 镜像（Docker 运行模式需要）

```bash
docker build -f docker/Dockerfile --target pi-agent -t pi-agent:latest .
```

## 配置参考

```yaml
telegram:
  bot_token: ""                # Telegram Bot Token
  group_chat_id: 0             # 论坛群聊 ID
  allowed_users: []            # 允许使用 Bot 的 Telegram 用户 ID 列表

plugins:
  - "@hahahhh/pi-trace@next"   # pi 扩展（pi-trace 是实现进度跟踪的必须组件）

plugin_update_interval_minutes: 1440

runner:
  local:
    idle_timeout: 5m           # 空闲超时
    session_dir: "~/.pi/agent/sessions"
  worktree:
    idle_timeout: 5m
    session_dir: ""
  docker:
    idle_timeout: 5m
    image: "pi-agent:latest"   # Docker 镜像名
    network: "bridge"          # Docker 网络模式
    agent_dir: "~/.pi/agent"
    skills_dir: "~/.agents/skills"
    agent_mount_mode: "rw"     # agent 挂载模式
    extra_mounts:
      - "~/.config"            # 宿主机 ~/.config -> 容器 ~/.config（ro）
      - host: "~/scratch"
        mode: "rw"             # 宿主机 ~/scratch -> 容器 ~/scratch
                                  # 非 ~/ 路径仍然保持同路径挂载

open_tool: "iterm2"            # macOS: iterm2 / terminal; Linux: xdg-open
global_model: ""               # 全局默认模型（为空则使用 pi 默认值）

diff:
  delivery: send               # send（发送到 Telegram）| open（在浏览器打开）| all
```

### 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `PICO_ENV` | 设置为 `dev` 时，使用 `./dev_assets/` 作为配置和数据库目录 | 空（生产环境） |

## npm 脚本

```bash
npm run dev           # 开发模式运行
npm test              # 运行测试
npm run build         # 通过 scripts/build-native.js 构建原生二进制
npm run check         # 代码检查（go vet ./...）
npm run release:patch # 升级补丁版本号、打标签并推送
npm run release:minor # 升级次版本号、打标签并推送
npm run release:major # 升级主版本号、打标签并推送
```

## 项目结构

```
pico/
├── cmd/pico/main.go          # 入口
├── internal/
│   ├── app/                  # 应用编排
│   ├── config/               # YAML 配置加载与热更新
│   ├── diff/                 # HTML Diff 渲染
│   ├── gitops/               # Git 操作的 Shell 脚本封装
│   ├── runner/               # Local / Worktree / Docker 三种模式的 pi 进程管理
│   ├── session/              # 会话扫描器（从磁盘同步）
│   ├── source/telegram/      # Telegram Bot：消息处理、键盘 UI、路由
│   ├── store/                # SQLite 持久化
│   ├── todos/                # 待办事项存储
│   └── crons/                # 定时任务存储和调度解析
├── docker/                   # Docker 镜像定义
├── scripts/                  # 构建和发布脚本
├── docs/                     # 补充文档
├── DESIGN.md                 # 架构与设计文档
└── package.json              # npm 包元数据
```

## 架构设计

详细的架构概览、设计决策和内部数据模型请参见 [DESIGN.md](./DESIGN.md)。

## 许可证

MIT
