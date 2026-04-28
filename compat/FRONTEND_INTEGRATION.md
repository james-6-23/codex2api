# 前端集成指南：2K/4K 图片放大

## 📱 前端 API 调用示例

### 1. 基础调用（生成原图）

```javascript
// 原有调用方式（不变）
const response = await fetch('/api/admin/image-studio/jobs', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'Authorization': 'Bearer YOUR_API_KEY'
  },
  body: JSON.stringify({
    prompt: "a beautiful sunset over mountains",
    model: "gpt-image-2",
    size: "1024x1024",
    quality: "standard",
    output_format: "png"
  })
});

const result = await response.json();
console.log(result.job);
```

### 2. 生成 2K 高清图片

```javascript
const response = await fetch('/api/admin/image-studio/jobs', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'Authorization': 'Bearer YOUR_API_KEY'
  },
  body: JSON.stringify({
    prompt: "a beautiful sunset over mountains",
    model: "gpt-image-2",
    size: "1024x1024",
    quality: "standard",
    output_format: "png",
    upscale: "2k"  // 🆕 新增参数
  })
});

const result = await response.json();
// result.job.assets[0].width === 2560
// result.job.assets[0].height === 2560
```

### 3. 生成 4K 超高清图片

```javascript
const response = await fetch('/api/admin/image-studio/jobs', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'Authorization': 'Bearer YOUR_API_KEY'
  },
  body: JSON.stringify({
    prompt: "a beautiful sunset over mountains",
    model: "gpt-image-2",
    size: "1024x1024",
    quality: "standard",
    output_format: "png",
    upscale: "4k"  // 🆕 新增参数
  })
});

const result = await response.json();
// result.job.assets[0].width === 3840
// result.job.assets[0].height === 3840
```

## 🎨 React 组件示例

### 完整的图片生成组件

```jsx
import React, { useState } from 'react';

function ImageGenerator() {
  const [prompt, setPrompt] = useState('');
  const [upscale, setUpscale] = useState(''); // '', '2k', '4k'
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState(null);

  const generateImage = async () => {
    setLoading(true);
    try {
      const response = await fetch('/api/admin/image-studio/jobs', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Authorization': `Bearer ${localStorage.getItem('apiKey')}`
        },
        body: JSON.stringify({
          prompt,
          model: 'gpt-image-2',
          size: '1024x1024',
          output_format: 'png',
          upscale  // 传递放大参数
        })
      });

      const data = await response.json();
      setResult(data.job);
    } catch (error) {
      console.error('生成失败:', error);
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="image-generator">
      <h2>AI 图片生成</h2>
      
      {/* 提示词输入 */}
      <textarea
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        placeholder="输入图片描述..."
        rows={4}
      />

      {/* 分辨率选择 */}
      <div className="upscale-selector">
        <label>输出分辨率：</label>
        <select value={upscale} onChange={(e) => setUpscale(e.target.value)}>
          <option value="">原图 (1024x1024)</option>
          <option value="2k">2K 高清 (2560x2560)</option>
          <option value="4k">4K 超高清 (3840x3840)</option>
        </select>
      </div>

      {/* 生成按钮 */}
      <button onClick={generateImage} disabled={loading || !prompt}>
        {loading ? '生成中...' : '生成图片'}
      </button>

      {/* 结果展示 */}
      {result && (
        <div className="result">
          <h3>生成结果</h3>
          {result.assets.map((asset, idx) => (
            <div key={idx} className="asset-card">
              <img 
                src={`/api/admin/image-studio/assets/${asset.id}/file`}
                alt={`Generated ${idx + 1}`}
              />
              <div className="asset-info">
                <p>尺寸: {asset.actual_size}</p>
                <p>大小: {(asset.bytes / 1024 / 1024).toFixed(2)} MB</p>
                <p>格式: {asset.output_format}</p>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export default ImageGenerator;
```

### 样式参考

```css
.image-generator {
  max-width: 800px;
  margin: 0 auto;
  padding: 20px;
}

textarea {
  width: 100%;
  padding: 12px;
  border: 1px solid #ddd;
  border-radius: 8px;
  font-size: 14px;
  margin-bottom: 16px;
}

.upscale-selector {
  margin-bottom: 16px;
}

.upscale-selector label {
  display: block;
  margin-bottom: 8px;
  font-weight: 500;
}

.upscale-selector select {
  width: 100%;
  padding: 10px;
  border: 1px solid #ddd;
  border-radius: 8px;
  font-size: 14px;
}

button {
  width: 100%;
  padding: 12px;
  background: #007bff;
  color: white;
  border: none;
  border-radius: 8px;
  font-size: 16px;
  cursor: pointer;
}

button:disabled {
  background: #ccc;
  cursor: not-allowed;
}

.result {
  margin-top: 24px;
}

.asset-card {
  border: 1px solid #ddd;
  border-radius: 8px;
  padding: 16px;
  margin-bottom: 16px;
}

.asset-card img {
  width: 100%;
  border-radius: 4px;
  margin-bottom: 12px;
}

.asset-info p {
  margin: 4px 0;
  color: #666;
  font-size: 14px;
}
```

## 📊 响应格式

### 成功响应

```json
{
  "job": {
    "id": 123,
    "status": "success",
    "prompt": "a beautiful sunset over mountains",
    "model": "gpt-image-2",
    "params_json": "{\"upscale\":\"2k\"}",
    "assets": [
      {
        "id": 456,
        "filename": "123-01-abc123.png",
        "storage_path": "/data/images/123-01-abc123.png",
        "mime_type": "image/png",
        "bytes": 8388608,
        "width": 2560,
        "height": 2560,
        "model": "gpt-image-2",
        "requested_size": "1024x1024",
        "actual_size": "2560x2560",
        "quality": "standard",
        "output_format": "png",
        "revised_prompt": "A breathtaking sunset..."
      }
    ],
    "created_at": "2026-04-28T15:30:00Z",
    "duration_ms": 12500
  }
}
```

### 错误响应

```json
{
  "error": "提示词不能为空"
}
```

## 🔍 参数说明

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `prompt` | string | ✅ | - | 图片描述提示词 |
| `model` | string | ❌ | `gpt-image-2` | 模型名称 |
| `size` | string | ❌ | `1024x1024` | 原始生成尺寸 |
| `quality` | string | ❌ | `standard` | 生成质量 |
| `output_format` | string | ❌ | `png` | 输出格式 |
| `upscale` | string | ❌ | `""` | 🆕 放大档位：`""` (原图) / `"2k"` (2560px) / `"4k"` (3840px) |

## ⚡ 性能说明

### 生成时间对比

| 配置 | 原图生成 | 2K 放大 | 4K 放大 |
|------|---------|---------|---------|
| 首次生成 | ~10s | ~11s | ~12s |
| 缓存命中 | ~10s | ~10s | ~10s |

**说明**：
- 首次放大会增加 1-2 秒处理时间
- 缓存命中后，放大几乎无额外耗时
- 同一张图片的相同放大档位会被缓存

### 文件大小对比

| 分辨率 | 文件大小 | 说明 |
|--------|---------|------|
| 1024x1024 (原图) | ~2-5 MB | PNG 格式 |
| 2560x2560 (2K) | ~8-15 MB | PNG 格式 |
| 3840x3840 (4K) | ~20-30 MB | PNG 格式 |

## 🎯 使用建议

### 何时使用不同档位

1. **原图（默认）**
   - 快速预览
   - 社交媒体分享
   - 网页展示

2. **2K 高清**
   - 高清壁纸
   - 印刷品（A4 尺寸）
   - 专业展示

3. **4K 超高清**
   - 大幅印刷
   - 商业用途
   - 最高质量要求

### 前端优化建议

```javascript
// 1. 根据用途智能推荐
function getRecommendedUpscale(purpose) {
  const recommendations = {
    'social': '',      // 社交媒体：原图
    'wallpaper': '2k', // 壁纸：2K
    'print': '4k'      // 印刷：4K
  };
  return recommendations[purpose] || '';
}

// 2. 显示预估文件大小
function estimateFileSize(upscale) {
  const sizes = {
    '': '2-5 MB',
    '2k': '8-15 MB',
    '4k': '20-30 MB'
  };
  return sizes[upscale] || '未知';
}

// 3. 显示预估生成时间
function estimateGenerationTime(upscale, cached = false) {
  if (cached) return '~10 秒';
  const times = {
    '': '~10 秒',
    '2k': '~11 秒',
    '4k': '~12 秒'
  };
  return times[upscale] || '~10 秒';
}
```

## 🐛 故障排查

### 问题 1：放大后图片模糊

**原因**：原图分辨率过低

**解决**：
- 使用更高质量的原图生成
- 选择 `quality: "hd"` 参数

### 问题 2：生成时间过长

**原因**：首次放大需要计算

**解决**：
- 正常现象，缓存命中后会很快
- 可以先生成原图，让用户预览，后台异步放大

### 问题 3：文件过大

**原因**：4K PNG 文件体积大

**解决**：
- 使用 2K 档位（通常已足够）
- 考虑使用 JPEG 格式（需要后端支持）

## 📚 完整示例项目

查看 `/d/cc/my-project/codex2api/compat/examples/` 目录获取完整的前端示例项目。

## 🔗 相关文档

- [后端集成指南](./INTEGRATION.md)
- [API 文档](./README.md)
- [变更日志](./CHANGELOG.md)
