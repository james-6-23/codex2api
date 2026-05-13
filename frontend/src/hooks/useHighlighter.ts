import { useEffect, useState } from "react";
import type { HighlighterCore } from "shiki/core";

let highlighterPromise: Promise<HighlighterCore> | null = null;
const htmlCache = new Map<string, string>();
const MAX_CACHE_SIZE = 80;

function getHighlighter() {
  if (!highlighterPromise) {
    highlighterPromise = Promise.all([
      import("shiki/core"),
      import("shiki/engine/javascript"),
      import("shiki/themes/dark-plus.mjs"),
      import("shiki/themes/github-light-default.mjs"),
      import("shiki/langs/json.mjs"),
      import("shiki/langs/shellscript.mjs"),
      import("shiki/langs/toml.mjs"),
    ])
      .then(
        ([core, engine, darkPlus, githubLight, json, shellscript, toml]) => {
          return core.createHighlighterCore({
            themes: [githubLight.default, darkPlus.default],
            langs: [json.default, toml.default, shellscript.default],
            engine: engine.createJavaScriptRegexEngine(),
          });
        },
      )
      .catch((error) => {
        highlighterPromise = null;
        throw error;
      });
  }
  return highlighterPromise;
}

function tuneLightTheme(html: string) {
  return html
    .replace(/#0550AE/gi, "#075985")
    .replace(/#0969DA/gi, "#075985")
    .replace(/#1F2328/gi, "#1f2937")
    .replace(/#953800/gi, "#9a3412")
    .replace(/#0A3069/gi, "#7c2d12")
    .replace(/#CF222E/gi, "#b91c1c");
}

export function useHighlightedHtml(code: string, lang?: string) {
  const [html, setHtml] = useState("");
  const [isDark, setIsDark] = useState(() =>
    document.documentElement.classList.contains("dark"),
  );

  useEffect(() => {
    const root = document.documentElement;
    const observer = new MutationObserver(() => {
      setIsDark(root.classList.contains("dark"));
    });
    observer.observe(root, { attributes: true, attributeFilter: ["class"] });
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    let cancelled = false;
    const resolvedLang =
      lang === "bash" || lang === "shell" || lang === "curl"
        ? "shellscript"
        : lang || "text";

    if (!code) {
      setHtml("");
      return () => {
        cancelled = true;
      };
    }
    const cacheKey = `${isDark ? "dark" : "light"}:${resolvedLang}:${code}`;
    const cached = htmlCache.get(cacheKey);
    if (cached) {
      setHtml(cached);
      return () => {
        cancelled = true;
      };
    }

    getHighlighter()
      .then((hl) => {
        if (cancelled) return;
        try {
          const result = hl.codeToHtml(code, {
            lang: resolvedLang,
            theme: isDark ? "dark-plus" : "github-light-default",
          });
          const cacheKey = `${isDark ? "dark" : "light"}:${resolvedLang}:${code}`;
          const nextHtml = isDark ? result : tuneLightTheme(result);
          if (htmlCache.size >= MAX_CACHE_SIZE) {
            const firstKey = htmlCache.keys().next().value;
            if (firstKey) htmlCache.delete(firstKey);
          }
          htmlCache.set(cacheKey, nextHtml);
          setHtml(nextHtml);
        } catch (error) {
          console.warn("highlight failed", {
            resolvedLang,
            isDark,
            codeLength: code.length,
            error,
          });
          setHtml("");
        }
      })
      .catch((error) => {
        if (cancelled) return;
        console.warn("highlight failed", {
          resolvedLang,
          isDark,
          codeLength: code.length,
          error,
        });
        setHtml("");
      });

    return () => {
      cancelled = true;
    };
  }, [code, isDark, lang]);

  return html;
}
