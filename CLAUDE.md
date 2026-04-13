# BBClaw Project Rules

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
