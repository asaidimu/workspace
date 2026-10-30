import { TurnTree } from "@asaidimu/utils-workspace";
import type { Turn, TurnNode } from "@asaidimu/utils-workspace";
import { writeFileSync, mkdirSync } from "node:fs";

const T0 = "2026-01-01T00:00:00.000Z";
const T1 = "2026-01-01T00:01:00.000Z";
const T2 = "2026-01-01T00:02:00.000Z";

type Ref = { id: string; version: number };

function turn(id: string, version: number, actor: Turn["actor"], parent?: Ref, text = ""): Turn {
  return {
    id,
    version,
    session: "sess-1",
    actor,
    blocks: text
      ? [{ id: `${id}-b${version}`, key: `k-${id}-${version}`, type: "text", mode: "readwrite", version: 0, created: T0, updated: T0, topics: [], value: { text } } as any]
      : [],
    created: T0,
    updated: T0,
    parent,
  };
}

function repo(turns: Turn[], head: Ref | null | undefined) {
  return {
    async loadAllTurns(_sessionId: string) {
      return turns;
    },
    async loadHead(_sessionId: string) {
      return head ?? null;
    },
  };
}

function nodeSummary(n: TurnNode) {
  return {
    id: n.id,
    version: n.version,
    parent: n.parent ?? null,
    versions: Object.keys(n.versions)
      .map(Number)
      .sort((a, b) => a - b),
    children: Object.fromEntries(
      Object.entries(n.children).map(([v, ids]) => [v, [...ids].sort()]),
    ),
    actor: n.actor,
  };
}

async function scenario(
  name: string,
  turns: Turn[],
  head: Ref | null,
  queries: { chainFrom?: string; siblings?: string; branchInfo?: string }[],
) {
  const tree = await TurnTree.build("sess-1", repo(turns, head) as any, head ?? undefined);
  const graph = tree.graph();
  const out: Record<string, unknown> = {
    name,
    input: { turns, head },
    expected: {
      head: tree.head(),
      chain: tree.chain().map((n) => `${n.id}#${n.version}`),
      nodes: Object.fromEntries(
        Object.keys(graph)
          .sort()
          .map((id) => [id, nodeSummary(graph[id])]),
      ),
      queries: [] as unknown[],
    },
  };
  const q = out.expected.queries as unknown[];
  for (const query of queries) {
    if (query.chainFrom !== undefined) {
      q.push({ op: "chainFrom", arg: query.chainFrom, result: tree.chainFrom(query.chainFrom).map((n) => `${n.id}#${n.version}`) });
    }
    if (query.siblings !== undefined) {
      q.push({ op: "siblings", arg: query.siblings, result: tree.getTurnSiblings(query.siblings).map((n) => `${n.id}#${n.version}`) });
    }
    if (query.branchInfo !== undefined) {
      q.push({ op: "branchInfo", arg: query.branchInfo, result: tree.branchInfo(query.branchInfo) });
    }
  }
  return out;
}

const cases = [];

// 1. Linear chain of three turns.
cases.push(
  await scenario(
    "linear-three",
    [
      turn("t1", 1, "user", undefined, "hello"),
      turn("t2", 1, "assistant", { id: "t1", version: 1 }, "hi"),
      turn("t3", 1, "user", { id: "t2", version: 1 }, "more"),
    ],
    { id: "t3", version: 1 },
    [{ chainFrom: "t1" }, { siblings: "t2" }, { branchInfo: "t2" }],
  ),
);

// 2. Edit creates version 2 of t2; head stays on v1 chain (conversation A in spec §6).
cases.push(
  await scenario(
    "edit-version-sibling",
    [
      turn("t1", 1, "user", undefined, "hello"),
      turn("t2", 1, "assistant", { id: "t1", version: 1 }, "hi"),
      turn("t2", 2, "assistant", { id: "t1", version: 1 }, "hi (edited)"),
      turn("t3", 1, "user", { id: "t2", version: 2 }, "more"),
    ],
    { id: "t3", version: 1 },
    [{ chainFrom: "t3" }, { siblings: "t2" }, { branchInfo: "t2" }],
  ),
);

// 3. Head on edited branch: active version of t2 becomes 2 (buildNodes rule).
cases.push(
  await scenario(
    "edit-head-switch",
    [
      turn("t1", 1, "user", undefined, "hello"),
      turn("t2", 1, "assistant", { id: "t1", version: 1 }, "hi"),
      turn("t2", 2, "assistant", { id: "t1", version: 1 }, "hi (edited)"),
      turn("t3", 1, "user", { id: "t2", version: 2 }, "more"),
      turn("t4", 1, "assistant", { id: "t3", version: 1 }, "done"),
    ],
    { id: "t4", version: 1 },
    [{ branchInfo: "t2" }],
  ),
);

// 4. Empty conversation: no turns, no head.
cases.push(await scenario("empty", [], null, [{ chainFrom: "missing" }, { siblings: "missing" }, { branchInfo: "missing" }]));

// 5. Two roots (orphans as siblings at top level).
cases.push(
  await scenario(
    "two-roots",
    [turn("a", 1, "user", undefined, "a"), turn("b", 1, "user", undefined, "b")],
    { id: "b", version: 1 },
    [{ siblings: "a" }],
  ),
);

// 6. Deep chain for limit/until planning (depth 12).
{
  const deep: Turn[] = [turn("d0", 1, "user", undefined, "0")];
  for (let i = 1; i < 12; i++) {
    deep.push(turn(`d${i}`, 1, "user", { id: `d${i - 1}`, version: 1 }, String(i)));
  }
  cases.push(await scenario("deep-chain", deep, { id: "d11", version: 1 }, [{ chainFrom: "d5" }]));
}

mkdirSync("../testdata/ts-fixtures", { recursive: true });
writeFileSync("../testdata/ts-fixtures/turntree.json", JSON.stringify({ fixture: "turntree", ts: "@asaidimu/utils-workspace@9.0.0", cases }, null, 2) + "\n");
console.log(`turntree: ${cases.length} cases`);
