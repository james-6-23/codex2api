# Codex2API 贡献指南

感谢您对 Codex2API 项目的关注！本指南将帮助您了解如何参与项目贡献。

## 目录

- [开发环境设置](#开发环境设置)
- [代码规范](#代码规范)
- [提交规范](#提交规范)
- [Pull Request 流程](#pull-request-流程)
- [测试要求](#测试要求)
- [文档更新](#文档更新)

---

## 开发环境设置

### 前置要求

- Go 1.26.5 或更高版本
- Node.js 22.12 或更高版本
- Docker 和 Docker Compose
- Git

### 本地开发环境搭建

1. **Fork 并克隆仓库**

```bash
git clone https://github.com/YOUR_USERNAME/codex2api.git
cd codex2api
git remote add upstream https://github.com/james-6-23/codex2api.git
```

2. **安装后端依赖**

```bash
go mod download
go mod verify
```

3. **安装前端依赖**

```bash
cd frontend
npm ci
cd ..
```

4. **配置环境**

```bash
cp .env.example .env
# 编辑 .env 配置本地数据库（或使用 SQLite 模式）
```

5. **构建前端**

```bash
cd frontend && npm run build && cd ..
```

6. **启动服务**

```bash
# 方式1: 本地运行
go run .

# 方式2: Docker 模式
docker compose -f docker-compose.local.yml up -d --build
```

---

## 代码规范

### Go 代码规范

我们遵循标准的 Go 代码规范：

1. **使用 gofmt 格式化代码**

```bash
gofmt -w .
```

2. **使用 golint 检查**

```bash
go install golang.org/x/lint/golint@latest
golint ./...
```

3. **使用 go vet 静态分析**

```bash
go vet ./...
```

4. **代码风格要求**

- 使用驼峰命名法（CamelCase）
- 导出的函数和类型必须添加注释
- 错误处理优先，避免忽略错误
- 使用有意义的变量名

```go
// 好的示例
// GetAccountByID 根据 ID 获取账号信息
func GetAccountByID(ctx context.Context, id int64) (*Account, error) {
    if id <= 0 {
        return nil, fmt.Errorf("invalid account id: %d", id)
    }
    // ...
}

// 不好的示例
func getaccount(id int64) *Account {
    // 忽略错误
    db.Query("SELECT * FROM accounts WHERE id = ?", id)
    // ...
}
```

### 前端代码规范

1. **运行前端验证**

```bash
cd frontend
npm test
npm run typecheck
npm run build
```

2. **代码风格**

- 使用 2 空格缩进
- 使用单引号
- 末尾分号可选
- 最大行长度 100 字符

### 项目结构规范

```
codex2api/
├── admin/          # 管理后台 API 处理器
├── auth/           # 账号池和调度逻辑
├── cache/          # 缓存抽象层
├── config/         # 配置加载
├── database/       # 数据库访问层
├── proxy/          # 代理转发和翻译
├── frontend/       # React 前端
│   ├── src/
│   │   ├── components/  # UI 组件
│   │   ├── pages/       # 页面组件
│   │   └── locales/     # 国际化
├── docs/           # 文档
└── main.go         # 入口文件
```

---

## 提交规范

我们使用 [Conventional Commits](https://www.conventionalcommits.org/) 规范：

### 提交格式

```
<type>(<scope>): <subject>

<body>

<footer>
```

### 类型说明

| 类型 | 说明 | 示例 |
|------|------|------|
| `feat` | 新功能 | `feat(scheduler): 添加快速调度器` |
| `fix` | 修复 Bug | `fix(auth): 修复账号冷却时间计算错误` |
| `docs` | 文档更新 | `docs(api): 更新 API 文档` |
| `style` | 代码格式 | `style: 格式化代码` |
| `refactor` | 重构 | `refactor(proxy): 优化请求执行器` |
| `test` | 测试相关 | `test(auth): 添加调度器单元测试` |
| `chore` | 构建/工具 | `chore: 更新依赖` |

### 示例

```
feat(api): 添加账号批量导入功能

- 支持 TXT 和 JSON 格式导入
- 添加 SSE 进度推送
- 自动去重和验证

Fixes #123
```

---

## Pull Request 流程

### 1. 创建分支

```bash
# 从 main 分支创建特性分支
git checkout main
git pull upstream main
git checkout -b feature/your-feature-name
```

分支命名规范：
- `feature/` - 新功能
- `fix/` - Bug 修复
- `docs/` - 文档更新
- `refactor/` - 代码重构

### 2. 开发提交

```bash
# 提交代码
git add .
git commit -m "feat: 添加某某功能"

# 推送到你的 Fork
git push origin feature/your-feature-name
```

### 3. 创建 PR

1. 访问你的 Fork 页面
2. 点击 "Compare & pull request"
3. 填写 PR 描述：

```markdown
## 描述
简要描述这个 PR 的目的

## 变更类型
- [ ] Bug 修复
- [ ] 新功能
- [ ] 文档更新
- [ ] 代码重构

## 测试
- [ ] 添加了单元测试
- [ ] 手动测试通过
- [ ] 所有现有测试通过

## 相关 Issue
Fixes #123
```

### 4. 代码审查

- 维护者会进行代码审查
- 根据反馈进行修改
- 确保 CI 检查通过

### 5. 合并

- PR 被批准后会被合并
- 可选择 Squash 合并保持提交历史整洁

---

## 测试要求

### 单元测试

```bash
# 运行所有测试
go test ./...

# 运行特定包测试
go test ./auth/...

# 带覆盖率
go test -cover ./...

# 生成覆盖率报告
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### 测试规范

1. **测试文件命名**: `xxx_test.go`
2. **测试函数命名**: `TestFunctionName`
3. **使用表格驱动测试**

```go
func TestCalculateScore(t *testing.T) {
    tests := []struct {
        name     string
        account  *Account
        expected float64
    }{
        {
            name:     "healthy account",
            account:  &Account{HealthTier: Healthy},
            expected: 100,
        },
        {
            name:     "banned account",
            account:  &Account{HealthTier: Banned},
            expected: 0,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := CalculateScore(tt.account)
            if result != tt.expected {
                t.Errorf("got %v, want %v", result, tt.expected)
            }
        })
    }
}
```

### 集成测试

```bash
# 启动测试环境
docker compose -f docker-compose.test.yml up -d

# 运行集成测试
go test -tags=integration ./...

# 清理
docker compose -f docker-compose.test.yml down
```

---

## 文档更新

### 文档位置

| 文档 | 路径 | 说明 |
|------|------|------|
| API 文档 | `docs/API.md` | API 端点说明 |
| 部署文档 | `docs/DEPLOYMENT.md` | 部署指南 |
| 配置文档 | `docs/CONFIGURATION.md` | 配置说明 |
| 架构文档 | `docs/ARCHITECTURE.md` | 架构设计 |
| 故障排查 | `docs/TROUBLESHOOTING.md` | 问题排查 |
| 贡献指南 | `docs/CONTRIBUTING.md` | 本文件 |

### 文档规范

1. 使用 Markdown 格式
2. 添加目录便于导航
3. 代码块标明语言类型
4. 使用表格展示参数

### API 文档更新

添加新 API 时，需要更新：

1. `docs/API.md` - 端点说明
2. `admin/handler.go` 或 `proxy/handler.go` - 代码注释
3. 如有必要，更新 README.md

---

## 发布流程

### 版本号规范

使用 [Semantic Versioning](https://semver.org/):

- `MAJOR.MINOR.PATCH`
- MAJOR: 不兼容的 API 变更
- MINOR: 向下兼容的功能添加
- PATCH: 向下兼容的问题修复

### 发布步骤

1. 更新版本号
2. 更新 CHANGELOG.md
3. 创建 Git Tag
4. 构建并推送镜像

```bash
# 创建发布
git tag -a v1.0.0 -m "Release version 1.0.0"
git push origin v1.0.0

# CI 会自动构建并推送镜像
```

---

## 获取帮助

- **GitHub Issues**: [提交问题](https://github.com/james-6-23/codex2api/issues)
- **Discussions**: [参与讨论](https://github.com/james-6-23/codex2api/discussions)

---

## 行为准则

- 尊重所有参与者
- 欢迎新手，耐心解答问题
- 专注于建设性的技术讨论
- 遵守开源社区规范

感谢您的贡献！
