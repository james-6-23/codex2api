import { createHash } from "node:crypto";
import { existsSync, readFileSync, realpathSync, renameSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";

// GHSA-fc5q-39xc-vjpf has no upstream release containing a fix. Review and
// remove this backport when updating picomatch. Pin the input to npm's 4.0.7.
const originalHash = "4e1e05447668aa1e94366c5516a2bcd8f5fff800cc92b1d0cd7d77703b8618c4";
const anchor = "    const analysis = analyzeRepeatedExtglob(body, opts);";
const replacement = anchor + `

    // Repeating a single filename pattern containing a star can nest two
    // unbounded quantifiers. A plain segment's star can absorb any extra
    // repetitions. For more complex bodies use picomatch's literal fallback.
    if (!analysis.risky && (token.type === 'plus' || token.type === 'star') &&
        body.includes('*') && splitTopLevel(body).length === 1) {
      const hasWildcard = tokens.slice(token.tokensIndex + 2).some(part =>
        part.value && (part.type === 'star' || part.type === 'globstar'));
      if (hasWildcard) {
        if (!opts.bash && /^[a-zA-Z0-9_.*-]+$/.test(body) && !body.includes('**')) {
          token.close = token.type === 'plus' ? ')' : ')?';
        } else {
          analysis.risky = true;
        }
      }
    }`;

function patchParser(path) {
  const source = readFileSync(path, "utf8");
  const original = source.replace(replacement, anchor);
  if (createHash("sha256").update(original).digest("hex") !== originalHash) {
    throw new Error(`Unexpected picomatch parser at ${path}. Review GHSA-fc5q-39xc-vjpf before continuing.`);
  }
  if (original === source) {
    // Replace the file atomically. pnpm may hard-link it to a shared store.
    const temporaryPath = `${path}.${process.pid}.tmp`;
    writeFileSync(temporaryPath, source.replace(anchor, replacement), { flag: "wx" });
    renameSync(temporaryPath, path);
  }
}

// npm ci --omit=dev leaves out Vite and its glob tooling.
const vite = new URL("../node_modules/vite/package.json", import.meta.url);
if (existsSync(vite)) {
  const fromVite = createRequire(realpathSync(vite));
  const fromTinyglobby = createRequire(fromVite.resolve("tinyglobby"));
  const fromFdir = createRequire(fromTinyglobby.resolve("fdir"));
  const parsers = [fromVite, fromTinyglobby, fromFdir].map(
    (consumer) => consumer.resolve("picomatch/lib/parse.js"),
  );
  for (const path of new Set(parsers)) patchParser(path);
}
