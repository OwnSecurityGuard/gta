import { useState, useCallback, useEffect, useRef, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { SessionSidebar } from "@/components/session-sidebar";
import { FilterBar } from "@/components/filter-bar";
import { EventTable } from "@/components/event-table";
import { RawPacketTable } from "@/components/raw-packet-table";
import { ConnectionsPage } from "@/components/connections-page";
import { PluginPanel } from "@/components/plugin-panel";
import { AnalyticsPanel } from "@/components/analytics-panel";
import { TimelinePanel } from "@/components/timeline-panel";
import { RunsPanel } from "@/components/runs-panel";
import { SchemaExplorer } from "@/components/schema-explorer";
import { TableBrowser } from "@/components/table-browser";
import { SettingsDialog } from "@/components/settings-dialog";
import { StartCaptureDialog } from "@/components/start-capture-dialog";
import { ProxyConfigDialog } from "@/components/proxy-config-dialog";
import { AgentDownloadDialog } from "@/components/agent-download-dialog";
import { MembersAdminDialog } from "@/components/members-admin-dialog";
import { ProbeAdminDialog } from "@/components/probe-admin-dialog";
import { MyCapturePage } from "@/components/my-capture-page";
import { ProjectPage } from "@/components/project-page";
import { SessionOverviewPage } from "@/components/session-overview-page";
import { Button } from "@/components/ui/button";
import { Sun, Moon, Settings, Play, Square, Cable, KeyRound, Download, ChevronDown, Check, UserRound, Users, Server } from "lucide-react";
import { RAW_DEBUG_ENABLED } from "@/lib/env";
import { usePluginEventStream, useStopCapture, useSessions } from "@/hooks/use-mcp";
import { useAuthError, useIdentity } from "@/hooks/use-auth";
import { toast } from "@/components/ui/toast";
import type { ProjectInfo } from "@/types/project";

type ViewTab = "home" | "overview" | "connections" | "timeline" | "analytics" | "decoded" | "runs" | "data" | "plugins" | "raw";

/** 一级视图：普通用户最常用的入口（我的抓包 / 会话概览 / 连接 / 时间线 / 分析）。 */
const PRIMARY_TABS: { id: ViewTab; label: string }[] = [
  { id: "home", label: "我的抓包" },
  { id: "overview", label: "概览" },
  { id: "connections", label: "连接" },
  { id: "timeline", label: "时间线" },
  { id: "analytics", label: "分析" },
];

/** 高级视图：插件 / 原始包 / 数据探查，默认收进「更多」下拉，降低普通用户的认知负担。 */
const ADVANCED_TABS: { id: ViewTab; label: string }[] = [
  { id: "decoded", label: "协议数据" },
  { id: "runs", label: "行为" },
  { id: "data", label: "数据探查" },
  { id: "plugins", label: "插件" },
  ...(RAW_DEBUG_ENABLED ? [{ id: "raw" as ViewTab, label: "原始包" }] : []),
];

const TABS: { id: ViewTab; label: string }[] = [...PRIMARY_TABS, ...ADVANCED_TABS];

/** 品牌标识：广播/信号图标，呼应"游戏调试自动化"。 */
function BrandMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 36 36" className={className} role="img" aria-label="GameTrace 标识">
      <rect width="36" height="36" rx="9" fill="url(#gt-brand-grad)" />
      <g fill="none" stroke="white" strokeWidth="2" strokeLinecap="round">
        <path d="M11 21a7.5 7.5 0 0 1 14 0" />
        <path d="M14.5 24.5a3.5 3.5 0 0 1 7 0" />
      </g>
      <circle cx="18" cy="28.5" r="2" fill="white" />
      <defs>
        <linearGradient id="gt-brand-grad" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#4f46e5" />
          <stop offset="1" stopColor="#0284c7" />
        </linearGradient>
      </defs>
    </svg>
  );
}

export default function App() {
  const [selectedSessionId, setSelectedSessionId] = useState<string | null>(null);
  // 与当前抓包会话联动的行为窗口（start_capture 成功后自动 begin，便于在「行为」Tab 直接查看）。
  const [linkedRunId, setLinkedRunId] = useState<string | null>(null);
  const [linkedRunSessionId, setLinkedRunSessionId] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [activeTab, setActiveTab] = useState<ViewTab>("home");
  // 「更多」下拉是否展开（普通用户把高级视图藏在这里）。
  const [moreOpen, setMoreOpen] = useState(false);
  // 从项目「一键抓包」带入的默认端口/插件/项目id，传给 StartCaptureDialog 预填。
  const [projectPrefill, setProjectPrefill] = useState<{ port?: number; plugin?: string; projectId?: string }>({});
  // 正在查看的项目详情（切换到 ProjectPage 而非首页）。
  const [selectedProjectId, setSelectedProjectId] = useState<string | null>(null);
  // 从连接详情点击 flow_id 跳转时预填到「行为」Tab 的 flow_id
  const [tracePrefill, setTracePrefill] = useState<string | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  // 服务器开启令牌校验且本地无有效凭证（401）：横幅提示并自动打开设置。
  const authError = useAuthError();
  // 当前身份（后端响应头回显）：顶栏展示"我是谁"，是协作（告知用户名/被加项目）的前提。
  const identity = useIdentity();
  const [startOpen, setStartOpen] = useState(false);
  const [proxyConfigOpen, setProxyConfigOpen] = useState(false);
  const [agentDownloadOpen, setAgentDownloadOpen] = useState(false);
  const [membersOpen, setMembersOpen] = useState(false);
  const [probesOpen, setProbesOpen] = useState(false);
  const [isDark, setIsDark] = useState(() => {
    const stored = localStorage.getItem("gt-theme");
    if (stored) return stored === "dark";
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  });

  // 筛选框引用：用于 "/" 快捷键聚焦
  const filterInputRef = useRef<HTMLInputElement>(null);
  // 会话搜索框引用：用于 Ctrl/Cmd+K 快捷键聚焦
  const sessionSearchInputRef = useRef<HTMLInputElement>(null);

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

  // Ctrl/Cmd+K 快捷键：全局聚焦左侧会话搜索框
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if ((e.ctrlKey || e.metaKey) && (e.key === "k" || e.key === "K")) {
        e.preventDefault();
        sessionSearchInputRef.current?.focus();
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

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
    localStorage.setItem("gt-theme", isDark ? "dark" : "light");
  }, [isDark]);

  // 若原始包调试未开启，避免停留在 raw Tab
  useEffect(() => {
    if (!RAW_DEBUG_ENABLED && activeTab === "raw") {
      setActiveTab("home");
    }
  }, [activeTab]);

  // 401 发生时自动打开设置弹窗，引导填入访问令牌（横幅常驻直至保存新 token）。
  // 其它弹窗打开时暂不抢占（避免遮挡下抢焦点/一次 ESC 关两个），关掉后仍会自动补开。
  useEffect(() => {
    if (authError && !startOpen && !proxyConfigOpen) setSettingsOpen(true);
  }, [authError, startOpen, proxyConfigOpen]);

  const handleSelectSession = useCallback((sessionId: string) => {
    setSelectedSessionId(sessionId);
    setFilter(""); // 切换 session 时清空 filter
  }, []);

  // 从连接详情点击 flow_id：「行为」Tab 预填 flow_id 并切换过去
  const handleJumpToRun = useCallback((flowId: string) => {
    setTracePrefill(flowId);
    setActiveTab("runs");
  }, []);

  // 从代理抓包状态卡一键跳转：选中常驻会话切到「连接」页并关闭弹窗
  const handleNavigateToSession = useCallback((sessionId: string) => {
    setSelectedSessionId(sessionId);
    setFilter("");
    setActiveTab("connections");
    setProxyConfigOpen(false);
  }, []);

  // 首页「本机抓包」：不带项目预填地打开开始抓包弹窗。
  const handleStartDefault = useCallback(() => {
    setProjectPrefill({});
    setStartOpen(true);
  }, []);

  // 首页「我的项目 → 以该项目抓包」：把项目的默认端口/插件/项目id 预填进开始抓包弹窗。
  const handleStartProject = useCallback((p: ProjectInfo) => {
    setProjectPrefill({
      port: p.default_port && p.default_port > 0 ? p.default_port : undefined,
      plugin: p.default_plugin || undefined,
      projectId: p.id,
    });
    setStartOpen(true);
  }, []);

  // 首页「我的项目 → 进入」：切换到项目详情页。
  const handleOpenProject = useCallback((projectId: string) => {
    setSelectedProjectId(projectId);
    setActiveTab("home");
  }, []);

  // 项目详情页「返回」：退回首页「我的抓包」。
  const handleBackFromProject = useCallback(() => {
    setSelectedProjectId(null);
  }, []);

  // 项目详情页「开始抓包」：带项目默认端口/插件/项目 id 打开开始抓包弹窗，
  // 抓包会话自动归属该项目（Project → Capture → Session 主流程无断点）。
  const handleProjectPageStart = useCallback(
    (p: { id: string; name: string; port?: number; plugin?: string }) => {
      setProjectPrefill({
        projectId: p.id,
        port: p.port && p.port > 0 ? p.port : undefined,
        plugin: p.plugin || undefined,
      });
      setStartOpen(true);
    },
    [],
  );

  // 从首页「最近会话」卡片进入会话详情，默认落在「概览」（会话工作区入口）。
  const handleHomeSelectSession = useCallback((sessionId: string) => {
    handleSelectSession(sessionId);
    setActiveTab("overview");
  }, [handleSelectSession]);

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
            <p className="text-sm font-semibold leading-tight gt-gradient-text">GameTrace</p>
            <p className="truncate text-[11px] leading-tight text-muted-foreground">
              Game Debug Automation
            </p>
          </div>
        </div>

        <SessionSidebar
          selectedSessionId={selectedSessionId}
          onSelectSession={handleSelectSession}
          searchInputRef={sessionSearchInputRef}
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
        {/* 401 横幅：服务器要求访问令牌而本地未配置/已失效 */}
        {authError && (
          <div className="flex items-center gap-2 border-b border-amber-300 bg-amber-50 px-4 py-2 text-sm text-amber-900 dark:border-amber-500/40 dark:bg-amber-500/10 dark:text-amber-200">
            <KeyRound className="h-4 w-4 shrink-0" />
            <span className="flex-1">
              服务器开启了访问令牌校验，请在设置中填入访问令牌后重试。
            </span>
            <Button
              size="sm"
              variant="outline"
              className="h-7"
              onClick={() => setSettingsOpen(true)}
            >
              打开设置
            </Button>
          </div>
        )}

        {/* 顶部工具栏：Tab 切换 + 开始抓包 + 主题/设置 */}
        <header className="flex items-center justify-between gap-3 border-b border-border bg-card/80 px-4 py-2.5 backdrop-blur">
          <div className="flex items-center gap-3">
            {/* 分段控件式 Tab：一级视图常驻；高级视图收进「更多」下拉 */}
            <div
              role="tablist"
              aria-label="视图切换"
              onKeyDown={handleTabKeyDown}
              className="flex items-center gap-1 rounded-lg bg-muted p-1"
            >
              {PRIMARY_TABS.map((t, i) => {
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

              {/* 高级视图下拉 */}
              <div className="relative">
                <button
                  type="button"
                  onClick={() => setMoreOpen((v) => !v)}
                  aria-expanded={moreOpen}
                  className={
                    "inline-flex items-center gap-1 rounded-md px-3 py-1.5 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40 " +
                    (ADVANCED_TABS.some((t) => t.id === activeTab)
                      ? "bg-card text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground")
                  }
                >
                  更多
                  <ChevronDown className="h-3.5 w-3.5" />
                </button>
                {moreOpen && (
                  <>
                    {/* 点击空白关闭下拉 */}
                    <div
                      className="fixed inset-0 z-40"
                      onClick={() => setMoreOpen(false)}
                      aria-hidden
                    />
                    <div className="absolute right-0 top-full z-50 mt-1 min-w-[180px] overflow-hidden rounded-lg border border-border bg-popover p-1 shadow-lg">
                      {ADVANCED_TABS.map((t) => {
                        const selected = activeTab === t.id;
                        return (
                          <button
                            key={t.id}
                            type="button"
                            role="menuitem"
                            onClick={() => {
                              setActiveTab(t.id);
                              setMoreOpen(false);
                            }}
                            className={
                              "flex w-full items-center justify-between gap-2 rounded-md px-2.5 py-1.5 text-left text-sm transition-colors " +
                              (selected
                                ? "bg-muted text-foreground"
                                : "text-muted-foreground hover:bg-muted/60 hover:text-foreground")
                            }
                          >
                            {t.label}
                            {selected && <Check className="h-3.5 w-3.5 text-primary" />}
                          </button>
                        );
                      })}
                    </div>
                  </>
                )}
              </div>
            </div>

            {/* 当前会话上下文 */}
            {selectedSessionId && (
              <span
                className="hidden items-center gap-1.5 rounded-md border border-border bg-background px-2 py-1 text-xs text-muted-foreground lg:inline-flex"
                title={selectedSessionId}
              >
                <span className="gt-live-dot" />
                <span className="max-w-[160px] truncate font-mono">{selectedSessionId}</span>
              </span>
            )}
          </div>

          <div className="flex items-center gap-1">
            <Button
              variant="default"
              size="sm"
              className="h-8"
              onClick={() => setProxyConfigOpen(true)}
              title="代理服务器配置（常驻代理抓包，生成手机连接二维码）"
              aria-label="代理服务器配置"
            >
              <Cable className="h-4 w-4" />
              代理配置
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="h-8"
              onClick={() => setAgentDownloadOpen(true)}
              title="接入设备（生成启动码，把成员电脑接进团队抓包）"
              aria-label="接入设备"
            >
              <Download className="h-4 w-4" />
              接入设备
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="h-8"
              onClick={() => setProbesOpen(true)}
              title="探针管理（接入的抓包机器：状态 / 停抓 / 本地留存导入）"
              aria-label="探针管理"
            >
              <Server className="h-4 w-4" />
              探针
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="h-8"
              onClick={() => setMembersOpen(true)}
              title="成员管理（邀请码 / 成员账号列表与撤销）"
              aria-label="成员管理"
            >
              <Users className="h-4 w-4" />
              成员管理
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="h-8"
              onClick={() => setStartOpen(true)}
              title="开始抓包（服务器网卡 / 抓包探针）"
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
              size="sm"
              className="h-8 gap-1.5 px-2"
              onClick={() => setSettingsOpen(true)}
              title={
                identity
                  ? `当前身份：${identity.owner}${identity.isAdmin ? "（管理员）" : ""}；点击打开设置`
                  : "未登录：点击打开设置（填令牌或注册身份）"
              }
            >
              <UserRound className="h-4 w-4" />
              <span className="max-w-[140px] truncate font-mono text-xs">
                {identity?.owner ?? "未登录"}
              </span>
              {identity?.isAdmin && (
                <span className="rounded bg-primary/10 px-1 py-0.5 text-[10px] text-primary">admin</span>
              )}
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
          {activeTab === "home" &&
            (selectedProjectId ? (
              <ProjectPage
                projectId={selectedProjectId}
                onBack={handleBackFromProject}
                onSelectSession={handleHomeSelectSession}
                onStartProject={handleProjectPageStart}
              />
            ) : (
              <MyCapturePage
                onStartDefault={handleStartDefault}
                onStartProject={handleStartProject}
                onAgentDownload={() => setAgentDownloadOpen(true)}
                onProxy={() => setProxyConfigOpen(true)}
                onSelectSession={handleHomeSelectSession}
                onOpenProject={handleOpenProject}
              />
            ))}
          {activeTab === "overview" && (
            <SessionOverviewPage
              sessionId={selectedSessionId}
              onNavigate={(tab) => setActiveTab(tab)}
            />
          )}
          {activeTab === "decoded" && (
            <div className="h-full overflow-auto p-4 gt-scroll">
              <EventTable sessionId={selectedSessionId} filter={filter} />
            </div>
          )}
          {activeTab === "connections" && (
            <div className="h-full overflow-auto p-4 gt-scroll">
              <ConnectionsPage sessionId={selectedSessionId} onJumpToRun={handleJumpToRun} />
            </div>
          )}
          {activeTab === "analytics" && <AnalyticsPanel sessionId={selectedSessionId} />}
          {activeTab === "timeline" && <TimelinePanel sessionId={selectedSessionId} />}
          {activeTab === "runs" && (
            <RunsPanel
              linkedRunId={linkedRunId}
              linkedSessionId={linkedRunSessionId}
              tracePrefill={tracePrefill}
            />
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
            <div className="h-full overflow-auto p-4 gt-scroll">
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
      {/* 代理服务器配置弹窗 */}
      <ProxyConfigDialog open={proxyConfigOpen} onClose={() => setProxyConfigOpen(false)} onNavigateToSession={handleNavigateToSession} />
      {/* 下载抓包探针弹窗（跨环境抓包上报） */}
      <AgentDownloadDialog
        open={agentDownloadOpen}
        onClose={() => setAgentDownloadOpen(false)}
        onNavigateToSession={handleNavigateToSession}
      />
      {/* 成员管理弹窗（邀请码 / 成员账号列表与撤销） */}
      <MembersAdminDialog open={membersOpen} onClose={() => setMembersOpen(false)} />
      {/* 探针管理弹窗（三维度状态 / 停抓 / 改名 / 吊销 / 本地留存离线导入） */}
      <ProbeAdminDialog
        open={probesOpen}
        onClose={() => setProbesOpen(false)}
        onImported={(sessionId) => {
          setSelectedSessionId(sessionId);
          setActiveTab("overview");
        }}
      />
      {/* 开始抓包弹窗（本机网卡 / 远程 agent 源） */}
      <StartCaptureDialog
        open={startOpen}
        onClose={() => setStartOpen(false)}
        initialPort={projectPrefill.port}
        initialPlugin={projectPrefill.plugin}
        initialProjectId={projectPrefill.projectId}
        onStarted={(sessionId) => {
          setSelectedSessionId(sessionId);
          setFilter("");
          // 抓包成功即进入「概览」（会话工作区默认入口），
          // 概览页可看到实时统计与最近连接/事件，并可一键跳到连接/时间线分析。
          setActiveTab("overview");
        }}
        onRunLinked={(runId, sessionId) => {
          setLinkedRunId(runId);
          setLinkedRunSessionId(sessionId);
        }}
      />
    </div>
  );
}
