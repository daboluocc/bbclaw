---
name: dot-matrix-ui
description: "BBClaw / daboluo.cc 统一前端设计语言——点阵 / Nothing-style。当需要新建或重做任何 Web 界面（adapter 管理页、daboluo.cc 官网/promo、内部工具页、落地页）并希望与 BBClaw 固件 UI 视觉统一时使用。提供权威 token、CSS 变量、组件配方（点阵纹理、wordmark、卡片、选中行、按钮、模态框、状态网格）与 do/don't。触发词：点阵风格、dot-matrix、Nothing 风格、bbclaw 风格、daboluo 官网风格、统一前端样式、admin 页样式、把页面改成点阵。源真相：design/UI_DESIGN_LANGUAGE.md。"
---

# Dot-Matrix UI — BBClaw / daboluo.cc 统一前端设计语言

把固件那套「点阵 / Nothing-style」视觉语言搬到 Web。**唯一真相源是
`design/UI_DESIGN_LANGUAGE.md`**（固件侧落地 `firmware/include/bb_ui_theme.h`）；
本技能是它的 Web 落地版，供 adapter 管理页、daboluo.cc 官网/promo 及任何内部页面复用。
已落地参考实现：`adapter/internal/httpapi/admin.html`。

## 何时用

- 新建/重做任何 BBClaw 或 daboluo.cc 的 Web 页面、组件、落地页。
- 用户说「点阵风格 / Nothing 风格 / bbclaw 风格 / daboluo 官网统一样式 / 把这页改成点阵」。
- 想让 Web 视觉和硬件屏幕 UI 一致。

## 设计原则（照抄固件四条）

1. **单色 + 单一强调**：深近黑底 + 冷白/冷灰单色层次；**青色 `#2ec4a0` 是唯一装饰强调色**
   （高亮、下划线、选中态、呼吸点）。绿/红只做成功/错误语义，不做装饰。
2. **点阵优先**：图形优先用圆点阵列表达——背景纹理、wordmark 下划线、分隔、强调点。
   基准网格 dot 5px / pitch 9px（小元素 4/7）。
3. **点亮节奏**：「最新元素青色闪一下 → 下一拍沉淀为冷白」。新增项短暂青色高亮再褪去。
4. **硬切换**：页面/视图之间硬切，少用大面积 fade；强调克制、留白、等宽。

## Token（Web 值 = 固件 hex）

把这段 `:root` 直接贴进任意页面，全站只引用变量、**禁止裸 hex**（语义色除外）：

```css
:root{
  --bg:#070b0e;        /* 唯一底色（含遮罩） */
  --lit:#dfeaec;       /* 点阵亮点 / 主文字（冷白） */
  --ghost:#152128;     /* 点阵 ghost / 分隔线 / 次级面 */
  --dim:#6e8a93;       /* 次级文字（冷蓝灰） */
  --wordmark:#4f6f67;  /* 页脚 wordmark（暗青灰） */
  --accent:#2ec4a0;    /* 唯一强调青 */
  --ok:#4cd964;        /* 成功 / 在线（语义） */
  --err:#e66f6f;       /* 错误 / 危险（语义） */
  --pitch:9px;         /* 点阵基准点距 */
}
```

字体：等宽优先 `ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,monospace`，
正文 13px/1.55；标题用 letter-spacing 拉开 + 大写。

## 组件配方（可直接复用）

**1. 点阵背景纹理**（整页极淡点网，别盖过可读性）：
```css
body{ background:var(--bg);
  background-image:radial-gradient(var(--ghost) 1px, transparent 1px);
  background-size:var(--pitch) var(--pitch); }
```

**2. Wordmark 头**（大写 + 字距 + 青色点状下划线）：
```css
.wordmark{ font-weight:700; letter-spacing:.42em; color:var(--lit);
  border-bottom:2px dotted var(--accent); padding-bottom:3px; }
```

**3. 卡片 / 区块**（ghost 描边 + 半透底）：
```css
.card{ background:rgba(13,18,22,.78); border:1px solid var(--ghost);
  border-radius:10px; padding:16px 18px; }
/* 小标题前缀三点阵：青点 + 两个 ghost 点 */
.card h2{ font-size:11px; letter-spacing:.14em; text-transform:uppercase; color:var(--dim); }
.card h2::before{ content:""; width:5px; height:5px; border-radius:50%; background:var(--accent);
  box-shadow:9px 0 0 var(--ghost),18px 0 0 var(--ghost); margin-right:8px; display:inline-block; }
```

**4. 选中行 motif**（固件「ghost 行面 + 青色左缘 3px 竖条 + 冷白字」直译）：
```css
.row.sel{ background:rgba(46,196,160,.10); box-shadow:inset 3px 0 0 var(--accent); color:var(--lit); }
```

**5. 按钮**（主＝青实心，次＝ghost 描边）：
```css
button{ border-radius:7px; padding:8px 15px; border:1px solid var(--accent);
  background:var(--accent); color:#04110f; font-weight:600; }
button.ghost{ background:transparent; color:var(--dim); border-color:var(--ghost); }
button.del{ background:transparent; color:var(--err); border-color:#3a2422; }
```

**6. 点亮节奏 / 新增高亮**（青闪一下沉淀）：
```css
tr.fresh{ animation:flash 1.2s ease-out; }
@keyframes flash{ 0%{ background:rgba(46,196,160,.22); } 100%{ background:transparent; } }
```

**7. 模态框 / 遮罩**（遮罩用底色加深，描边带极淡青辉）：
```css
.overlay{ background:rgba(3,6,8,.7); }
.modal{ background:var(--bg); border:1px solid var(--ghost);
  box-shadow:0 0 0 1px rgba(46,196,160,.08),0 20px 60px rgba(0,0,0,.5); }
```

**8. 状态 / 在线**：在线/ok 值用 `color:var(--accent)`；离线/缺失用 `var(--dim)`；
错误用 `var(--err)`。chip/tag 用 ghost 描边 + dim 字，强调态切青。

## Do / Don't

- ✅ 只用 token 变量；强调一律青；语义色仅成功/错误。
- ✅ 等宽字体 + 大写小标题 + 字距；点阵纹理/点状下划线点到为止。
- ✅ 选中＝青左竖条；新增＝青闪一拍再褪。
- ❌ 别引入第二种装饰色（蓝/紫/暖色），别用裸 hex，别上渐变大色块。
- ❌ 别用大面积 opa fade 过场；别让点阵纹理影响正文可读性。

## 落地到 daboluo.cc 官网

官网（`bbclaw-reference/web`，闭源、需重新部署）统一时：把上面 `:root` 设为全站 token，
nav/hero/卡片/按钮按本配方替换；wordmark 用点状下划线；CTA 用青实心、次级用 ghost。
改完走 `make deploy-web`。**改任何颜色/点阵几何前，先更新 `design/UI_DESIGN_LANGUAGE.md`
并同步 `firmware/include/bb_ui_theme.h`，保持固件/Web 同源。**
