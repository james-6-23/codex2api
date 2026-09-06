// 测连弹窗与账号页共用的模型候选/展示辅助:Codex、Claude、Grok/Responses、
// Antigravity 四类账号的测试模型来源各不相同,这里只放与渠道无关的纯函数。
import type { api } from "../api";
import type { AccountRow } from "../types";
import { parseModelMappingEntries } from "./modelMapping";

export function formatTestErrorMessage(message: string) {
  const normalized = message.trim();
  const jsonStart = normalized.indexOf("{");

  if (jsonStart === -1) {
    return normalized;
  }

  const prefix = normalized
    .slice(0, jsonStart)
    .trim()
    .replace(/[：:]\s*$/, "");
  const jsonText = normalized.slice(jsonStart);

  try {
    const parsed = JSON.parse(jsonText);
    const prettyJson = JSON.stringify(parsed, null, 2);
    return prefix ? `${prefix}\n${prettyJson}` : prettyJson;
  } catch {
    return normalized;
  }
}

export const DEFAULT_TEST_MODEL = "gpt-5.4";

export function isConnectionTestModel(model: string) {
  const value = model.trim().toLowerCase();
  return value !== "" && !value.includes("image");
}

export function extractTextModels(
  modelsResp: Awaited<ReturnType<typeof api.getModels>>,
) {
  if (modelsResp.items && modelsResp.items.length > 0) {
    return modelsResp.items
      .filter(
        (item) =>
          item.enabled &&
          item.category !== "image" &&
          !item.id.includes("image"),
      )
      .map((item) => item.id);
  }
  return (modelsResp.models ?? []).filter(isConnectionTestModel);
}

export function uniqueTestModels(
  models: string[],
  preferredModel?: string,
  includeDefault = true,
) {
  const seen = new Set<string>();
  const result: string[] = [];
  const candidates = [
    preferredModel ?? "",
    ...models,
    ...(includeDefault ? [DEFAULT_TEST_MODEL] : []),
  ];

  for (const model of candidates) {
    const value = model.trim();
    if (!isConnectionTestModel(value) || seen.has(value)) continue;
    seen.add(value);
    result.push(value);
  }
  return result;
}

export function exactModelMappingAliases(
  value?: string,
  supportedModels: string[] = [],
): string[] {
  const parsed = parseModelMappingEntries(value ?? "");
  if (!parsed.ok) return [];
  const supported = new Set(
    supportedModels.map((model) => model.trim().toLowerCase()).filter(Boolean),
  );
  return parsed.entries
    .filter((entry) => {
      const alias = entry.from.trim();
      const target = entry.to.trim().toLowerCase();
      return (
        alias &&
        !alias.includes("*") &&
        isConnectionTestModel(alias) &&
        (supported.size === 0 || supported.has(target))
      );
    })
    .map((entry) => entry.from.trim());
}

export function formatAccountName(account: AccountRow): string {
  if (account.openai_responses_api || account.grok_api) {
    return account.name?.trim() || `ID ${account.id}`;
  }
  return account.email || account.name || `ID ${account.id}`;
}
