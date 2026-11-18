import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";

const require = createRequire(import.meta.url);
const fromVite = createRequire(require.resolve("vite/package.json"));
const picomatch = fromVite("picomatch");

test("single-branch wildcard extglobs compile without an outer unbounded repeat", () => {
  for (const operator of ["+", "*"]) {
    for (const body of ["*", "*icon", "icon*", "i*n"]) {
      const pattern = operator + "(" + body + ").svg";
      assert.doesNotMatch(picomatch.makeRe(pattern).source, /\)[+*]/);
      assert.ok(picomatch(pattern)("icon.svg"));
      assert.equal(picomatch(pattern)("icons/icon.svg"), false);
    }
  }
  assert.ok(picomatch("pre*(icon*)end")("preend"));
  assert.equal(picomatch("pre+(icon*)end")("preend"), false);
});

test("complex repeated wildcards use the literal fallback, ordinary globs still match", () => {
  const complex = "+([ab]*).svg";
  assert.ok(picomatch(complex)(complex));
  assert.equal(picomatch(complex)("abc.svg"), false);
  assert.ok(picomatch("assets/**/*.{svg,png}")("assets/icons/logo.svg"));
  assert.ok(picomatch("+(a|b).svg")("abba.svg"));
});
