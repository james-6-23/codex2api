// 自动暂停阈值在后端/数据库里是 0..1 的比例,界面上统一按百分比(0..100)输入与展示。
// 各渠道的单号调度弹窗与分组管理弹窗都必须经由这里换算,避免把 95 原样提交给后端
// 触发「超出范围,必须在 0..1 之间」(issue #843)。

// formatAutoPauseThresholdPercent 把后端比例转换成百分比输入框的文本;未设置/0 返回空串。
export function formatAutoPauseThresholdPercent(value?: number | null): string {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) return "";
  const percent = value * 100;
  if (Number.isInteger(percent)) return String(percent);
  return String(Number(percent.toFixed(2)));
}

// isAutoPauseThresholdPercentInvalid 校验百分比输入:空串合法(表示继承/不设置),否则须在 0..100。
export function isAutoPauseThresholdPercentInvalid(value: string): boolean {
  const trimmed = value.trim();
  if (!trimmed) return false;
  const parsed = Number(trimmed);
  return !Number.isFinite(parsed) || parsed < 0 || parsed > 100;
}

// autoPauseThresholdPercentToRatio 把百分比输入转换成后端比例;空串或 <=0 返回 null(继承/不设置)。
export function autoPauseThresholdPercentToRatio(value: string): number | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const parsed = Number(trimmed);
  if (!Number.isFinite(parsed) || parsed <= 0) return null;
  return parsed / 100;
}
