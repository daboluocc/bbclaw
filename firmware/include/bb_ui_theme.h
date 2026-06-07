/**
 * BBClaw UI theme tokens — dot-matrix / Nothing-style design language.
 *
 * Source of truth: design/UI_DESIGN_LANGUAGE.md. Every UI surface includes
 * this header instead of defining a local palette; do NOT add bare hex
 * colors in page code — register a token here (and in the design doc) first.
 *
 * Monochrome + single accent: deep near-black bg, cool-white / cool-grey
 * text layers, teal as the ONLY decorative accent. Green/red are reserved
 * for charging/error semantics.
 */
#ifndef BB_UI_THEME_H
#define BB_UI_THEME_H

/* ── surfaces ── */
#define BB_UI_BG 0x070b0e /* the one screen background (incl. overlay masks) */

/* ── dot matrix ── */
#define BB_UI_DOT_LIT 0xdfeaec   /* lit dot / primary text (cool white)      */
#define BB_UI_DOT_GHOST 0x152128 /* ghost dot / separators / assistant面     */

/* ── text ── */
#define BB_UI_TEXT_DIM 0x6e8a93 /* secondary text (cool blue-grey)           */
#define BB_UI_WORDMARK 0x4f6f67 /* footer wordmark (dim teal-grey)           */

/* ── accent + semantic (the only colors) ── */
#define BB_UI_ACCENT 0x2ec4a0 /* teal — the single decorative accent         */
#define BB_UI_OK 0x4cd964     /* charging / success                          */
#define BB_UI_ERR 0xe66f6f    /* low battery / error                         */

/* ── dot-matrix geometry (base grid; small elements may use 4/7) ── */
#define BB_UI_MX_DOT 5
#define BB_UI_MX_PITCH 9

#endif /* BB_UI_THEME_H */
