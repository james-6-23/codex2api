import { cn } from "@/lib/utils";
import type { UpstreamChannel } from "@/types";

/**
 * 渠道品牌图标（Codex / Grok / Antigravity）。
 *
 * 使用 `@lobehub/icons-static-svg`，对齐 `@lobehub/icons` 的 Codex.Avatar 彩色方案，
 * 但不引入 antd / @lobehub/ui peer deps。
 *
 * - Codex：`codex-color.svg`（白底圆角 + 紫蓝渐变 mark，即 Codex.Avatar）
 * - Grok：仅有 mono `grok.svg`（fill=currentColor，跟随文字色适配深浅主题）
 * - Antigravity：`antigravity-color.svg`（官方彩色 mark）
 */
const ICON_URLS = import.meta.glob(
  [
    "../../node_modules/@lobehub/icons-static-svg/icons/codex-color.svg",
    "../../node_modules/@lobehub/icons-static-svg/icons/grok.svg",
    "../../node_modules/@lobehub/icons-static-svg/icons/antigravity-color.svg",
    "../../node_modules/@lobehub/icons-static-svg/icons/claudecode-color.svg",
    "../../node_modules/@lobehub/icons-static-svg/icons/claude-color.svg",
  ],
  { eager: true, query: "?url", import: "default" },
) as Record<string, string>;

const RAW_SVGS = import.meta.glob(
  ["../../node_modules/@lobehub/icons-static-svg/icons/grok.svg"],
  { eager: true, query: "?raw", import: "default" },
) as Record<string, string>;

const URL_BY_FILE = (() => {
  const map = new Map<string, string>();
  for (const [path, url] of Object.entries(ICON_URLS)) {
    const file = path.split("/").pop();
    if (file) map.set(file.replace(/\.svg$/, ""), url);
  }
  return map;
})();

const RAW_BY_FILE = (() => {
  const map = new Map<string, string>();
  for (const [path, raw] of Object.entries(RAW_SVGS)) {
    const file = path.split("/").pop();
    if (file) map.set(file.replace(/\.svg$/, ""), raw);
  }
  return map;
})();

function MonoSvg({
  name,
  className,
}: {
  name: string;
  className?: string;
}) {
  const raw = RAW_BY_FILE.get(name);
  if (!raw) return null;
  return (
    <span
      aria-hidden
      className={cn(
        "inline-flex shrink-0 items-center justify-center leading-none [&>svg]:block [&>svg]:h-[1em] [&>svg]:w-[1em]",
        className,
      )}
      // 静态构建期内联的本地 SVG 资源，非用户输入
      dangerouslySetInnerHTML={{ __html: raw }}
    />
  );
}

export default function ChannelLogo({
  channel,
  size = 14,
  className,
  title,
}: {
  channel: UpstreamChannel;
  size?: number;
  className?: string;
  title?: string;
}) {
  if (channel === "codex" || channel === "antigravity" || channel === "claude") {
    const fileByChannel: Record<string, { file: string; alt: string }> = {
      codex: { file: "codex-color", alt: "Codex" },
      antigravity: { file: "antigravity-color", alt: "Antigravity" },
      claude: { file: "claude-color", alt: "Claude" },
    };
    const meta = fileByChannel[channel];
    const src = URL_BY_FILE.get(meta.file);
    if (!src) return null;
    const rounded = channel === "codex";
    return (
      <img
        src={src}
        alt={title ?? meta.alt}
        title={title}
        width={size}
        height={size}
        draggable={false}
        className={cn(
          "inline-block shrink-0 select-none",
          rounded && "rounded-[20%]",
          className,
        )}
        style={{ width: size, height: size }}
      />
    );
  }

  return (
    <span
      title={title}
      className={cn("inline-flex shrink-0", className)}
      style={{ fontSize: size }}
    >
      <MonoSvg name="grok" />
    </span>
  );
}
