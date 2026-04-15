# BBClaw Project Rules

## ⚠️ 设计文档优先原则

**设计文档是开发决策的唯一真相来源（Source of Truth）**

- 设计文档位于仓库根目录 `design/` 下
- 所有代码实现必须符合设计文档
- **如有冲突，先解决设计问题再实现代码**，不能以"代码能跑"为由绕过设计
- 新功能设计必须先编写/更新设计文档，再写代码

## Release & Tag Policy

**Only create git tags and GitHub releases in the main `daboluocc/bbclaw` repo when:**
- There is a user-facing new feature in the **firmware** (ESP32), OR
- There is a fix or new feature in the **adapter** binary (new binary needs to be published)

**Do NOT create a tag or release for:**
- Cloud-only fixes (backend logic, API changes)
- Web portal-only fixes (UI, CSS, React changes)
- Internal refactors with no adapter binary change

**Why:** Releases in `daboluocc/bbclaw` are public-facing and contain adapter binaries for end users. A release implies users need to download something new. If only cloud/web changed (deployed server-side), there is nothing for users to download.

## Project Layout

- `daboluocc/bbclaw` — main public repo: firmware, docs, CHANGELOG, GitHub releases with adapter binaries
- `references/bbclaw-reference/` — private repo (gitignored in main repo): adapter (Go), cloud (Go), web (React)
  - Adapter binaries are built from `bbclaw-reference/adapter` and published as releases in `daboluocc/bbclaw`

## Versioning

- Bump version and add CHANGELOG entry for every meaningful change
- Tag + release only when adapter binary needs to ship (see policy above)
