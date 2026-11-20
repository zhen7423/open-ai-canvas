# 沧元 Midjourney

独立声明式协议插件（`yingce.plugin/v2`），提供 `cangyuan-midjourney-v7`（V7）、`cangyuan-midjourney-v82`（1K/2K）、`cangyuan-midjourney-v82-web`（V8.2 官网模型名）和 `cangyuan-midjourney-mj2`（MJ2）四条线路。使用宿主的渠道鉴权、出站安全检查、下载和资源持久化，不执行额外服务代码。

V7 使用 `midjourney-v7`，无分辨率和速度档。1K/2K 使用 `midjourney-1k` / `midjourney-2k`，供应商默认版本为 V8.2，提示词可显式写 `--v 8.2`。供应商另把 V8.2 以官网模型名 `Midjourney-v8.2` 单独上架，参数合同与 1K/2K 不同，走 `cangyuan-midjourney-v82-web`；模型 ID 校验不区分大小写，出站固定按 `Midjourney-v8.2` 发送。

`cangyuan-midjourney-v82-web` 固定不传 `n`（每次返回 4 张，全部保存）、不接受蒙版、提示词最多 1024 个字符、比例只写 `size`；参考图最多 8 张，超过 4 张时必须逐项标注用途 `content` / `style`；`weird` 与 `hd` 未设置时不发送，`hd` 缺省由供应商按开启处理。

`cangyuan-midjourney-mj2` 使用按次计费的 `mj2-midjourney`：不拆分辨率档，不支持蒙版、速度和质量档，参考图最多 5 张，`n` 固定为 1（结果里返回的多张图片全部保存）。版本与风格按供应商要求写在提示词末尾，比例只写 `size`；参考图一律进 `images` 并固定走 generations 端点，没有 edits 分支，也不发送 quality、resolution、speed、background、output_format、mask 或 `providerOptions`。

四条线路不能互换。1K/2K 返回官网 CDN 原链，服务器下载可能遇到 HTTP 403；本插件不会伪报保存成功或自动换模型。V7 返回供应商可直接下载的结果地址。`n=1` 表示一次请求，返回的所有图片均交给宿主保存。

完整接口见 [docs/interface.md](docs/interface.md)。
