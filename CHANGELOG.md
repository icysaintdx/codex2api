# 版本更新日志


## [v1.10.0] - 2026-05-02 22:30
**版本代号**: Free 账号图片生成版
**文档总数**: 1

### 🆕 新增功能
#### Free 账号图片生成 ⭐
- **功能**: Free 账号支持图片生成功能
- **实现**: 使用 `/backend-api/f/conversation` 接口
- **参数**: `system_hints: ["picture_v2"]`
- **位置**: proxy/images_free.go
- **文档**: [Free 账号图片生成功能说明](docs/feature/Free账号图片生成功能说明.md)
- **文件**: `proxy/images_free.go`

### 🐛 Bug 修复
#### 移除 action 字段 ⭐
- **问题**: 上游 API 拒绝包含 action 字段的请求
- **原因**: action 字段不再被上游支持
- **修复**: 从 tool 参数中移除 action 字段
- **文件**: `proxy/images.go`

### 📚 文档更新
- [Free 账号图片生成功能说明](docs/feature/Free账号图片生成功能说明.md) ⭐

---


---
