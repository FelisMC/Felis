# Felis 图标

基于参考图片手工临摹。矢量文件仅使用路径、形状、渐变和蒙版，没有嵌入位图。所有彩色导出均来自 [`../felis-logo.svg`](../felis-logo.svg)。

| 文件 | 用途 |
| --- | --- |
| `../felis-logo.svg` | 1040 × 640 紧裁透明矢量原稿 |
| `../felis-logo.png` | README 用图，保留现有 540 × 341 尺寸 |
| `felis-reference.svg` / `.png` | 保留参考图的 1254 × 1254 白底构图 |
| `felis-icon.svg` | 居中的方形透明图标 |
| `felis-app.svg` / `.png` | 浅绿圆角应用图标，1024 × 1024 |
| `felis-app-dark.svg` / `.png` | 深绿圆角应用图标，1024 × 1024 |
| `felis-maskable.svg` / `.png` | 不透明方形背景，猫脸位于中心安全圆内 |
| `felis-monochrome.svg` | 紧裁黑色单色版，眼睛和鼻子镂空 |
| `felis-mask.svg` | 方形单色版，供 Safari 固定标签使用 |
| `png/icon-{size}.png` | 透明图标：16、20、24、32、40、48、64、96、128、180、192、256、512、1024 px |
| `felis.ico` | Windows 多分辨率图标：16、24、32、48、64、128、256 px |
| `felis.icns` / `felis.iconset/` | macOS 图标及 16、32、128、256、512 pt 的 1× / 2× PNG |
| `preview.png` | 图标套装预览 |

网页资源已放入 `panel/public/`，并由 `panel/index.html` 引用：SVG / ICO / PNG favicon、180 px Apple Touch Icon、Safari 固定标签图标，以及 192 / 512 px 普通和 maskable 图标。`site.webmanifest` 提供应用名称和图标元数据，不提供离线缓存。

修改矢量原稿后可重新导出；需要 Node.js 和 `sharp`，macOS 的 ICNS 导出还使用系统 `iconutil`。在仓库根目录运行：

```sh
NODE_PATH=/path/to/node_modules node scripts/generate-icons.cjs
```

`NODE_PATH` 指向已安装 `sharp` 的 `node_modules`。导出工具不会增加面板的运行依赖；非 macOS 平台仍会生成其余格式和 `iconset`。
