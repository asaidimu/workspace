import { ContentRegistry, PromptAssembler } from "@asaidimu/utils-workspace";
import { writeFileSync, mkdirSync, readFileSync } from "node:fs";

// ToolRegistry is not exported; replicate its description()/schema() exactly.
// The leading policy text is lifted from the pinned bundle so output stays byte-faithful.
const bundle = readFileSync("node_modules/@asaidimu/utils-workspace/index.mjs", "utf8");
const policyMatch = bundle.match(/const policy = `([\s\S]*?)`;\n/);
if (!policyMatch) throw new Error("policy const not found in pinned bundle");
const policy = policyMatch[1];

const logs: string[] = [];
const logger = {
  info: () => {},
  debug: () => {},
  error: (msg: string) => logs.push(`error:${msg}`),
  warn: (msg: string) => logs.push(`warn:${msg}`),
  child: () => logger,
} as any;

const content = new ContentRegistry({ logger });

const toolDefs: any[] = [
  {
    name: "getWeather",
    description: "Get current weather for a city.",
    parameters: {
      type: "object",
      properties: { city: { type: "string" } },
      required: ["city"],
    },
    example: { input: { city: "Nairobi" }, explanation: "capital of Kenya" },
  },
];

const tools = {
  count: () => toolDefs.length,
  schema: () =>
    toolDefs.map((t) => {
      const result: any = {
        type: "object",
        properties: { type: { enum: [`tool:${t.name}`] } },
        required: ["type"],
        additionalProperties: false,
      };
      if (t.parameters) {
        result.properties.parameters = t.parameters;
        result.required.push("parameters");
      }
      return result;
    }),
  description: () => {
    if (toolDefs.length === 0) return "";
    return `${policy}\n
# TOOL INVOCATION

Use this exact format to invoke tools:

\`\`\`json
{
  "type": "tool:<tool_name>",
  "parameters": <parameters>
}
\`\`\`

# AVAILABLE TOOLS
Only use tools listed below.

${toolDefs
  .map((t) => {
    const example: any = { type: `tool:${t.name}` };
    if (t.example.input) example.parameters = t.example.input;
    return [`*tool:${t.name}*`, `Description: ${t.description}`, "Example:", "```json", JSON.stringify(example, null, 2), "```"].join("\n");
  })
  .join("\n\n")}
`.trim();
  },
};

// Provider mapping for "testprov" on text and json so transcript mapping is observable.
content.update("text", {
  mappings: { testprov: { to: ({ block }: any) => ({ kind: "text", text: block.value.text }), from: null } },
} as any);
content.update("json", {
  mappings: { testprov: { to: ({ block }: any) => ({ kind: "json", data: block.value.json }), from: null } },
} as any);

const assembler = new PromptAssembler(content, tools, async (ref: any, provider: string) => ({ id: ref.id, provider, resolved: true }), logger);

function textBlock(key: string, text: string) {
  return { id: `id-${key}`, key, type: "text", mode: "readwrite", version: 0, created: "2026-01-01T00:00:00.000Z", updated: "2026-01-01T00:00:00.000Z", topics: [], value: { text } };
}
function prefBlock(key: string, instruction: string) {
  return { id: `id-${key}`, key, type: "preference", mode: "read", version: 0, created: "2026-01-01T00:00:00.000Z", updated: "2026-01-01T00:00:00.000Z", topics: [], value: { instruction } };
}
function jsonBlock(key: string, data: any) {
  return { id: `id-${key}`, key, type: "json", mode: "readwrite", version: 0, created: "2026-01-01T00:00:00.000Z", updated: "2026-01-01T00:00:00.000Z", topics: [], value: { json: data } };
}
function thinkingBlock(key: string, thought: string) {
  return { id: `id-${key}`, key, type: "thinking", mode: "internal", version: 0, created: "2026-01-01T00:00:00.000Z", updated: "2026-01-01T00:00:00.000Z", topics: [], value: { thinking: thought } };
}
function turn(id: string, version: number, actor: string, blocks: any[], parent?: { id: string; version: number }) {
  return { id, version, session: "s1", actor, blocks, created: "2026-01-01T00:00:00.000Z", updated: "2026-01-01T00:00:00.000Z", parent } as any;
}

const mapTurn = ({ turn, parts }: any) => ({ id: turn.id, version: turn.version, actor: turn.actor, parts });

async function buildCase(name: string, prompt: any, extensions: any[] = []) {
  logs.length = 0;
  const result = await assembler.build("testprov", prompt, mapTurn, extensions);
  return {
    name,
    input: { provider: "testprov", prompt, extensions },
    expected: {
      sectionLabels: result.sections.map((s: any) => s.label),
      system: result.system,
      transcript: result.transcript,
      schema: result.schema,
      logs: [...logs],
    },
  };
}

const cases: any[] = [];

// 1. Minimal prompt: OS + block architecture only.
cases.push(
  await buildCase("minimal", {
    transcript: [turn("t1", 1, "user", [textBlock("k1", "hello")])],
    results: [],
  }),
);

// 2. Persona, preferences (one invalid), context + artifacts split, instructions, tool section.
cases.push(
  await buildCase("full-sections", {
    system: {
      persona: "You are a concise assistant.",
      preferences: [{ key: "p1", value: { instruction: "Always answer in metric units." } }, { key: "p2", value: {} }],
      content: [prefBlock("pref-1", "Prefer British English."), jsonBlock("cfg-1", { theme: "dark" })],
      instructions: "Cite sources.",
    },
    transcript: [turn("t1", 1, "user", [textBlock("k1", "hello")])],
    results: [],
  }),
);

// 3. Extensions: prepend, before:persona, after:tools, append, unknown anchor falls back to append.
cases.push(
  await buildCase(
    "extension-anchors",
    {
      system: { persona: "Persona." },
      transcript: [turn("t1", 1, "user", [textBlock("k1", "hi")])],
      results: [],
    },
    [
      { position: "prepend", label: "company", content: "Company policy." },
      { position: "before:persona", label: "legal", content: "Legal notice." },
      { position: "after:tools", label: "toolpolicy", content: "Tool policy." },
      { position: "append", label: "footer", content: "Footer." },
      { position: "before:does-not-exist", label: "orphan", content: "Orphan." },
    ],
  ),
);

// 4. Transcript mapping: text/json map through provider; thinking (internal) and unmapped blocks skipped; results appended to every turn (TS quirk).
cases.push(
  await buildCase("transcript-mapping", {
    transcript: [
      turn("t1", 1, "user", [textBlock("k1", "hello"), thinkingBlock("th1", "wondering")]),
      turn("t2", 1, "assistant", [jsonBlock("k2", { a: 1 })], { id: "t1", version: 1 }),
    ],
    results: [textBlock("r1", "tool says hi")],
  }),
);

mkdirSync("../testdata/ts-fixtures", { recursive: true });
writeFileSync("../testdata/ts-fixtures/prompt.json", JSON.stringify({ fixture: "prompt", ts: "@asaidimu/utils-workspace@9.0.0", cases }, null, 2) + "\n");
console.log(`prompt: ${cases.length} cases`);
