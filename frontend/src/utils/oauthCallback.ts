export function parseOAuthCallbackParams(rawUrl: string): { code: string; state: string } {
  const raw = rawUrl.trim();
  try {
    const url = new URL(raw);
    return {
      code: url.searchParams.get("code") ?? "",
      state: url.searchParams.get("state") ?? "",
    };
  } catch {
    const qs = raw.includes("?") ? raw.split("?")[1] : raw;
    const params = new URLSearchParams(qs);
    return {
      code: params.get("code") ?? "",
      state: params.get("state") ?? "",
    };
  }
}
