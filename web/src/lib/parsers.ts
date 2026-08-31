import type { RegisteredPlugin } from "@/types/registered-plugin";
import type { ParserOption } from "@/types/agent";

type PluginLike = Pick<RegisteredPlugin, "name" | "protocol" | "online">;

const GROUP_RULES: Array<{
  group: "godot" | "unity" | "http";
  test: (p: { name: string; protocol: string }) => boolean;
}> = [
  { group: "godot", test: (p) => /godot/i.test(p.name) || /godot/i.test(p.protocol) },
  { group: "unity", test: (p) => /unity/i.test(p.name) || /unity/i.test(p.protocol) },
  { group: "http", test: (p) => /^http$/i.test(p.protocol) || /http/i.test(p.name) },
];

export const GROUP_LABEL: Record<string, string> = {
  godot: "Godot",
  unity: "Unity",
  http: "HTTP",
  custom: "自定义",
};

/**
 * 按协议/标识把已注册插件归组成 { group -> ParserOption[] }；无法匹配的落入 custom。
 * order 保证渲染顺序为 Godot/Unity/HTTP/自定义，且仅保留非空组。
 */
export function groupParsers(plugins: PluginLike[]): {
  order: Array<ParserOption["group"]>;
  byGroup: Record<ParserOption["group"], ParserOption[]>;
} {
  const byGroup: Record<ParserOption["group"], ParserOption[]> = {
    godot: [],
    unity: [],
    http: [],
    custom: [],
  };
  for (const p of plugins) {
    let group: ParserOption["group"] = "custom";
    for (const r of GROUP_RULES) {
      if (r.test(p)) {
        group = r.group;
        break;
      }
    }
    byGroup[group].push({ group, label: p.name, plugin: p.name, online: p.online });
  }
  const order: ParserOption["group"][] = ["godot", "unity", "http"];
  if (byGroup.custom.length > 0) order.push("custom");
  return { order: order.filter((g) => byGroup[g].length > 0), byGroup };
}