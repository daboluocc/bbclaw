# 发布记录索引

本目录记录每次 BBClaw 固件 / Adapter OTA 发布的完整信息，方便下次执行时对比参数、改动、回滚计划。

## 使用方式

每次发布完成后，生成一份记录文件：

```bash
# 文件名命名：版本号.md 或 YYYY-MM-DD-版本号.md
cp releases/_template.md releases/v0.5.2.md
# 然后编辑填充细节
```

查看历史：
```bash
ls -ltr releases/v*.md     # 按时间顺序列出
cat releases/v0.5.2.md     # 查看某次发版的完整信息
```

## 每条记录包含

| 字段 | 用途 |
|------|------|
| 版本号 / 日期 | 快速定位 |
| 发布方式 | 灰度 / 正式（Tag） |
| 执行命令 | 精确再现（环境变量、flag 等） |
| 前置状态 | git hash、IDF 版本、固件大小 |
| 关键改动 | commit message 或 CHANGELOG 行号 |
| OTA 服务端状态 | URL、active bundle、用户可见性 |
| 验证方式 | 测试命令 |
| 回滚计划 | 若出问题如何处理 |

