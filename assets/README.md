# Assets / 图标资源

## DeepSeek icon

`logo.svg` places the blue whale favicon used by the
[official DeepSeek API documentation](https://api-docs.deepseek.com/) on a
512 × 512 white canvas. The whale path and its `#4D6BFE` color are copied
unchanged; it is only scaled 5.6× and centered so the mark is not cropped by
square icon slots. `logo.png` is its 512 × 512 raster version, used by CPA
management clients and the plugin store. The READMEs use the SVG.

- Official asset: <https://api-docs.deepseek.com/img/favicon.svg>
- Retrieved: 2026-10-28
- Original view box: `0 0 63.1196 46.4033`

Plugin icon URL:

```text
https://raw.githubusercontent.com/dillonzq/cpa-deepseek/main/assets/logo.png
```

To regenerate the PNG after editing the SVG:

```sh
rsvg-convert -w 512 -h 512 assets/logo.svg -o assets/logo.png
```

The DeepSeek name and icon belong to their respective rights holders and identify
the service this plugin connects to. This project is an independent integration;
use of the icon does not imply endorsement by DeepSeek. The repository's MIT
license applies to the plugin code and does not grant rights to DeepSeek's marks.

## DeepSeek 图标

`logo.svg` 将 [DeepSeek 官方 API 文档](https://api-docs.deepseek.com/)使用的
蓝色鲸鱼 favicon 置于 512 × 512 白色画布上。鲸鱼路径及 `#4D6BFE` 配色原样保留，
仅放大 5.6 倍并居中，避免在方形图标位中被裁切。`logo.png` 为其 512 × 512 位图版本，
用于 CPA 管理界面和插件商店；中英文 README 使用 SVG。

- 官方文件：<https://api-docs.deepseek.com/img/favicon.svg>
- 获取日期：2026-10-28
- 原始 view box：`0 0 63.1196 46.4033`

DeepSeek 的名称和图标归其各自权利人所有，用于标识本插件对接的服务。
本项目为独立集成，图标使用不代表 DeepSeek 官方背书。本仓库的 MIT 许可证适用于
插件代码，不授予 DeepSeek 标志的相关权利。
