// MyCapturePage — 「我的抓包」首页（Web First · P1）。
//
// 把用户真正关心的三件事放在第一屏：开始抓包（三条清晰来源）、我的项目（一键复用配置）、
// 最近会话（一眼看出哪个值得进去看）。Session 不再是第一概念，只是后台会话的实现载体。
import { useState } from "react";
import {
  Laptop,
  Smartphone,
  Server,
  Plus,
  Trash2,
  Play,
  ChevronRight,
  ChevronDown,
  Dot,
  FolderGit2,
} from "lucide-react";
import { DeviceStatusList } from "@/components/device-status";
import { useSessions, useProjects, useCreateProject, useDeleteProject } from "@/hooks/use-mcp";
import type { ProjectInfo } from "@/types/project";
import { toast } from "@/components/ui/toast";

function statusDot(status: string) {
  return status === "running" ? "bg-emerald-500" : "bg-muted-foreground/40";
}

function sourceLabel(source: string) {
  switch (source) {
    case "proxy":
      return "手机代理";
    case "agent":
      return "抓包探针";
    default:
      return "服务器抓包";
  }
}

function fmtTime(iso: string) {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const now = new Date();
  const diffDay = (now.getTime() - d.getTime()) / 86_400_000;
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  if (diffDay < 1 && d.getDate() === now.getDate()) return `今天 ${hh}:${mm}`;
  if (diffDay < 2) return `昨天 ${hh}:${mm}`;
  return `${d.getMonth() + 1}月${d.getDate()}日 ${hh}:${mm}`;
}

interface MyCapturePageProps {
  onStartDefault: () => void;
  onStartProject: (p: ProjectInfo) => void;
  onAgentDownload: () => void;
  onProxy: () => void;
  onSelectSession: (sessionId: string) => void;
  /** 进入项目详情页（项目作为一等组织单元） */
  onOpenProject: (projectId: string) => void;
}

export function MyCapturePage({
  onStartDefault,
  onStartProject,
  onAgentDownload,
  onProxy,
  onSelectSession,
  onOpenProject,
}: MyCapturePageProps) {
  const { data: sessionsData } = useSessions();
  const sessions = sessionsData?.sessions ?? [];
  const { data: projectsData } = useProjects();
  const projects = projectsData?.projects ?? [];
  const createProject = useCreateProject();
  const deleteProject = useDeleteProject();

  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [newPort, setNewPort] = useState("");
  // 高级入口：把「服务器本机抓包」收进折叠，普通用户第一屏只看到两条主路径。
  const [showAdvanced, setShowAdvanced] = useState(false);

  function handleCreate() {
    const name = newName.trim();
    if (!name) return;
    const p = parseInt(newPort || "0", 10);
    createProject.mutate(
      { name, port: p > 0 ? p : 0 },
      {
        onSuccess: () => {
          setNewName("");
          setNewPort("");
          setCreating(false);
          toast.success("项目已创建", name);
        },
        onError: (err) => toast.error("创建失败", err.message),
      },
    );
  }

  const recent = [...sessions].slice(0, 6);

  return (
    <div className="h-full overflow-auto gta-scroll">
      <div className="mx-auto max-w-5xl space-y-8 p-6">
        {/* 欢迎语 */}
        <header>
          <h1 className="text-xl font-semibold gta-gradient-text">我的抓包</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            从哪抓、抓什么，一次说清——后面的插件与端口配置由 GTA 帮你记住。
          </p>
        </header>

        {/* 开始抓包：两条主路径 + 高级服务器抓包 */}
        <section>
          <div className="rounded-2xl border border-border bg-card/60 p-5">
            <div className="flex items-center justify-between">
              <h2 className="text-sm font-medium text-foreground">开始抓包</h2>
              <span className="text-[11px] text-muted-foreground">选择接入方式</span>
            </div>
            <div className="mt-4 grid gap-3 sm:grid-cols-2">
              <button
                type="button"
                onClick={onAgentDownload}
                className="group rounded-xl border border-border bg-background p-4 text-left transition-colors hover:border-primary/40 hover:bg-muted/40"
              >
                <Laptop className="h-6 w-6 text-primary" />
                <p className="mt-2.5 text-sm font-medium">我的电脑</p>
                <p className="mt-1 text-xs text-muted-foreground">
                  在我的电脑上运行 GTA 探针，接入后开始抓包
                </p>
              </button>
              <button
                type="button"
                onClick={onProxy}
                className="group rounded-xl border border-border bg-background p-4 text-left transition-colors hover:border-primary/40 hover:bg-muted/40"
              >
                <Smartphone className="h-6 w-6 text-primary" />
                <p className="mt-2.5 text-sm font-medium">手机代理</p>
                <p className="mt-1 text-xs text-muted-foreground">
                  手机通过代理接入 GTA，扫码即可抓包
                </p>
              </button>
            </div>

            {/* 高级：服务器本机网卡抓包（服务端管理用，普通用户无需面对） */}
            <button
              type="button"
              onClick={() => setShowAdvanced((v) => !v)}
              aria-expanded={showAdvanced}
              className="mt-3 inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
            >
              <ChevronDown className={`h-3.5 w-3.5 transition-transform ${showAdvanced ? "rotate-180" : ""}`} />
              高级
            </button>
            {showAdvanced && (
              <div className="mt-2 grid gap-3 sm:grid-cols-2">
                <button
                  type="button"
                  onClick={onStartDefault}
                  className="group rounded-xl border border-border bg-background p-4 text-left transition-colors hover:border-primary/40 hover:bg-muted/40"
                >
                  <Server className="h-6 w-6 text-muted-foreground" />
                  <p className="mt-2.5 text-sm font-medium">服务器抓包</p>
                  <p className="mt-1 text-xs text-muted-foreground">
                    直接在 GTA 服务器网卡上抓包（面向服务端管理）
                  </p>
                </button>
              </div>
            )}
          </div>
        </section>

        {/* 我的设备：接入状态闭环 */}
        <section>
          <h2 className="text-sm font-medium text-foreground">我的设备</h2>
          <div className="mt-3">
            <DeviceStatusList onSelectSession={onSelectSession} />
          </div>
        </section>

        {/* 我的项目：一键开始抓包 */}
        <section>
          <div className="flex items-center justify-between">
            <h2 className="text-sm font-medium text-foreground">我的项目</h2>
            <button
              type="button"
              onClick={() => setCreating((v) => !v)}
              className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
            >
              <Plus className="h-3.5 w-3.5" />
              新建项目
            </button>
          </div>

          {creating && (
            <div className="mt-3 rounded-xl border border-border bg-card/60 p-3">
              <div className="grid gap-2 sm:grid-cols-[1fr_120px_auto]">
                <input
                  value={newName}
                  onChange={(e) => setNewName(e.target.value)}
                  placeholder="项目名称，如 Godot Game"
                  className="h-9 rounded-md border border-input bg-background px-2.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                />
                <input
                  value={newPort}
                  onChange={(e) => setNewPort(e.target.value)}
                  placeholder="端口"
                  inputMode="numeric"
                  className="h-9 rounded-md border border-input bg-background px-2.5 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                />
                <button
                  type="button"
                  onClick={handleCreate}
                  disabled={createProject.isPending || !newName.trim()}
                  className="h-9 rounded-md bg-primary px-4 text-sm font-medium text-primary-foreground disabled:opacity-50"
                >
                  创建
                </button>
              </div>
              <p className="mt-2 text-xs text-muted-foreground">
                项目只记住 名称 + 默认端口（默认插件稍后选择），下次从项目一键开始抓包。
              </p>
            </div>
          )}

          <div className="mt-3 grid gap-2 sm:grid-cols-2">
            {projects.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                还没有项目。新建一个项目，以后从项目一键开始抓包，不必重复输入端口与插件。
              </p>
            ) : (
              projects.map((p) => {
                // 从已加载的 sessions 中按 project_id 派生在线/离线状态，避免在循环里调用 Hook。
                const list = sessions.filter((s) => s.session_id && s.project_id === p.id);
                const running = list.some((s) => s.status === "running");
                const latest = list[0];
                const latestText = running ? "抓包中" : latest ? "已停止" : "暂无会话";
                return (
                  <div
                    key={p.id}
                    className="flex items-center gap-3 rounded-xl border border-border bg-card/60 p-3"
                  >
                    <FolderGit2 className="h-4 w-4 shrink-0 text-muted-foreground" />
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-1.5">
                        <span
                          className={`h-2 w-2 shrink-0 rounded-full ${running ? "bg-emerald-500" : "bg-muted-foreground/40"}`}
                          title={running ? "在线" : "离线"}
                        />
                        <p className="truncate text-sm font-medium">{p.name}</p>
                      </div>
                      <p className="truncate text-[11px] text-muted-foreground">
                        {p.default_plugin ? `插件 ${p.default_plugin}` : "默认插件未设置"}
                        {p.default_port ? ` · 端口 ${p.default_port}` : ""}
                      </p>
                      <p className="truncate text-[11px] text-muted-foreground">
                        {latest
                          ? `最近 ${latestText} · ${latest.events?.toLocaleString() ?? 0} events`
                          : "暂无会话"}
                      </p>
                    </div>
                    <button
                      type="button"
                      onClick={() => onOpenProject(p.id)}
                      title="进入项目详情"
                      className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs text-foreground hover:bg-muted/50"
                    >
                      进入
                    </button>
                    <button
                      type="button"
                      onClick={() => onStartProject(p)}
                      title="以该项目开始抓包"
                      className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs text-foreground hover:bg-muted/50"
                    >
                      <Play className="h-3 w-3" />
                      抓包
                    </button>
                    <button
                      type="button"
                      onClick={() => {
                        if (!window.confirm(`删除项目「${p.name}」？`)) return;
                        deleteProject.mutate(
                          { id: p.id },
                          {
                            onSuccess: () => toast.success("已删除", p.name),
                            onError: (err) => toast.error("删除失败", err.message),
                          },
                        );
                      }}
                      title="删除项目"
                      className="rounded-md p-1 text-muted-foreground hover:bg-muted/50 hover:text-destructive"
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </button>
                  </div>
                );
              })
            )}
          </div>
        </section>

        {/* 最近会话 */}
        <section>
          <h2 className="text-sm font-medium text-foreground">最近会话</h2>
          <div className="mt-3 space-y-2">
            {recent.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                还没有抓包会话。点上方「开始一次抓包」开始你的第一次抓包。
              </p>
            ) : (
              recent.map((s) => (
                <button
                  key={s.session_id}
                  type="button"
                  onClick={() => onSelectSession(s.session_id)}
                  className="flex w-full items-center gap-3 rounded-xl border border-border bg-card/60 p-3 text-left transition-colors hover:border-primary/30 hover:bg-muted/40"
                >
                  <span className="relative flex h-2.5 w-2.5 shrink-0">
                    <Dot className="h-4 w-4 text-muted-foreground/0" />
                    <span className={`absolute inset-0 rounded-full ${statusDot(s.status)}`} />
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <p className="truncate text-sm font-medium">
                        {s.plugin || "无插件抓包"}
                      </p>
                      {s.status === "running" && (
                        <span className="rounded bg-emerald-500/10 px-1.5 py-0.5 text-[10px] font-medium text-emerald-600 dark:text-emerald-400">
                          抓包中
                        </span>
                      )}
                    </div>
                    <p className="mt-0.5 truncate text-[11px] text-muted-foreground">
                      {fmtTime(s.started_at)} · {sourceLabel(s.source)}
                      {s.port ? ` · 端口 ${s.port}` : ""}
                    </p>
                  </div>
                  <div className="shrink-0 text-right">
                    <p className="font-mono text-sm text-foreground">
                      {s.events?.toLocaleString() ?? 0}{" "}
                      <span className="text-[10px] text-muted-foreground">events</span>
                    </p>
                    <p className="font-mono text-[11px] text-muted-foreground">
                      {s.raw_packets?.toLocaleString() ?? 0} packets
                    </p>
                  </div>
                  <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
                </button>
              ))
            )}
          </div>
        </section>
      </div>
    </div>
  );
}