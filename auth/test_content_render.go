package auth

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// ==================== 测活内容渲染 (issue #320) ====================
//
// test_content 支持两层随机化，减少"一批账号都发同一句测活内容"的指纹特征：
//  1. 多行 = 候选池：按行拆分（滤空行），每次探活随机抽一行。
//     单行输入行为与旧版完全一致，向后兼容。
//  2. 变量模板：抽中的行内的 {{...}} 占位符在发送前展开：
//     {{time}}           当前时间 15:04:05
//     {{date}}           当前日期 2006-01-02
//     {{datetime}}       2006-01-02 15:04:05
//     {{timestamp}}      Unix 秒级时间戳
//     {{rand}}           0-9999 随机整数
//     {{rand:min-max}}   [min,max] 范围随机整数（min>max 时自动交换）
//     未知变量原样保留（测活内容是自由文本，宽松处理不报错）。

// RenderTestContent 把配置的测活内容渲染为本次探活实际发送的文本：
// 按行随机抽取候选 + 展开变量。空输入回落 DefaultTestContent。
func RenderTestContent(raw string) string {
	line := pickTestContentLine(raw)
	if line == "" {
		return DefaultTestContent
	}
	return expandTestContentVariables(line)
}

// TestContentLines 返回按行拆分后的非空候选行（用于校验/预览）。
func TestContentLines(raw string) []string {
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func pickTestContentLine(raw string) string {
	lines := TestContentLines(raw)
	switch len(lines) {
	case 0:
		return ""
	case 1:
		return lines[0]
	default:
		return lines[rand.IntN(len(lines))]
	}
}

func expandTestContentVariables(line string) string {
	if !strings.Contains(line, "{{") {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for {
		start := strings.Index(line, "{{")
		if start < 0 {
			b.WriteString(line)
			break
		}
		end := strings.Index(line[start+2:], "}}")
		if end < 0 {
			b.WriteString(line)
			break
		}
		b.WriteString(line[:start])
		token := line[start+2 : start+2+end]
		if value, ok := resolveTestContentVariable(token); ok {
			b.WriteString(value)
		} else {
			// 未知变量原样保留，包括花括号
			b.WriteString(line[start : start+2+end+2])
		}
		line = line[start+2+end+2:]
	}
	return b.String()
}

func resolveTestContentVariable(token string) (string, bool) {
	token = strings.TrimSpace(token)
	now := time.Now()
	switch strings.ToLower(token) {
	case "time":
		return now.Format("15:04:05"), true
	case "date":
		return now.Format("2006-01-02"), true
	case "datetime":
		return now.Format("2006-01-02 15:04:05"), true
	case "timestamp":
		return strconv.FormatInt(now.Unix(), 10), true
	case "rand":
		return strconv.Itoa(rand.IntN(10000)), true
	}
	if rangeSpec, ok := strings.CutPrefix(strings.ToLower(token), "rand:"); ok {
		if value, ok := randInRange(rangeSpec); ok {
			return value, true
		}
	}
	return "", false
}

// randInRange 解析 "min-max" 并返回范围内随机整数。负数下界（如 "-5-5"）
// 不支持——分隔歧义且测活场景无此需求，解析失败按未知变量原样保留。
func randInRange(spec string) (string, bool) {
	minStr, maxStr, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return "", false
	}
	minV, err1 := strconv.Atoi(strings.TrimSpace(minStr))
	maxV, err2 := strconv.Atoi(strings.TrimSpace(maxStr))
	if err1 != nil || err2 != nil || minV < 0 {
		return "", false
	}
	if minV > maxV {
		minV, maxV = maxV, minV
	}
	return strconv.Itoa(minV + rand.IntN(maxV-minV+1)), true
}
