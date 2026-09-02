// 账号代理绑定判定：把后端 resolveProxyForAccountSnapshot 的优先级
// (账号自绑 > 分组 > 代理池 > 全局 > 直连) 翻译成列表徽章能显示的一个状态。
//
// 两条刻意的边界：
//   1. 继承态只报来源、不报具体 URL。池内与组内都是按账号 ID 粘性散列选路的，
//      前端复刻散列必然与调度器漂移——要精确到生效 URL 得由后端在列表投影下发。
//   2. URL 只 trim、不做任何规范化。后端 buildProxyPoolSet 就是 trim 后精确匹配，
//      两边必须恒等；任何"聪明"的归一化都会造出假的失配。

export type AccountProxyBindingKind =
  // 账号自绑到代理池里的一条健康条目
  | "bound"
  // 账号自绑到托管代理，但该条已禁用/测试失败/被删除；代理池开启时无可用出口
  | "bound_unusable"
  // 账号自绑到未纳入代理池的 URL，不受池启停影响
  | "bound_custom"
  // 未自绑，继承分组代理
  | "group"
  // 未自绑，按账号 ID 粘性从代理池选路
  | "pool"
  // 未自绑，回落到全局代理
  | "global"
  // 未自绑且无任何代理，直连上游
  | "direct"
  // 代理池已开启却无可用条目、也没有全局代理：直连不被允许，账号会被跳过
  | "direct_blocked";

export interface ProxyBindingProxy {
  id: number;
  url: string;
  label?: string;
  enabled: boolean;
  test_status?: string;
  test_ip?: string;
  test_location?: string;
  test_latency_ms?: number;
}

export interface ProxyBindingGroup {
  id: number;
  name: string;
  proxy_urls?: string[] | null;
}

export interface ProxyBindingAccount {
  proxy_url?: string | null;
  group_ids?: number[] | null;
}

export interface ProxyBindingContext {
  poolEnabled: boolean;
  globalProxy: string;
  /** 启用池条目数：enabled 且测试未失败，与后端 ListEnabledProxies 同口径。 */
  poolSize: number;
  /** proxies 表全量条目(含禁用/测试失败)，用于识别"托管但不可用"。 */
  managed: Map<string, ProxyBindingProxy>;
  /** 启用池 URL 集合。 */
  usable: Set<string>;
  groups: Map<number, ProxyBindingGroup>;
}

export interface AccountProxyBinding {
  kind: AccountProxyBindingKind;
  /** 账号自绑时为该 URL；继承态为空串。 */
  url: string;
  /** 命中 proxies 表条目时的那一行，供徽章显示 label/出口/延迟。 */
  proxy: ProxyBindingProxy | null;
  /** kind === "group" 时的分组名。 */
  groupName: string;
  /** 该账号当前是否有可用出口；false 表示会被调度器跳过。 */
  usable: boolean;
}

function trimValue(value: string | null | undefined): string {
  return (value ?? "").trim();
}

/** 与后端 ListEnabledProxies 的 WHERE 同口径：启用且测试未失败。 */
export function proxyIsUsable(
  proxy: Pick<ProxyBindingProxy, "enabled" | "test_status">,
): boolean {
  return proxy.enabled && (proxy.test_status || "untested") !== "error";
}

export function buildProxyBindingContext(input: {
  proxies?: ProxyBindingProxy[] | null;
  groups?: ProxyBindingGroup[] | null;
  poolEnabled?: boolean | null;
  globalProxy?: string | null;
}): ProxyBindingContext {
  const managed = new Map<string, ProxyBindingProxy>();
  const usable = new Set<string>();
  for (const proxy of input.proxies ?? []) {
    const url = trimValue(proxy?.url);
    if (!url) continue;
    managed.set(url, proxy);
    if (proxyIsUsable(proxy)) usable.add(url);
  }

  const groups = new Map<number, ProxyBindingGroup>();
  for (const group of input.groups ?? []) {
    if (!group) continue;
    groups.set(group.id, group);
  }

  return {
    poolEnabled: Boolean(input.poolEnabled),
    globalProxy: trimValue(input.globalProxy),
    poolSize: usable.size,
    managed,
    usable,
    groups,
  };
}

/**
 * 托管但不可用：URL 在 proxies 表里、却不在启用池中。代理池关闭时该规则不生效
 * (此时禁用条目照样能用)，未纳入池的自定义 URL 也永远不受影响。
 */
export function isManagedProxyUnavailable(
  url: string,
  ctx: ProxyBindingContext,
): boolean {
  if (!ctx.poolEnabled) return false;
  if (!ctx.managed.has(url)) return false;
  return !ctx.usable.has(url);
}

export function resolveAccountProxyBinding(
  account: ProxyBindingAccount | null | undefined,
  ctx: ProxyBindingContext,
): AccountProxyBinding {
  const base = { url: "", proxy: null, groupName: "", usable: true } as const;

  const accountProxy = trimValue(account?.proxy_url);
  if (accountProxy) {
    const proxy = ctx.managed.get(accountProxy) ?? null;
    if (isManagedProxyUnavailable(accountProxy, ctx)) {
      return { ...base, kind: "bound_unusable", url: accountProxy, proxy, usable: false };
    }
    return {
      ...base,
      kind: proxy ? "bound" : "bound_custom",
      url: accountProxy,
      proxy,
    };
  }

  // 分组按 group_ids 顺序取第一个"有可用代理"的组；组内全不可用则继续下一个，
  // 与后端组循环的跳过语义一致。
  for (const groupID of account?.group_ids ?? []) {
    const group = ctx.groups.get(groupID);
    if (!group) continue;
    const hasUsable = (group.proxy_urls ?? []).some((raw) => {
      const url = trimValue(raw);
      return Boolean(url) && !isManagedProxyUnavailable(url, ctx);
    });
    if (!hasUsable) continue;
    return { ...base, kind: "group", groupName: group.name };
  }

  if (ctx.poolEnabled && ctx.poolSize > 0) {
    return { ...base, kind: "pool" };
  }
  if (ctx.globalProxy) {
    return { ...base, kind: "global" };
  }
  // 池开着却无处可走：后端此时不允许直连，账号被 AccountHasUsableEgress 过滤掉。
  if (ctx.poolEnabled) {
    return { ...base, kind: "direct_blocked", usable: false };
  }
  return { ...base, kind: "direct" };
}

/** 徽章里显示的短名：优先代理备注，其次 host:port，最后原样 URL。 */
export function formatProxyShortName(
  proxy: ProxyBindingProxy | null,
  url: string,
): string {
  const label = trimValue(proxy?.label);
  if (label) return label;
  const target = trimValue(url) || trimValue(proxy?.url);
  if (!target) return "";
  try {
    const parsed = new URL(target);
    return parsed.host || target;
  } catch {
    return target;
  }
}

/**
 * 悬浮提示里显示的 URL：保留 scheme/host/port 以便认出是哪条代理，但把账密打码。
 * 代理页对 URL 默认打码并配显隐开关，列表上的 hover 提示不该比它更宽松——用户
 * 没有"我要看凭据"的动作。真要看完整值，编辑弹窗的输入框里有。
 */
export function formatProxyDisplayURL(url: string): string {
  const target = trimValue(url);
  if (!target) return "";
  try {
    const parsed = new URL(target);
    if (!parsed.username && !parsed.password) return target;
    return `${parsed.protocol}//***:***@${parsed.host}`;
  } catch {
    return target;
  }
}

/** 代理池条目的出口信息摘要（IP / 地区 / 延迟），未测过则为空串。 */
export function formatProxyEgressSummary(
  proxy: ProxyBindingProxy | null,
): string {
  if (!proxy || proxy.test_status !== "success") return "";
  const latency = proxy.test_latency_ms ?? 0;
  return [
    trimValue(proxy.test_ip),
    trimValue(proxy.test_location),
    latency > 0 ? `${latency}ms` : "",
  ]
    .filter(Boolean)
    .join(" · ");
}
