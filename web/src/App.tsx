import { useState, useCallback, useEffect, useRef, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { SessionSidebar } from "@/components/session-sidebar";
import { FilterBar } from "@/components/filter-bar";
import { EventTable } from "@/components/event-table";
import { RawPacketTable } from "@/components/raw-packet-table";
import { PluginPanel } from "@/components/plugin-panel";
import { AnalyticsPanel } from "@/components/analytics-panel";
import { RelationshipPanel } from "@/components/relationship-panel";
import { RunsPanel } from "@/components/runs-panel";
import { SchemaExplorer } from "@/components/schema-explorer";
import { TableBrowser } from "@/components/table-browser";
import { SettingsDialog } from "@/components/settings-dialog";
import { StartCaptureDialog } from "@/components/start-capture-dialog";
import { Button } from "@/components/ui/button";
import { Sun, Moon, Settings, Play, Square } from "lucide-react";
import { RAW_DEBUG_ENABLED } from "@/lib/env";
import { usePluginEventStream, useStopCapture, useSessions } from "@/hooks/use-mcp";
import { toast } from "@/components/ui/toast";

type ViewTab = "decoded" | "analytics" | "relationship" | "runs" | "data" | "plugins" | "raw";

/** 品牌标识：广播/信号图标，呼应"协议流量分析"。 */
function BrandMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 36 36" className={className} role="img" aria-label="GTA 标识">
      <rect width="36" height="36" rx="9" fill="url(#gta-brand-grad)" />
      <g fill="none" stroke="white" strokeWidth="2" strokeLinecap="round">
        <path d="M11 21a7.5 7.5 0 0 1 14 0" />
        <path d="M14.5 24.5a3.5 3.5 0 0 1 7 0" />
      </g>
      <circle cx="18" cy="28.5" r="2" fill="white" />
      <defs>
        <linearGradient id="gta-brand-grad" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#4f46e5" />
          <stop offset="1" stopColor="#0284c7" />
        </linearGradient>
      </defs>
    </svg>
  );
}

const TABS: { id: ViewTab; label: string }[] = [
  { id: "decoded", label: "协议数据" },
  { id: "analytics", label: "分析" },
  { id: "relationship", label: "关系" },
  { id: "runs", label: "行为" },
  { id: "data", label: "数据探查" },
  { id: "plugins", label: "插件" },
  ...(RAW_DEBUG_ENABLED ? [{ id: "raw" as ViewTab, label: "原始包" }] : []),
];

export default function App() {
  const [selectedSessionId, setSelectedSessionId] = useState<string | null>(null);
  // 与当前抓包会话联动的行为窗口（start_capture 成功后自动 begin，便于在「行为」Tab 直接查看）。
  const [linkedRunId, setLinkedRunId] = useState<string | null>(null);
  const [linkedRunSessionId, setLinkedRunSessionId] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [activeTab, setActiveTab] = useState<ViewTab>("decoded");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [startOpen, setStartOpen] = useState(false);
  const [isDark, setIsDark] = useState(() => {
    const stored = localStorage.getItem("gta-theme");
    if (stored) return stored === "dark";
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  });

  // 筛选框引用：用于 "/" 快捷键聚焦
  const filterInputRef = useRef<HTMLInputElement>(null);

  // "/" 快捷键：在协议数据 Tab 聚焦筛选框（不在输入框/可编辑区时）
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (activeTab !== "decoded") return;
      const t = e.target as HTMLElement | null;
      const tag = t?.tagName;
      const editable =
        tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || t?.isContentEditable;
      if (e.key === "/" && !editable && !e.metaKey && !e.ctrlKey && !e.altKey) {
        e.preventDefault();
        filterInputRef.current?.focus();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [activeTab]);

  // 订阅后端插件事件 SSE 流，插件上下线/热更时零延迟刷新面板与侧边栏。
  usePluginEventStream();

  const stopCapture = useStopCapture();
  const { data: sessionsData } = useSessions();
  const isSelectedRunning = sessionsData?.sessions?.some(
    (s) => s.session_id === selectedSessionId && s.status === "running",
  ) ?? false;

  function handleStop() {
    if (!selectedSessionId) return;
    stopCapture.mutate(
      { sessionId: selectedSessionId },
      {
        onSuccess: () => {
          toast.success("抓包已停止", `会话 ${selectedSessionId}`);
        },
        onError: (err) => {
          toast.error("停止失败", err.message);
        },
      },
    );
  }

  // 同步暗色模式到 DOM 和 localStorage
  useEffect(() => {
    document.documentElement.classList.toggle("dark", isDark);
    localStorage.setItem("gta-theme", isDark ? "dark" : "light");
  }, [isDark]);

  // 若原始包调试未开启，避免停留在 raw Tab
  useEffect(() => {
    if (!RAW_DEBUG_ENABLED && activeTab === "raw") {
      setActiveTab("decoded");
    }
  }, [activeTab]);

  const handleSelectSession = useCallback((sessionId: string) => {
    setSelectedSessionId(sessionId);
    setFilter(""); // 切换 session 时清空 filter
  }, []);

  // Tab 键导航：左右方向键在 tablist 中移动焦点
  const tabRefs = useRef<(HTMLButtonElement | null)[]>([]);
  function handleTabKeyDown(e: ReactKeyboardEvent) {
    if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
    const idx = TABS.findIndex((t) => t.id === activeTab);
    const next =
      e.key === "ArrowRight"
        ? (idx + 1) % TABS.length
        : (idx - 1 + TABS.length) % TABS.length;
    e.preventDefault();
    setActiveTab(TABS[next]!.id);
    tabRefs.current[next]?.focus();
  }

  return (
    <div className="flex h-screen overflow-hidden bg-background text-foreground">
      {/* 左侧边栏：品牌 + 会话列表 */}
      <aside className="flex w-72 shrink-0 flex-col border-r border-border bg-card">
        {/* 品牌块 */}
        <div className="flex items-center gap-2.5 border-b border-border px-4 py-3.5">
          <BrandMark className="h-9 w-9 shrink-0" />
          <div className="min-w-0">
            <p className="text-sm font-semibold leading-tight gta-gradient-text">GTA</p>
            <p className="truncate text-[11px] leading-tight text-muted-foreground">
              协议流量分析
            </p>
          </div>
        </div>

        <SessionSidebar
          selectedSessionId={selectedSessionId}
          onSelectSession={handleSelectSession}
          onDeleted={(id) => {
            if (id === selectedSessionId) setSelectedSessionId(null);
            // 被删会话若正联动某行为窗口，一并清除联动状态
            if (id && id === linkedRunSessionId) {
              setLinkedRunId(null);
              setLinkedRunSessionId(null);
            }
          }}
        />
      </aside>

      {/* 右侧主内容区 */}
      <main className="flex min-w-0 flex-1 flex-col">
        {/* 顶部工具栏：Tab 切换 + 开始抓包 + 主题/设置 */}
        <header className="flex items-center justify-between gap-3 border-b border-border bg-card/80 px-4 py-2.5 backdrop-blur">
          <div className="flex items-center gap-3">
            {/* 分段控件式 Tab */}
            <div
              role="tablist"
              aria-label="视图切换"
              onKeyDown={handleTabKeyDown}
              className="flex items-center gap-1 rounded-lg bg-muted p-1"
            >
              {TABS.map((t, i) => {
                const selected = activeTab === t.id;
                return (
                  <button
                    key={t.id}
                    ref={(el) => {
                      tabRefs.current[i] = el;
                    }}
                    role="tab"
                    aria-selected={selected}
                    tabIndex={selected ? 0 : -1}
                    onClick={() => setActiveTab(t.id)}
                    className={
                      "rounded-md px-3 py-1.5 text-sm font-medium transition-[background-color,color,box-shadow] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 " +
                      (selected
                        ? "bg-card text-foreground shadow-sm"
                        : "text-muted-foreground hover:text-foreground")
                    }
                  >
                    {t.label}
                  </button>
                );
              })}
            </div>

            {/* 当前会话上下文 */}
            {selectedSessionId && (
              <span
                className="hidden items-center gap-1.5 rounded-md border border-border bg-background px-2 py-1 text-xs text-muted-foreground lg:inline-flex"
                title={selectedSessionId}
              >
                <span className="gta-live-dot" />
                <span className="max-w-[160px] truncate font-mono">{selectedSessionId}</span>
              </span>
            )}
          </div>

          <div className="flex items-center gap-1">
            <Button
              variant="default"
              size="sm"
              className="h-8"
              onClick={() => setStartOpen(true)}
              title="开始抓包"
              aria-label="开始抓包"
            >
              <Play className="h-4 w-4" />
              开始抓包
            </Button>
            {isSelectedRunning && (
              <Button
                variant="destructive"
                size="sm"
                className="h-8"
                onClick={handleStop}
                disabled={stopCapture.isPending}
                title="停止抓包"
                aria-label="停止抓包"
              >
                <Square className="h-3.5 w-3.5" />
                {stopCapture.isPending ? "停止中…" : "停止抓包"}
              </Button>
            )}
            <Button
              variant="ghost"
              size="icon"
              className="h-8 w-8"
              onClick={() => setIsDark((prev) => !prev)}
              title={isDark ? "切换亮色" : "切换暗色"}
              aria-label={isDark ? "切换为亮色模式" : "切换为暗色模式"}
            >
              {isDark ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
            </Button>
            <Button
              variant="ghost"
              size="icon"
              className="h-8 w-8"
              onClick={() => setSettingsOpen(true)}
              title="设置"
              aria-label="设置"
            >
              <Settings className="h-4 w-4" />
            </Button>
          </div>
        </header>

        {/* 筛选栏：仅协议数据 Tab 显示 */}
        {activeTab === "decoded" && (
          <div className="border-b border-border bg-card/40 px-4 py-3">
            <FilterBar
              sessionId={selectedSessionId}
              filter={filter}
              onFilterChange={setFilter}
              inputRef={filterInputRef}
            />
          </div>
        )}

        {/* 数据表格 */}
        <div className="min-h-0 flex-1 overflow-hidden">
          {activeTab === "decoded" && (
            <div className="h-full overflow-auto p-4 gta-scroll">
              <EventTable sessionId={selectedSessionId} filter={filter} />
            </div>
          )}
          {activeTab === "analytics" && <AnalyticsPanel sessionId={selectedSessionId} />}
          {activeTab === "relationship" && <RelationshipPanel sessionId={selectedSessionId} />}
          {activeTab === "runs" && (
            <RunsPanel linkedRunId={linkedRunId} linkedSessionId={linkedRunSessionId} />
          )}
          {activeTab === "data" && (
            <div className="flex h-full flex-col">
              <div className="min-h-0 flex-1 border-b border-border">
                <SchemaExplorer sessionId={selectedSessionId} />
              </div>
              <div className="min-h-0 flex-1">
                <TableBrowser sessionId={selectedSessionId} />
              </div>
            </div>
          )}
          {activeTab === "plugins" && <PluginPanel />}
          {activeTab === "raw" && (
            <div className="h-full overflow-auto p-4 gta-scroll">
              <RawPacketTable
                sessionId={selectedSessionId}
                onDecoded={() => setActiveTab("decoded")}
              />
            </div>
          )}
        </div>
      </main>

      {/* 设置弹窗 */}
      <SettingsDialog open={settingsOpen} onClose={() => setSettingsOpen(false)} />
      {/* 开始抓包弹窗 */}
      <StartCaptureDialog
        open={startOpen}
        onClose={() => setStartOpen(false)}
        onStarted={(sessionId) => {
          setSelectedSessionId(sessionId);
          setFilter("");
          setActiveTab("decoded");
        }}
        onRunLinked={(runId, sessionId) => {
          setLinkedRunId(runId);
          setLinkedRunSessionId(sessionId);
        }}
      />
    </div>
  );
}
