# Runner 运行模式与 Git Worktree 隔离方案

## 概述

为 pi-coordinator 新增基于 Git worktree 的隔离运行方案，并支持三种 per-session 运行模式：

| 模式 | 工作目录 | 进程位置 | 适用场景 |
|---|---|---|---|
| `local` | 用户选择的真实 workspace | 宿主机 | 兼容现有行为，直接修改原 workspace |
| `worktree` | per-session Git worktree | 宿主机 | 隔离文件改动，但不隔离运行环境 |
| `docker` | per-session Git worktree | Docker 容器 | 同时隔离文件改动和运行环境 |

核心原则：

- `worktree` 是隔离工作区的唯一抽象，Docker 不再创建额外 sandbox copy。
- `docker` 模式挂载的是 per-session worktree，不直接挂载真实 workspace。
- `worktree` 和 `docker` 模式的产物都保留在 worktree 中，用户可以直接进入该目录查看、编辑、提交或删除。
- 真实 workspace 只作为创建 worktree 的来源，不做自动回写。

---

## 一、配置

### 1.1 配置条目

```yaml
runner:
  idle_timeout: 5m
  session_dir: "~/.pi/agent/sessions"
  binary: "pi"

  # Docker mode
  docker_image: "pi-agent:latest"

  plugins:
    - "@hahahhh/pi-trace@next"
  plugin_update_interval_minutes: 1440
```

### 1.2 Docker 镜像

- `runner.docker_image` 指定 Docker 镜像名。
- Docker container home 固定默认 `/home/pi`。
- Docker agent mount mode 固定默认 `rw`，因为 pi 启动时会写 `settings.json.lock`、刷新 package metadata 或安装全局配置里的 package。
- Worktree root 固定为 pico data dir 下的 `worktrees/`。
- v1 阶段用户自行 `docker build` 构建镜像。
- 鉴权配置（API key 等）不打进镜像，运行时通过 volume mount 挂入。

---

## 二、Per-Session 运行模式选择

### 2.1 交互流程

```
/new <prompt>
  → 选择 workspace
  → 输入 prompt
  → 创建 Forum Topic（pi 尚未启动）
  → 发送 "Topic Created: xxx"
    + InlineKeyboard:
      [Run Local] [Run Worktree] [Run Docker]
  → Local    → 在真实 workspace 启动 pi
  → Worktree → 创建 / 复用 Git worktree，在宿主机启动 pi
  → Docker   → 创建 / 复用 Git worktree，在容器中启动 pi
```

### 2.2 模式语义

- `local`
  - 完全兼容现有 Local runner。
  - `cwd` 为用户选择的真实 workspace。
  - 会直接读写真实 workspace。
- `worktree`
  - 先创建 per-session Git worktree。
  - `cwd` 为 worktree 路径。
  - pi 进程仍在宿主机运行。
  - 适合希望隔离代码改动，但不需要容器环境隔离的任务。
- `docker`
  - 先创建 per-session Git worktree。
  - Docker 容器挂载并使用该 worktree。
  - 适合同时需要代码改动隔离和运行环境隔离的任务。

---

## 三、Session 持久化

### 3.1 sessions 表字段

`sessions` 表新增字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| `runner_type` | `TEXT` | `""` / `"local"` / `"worktree"` / `"docker"` |
| `original_workspace_path` | `TEXT` | 用户创建 session 时选择的真实 workspace |
| `worktree_path` | `TEXT` | `worktree` / `docker` 模式使用的 worktree 路径 |
| `worktree_branch` | `TEXT` | session 对应的 worktree branch |
| `base_commit` | `TEXT` | 创建 worktree 时的 base commit SHA |

兼容规则：

- 旧数据 `runner_type = ""` 视为 `local`。
- Session 创建后 `runner_type` 不可变更。
- 继续已有会话时按 `runner_type` 分发：
  - `""` / `"local"` → Local runner
  - `"worktree"` → Worktree runner
  - `"docker"` → Docker runner

### 3.2 Session 目录

| 模式 | 宿主机 session 目录 | 容器内 session 目录 |
|---|---|---|
| `local` | 现有 `runner.session_dir` | 不适用 |
| `worktree` | `~/.mypi/pico/sessions/worktree/` | 不适用 |
| `docker` | `~/.mypi/pico/sessions/docker/` | `<container_home>/.mypi/pico/sessions/docker/` |

Session scanner 需要扫描以上所有目录下的 `*.jsonl`。

---

## 四、Git Worktree 管理

### 4.1 适用范围

`worktree` 和 `docker` 模式 v1 仅支持 Git workspace。

启动前需要校验：

```bash
git -C <workspace> rev-parse --show-toplevel
git -C <workspace> rev-parse --verify HEAD
```

如果 workspace 不是 Git 仓库，隐藏 `Run Worktree` / `Run Docker`，或在用户点击时提示该模式需要 Git workspace。

### 4.2 创建规则

创建 session worktree：

```bash
base_commit=$(git -C <workspace> rev-parse HEAD)
suffix="<10-char-random-alnum>"
branch="$suffix"
worktree_path="$HOME/.mypi/pico/worktrees/<original-dir-name>_$suffix"

git -C <workspace> worktree add -b "$branch" "$worktree_path" "$base_commit"
```

要求：

- `worktree_path` 按 session 唯一，格式为 `<原 workspace 目录名>_<10 位字母数字随机串>`。
- `worktree_branch` 按 session 唯一，v1 使用 10 位字母数字随机串；后续可扩展为模型生成。
- `base_commit` 记录到 session 元数据。
- 创建后，`worktree` 和 `docker` 模式都只在该 worktree 中运行。
- 如果继续已有 session，直接复用已有 `worktree_path`，不重新创建或覆盖。

### 4.3 原 workspace 有未提交改动

v1 默认不把真实 workspace 的未提交改动带入 worktree。

原因：

- Git worktree 的语义是从某个 commit 创建隔离分支。
- 自动复制 dirty changes 容易把真实 workspace 的临时状态混入 session，后续 diff 和合并语义会变复杂。

处理方式：

- 如果真实 workspace 有 dirty changes，创建前提示用户：
  - `local` 模式会直接基于当前真实 workspace 运行。
  - `worktree` / `docker` 模式会从当前 `HEAD` 创建干净 worktree。
- 后续可增加显式选项：`include dirty changes`，通过 patch + untracked copy 应用到新 worktree。

### 4.4 清理规则

- 任务结束不自动删除 worktree。
- 用户删除 session 或显式清理时执行：

```bash
git -C <original_workspace> worktree remove <worktree_path>
```

- 如果 worktree branch 上有提交，默认保留 branch。
- 如用户明确选择删除 branch，再执行：

```bash
git -C <original_workspace> branch -D <worktree_branch>
```

---

## 五、Worktree 查看方式

系统需要提供 worktree 的明确入口：

- Topic 中展示 worktree 路径：
  - `Worktree: ~/.mypi/pico/worktrees/<original-dir-name>_<10-char-random-alnum>`
- diff、提交、合并等操作交给用户在 worktree 中使用常规 Git 工具处理。

```bash
cd ~/.mypi/pico/worktrees/<original-dir-name>_<10-char-random-alnum>
git status
git diff
git commit
```

---

## 六、Local / Worktree Runner 启动

### 6.1 Local 模式

Local 模式保持现有行为：

```bash
cd <original_workspace>
pi --mode rpc --session-dir <runner.session_dir> --session-id <id> --name <title> [--model <model>] [--extension ...]
```

### 6.2 Worktree 模式

Worktree 模式复用 Local runner 的进程管理能力，但 `cwd` 和 session 目录不同：

```bash
cd <worktree_path>
pi --mode rpc --session-dir ~/.mypi/pico/sessions/worktree --session-id <id> --name <title> [--model <model>] [--extension ...]
```

要求：

- 与 Local runner 一样通过 stdin/stdout 进行 JSON-RPC 通信。
- 空闲超时行为与 Local runner 一致。
- 插件参数无需路径转换，仍使用宿主机路径。

---

## 七、Docker Runner 启动

### 7.1 挂载原则

Docker 模式挂载 worktree，而不挂载真实 workspace。

所有 Docker bind mount 的宿主机路径必须在启动前展开为绝对路径，并由 pico 预先创建或校验存在。不要把 `~` 直接传给 Docker，也不要依赖 Docker 自动创建宿主机目录。

注意：Git worktree 的 `.git` 通常是一个文件，内容指向原仓库 `.git/worktrees/...`。因此容器内如果要运行 Git 命令，仅挂载 worktree 目录不够，还必须让 worktree 指向的 git metadata 路径在容器内可见。

Docker runner 启动前需要解析：

```bash
git_dir=$(git -C <worktree_path> rev-parse --git-dir)
common_git_dir=$(git -C <worktree_path> rev-parse --git-common-dir)
```

将 `git_dir` 和 `common_git_dir` 解析为绝对路径，去重后以相同绝对路径挂载到容器中。

### 7.2 docker run 示例

```bash
docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e HOME=<container_home> \
  -v <worktree_path>:<worktree_path>:rw \
  -v <git_dir>:<git_dir>:rw \
  -v <common_git_dir>:<common_git_dir>:rw \
  -v <host_agent_dir>:<container_home>/.pi/agent:rw \
  -v <host_plugin_dir>:<container_home>/.mypi/pico/agent:ro \
  -v <host_skills_dir>:<container_home>/.agents/skills:ro \
  -v <host_docker_session_dir>:<container_home>/.mypi/pico/sessions/docker:rw \
  -w <worktree_path> \
  -i \
  <image> \
  pi --mode rpc --session-dir <container_home>/.mypi/pico/sessions/docker --session-id <id> --name <title> [--model <model>] [--extension ...]
```

说明：

- `-w` 使用 `<worktree_path>`，不伪装成原 workspace 路径。
- `--user "$(id -u):$(id -g)"` 避免容器创建 root-owned 文件。
- `<container_home>` 建议使用镜像内固定路径，例如 `/home/pi`。
- `<host_agent_dir>` 对应宿主机 `~/.pi/agent/`，挂载到 pi 默认鉴权配置目录。
- `<host_agent_dir>` 默认以 `rw` 挂载；pi 启动时会写 lock/cache/package metadata。
- `<host_plugin_dir>` 对应宿主机 `~/.mypi/pico/agent/`，只读挂载到 pico 插件目录。
- `<host_skills_dir>` 对应宿主机 `~/.agents/skills/`，只读挂载到容器用户 skills 目录。
- `<host_docker_session_dir>` 对应宿主机 `~/.mypi/pico/sessions/docker/`。
- Session 目录需要 `rw`，用于 jsonl 持久化。
- 鉴权、插件、session 三类目录不要使用嵌套挂载，避免只读父目录下再挂可写子目录导致跨平台行为不一致。
- 如果 `git_dir` 已经位于 `common_git_dir` 内，挂载 `common_git_dir` 即可，实际实现需去重。

### 7.3 插件路径转换

Local / Worktree runner 传宿主机插件路径，例如：

```bash
--extension /home/user/.mypi/pico/agent/npm/node_modules/xxx/index.js
```

Docker runner 需要转换为容器内路径：

- 宿主机 `~/.mypi/pico/agent/`
- 容器内 `<container_home>/.mypi/pico/agent/`

即做前缀替换：

```text
~/.mypi/pico/agent/... → <container_home>/.mypi/pico/agent/...
```

Docker runner 启动前需要校验转换后的插件入口存在：

```bash
test -f <container_extension_path>
```

实际实现可通过一次性容器校验，或在宿主机上先校验原始 `<host_extension_path>` 位于 `<host_plugin_dir>` 下且文件存在。

### 7.4 鉴权、插件、Session 持久化验收

Docker runner 需要满足以下验收条件：

1. 鉴权配置可读
   - 宿主机 `~/.pi/agent/` 挂载到 `<container_home>/.pi/agent`。
   - 挂载模式固定为 `rw`，用于 pi 写 lock/cache/package metadata。
   - 容器内 `pi` 使用默认 HOME 时可以读取该目录下的 API key、模型配置等文件。
2. 插件可传入
   - 宿主机 pico 维护的插件目录 `~/.mypi/pico/agent/` 挂载到 `<container_home>/.mypi/pico/agent`。
   - Docker runner 对每个 `--extension` 做前缀转换。
   - 转换后传给容器内 pi 的 `--extension` 必须指向容器内真实存在的文件。
3. Session 可持久化
   - 宿主机 `~/.mypi/pico/sessions/docker/` 挂载到 `<container_home>/.mypi/pico/sessions/docker`。
   - `pi --session-dir` 必须使用 `<container_home>/.mypi/pico/sessions/docker`。
   - 容器退出后，宿主机 `~/.mypi/pico/sessions/docker/<session-id>.jsonl` 仍然存在并可被 scanner 扫描。
4. 用户 skills 可读
   - 宿主机 `~/.agents/skills/` 挂载到 `<container_home>/.agents/skills`。
   - 挂载模式为 `ro`。
   - Docker runner 启动前创建该宿主机目录，避免 Docker 自动创建 root-owned 目录。

---

## 八、生命周期与超时

- 启动后通过 stdin/stdout 进行 JSON-RPC 通信。
- 使用 `-i` 保持 Docker stdin 打开。
- 使用 `--rm` 确保容器退出后自动删除。
- 空闲超时：pi 返回 `agent_end` / `done` 事件后开始计时。
- 超时时间复用 `runner.idle_timeout` 配置。
- 超时后：
  - `local` / `worktree`：停止 pi 进程。
  - `docker`：`docker stop` 容器，容器因 `--rm` 自动清理。
- worktree 不随进程或容器退出删除。

---

## 九、AvailableModels

### 9.1 Local / Worktree

Local 和 Worktree 模式可复用现有 AvailableModels 逻辑。

### 9.2 Docker

Docker runner 的 `AvailableModels` 通过启动临时容器查询：

```bash
docker run --rm \
  -e HOME=<container_home> \
  -v <host_agent_dir>:<container_home>/.pi/agent:rw \
  -i <image> \
  pi --mode rpc --no-session
```

- 通过 stdin 发送 `get_available_models` 请求。
- 读取响应后容器自动退出并删除。
- 不创建 worktree。
- 不支持插件模型（临时容器不挂载插件目录、不传 `--extension`），v1 可接受。