import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

// Compile the small, browser-independent content graph with the project's
// compiler. This also works on Node 22.12 without a custom TS loader.
const modules = new Map();
function loadContent(name) {
  if (modules.has(name)) return modules.get(name);
  const source = readFileSync(
    new URL(`../pages/docs/${name}.ts`, import.meta.url),
    "utf8",
  );
  const { outputText } = ts.transpileModule(source, {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2020,
    },
  });
  const exports = {};
  modules.set(name, exports);
  new Function("require", "exports", outputText)(
    (relative) => loadContent(relative.replace("./", "")),
    exports,
  );
  return exports;
}
const { buildEndpointSpecs, buildDocsMarkdown } = loadContent("docsContent");
const { buildGuides } = loadContent("docsGuides");
const base = "https://gateway.example.com";

test("public documentation covers the new media and compatibility routes in both languages", () => {
  for (const locale of ["zh", "en"]) {
    const specs = buildEndpointSpecs(base, locale);
    assert.equal(new Set(specs.map((e) => e.id)).size, specs.length);
    for (const path of [
      "/v1/images/jobs",
      "/v1/images/jobs/:id",
      "/v1/videos/generations",
      "/v1/videos/edits",
      "/v1/videos/extensions",
      "/v1/videos/:request_id",
      "/v1/videos/:request_id/content",
      "/v1/messages/count_tokens",
      "/v1/responses/input_tokens",
      "/v1/responses/compact",
    ]) {
      assert.ok(
        specs.some((e) => e.path === path),
        `${locale}: ${path}`,
      );
    }
    for (const spec of specs) {
      if (spec.defaultBody)
        assert.doesNotThrow(() => JSON.parse(spec.defaultBody), spec.id);
      assert.doesNotMatch(spec.curl, /\n\+\s+--/);
      assert.ok(spec.category, spec.id);
    }
  }
});

test("same-status image and video responses are distinguishable; quota status matches the gateway", () => {
  const specs = buildEndpointSpecs(base, "en");
  for (const id of ["api-images-gen", "api-video-status"]) {
    const responses = specs.find((e) => e.id === id).responses;
    assert.equal(
      new Set(responses.map((r) => `${r.code}:${r.label}`)).size,
      responses.length,
    );
    assert.notEqual(responses[0].body, responses[1].body);
  }
  const responses = specs.find((e) => e.id === "api-responses");
  assert.match(responses.curl, /"stream": false/);
  assert.equal(
    responses.responses.find((r) =>
      r.body.includes("account_pool_usage_limit_reached"),
    ).code,
    503,
  );
});

test("Markdown includes the same guides, parameters and alternate examples as the page", () => {
  for (const locale of ["zh", "en"]) {
    const markdown = buildDocsMarkdown({
      baseUrl: base,
      locale,
      quickTools: [],
      apiKeyExample: "YOUR_API_KEY",
      clientConfigs: [
        {
          label: "custom-config",
          lang: "toml",
          content: 'model = "chosen-model"',
        },
      ],
    });
    for (const guide of buildGuides(base, locale))
      assert.ok(markdown.includes(`### ${guide.title}`));
    assert.match(markdown, /images\[\]\.image_url/);
    assert.match(markdown, /\*\*200 · URL\*\*/);
    assert.match(markdown, /\*\*multipart\*\*/);
    assert.equal(markdown.split('model = "chosen-model"').length, 2);
    assert.match(markdown, /previous_response_not_found/);
    assert.match(markdown, /model_request_limits/);
  }
});
