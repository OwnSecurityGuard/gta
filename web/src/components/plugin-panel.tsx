import { useEffect, useRef, useState } from "react";
import {
  useRegisteredPlugins,
  useDeregisterPlugin,
  useTestPlugin,
  useDecodeRawPackets,
  useSessions,
  useListPlugins,
} from "@/hooks/use-mcp";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { Dialog } from "@/components/ui/dialog";
import { toast } from "@/components/ui/toast";
import { Power, Wifi, WifiOff, Activity, FlaskConical, Lock, Puzzle, AlertTriangle, RotateCw, X } from "lucide-react";
import type { RegisteredPlugin } from "@/types/registered-plugin";
import type { SessionInfo } from "@/types/session";
import type { TestPluginResult, TestEventLite } from "@/types/plugin-test";
import { RAW_DEBUG_ENABLED } from "@/lib/env";

// 稳定的空数组引用：避免在 data 未加载时每次渲染生成新的 [] 引用，
// 否则以它为依赖的 useEffect 会无限重渲染（Maximum update depth exceeded）。
const EMPTY_PLUGINS: RegisteredPlugin[] = [];
const EMPTY_SESSIONS: SessionInfo[] = [];
const EMPTY_STRINGS: string[] = [];

/** 格式化为 HH:mm:ss（last_heartbeat 为 unix 秒） */
function fmtClock(unix: number): string {
  if (!unix) return "—";
  const d = new Date(unix * 1000);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

/** 相对时间（"12s 前"） */
function fmtRelative(unix: number): string {
  if (!unix) return "";
  const diff = Math.floor(Date.now() / 1000 - unix);
  if (diff < 0) return "刚刚";
  if (diff < 60) return `${diff}s 前`;
  if (diff < 3600) return `${Math.floor(diff / 60)}m 前`;
  return `${Math.floor(diff / 3600)}h 前`;
}

function truncate(s: string, n = 10): string {
  return s.length > n ? s.slice(0, n) + "…" : s;
}

function fmtTime(unix: number): string {
  if (!unix) return "—";
  const d = new Date(unix * 1000);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export function PluginPanel() {
  const { data, isLoading, isError, error, refetch } = useRegisteredPlugins();
  const deregister = useDeregisterPlugin();
  const { data: sessionsData } = useSessions();
  const { data: dirPlugins } = useListPlugins();
  const test = useTestPlugin();
  const decode = useDecodeRawPackets();

  // 热更检测：同名插件 instance_id 变化时记录热更时间（基线加载不标记）。
  const [hotReloads, setHotReloads] = useState<Record<string, number>>({});
  const nameToInstance = useRef<Record<string, string>>({});
  const baselineSet = useRef(false);

  const plugins = data?.plugins ?? EMPTY_PLUGINS;

  useEffect(() => {
    setHotReloads((prev) => {
      const next = { ...prev };
      let changed = false;
      for (const p of plugins) {
        if (baselineSet.current) {
          const last = nameToInstance.current[p.name];
          if (last && last !== p.instance_id && p.online) {
            next[p.instance_id] = Date.now();
            changed = true;
          }
        }
        nameToInstance.current[p.name] = p.instance_id;
      }
      baselineSet.current = true;
      // 无变更时返回相同引用，避免无限重渲染。
      return changed ? next : prev;
    });
  }, [plugins]);

  const onlineCount = plugins.filter((p) => p.online).length;

  // 强制下线确认：破坏性操作前二次确认
  const [confirmTarget, setConfirmTarget] = useState<RegisteredPlugin | null>(null);

  // 离线解码工具栏状态（仅 dev 构建）
  const [decodeSession, setDecodeSession] = useState("");
  const [decodePlugin, setDecodePlugin] = useState("");
  const sessions = sessionsData?.sessions ?? EMPTY_SESSIONS;
  const dirPluginList = dirPlugins?.plugins ?? EMPTY_STRINGS;

  useEffect(() => {
    if (!decodeSession && sessions.length > 0) setDecodeSession(sessions[0]!.session_id);
  }, [sessions, decodeSession]);
  useEffect(() => {
    if (!decodePlugin && dirPluginList.length > 0) setDecodePlugin(dirPluginList[0]!);
  }, [dirPluginList, decodePlugin]);

  // ===== 测试插件 状态（常驻，不依赖 raw-debug）=====
  const [testPlugin, setTestPlugin] = useState("");
  const [testSession, setTestSession] = useState("");
  const [testProtocol, setTestProtocol] = useState("");
  const [testSrc, setTestSrc] = useState("");
  const [testDst, setTestDst] = useState("");
  const [testLimit, setTestLimit] = useState(0); // 0 = 全部
  const [showTest, setShowTest] = useState(false);
  const [expandedEvent, setExpandedEvent] = useState<string | null>(null);
  const testSectionRef = useRef<HTMLDivElement>(null);

  // 测试插件支持所有会话（含运行中）：后端 test_plugin 仅只读 SELECT raw_packets 且不回写，
  // 会话库开启 WAL，与运行中 writer 并发读安全。运行中会话在下拉中标注「（运行中）」。
  const testableSessions = sessions;

  useEffect(() => {
    if (!testPlugin) {
      const firstOnline = plugins.find((p) => p.online);
      if (firstOnline) setTestPlugin(firstOnline.name);
    }
  }, [plugins, testPlugin]);

  useEffect(() => {
    if (!testSession && testableSessions.length > 0) {
      setTestSession(testableSessions[0]!.session_id);
    }
  }, [testableSessions, testSession]);

  const handleTestClick = (pluginName: string) => {
    setTestPlugin(pluginName);
    setShowTest(true);
    // 等待渲染后滚动到测试区
    requestAnimationFrame(() => {
      testSectionRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
    });
  };

  const runTest = () => {
    if (!testSession || !testPlugin) return;
    test.mutate(
      {
        sessionId: testSession,
        plugin: testPlugin,
        protocol: testProtocol || undefined,
        src: testSrc || undefined,
        dst: testDst || undefined,
        limit: testLimit || undefined,
      },
      {
        onError: (err) => toast.error("测试失败", err.message),
      },
    );
  };

  // 后端 proto3 在 repeated 字段为空时会序列化为 null，这里统一归一化，避免渲染时 .length / .map 崩溃。
  const rawResult = test.data as Partial<TestPluginResult> | undefined;
  const result = rawResult
    ? {
        ...rawResult,
        total_raw: rawResult.total_raw ?? 0,
        decoded: rawResult.decoded ?? 0,
        decode_errors: rawResult.decode_errors ?? 0,
        type_histogram: rawResult.type_histogram ?? {},
        sample_events: rawResult.sample_events ?? [],
        error_samples: rawResult.error_samples ?? [],
      }
    : undefined;
  const histogram = result?.type_histogram ?? {};
  const histEntries = Object.entries(histogram);
  const maxHist = histEntries.reduce((m, [, v]) => Math.max(m, v), 0);

  if (isLoading) {
    return (
      <div className="space-y-3 p-4">
        {Array.from({ length: 3 }).map((_, i) => (
          <Skeleton key={i} className="h-20 w-full rounded-lg" />
        ))}
      </div>
    );
  }

  if (isError) {
    return (
      <div
        role="alert"
        className="m-4 flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive"
      >
        <AlertTriangle className="h-4 w-4 shrink-0" />
        <span className="flex-1">加载插件失败：{error?.message ?? "未知错误"}</span>
        <Button variant="outline" size="sm" onClick={() => refetch()} className="h-7">
          <RotateCw className="h-3.5 w-3.5" />
          重试
        </Button>
      </div>
    );
  }

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center justify-between px-4 py-3 border-b">
        <h2 className="text-sm font-semibold">插件管理</h2>
        {data && (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
            {onlineCount > 0 && <span className="gta-live-dot" />}
            {plugins.length} 个{onlineCount > 0 && ` · ${onlineCount} 在线`}
          </span>
        )}
      </div>

      <div className="flex-1 overflow-auto p-3 space-y-2 gta-scroll">
        {plugins.length === 0 && (
          <EmptyState
            icon={<Puzzle className="h-5 w-5" />}
            title="暂无已注册插件"
            hint="启动插件进程后，它会自动注册到 Pipeline 并出现在此处。"
          />
        )}
        {plugins.map((p) => (
          <PluginCard
            key={p.instance_id}
            plugin={p}
            hotReloadAt={hotReloads[p.instance_id]}
            onDeregister={() => setConfirmTarget(p)}
            deregistering={deregister.isPending && confirmTarget?.instance_id === p.instance_id}
            onTest={() => handleTestClick(p.name)}
          />
        ))}
      </div>

      {/* 测试插件：隐私安全通道。原始包仅服务端解码，不回传、不落库。 */}
      <div ref={testSectionRef} className="border-t">
        <button
          type="button"
          className="w-full flex items-center justify-between px-4 py-3 text-left"
          onClick={() => setShowTest((v) => !v)}
        >
          <span className="flex items-center gap-2 text-xs font-medium text-muted-foreground">
            <FlaskConical className="h-3.5 w-3.5" />
            测试插件
            <Badge variant="outline" className="text-emerald-600 border-emerald-300">
              <Lock className="h-3 w-3 mr-1" /> 不暴露原始包
            </Badge>
          </span>
          <span className="text-xs text-muted-foreground">{showTest ? "收起" : "展开"}</span>
        </button>

        {showTest && (
          <div className="px-4 pb-4 space-y-3">
            {/* 配置区 */}
            <div className="grid grid-cols-2 gap-2">
              <label className="text-xs text-muted-foreground flex flex-col gap-1">
                目标插件
                <select
                  className="h-8 rounded-md border bg-background px-2 text-xs"
                  value={testPlugin}
                  onChange={(e) => setTestPlugin(e.target.value)}
                >
                  {plugins.length === 0 ? (
                    <option value="">无插件</option>
                  ) : (
                    plugins.map((p) => (
                      <option key={p.instance_id} value={p.name}>
                        {p.name}
                        {p.online ? "" : "（离线）"}
                      </option>
                    ))
                  )}
                </select>
              </label>
              <label className="text-xs text-muted-foreground flex flex-col gap-1">
                来源会话（含运行中）
                <select
                  className="h-8 rounded-md border bg-background px-2 text-xs"
                  value={testSession}
                  onChange={(e) => setTestSession(e.target.value)}
                >
                  {testableSessions.length === 0 ? (
                    <option value="">无可用会话</option>
                  ) : (
                    testableSessions.map((s) => (
                      <option key={s.session_id} value={s.session_id}>
                        {s.session_id}
                        {s.status === "running" ? "（运行中）" : ""}
                      </option>
                    ))
                  )}
                </select>
              </label>
              <label className="text-xs text-muted-foreground flex flex-col gap-1">
                协议过滤（可选）
                <input
                  className="h-8 rounded-md border bg-background px-2 text-xs"
                  placeholder="如 tcp"
                  value={testProtocol}
                  onChange={(e) => setTestProtocol(e.target.value)}
                />
              </label>
              <label className="text-xs text-muted-foreground flex flex-col gap-1">
                测试包上限
                <select
                  className="h-8 rounded-md border bg-background px-2 text-xs"
                  value={String(testLimit)}
                  onChange={(e) => setTestLimit(Number(e.target.value))}
                >
                  <option value="50">50</option>
                  <option value="100">100</option>
                  <option value="500">500</option>
                  <option value="0">全部</option>
                </select>
              </label>
              <label className="text-xs text-muted-foreground flex flex-col gap-1">
                源 IP 过滤（可选）
                <input
                  className="h-8 rounded-md border bg-background px-2 text-xs"
                  placeholder="src"
                  value={testSrc}
                  onChange={(e) => setTestSrc(e.target.value)}
                />
              </label>
              <label className="text-xs text-muted-foreground flex flex-col gap-1">
                目的 IP 过滤（可选）
                <input
                  className="h-8 rounded-md border bg-background px-2 text-xs"
                  placeholder="dst"
                  value={testDst}
                  onChange={(e) => setTestDst(e.target.value)}
                />
              </label>
            </div>

            <Button
              size="sm"
              onClick={runTest}
              disabled={!testSession || !testPlugin || test.isPending || testableSessions.length === 0}
            >
              {test.isPending ? "测试中..." : "运行测试"}
            </Button>
            {testableSessions.length === 0 && (
              <p className="text-xs text-muted-foreground">没有可用会话可供测试。</p>
            )}

            {/* 展示区：插件解出来的相关数据 */}
            {result && (
              <div className="space-y-3 pt-1">
                <div className="flex items-center gap-2 text-xs">
                  <Badge variant="outline" className="text-emerald-600 border-emerald-300">
                    成功 {result.decoded} / 失败 {result.decode_errors}（共 {result.total_raw} 包）
                  </Badge>
                  <Badge variant="outline" className="text-emerald-600 border-emerald-300">
                    <Lock className="h-3 w-3 mr-1" /> 原始包未传前端
                  </Badge>
                </div>

                {/* 事件类型分布 */}
                {histEntries.length > 0 && (
                  <div className="space-y-1">
                    <p className="text-xs font-medium text-muted-foreground">事件类型分布</p>
                    {histEntries.map(([type, count]) => (
                      <div key={type} className="flex items-center gap-2 text-xs">
                        <span className="w-40 truncate font-mono" title={type}>
                          {type}
                        </span>
                        <div className="flex-1 h-2 overflow-hidden rounded-full bg-muted">
                          <div
                            className="h-full rounded-full"
                            style={{
                              width: `${maxHist ? (count / maxHist) * 100 : 0}%`,
                              backgroundImage:
                                "linear-gradient(90deg, var(--color-primary), var(--color-info))",
                            }}
                          />
                        </div>
                        <span className="w-10 text-right tabular-nums">{count}</span>
                      </div>
                    ))}
                  </div>
                )}

                {/* 采样事件预览 */}
                {result.sample_events.length > 0 && (
                  <div className="space-y-1">
                    <p className="text-xs font-medium text-muted-foreground">
                      采样事件（最多 {result.sample_events.length} 条）
                    </p>
                    <div className="border rounded-md divide-y">
                      {result.sample_events.map((ev) => (
                        <SampleEventRow
                          key={ev.id}
                          ev={ev}
                          expanded={expandedEvent === ev.id}
                          onToggle={() =>
                            setExpandedEvent((cur) => (cur === ev.id ? null : ev.id))
                          }
                        />
                      ))}
                    </div>
                  </div>
                )}

                {/* 错误样例 */}
                {result.error_samples.length > 0 && (
                  <details className="text-xs">
                    <summary className="cursor-pointer text-muted-foreground">
                      解码错误样例（{result.error_samples.length}）
                    </summary>
                    <div className="mt-1 border rounded-md divide-y">
                      {result.error_samples.map((e, i) => (
                        <div key={i} className="px-2 py-1 font-mono text-destructive/80">
                          {e.raw_packet_id} {e.src} → {e.dst}：{e.error}
                        </div>
                      ))}
                    </div>
                  </details>
                )}

                <p className="text-xs text-muted-foreground/80">
                  本测试不修改会话真实解码数据（只读采样，不落库）。
                </p>
              </div>
            )}
          </div>
        )}
      </div>

      {/* 离线解码工具栏：仅 dev 构建（与后端 --enable-raw-debug 对齐）。 */}
      {RAW_DEBUG_ENABLED && (
        <div className="border-t p-3 space-y-2">
          <p className="text-xs font-medium text-muted-foreground">离线解码（插件调试）</p>
          <div className="flex flex-wrap items-center gap-2">
            <select
              className="h-8 rounded-md border bg-background px-2 text-xs"
              value={decodeSession}
              onChange={(e) => setDecodeSession(e.target.value)}
            >
              {sessions.length === 0 ? (
                <option value="">无会话</option>
              ) : (
                sessions.map((s) => (
                  <option key={s.session_id} value={s.session_id}>
                    {s.session_id}
                  </option>
                ))
              )}
            </select>
            <select
              className="h-8 rounded-md border bg-background px-2 text-xs"
              value={decodePlugin}
              onChange={(e) => setDecodePlugin(e.target.value)}
              disabled={dirPluginList.length === 0}
            >
              {dirPluginList.length === 0 ? (
                <option value="">无插件</option>
              ) : (
                dirPluginList.map((pl) => <option key={pl} value={pl}>{pl}</option>)
              )}
            </select>
            <Button
              size="sm"
              onClick={() =>
                decode.mutate(
                  { sessionId: decodeSession, plugin: decodePlugin },
                  {
                    onSuccess: (res) =>
                      toast.success(
                        "解码完成",
                        `成功 ${res?.decoded ?? 0} / 失败 ${res?.decode_errors ?? 0}（共 ${res?.total_raw ?? 0} 包）`,
                      ),
                    onError: (err) => toast.error("解码失败", err.message),
                  },
                )
              }
              disabled={!decodeSession || !decodePlugin || decode.isPending}
            >
              {decode.isPending ? "解码中..." : "用插件解码"}
            </Button>
          </div>
        </div>
      )}

      {/* 强制下线确认对话框 */}
      {confirmTarget && (
        <Dialog
          open
          onClose={() => setConfirmTarget(null)}
          icon={<Power className="h-5 w-5" />}
          title="强制下线插件"
          description="该操作会断开插件的解码流并把它从注册表中移除，正在进行的抓包将切换到裸包模式。"
          footer={
            <>
              <Button variant="outline" onClick={() => setConfirmTarget(null)}>
                <X className="h-4 w-4" />
                取消
              </Button>
              <Button
                variant="destructive"
                onClick={() =>
                  deregister.mutate(
                    { instanceId: confirmTarget.instance_id, name: confirmTarget.name },
                    {
                      onSuccess: () => {
                        toast.success("已强制下线插件", confirmTarget.name);
                        setConfirmTarget(null);
                      },
                      onError: (err) => {
                        toast.error("下线失败", err.message);
                        setConfirmTarget(null);
                      },
                    },
                  )
                }
                disabled={deregister.isPending}
              >
                {deregister.isPending ? "下线中…" : "确认下线"}
              </Button>
            </>
          }
        >
          <div className="rounded-md bg-muted px-2 py-1.5 font-mono text-xs text-muted-foreground break-all">
            {confirmTarget.instance_id}
          </div>
        </Dialog>
      )}
    </div>
  );
}

function SampleEventRow({
  ev,
  expanded,
  onToggle,
}: {
  ev: TestEventLite;
  expanded: boolean;
  onToggle: () => void;
}) {
  let preview = ev.data_json;
  try {
    const parsed = JSON.parse(ev.data_json);
    preview = JSON.stringify(parsed);
  } catch {
    // data_json 可能被截断导致无法解析，保留原始串
  }
  return (
    <div>
      <button
        type="button"
        className="w-full flex items-center gap-2 px-2 py-1.5 text-left text-xs hover:bg-muted/50"
        onClick={onToggle}
      >
        <span className="w-16 shrink-0 text-muted-foreground">{fmtTime(ev.timestamp_unix)}</span>
        <span className="w-40 shrink-0 truncate font-mono" title={ev.type}>
          {ev.type}
        </span>
        <span className="w-28 shrink-0 truncate text-muted-foreground" title={ev.schema_id}>
          {ev.schema_id}
        </span>
        <span className="flex-1 truncate font-mono text-muted-foreground">{preview}</span>
      </button>
      {expanded && (
        <pre className="px-3 py-2 text-xs bg-muted/40 overflow-auto max-h-60">
          {ev.data_json}
        </pre>
      )}
    </div>
  );
}

function PluginCard({
  plugin,
  hotReloadAt,
  onDeregister,
  deregistering,
  onTest,
}: {
  plugin: RegisteredPlugin;
  hotReloadAt?: number;
  onDeregister: () => void;
  deregistering: boolean;
  onTest: () => void;
}) {
  return (
    <div className="gta-card p-3 transition-[box-shadow,border-color] hover:shadow-md">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2 min-w-0">
          <Activity className="h-4 w-4 shrink-0 text-primary" />
          <span className="text-sm font-medium truncate">{plugin.name}</span>
          <Badge
            variant={plugin.online ? "outline" : "secondary"}
            className={plugin.online ? "border-success/30 bg-success/10 text-success" : ""}
          >
            {plugin.online ? (
              <>
                <span className="gta-live-dot" />
                在线
              </>
            ) : (
              "离线"
            )}
          </Badge>
        </div>
        <div className="flex items-center gap-1 shrink-0">
          <Button
            variant="outline"
            size="sm"
            className="h-7 text-xs"
            onClick={onTest}
            title="测试插件"
          >
            <FlaskConical className="h-3.5 w-3.5" />
          </Button>
          <Button
            variant="outline"
            size="sm"
            className="h-7 shrink-0 text-destructive"
            onClick={onDeregister}
            disabled={deregistering}
            title="强制下线"
          >
            <Power className="h-3.5 w-3.5" />
          </Button>
        </div>
      </div>

      <div className="mt-2 flex items-center gap-2 text-xs text-muted-foreground">
        {plugin.online ? <Wifi className="h-3 w-3" /> : <WifiOff className="h-3 w-3" />}
        <span className="truncate">{plugin.protocol || "—"}</span>
        <span className="text-muted-foreground/50">·</span>
        <span className="font-mono truncate" title={plugin.instance_id}>
          {truncate(plugin.instance_id, 10)}
        </span>
      </div>

      <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
        <span>心跳 {fmtRelative(plugin.last_heartbeat)}</span>
        {hotReloadAt && (
          <Badge variant="outline" className="text-amber-600 border-amber-300">
            热更 {fmtClock(Math.floor(hotReloadAt / 1000))}
          </Badge>
        )}
      </div>
    </div>
  );
}
