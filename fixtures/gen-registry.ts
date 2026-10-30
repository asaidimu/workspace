import { ContentRegistry, ToolCallRegex } from "@asaidimu/utils-workspace";
import { writeFileSync, mkdirSync } from "node:fs";

const quiet = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {}, child: () => quiet };
const registry = new ContentRegistry({ logger: quiet } as any);

const cases: any[] = [];

// Modes and flags for every default type (spec §8 mapping: read=consumable, write=emittable).
const flags = (mode: string) => {
  switch (mode) {
    case "read": return { consumable: true, emittable: false };
    case "write": return { consumable: false, emittable: true };
    case "readwrite": return { consumable: true, emittable: true };
    default: return { consumable: false, emittable: false };
  }
};

cases.push({
  name: "default-types-modes",
  input: {},
  expected: Object.fromEntries(
    ["text", "preference", "json", "blob", "image", "document", "thinking", "tool:call", "tool:result", "tool:report", "unknown"].map((t) => {
      const def = registry.get(t)!;
      return [t, { mode: def.mode, flags: flags(def.mode) }];
    }),
  ),
});

// get(): tool:custom<Name> falls back to the tool:call definition via ToolCallRegex.
cases.push({
  name: "tool-call-regex-fallback",
  input: { keys: ["tool:getWeather", "tool:call", "tool:execute", "tool:result", "tool:report", "text"] },
  expected: {
    "tool:getWeather": "tool:call",
    "tool:call": "tool:call",
    "tool:execute": "tool:execute",
    "tool:result": "tool:result",
    "tool:report": "tool:report",
    text: "text",
  } as any,
});
{
  const c = cases[cases.length - 1];
  const exp: Record<string, string | null> = {};
  for (const k of c.input.keys as string[]) {
    exp[k] = registry.get(k)?.type ?? null;
  }
  // registry.get returns the definition object; record which registered key served it.
  const via: Record<string, string | null> = {};
  for (const k of c.input.keys as string[]) {
    const d = registry.get(k);
    via[k] = d ? d.type : null;
  }
  c.expected = {
    toolCallRegex: { "tool:getWeather": ToolCallRegex.test("tool:getWeather"), "tool:call": ToolCallRegex.test("tool:call"), "tool:execute": ToolCallRegex.test("tool:execute") },
    resolvedType: via,
  };
}

// parse(): known type without provider mapping keeps the raw block through createContent as unknown;
// unknown type is preserved as unknown with originalType.
{
  const knownRaw = { type: "text", text: "hello" };
  const unknownRaw = { type: "acme.chart", series: [1, 2, 3] };
  const parsedKnown = registry.parse(knownRaw as any, "openai");
  const parsedUnknown = registry.parse(unknownRaw as any, "openai");
  cases.push({
    name: "parse-unknown-and-unmapped",
    input: { knownRaw, unknownRaw, providerId: "openai" },
    expected: {
      known: { type: parsedKnown.type, mode: parsedKnown.mode, value: parsedKnown.value },
      unknown: { type: parsedUnknown.type, mode: parsedUnknown.mode, value: parsedUnknown.value },
    },
  });
}

// parse(): provider mapping short-circuits; a custom definition with a mapping returning a block.
{
  const custom = {
    type: "acme.card",
    mode: "readwrite" as const,
    schema: { type: "object", properties: { type: { enum: ["acme.card"] } } },
    description: "card",
    rules: [],
    mappings: {
      openai: {
        to: () => null,
        from: (raw: any) => ({ ...raw, type: "acme.card", mapped: true }),
      },
    },
  };
  registry.register("acme.card", custom as any);
  const raw = { type: "acme.card", title: "T" };
  const parsed = registry.parse(raw as any, "openai");
  cases.push({
    name: "parse-provider-mapping",
    input: { raw, providerId: "openai" },
    expected: { type: parsed.type, mapped: (parsed as any).mapped ?? null },
  });
}

// schema()/description(): emittable-only, excluding tool:call per PromptAssembler filter.
{
  const schema = registry.schema({ exclude: ["tool:call"] } as any, []);
  const emittable = (schema.properties.blocks.items as any).anyOf.map((s: any) => s.properties.type.enum[0]);
  cases.push({
    name: "schema-emittable-exclude-tool-call",
    input: { filter: { exclude: ["tool:call"] } },
    expected: { required: schema.required, emittableTypes: emittable.sort() },
  });
}

// content flags filter on instances.
{
  const instances = [
    registry.createContent("text", { text: "a" }),
    registry.createContent("preference", { instruction: "be nice" }),
    registry.createContent("thinking", { thinking: "hmm" }),
  ];
  const consumable = registry.filterContentInstances(instances as any, { consumable: true }).map((c: any) => c.type);
  const emittable = registry.filterContentInstances(instances as any, { emittable: true }).map((c: any) => c.type);
  cases.push({
    name: "instance-filter-flags",
    input: { types: instances.map((c: any) => c.type) },
    expected: { consumable: consumable.sort(), emittable: emittable.sort() },
  });
}

// process(): tool:call definitions emit tool:execute commands.
{
  const call = registry.createContent("tool:call", { name: "getWeather", parameters: { city: "Nairobi" } }, { key: "call_1" });
  const commands = registry.process([call] as any);
  cases.push({
    name: "process-tool-call-commands",
    input: { blockType: "tool:call", key: "call_1" },
    expected: { count: commands.length, first: commands[0] ? { type: commands[0].type, payload: commands[0].payload } : null },
  });
}

// format(): text, preference, tool:call, tool:result render; image/unknown return null.
{
  const text = registry.createContent("text", { text: "hello world" }, { key: "k1" });
  const pref = registry.createContent("preference", { instruction: "Always be kind" }, { key: "k2" });
  const call = registry.createContent("tool:call", { name: "getWeather", parameters: { city: "Nairobi" } }, { key: "call_1" });
  const res = registry.createContent("tool:result", { name: "getWeather", result: { temp: 25 }, key: "call_1" }, { key: "call_1" });
  const err = registry.createContent("tool:result", { name: "getWeather", result: null, error: "not found", key: "call_2" }, { key: "call_2" });
  const image = registry.createContent("image", { ref: { id: "i1", checksum: "sha256:x", mime: "image/png", size: 1 }, altText: "cat" });
  const unk = registry.createContent("unknown", { raw: { weird: 1 }, originalType: "old:thing" });
  cases.push({
    name: "format-renderers",
    input: {},
    expected: {
      text: registry.format(text as any),
      preference: registry.format(pref as any),
      toolCall: registry.format(call as any),
      toolResult: registry.format(res as any),
      toolError: registry.format(err as any),
      image: registry.format(image as any),
      unknown: registry.format(unk as any),
    },
  });
}

mkdirSync("../testdata/ts-fixtures", { recursive: true });
writeFileSync("../testdata/ts-fixtures/registry.json", JSON.stringify({ fixture: "registry", ts: "@asaidimu/utils-workspace@9.0.0", cases }, null, 2) + "\n");
console.log(`registry: ${cases.length} cases`);
