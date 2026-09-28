# Dashboard design: Liquid Glass

The dashboard keeps the look of the Windows prototype: a frosted, refracting glass popup with a status glyph, three cards, recent activity and five buttons. This page records the design **as the code implements it**. The prototype's old design notes gave different numbers in several places. Wherever they disagree, the code constants below are the reference, because they are what the prototype actually shipped.

Where the values live:

- Colours, grid, type and motion tokens: [`internal/ui/web/tokens.css`](../internal/ui/web/tokens.css)
- The glass optics: [`internal/glass`](../internal/glass)
- The motion primitives: [`internal/ui/web/anim.js`](../internal/ui/web/anim.js), a one-to-one port of the prototype's `Anim.cs`

## The rule that is never broken: no hover raise

**Hover never raises anything.** On hover, an element may only change its **fill** and **rim** (border) brightness, and its text colour. It may never use any of these, on buttons, cards, rows or icons:

- scale, translate or any other transform
- `translateY` lift
- `box-shadow` raise
- `top` or `margin-top` offset
- `filter: drop-shadow`
- elevation of any kind

The prototype's 1.02 hover scale was removed on purpose.

`internal/ui/hover_lint_test.go` enforces this in CI:

- It parses every CSS rule whose selector contains `:hover`, and fails on `transform`, `translate`, `scale`, `box-shadow`, `top`, `margin-top` or `filter: drop-shadow`.
- It fails if `app.js` attaches `mouseenter` or `mouseover` handlers that write `style.transform`.

The only press feedback is the "gel press" described under Motion, which reacts to a pointer press, not to hover.

## Window

| Property | Value |
|---|---|
| Size | 460 × 640 DIP, multiplied by the monitor scale (per-monitor DPI aware) |
| Shape | Squircle, superellipse exponent n = 4 (continuous curvature), corner radius **40** |
| Frame | Borderless `WS_POPUP`, tool window (no taskbar button), topmost, `CS_DROPSHADOW` |
| Placement | On the monitor under the cursor, in the corner at the taskbar edge, inset 14 DIP |
| Dismissal | Esc, focus loss, or the close action. The window hides and stays warm for the next show. |

## Glass backdrop (Windows)

On every show, the tray process runs these steps:

1. **Capture.** It captures the desktop behind the popup's rectangle while the popup is hidden. If the popup was visible, it first waits 45 ms after hiding it.
2. **Measure.** It measures the mean Rec. 709 luminance on a 3-pixel grid.
3. **Blur.** It downsamples to half resolution with a 2×2 box filter. It runs a box blur with radius `max(1, round(5 × scale) / 2)`, twice, which approximates a Gaussian. It boosts saturation by 2.0, then upsamples bilinearly.
4. **Refract.** It refracts the blurred image through a squircle lens. The lens is a displacement map cached per size, with up to 4 entries.
5. **Serve.** It serves the result as `/api/backdrop.png`. The page fades it in.

| Constant | Code value | Prototype design notes said |
|---|---|---|
| Window corner radius | **40** | 22 |
| Blur radius | **5** (full-res units, run at half resolution) | 28 px |
| Blur passes and resolution | **2 passes at ½ resolution** | 3 passes at ¼ resolution |
| Saturation | **2.0** | 1.4 |
| Index of refraction | **1.5** | 1.5 |
| Chromatic dispersion | **22 %** of the shift split across R and B at the rim | 0.9 (IOR delta) |
| Bevel depth | **30** DIP | 22 px |
| Maximum inward pull at the rim | **30** DIP | — |
| Refraction strength | **1.0** | — |
| Base tint | `#11131A`, alpha **0.12** (dark backdrop) or **0.20** (bright backdrop, mean luminance > 0.5) | rgba(255,255,255,0.06) |
| Darkening in the lens | × **0.78** (dark) or × **0.62** (bright) | — |
| Cool shift | R × 0.955, G × 0.985, B × 1.045 | (0.93, 0.96, 1.06) |
| Key light | from the upper left, direction (−0.62, −0.78) | — |
| Specular strength | **0.90** (Blinn-Phong) | — |
| Fresnel rim | **0.40**, Schlick with F0 = 0.04 | F0 = 0.04 |
| Target platform | Windows 10 1809+ and 11 | Windows 11 only |

The radius, bevel and maximum shift scale with the monitor. Legibility comes from the content scrim and the card floors (next section), not from darkening the backdrop further. A heavy multiply would kill the refraction that makes the material read as glass.

## Layers above the backdrop (D4)

- **Scrim:** a vertical wash of `rgb(8, 9, 13)`. Its alpha is 0.471 at the top, 0.376 from 22 % to 72 % of the height, and 0.588 at the bottom, behind the buttons.
- **Cards:** radius 16 and width 400, with a gap of 12 between them. Each card has a legibility floor of `rgba(10, 12, 17, 0.408)` (alpha 104), a sheen of alpha 20, a rim of alpha 34 and a top highlight line of alpha 54.
- **Window rim:** one rim along the squircle edge, plus the **breathing specular arc** on the upper left. The arc drifts on a 6 s sine while a peer is connected.
- **Entrance sweep:** a specular band crosses the window once per show (0.62 s, OutCubic).

## Tokens (D12)

| Token | Value |
|---|---|
| Text | Fg `#F6F8FC`, Dim `#BCC2CE`, Faint `#9AA3A8`, Accent `#60A6FF` |
| State colours (dashboard) | In sync `#3AC672`, Syncing `#569CF6`, Scanning `#F0B838`, Disconnected and Error `#EE5C52`, Paused `#96969E`, Not running and API key rejected `#787C86` |
| Grid | PadX 30, ColL 46, ColR 414, Col2 230 (px) |
| Type sizes | 15.5 pt title, 11.5 pt values, 9 pt sublines, 8.5 pt buttons and mono, 7.5 pt labels |
| Fonts | `"Segoe UI", -apple-system, "SF Pro Text", Cantarell, Inter, "DejaVu Sans", sans-serif`; mono `Consolas, "SF Mono", Menlo, "DejaVu Sans Mono", monospace` |
| Buttons | height 34, gap 7, radius 11. Fill alpha 16 (hover 42), rim alpha 42 (hover 114), top line alpha 44 (hover 128). |

**Tray icon colours**, drawn by `internal/icon`:

- In sync `#2EBE64`
- Syncing `#3896F0`
- Scanning `#F0B428`
- Disconnected and Error `#E65046`
- Paused `#96969B`
- Not running and API key rejected `#6E6E73`

The tray glyphs are a tick (in sync), a progress arc (syncing), a centre dot (scanning, paused and not running) and an X (disconnected and error). "API key rejected" shows the X in the grey "not running" colour. The dashboard's larger status glyph adds pause bars for Paused and a specular glint.

![The tray icon in each state](../assets/screenshots/tray-states.png)

## Motion (D11)

| Moment | Animation |
|---|---|
| Show | OutBack over 0.34 s. Scale goes from 0.965 to 1 with the origin at the tray corner, content rises 12 px, and the backdrop and scrim fade in. The specular sweep plays (0.62 s). |
| Hide | OutCubic over 0.18 s, then the host hides the window. |
| State change | The state colour lerps toward its new value, and the glyph sweep replays. |
| Sync progress bar | A spring (stiffness 120, damping 20). While syncing or scanning, a shimmer band travels along the fill on a 2.2 s loop. |
| Transfer meters | Springs (150, 22). The meters are full at 8 MB/s. |
| Recent activity | 40 items kept and 4 shown. New rows slide in 10 px and fade in. |
| Button press ("gel press") | On pointer down, a spring (420, 30) scales the button to 0.95. A click fires only when the pointer is released inside the button. |
| Presence | The connected-peer dot pulses with the 6 s breathing loop. |

The animation loop runs on `requestAnimationFrame` and stops when nothing is moving, except for the presence pulse while a peer is connected. With **`prefers-reduced-motion: reduce`**, the sweep, shimmer, breathing and entrance are off, and springs snap to their targets.

## Materials per OS

| OS | Page mode | Material |
|---|---|---|
| Windows | `?mode=glass` | The refracted glass backdrop above, with the scrim, card floors, rim and breathing arc |
| macOS | `?mode=browser` | A centred 460 × 640 squircle card with a static dark gradient material, the same cards, and no screen capture. It opens in the default browser. |
| Linux | `?mode=browser` | Same as macOS |
| Windows without WebView2 | `?mode=browser` | Same as macOS, in the default browser |

The page also has a `?mode=vibrancy` material (a transparent body over a native blur) for a future native macOS host. Release 1.0 does not use it.

## Accessibility

- Every control is a real `<button>` or form element with a label. The focus ring is always visible.
- Text colours under 12 px keep at least 4.5:1 contrast on the glass. The card floors guarantee this over any wallpaper.
- `prefers-reduced-motion` is honoured, as described above.
- The state is never shown by colour alone: each state has its own glyph and text.
