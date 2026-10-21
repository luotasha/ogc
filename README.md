# OGC Admin Routing

本项目基于 [xynogen/ogc](https://github.com/xynogen/ogc) 扩展，专门适配 **OpenCode Go 订阅**，用于让 Claude Code 通过 OpenCode Go 的模型服务工作。它保留上游的 Anthropic ↔ OpenAI 协议转换能力，并增加多订阅账号、会话固定路由、可视化管理和推理内容兼容处理。

## 相比上游增加的功能

- **多账号 Key 管理**：支持多个命名账号；可在管理页面添加、修改名称、启用、禁用和删除 Key。Key 写入本地配置文件，不需要放在 shell 环境变量中。
- **会话固定到账号**：首次请求为会话选择账号，之后该会话持续使用同一个 Key，避免一个 Claude Code 会话在多个订阅账号之间切换。
- **按账号选择模型**：每个账号可以设置自己的默认模型。当 Claude Code 发送 `automodel` 时，OGC 将其替换为该账号配置的真实模型。
- **价格排序与默认模型**：管理页面的模型按照输入价格从低到高排列；账号没有指定模型以及新增账号时，默认选择 DeepSeek V4.1 Flash。
- **按会话切换模型**：已经存在的会话可以单独指定真实模型，不影响同一账号下的其他会话。
- **本地管理页面**：访问 `http://127.0.0.1:3456/admin/` 管理账号、模型及会话绑定。页面仅允许本机访问。
- **会话识别信息**：会话列表显示最后通信时间和真实模型；鼠标悬停 Session ID 可查看近期对话摘要，方便对应终端窗口。
- **会话列表清理**：管理页面只展示最近 30 天内实际通信过的会话，不显示没有通信时间的陈旧绑定。
- **稳定的 Session ID 传递**：补充 Claude Code 与 OpenCode 上游之间的会话标识传播，减少恢复会话后重新分配 Key 的情况。
- **上游错误保真**：保留上游返回的错误状态与正文，并改进熔断分类，避免把请求格式错误误判成模型故障。
- **请求兼容修复**：处理消息顺序、缺失 `content` 字段等兼容问题，降低长会话或工具调用时的 400/422 错误。
- **推理内容兼容代理**：附带 `ogc-reasoning-proxy`，缓存工具调用对应的 `reasoning_content`，并在后续请求中恢复，解决部分思考模型要求回传推理内容的问题。
- **跨平台构建**：GitHub Actions 同时构建 macOS、Linux、Windows 的 AMD64/ARM64 版本，并产出 `ogc` 与 `ogc-reasoning-proxy`。

## 构建

需要 Go 1.25 或更高版本。

```bash
git clone https://github.com/luotasha/ogc.git
cd ogc
make test
make build
```

生成的程序位于：

```text
bin/ogc
bin/ogc-reasoning-proxy
```

构建所有支持的平台：

```bash
make dist
```

## 配置和启动

首次创建配置：

```bash
./bin/ogc init
```

配置文件默认位于 `~/.config/ogc/config.json`。启动 OGC：

```bash
./bin/ogc serve
```

Claude Code 使用本地端点：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:3456
export ANTHROPIC_API_KEY=unused
claude --dangerously-skip-permissions
```

打开管理页面：

```text
http://127.0.0.1:3456/admin/
```

在 Claude Code 的模型配置中使用 `automodel`，OGC 会根据会话绑定的账号和模型设置选择真实模型。客户端明确传入其他模型名时，OGC 保留该模型名。

## 推理内容兼容代理

部分模型在工具调用后的下一次请求中要求带回上一条回复的 `reasoning_content`。遇到这类错误时，先启动兼容代理：

```bash
./bin/ogc-reasoning-proxy \
  -listen 127.0.0.1:3460 \
  -target https://opencode.ai/zen/go \
  -cache ~/.config/ogc/reasoning-cache.json
```

然后把 OGC 的 OpenAI 上游地址配置为：

```text
http://127.0.0.1:3460/v1
```

`reasoning-cache.json` 含有会话推理数据，不要提交到 Git。

## 数据与安全

- 账号 Key、会话绑定和推理缓存只保存在本机。
- 不要把真实 Key、`config.json`、会话文件或推理缓存提交到仓库。
- 如果设置了 `OGC_API_KEYS` 环境变量，账号由环境变量控制，管理页面不能修改账号配置；需要页面管理时请取消该变量。

## 上游文档

OGC 的基础代理原理、完整配置结构和上游功能请阅读：[xynogen/ogc README](https://github.com/xynogen/ogc/blob/main/README.md)。

## Claude Code 通过 OGC 接入

启动 OGC 并在 `/admin/` 添加启用账号，点击「Claude Code 接入」可查看、选择模型并复制 macOS/Linux、Windows PowerShell 或 settings.json 配置。复制内容只含占位 Key，不会泄露已有密钥。

本机 `claude_opencode` 启动脚本的核心配置如下：

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:3456"
export ANTHROPIC_API_KEY="YOUR_API_KEY"
unset ANTHROPIC_AUTH_TOKEN
export ANTHROPIC_MODEL="automodel"
export ANTHROPIC_DEFAULT_OPUS_MODEL="automodel"
export ANTHROPIC_DEFAULT_SONNET_MODEL="automodel"
export ANTHROPIC_DEFAULT_HAIKU_MODEL="automodel"
export ANTHROPIC_CUSTOM_HEADERS="x-opencode-session: $(uuidgen)"
claude
```

YOUR_API_KEY 使用 admin 已添加账号的真实 Key。macOS/Linux 执行上面的命令；Windows 在接入浮窗中选择 PowerShell，使用 `[guid]::NewGuid()` 生成会话标识。已有本机脚本时直接运行 `claude_opencode`，参数原样传递给 Claude Code。原脚本还设置 SNAPVIEW_AUTO_TRIGGER=1、CLAUDE_CODE_AUTO_COMPACT_WINDOW=190000，并使用 --dangerously-skip-permissions；这些不是 OGC 接入的必要条件。

**输入具体模型名称或 ID 即固定模型；输入 automodel 则由 admin 确定最终模型。** 新会话读取账号默认模型；已有会话在「会话绑定 → 真实模型」切换，下一次请求生效。账号默认模型的修改只影响新会话。固定模型与既有会话绑定冲突时，新建会话再使用；不要把一个会话 ID 复用于不同会话。临时指定模型可用 `claude_opencode --model 具体模型ID`。

也可把 env 字段合并到 `~/.claude/settings.json`（Windows：`%USERPROFILE%\.claude\settings.json`），保留原有配置并重启 Claude Code。JSON 配置模板见 admin 浮窗，其中 SESSION_ID 要为新会话生成并替换，同一会话续用时保持不变。不要同时保留外部的 ANTHROPIC_AUTH_TOKEN，以免覆盖 API Key 认证。
