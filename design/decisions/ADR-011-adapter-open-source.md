# ADR-011: Adapter 开源（搬到主仓）

- **日期**: 2026-04-27
- **状态**: 已接受
- **关联**: ADR-001（adapter 作为 Agent Bus）、ADR-003（Router + 多 driver 路由）

## 背景

`bbclaw-reference` 闭源仓有三块：`cloud/`（云端后端）、`web/`（用户面板）、`adapter/`（本地代理）。
其中 cloud 和 web 是 ToC 真正的护城河——账户、计费、ASR/TTS 路由、多租户调度都在那里。
adapter 不一样：它就是一个把 CLI（claude-code、aider、ollama…）封装成统一 HTTP/WS 接口的桥，
没有专有算法、没有云配置、没有数据残留。

继续把 adapter 闭源带来三个摩擦：

1. **社区贡献阻塞** —— driver 矩阵（Aider、Codex、Gemini…）天然适合社区贡献，但 PR 要给一个看不见源码的仓库，没人愿意做。
2. **自托管门槛** —— `local_home` 模式的目标用户是开发者/玩家，他们想读源码。给他们一个 binary 等于劝退。
3. **跨仓发布管道** —— 每次发版要先在 `bbclaw-reference` 编译 adapter binary，再 copy 到 `daboluocc/bbclaw` 发 release。手工步骤易出错。

## 决策

把 `bbclaw-reference/adapter/` 整体搬到 `daboluocc/bbclaw/adapter/`，保留 git 历史（subtree split + add）。

- **Module 路径不变**：原本就是 `github.com/daboluocc/bbclaw/adapter`，搬过来后路径完美对得上，下游 `go get` 不影响。
- **Cloud 和 Web 继续闭源**：那才是 ToC 护城河。
- **闭源仓 `bbclaw-reference/adapter/` 删除**，留 `MOVED.md` 指向新位置。

## 评估过的备选方案

| 方案 | 为什么不选 |
|---|---|
| 整个 adapter 包括 homeadapter 也开源 | ✅ 选了这个；homeadapter 是裸 HTTP/WS 客户端，没有秘密 |
| 留 homeadapter 在闭源仓，只开 driver 部分 | 拆开包反而泄露 cloud 的接口形状（"哦原来云端是这种 frame"），还不如整开 |
| 用 `git filter-repo` 完全重写 history | 太重；`git subtree split` 已经够用，且单 squash commit 也能 trace 回原 SHA |
| 不搬，只 mirror 一个公开 binary release | 等于现状，三个摩擦都在 |

## 实施

```bash
# 1. 在闭源仓抽出 adapter 历史
cd bbclaw-reference
git subtree split --prefix=adapter -b _adapter_only_export

# 2. 在开源仓 import
cd ../bbclaw
git fetch /path/to/bbclaw-reference _adapter_only_export
git subtree add --prefix=adapter FETCH_HEAD

# 3. 验证
cd adapter && go build ./... && go test ./...

# 4. 闭源仓 rm + 留 MOVED.md
```

## 后果

### 正向

- 社区可以为 driver 矩阵 PR；driver 写作 guide 进 `adapter/docs/`
- `local_home` 用户能直接 `go install github.com/daboluocc/bbclaw/adapter/cmd/bbclaw-adapter@latest`
- 发版流程简化：单仓 GitHub Actions 同时 build firmware OTA bin 和 adapter binary
- 与"firmware 已开源"的叙事对齐，对开发者社区更友好

### 负向

- 历史 PR 流量分散（闭源仓 issue 跟不过来）—— 用 `MOVED.md` 引导
- adapter 任何 fix 都进 public history —— 必须 grep 一遍确认没有 hardcoded token / 内网 URL（pre-flight 已做：闭源仓 adapter 无此类内容）
- 维护双仓的人需要明白哪些改动属于 adapter（开源）哪些属于 cloud（闭源）—— CLAUDE.md 已写清

### 开放问题

- 谁负责审 driver PR？短期：我；长期：等社区有 maintainer 候选
- 是否在 README 显式声明 "adapter 只支持 ToC 之外的开发者场景"？暂不写，留给文档自然演进

## 立即行动

- [x] subtree split + add（commit `bf24299`）
- [x] CLAUDE.md 项目布局描述更新
- [ ] adapter/README 改写（强调 driver 写作）
- [ ] 闭源仓 `adapter/` 删除 + `MOVED.md`（等 user 确认时机）
- [ ] CI：把 adapter binary build 从闭源仓挪到开源仓的 release workflow
