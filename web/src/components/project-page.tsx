// ProjectPage — 「项目详情」页（项目作为一等组织单元）。
//
// 展示项目身份（名称/game/创建者/描述）、运行状态（派生自最近会话）、成员、
// 解码插件与规则（关联 chips，管理员可增删），以及最近会话入口。不做复杂管理后台，
// 仅提供轻量的关联 chips + 增删表单 + 一键抓包。
import { useState } from "react";
import { ArrowLeft, Play, Plus, X } from "lucide-react";
import {
  useProject,
  useAddProjectMember,
  useRemoveProjectMember,
  useSetProjectPlugins,
  useSetProjectRules,
} from "@/hooks/use-mcp";
import { useIdentity } from "@/hooks/use-auth";
import { toast } from "@/components/ui/toast";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/ui/empty-state";
import { Badge } from "@/components/ui/badge";
import type { ProjectDetail, ProjectRecentSession, ProjectRole } from "@/types/project";

interface ProjectPageProps {
  projectId: string;
  onBack: () => void;
  onSelectSession: (sessionId: string) => void;
  /** 从项目一键抓包 */
  onStartProject: (p: { id: string; name: string }) => void;
}

function statusDot(status?: string) {
  return status === "running" ? "bg-emerald-500" : "bg-muted-foreground/40";
}

function fmtTime(iso?: string) {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  return `${d.getMonth() + 1}月${d.getDate()}日 ${hh}:${mm}`;
}

function uid(): string {
  // 给新增的插件/规则一行分配一个前端唯一 id，兼做 React key。
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) return crypto.randomUUID();
  return `id-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
}

export function ProjectPage({
  projectId,
  onBack,
  onSelectSession,
  onStartProject,
}: ProjectPageProps) {
  const { data, isLoading, isError } = useProject(projectId);
  const project: ProjectDetail | undefined = data?.project;
  const recentSessions: ProjectRecentSession[] =
    data?.recent_sessions ?? project?.recent_sessions ?? [];

  // 管理员判权：项目创建者本人 或 全局管理员。
  const identity = useIdentity();
  const isProjectAdmin =
    (project?.created_by != null && project.created_by === identity?.owner) ||
    identity?.isAdmin === true;

  // —— 增删成员 / 插件 / 规则 ——
  const addMember = useAddProjectMember(projectId);
  const removeMember = useRemoveProjectMember(projectId);
  const setPlugins = useSetProjectPlugins(projectId);
  const setRules = useSetProjectRules(projectId);

  const [memberUser, setMemberUser] = useState("");
  const [memberRole, setMemberRole] = useState<ProjectRole>("member");
  const [pluginName, setPluginName] = useState("");
  const [ruleName, setRuleName] = useState("");

  const members = project?.members ?? [];
  const plugins = project?.plugins ?? [];
  const rules = project?.rules ?? [];
  const running = recentSessions.some((s) => s.status === "running");

  function handleAddMember() {
    const user = memberUser.trim();
    if (!user) return;
    addMember.mutate(
      { user, role: memberRole },
      {
        onSuccess: () => {
          setMemberUser("");
          toast.success("已添加成员", user);
        },
        onError: (err) => toast.error("添加失败", err.message),
      },
    );
  }

  function handleRemoveMember(user: string) {
    removeMember.mutate(
      { user },
      {
        onSuccess: () => toast.success("已移除成员", user),
        onError: (err) => toast.error("移除失败", err.message),
      },
    );
  }

  function handleAddPlugin() {
    const name = pluginName.trim();
    if (!name) return;
    setPlugins.mutate(
      { plugins: [...plugins, { id: uid(), name }] },
      {
        onSuccess: () => {
          setPluginName("");
          toast.success("已添加插件", name);
        },
        onError: (err) => toast.error("添加失败", err.message),
      },
    );
  }

  function handleRemovePlugin(id: string) {
    setPlugins.mutate(
      { plugins: plugins.filter((pl) => pl.id !== id) },
      {
        onError: (err) => toast.error("移除失败", err.message),
      },
    );
  }

  function handleAddRule() {
    const name = ruleName.trim();
    if (!name) return;
    setRules.mutate(
      { rules: [...rules, { id: uid(), name }] },
      {
        onSuccess: () => {
          setRuleName("");
          toast.success("已添加规则", name);
        },
        onError: (err) => toast.error("添加失败", err.message),
      },
    );
  }

  function handleRemoveRule(id: string) {
    setRules.mutate(
      { rules: rules.filter((r) => r.id !== id) },
      {
        onError: (err) => toast.error("移除失败", err.message),
      },
    );
  }

  // —— 加载中 / 未找到 ——
  if (isLoading) {
    return (
      <div className="mx-auto max-w-4xl space-y-6 p-6">
        <Skeleton className="h-6 w-40" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  if (isError || !project) {
    return (
      <div className="mx-auto max-w-4xl p-6">
        <EmptyState
          icon={<Play className="h-5 w-5" />}
          title={isError ? "项目加载失败" : "项目不存在"}
          hint={isError ? data?.error ?? "无法加载项目详情，请稍后重试。" : "该项目可能已被删除，或你暂无访问权限。"}
          action={
            <Button variant="outline" onClick={onBack}>
              <ArrowLeft className="h-4 w-4" />
              返回
            </Button>
          }
        />
      </div>
    );
  }

  return (
    <div className="h-full overflow-auto gta-scroll">
      <div className="mx-auto max-w-4xl space-y-6 p-6">
        {/* 返回 + 头部 */}
        <button
          type="button"
          onClick={onBack}
          className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
        >
          <ArrowLeft className="h-3.5 w-3.5" />
          返回我的抓包
        </button>

        <header className="rounded-2xl border border-border bg-card/60 p-5">
          <div className="flex items-start justify-between gap-4">
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-2">
                <h1 className="text-lg font-semibold gta-gradient-text">{project.name}</h1>
                {project.game && <Badge variant="outline">{project.game}</Badge>}
                <Badge variant="secondary">
                  <span className={`mr-1 h-1.5 w-1.5 rounded-full ${statusDot(running ? "running" : undefined)}`} />
                  {running ? "在线" : "离线"}
                </Badge>
              </div>
              {project.description && (
                <p className="mt-1.5 text-sm text-muted-foreground">{project.description}</p>
              )}
              <p className="mt-2 text-[11px] text-muted-foreground">
                {project.created_by ? `由 ${project.created_by} 创建` : "匿名创建"} ·{" "}
                {project.default_port ? `端口 ${project.default_port}` : "未设默认端口"}
                {isProjectAdmin ? " · 你是管理员" : ""}
              </p>
            </div>
            <Button
              variant="default"
              size="sm"
              className="h-8 shrink-0"
              onClick={() => onStartProject({ id: project.id, name: project.name })}
              title="以该项目开始抓包"
            >
              <Play className="h-4 w-4" />
              开始抓包
            </Button>
          </div>
        </header>

        {/* 成员 */}
        <section>
          <h2 className="text-sm font-medium text-foreground">成员</h2>
          <div className="mt-2 rounded-2xl border border-border bg-card/60 p-4">
            {members.length === 0 ? (
              <p className="text-sm text-muted-foreground">暂无成员。</p>
            ) : (
              <ul className="space-y-1.5">
                {members.map((m) => {
                  const isOwner = project.created_by === m.user;
                  return (
                    <li
                      key={m.user}
                      className="flex items-center justify-between gap-2 rounded-lg px-2 py-1.5 hover:bg-muted/40"
                    >
                      <div className="flex min-w-0 items-center gap-2">
                        <span className="truncate text-sm">{m.user}</span>
                        <Badge variant={isOwner ? "default" : "outline"}>
                          {isOwner ? "创建者" : m.role === "admin" ? "管理员" : "成员"}
                        </Badge>
                      </div>
                      {isProjectAdmin && !isOwner && (
                        <button
                          type="button"
                          onClick={() => handleRemoveMember(m.user)}
                          title="移除成员"
                          className="rounded-md p-1 text-muted-foreground hover:bg-muted/50 hover:text-destructive"
                        >
                          <X className="h-3.5 w-3.5" />
                        </button>
                      )}
                    </li>
                  );
                })}
              </ul>
            )}

            {isProjectAdmin && (
              <div className="mt-3 grid gap-2 border-t border-border pt-3 sm:grid-cols-[1fr_120px_auto]">
                <input
                  value={memberUser}
                  onChange={(e) => setMemberUser(e.target.value)}
                  placeholder="用户名"
                  className="h-9 rounded-md border border-input bg-background px-2.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                />
                <select
                  value={memberRole}
                  onChange={(e) => setMemberRole(e.target.value as ProjectRole)}
                  className="h-9 rounded-md border border-input bg-background px-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                >
                  <option value="member">成员</option>
                  <option value="admin">管理员</option>
                </select>
                <button
                  type="button"
                  onClick={handleAddMember}
                  disabled={addMember.isPending || !memberUser.trim()}
                  className="inline-flex h-9 items-center justify-center gap-1 rounded-md bg-primary px-3 text-sm font-medium text-primary-foreground disabled:opacity-50"
                >
                  <Plus className="h-3.5 w-3.5" />
                  添加
                </button>
              </div>
            )}
          </div>
        </section>

        {/* 解码插件 */}
        <section>
          <h2 className="text-sm font-medium text-foreground">解码插件</h2>
          <div className="mt-2 rounded-2xl border border-border bg-card/60 p-4">
            {plugins.length === 0 ? (
              <p className="text-sm text-muted-foreground">未配置解码插件。</p>
            ) : (
              <div className="flex flex-wrap gap-1.5">
                {plugins.map((pl) => (
                  <span
                    key={pl.id}
                    className="inline-flex items-center gap-1 rounded-md border border-border bg-muted px-2 py-0.5 text-xs text-foreground"
                  >
                    {pl.name}
                    {isProjectAdmin && (
                      <button
                        type="button"
                        onClick={() => handleRemovePlugin(pl.id)}
                        title="移除插件"
                        className="text-muted-foreground hover:text-destructive"
                      >
                        <X className="h-3 w-3" />
                      </button>
                    )}
                  </span>
                ))}
              </div>
            )}

            {isProjectAdmin && (
              <div className="mt-3 grid gap-2 border-t border-border pt-3 sm:grid-cols-[1fr_auto]">
                <input
                  value={pluginName}
                  onChange={(e) => setPluginName(e.target.value)}
                  placeholder="插件名称"
                  className="h-9 rounded-md border border-input bg-background px-2.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                />
                <button
                  type="button"
                  onClick={handleAddPlugin}
                  disabled={setPlugins.isPending || !pluginName.trim()}
                  className="inline-flex h-9 items-center justify-center gap-1 rounded-md bg-primary px-3 text-sm font-medium text-primary-foreground disabled:opacity-50"
                >
                  <Plus className="h-3.5 w-3.5" />
                  添加
                </button>
              </div>
            )}
          </div>
        </section>

        {/* 规则 */}
        <section>
          <h2 className="text-sm font-medium text-foreground">规则</h2>
          <div className="mt-2 rounded-2xl border border-border bg-card/60 p-4">
            {rules.length === 0 ? (
              <p className="text-sm text-muted-foreground">未配置规则。</p>
            ) : (
              <div className="flex flex-wrap gap-1.5">
                {rules.map((r) => (
                  <span
                    key={r.id}
                    className="inline-flex items-center gap-1 rounded-md border border-border bg-muted px-2 py-0.5 text-xs text-foreground"
                  >
                    {r.name}
                    {isProjectAdmin && (
                      <button
                        type="button"
                        onClick={() => handleRemoveRule(r.id)}
                        title="移除规则"
                        className="text-muted-foreground hover:text-destructive"
                      >
                        <X className="h-3 w-3" />
                      </button>
                    )}
                  </span>
                ))}
              </div>
            )}

            {isProjectAdmin && (
              <div className="mt-3 grid gap-2 border-t border-border pt-3 sm:grid-cols-[1fr_auto]">
                <input
                  value={ruleName}
                  onChange={(e) => setRuleName(e.target.value)}
                  placeholder="规则名称"
                  className="h-9 rounded-md border border-input bg-background px-2.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
                />
                <button
                  type="button"
                  onClick={handleAddRule}
                  disabled={setRules.isPending || !ruleName.trim()}
                  className="inline-flex h-9 items-center justify-center gap-1 rounded-md bg-primary px-3 text-sm font-medium text-primary-foreground disabled:opacity-50"
                >
                  <Plus className="h-3.5 w-3.5" />
                  添加
                </button>
              </div>
            )}
          </div>
        </section>

        {/* 最近会话 */}
        <section>
          <h2 className="text-sm font-medium text-foreground">最近会话</h2>
          <div className="mt-2 rounded-2xl border border-border bg-card/60 p-4">
            {recentSessions.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                该项目还没有会话。点右上角「开始抓包」发起第一次抓包。
              </p>
            ) : (
              <ul className="space-y-1.5">
                {recentSessions.map((s) => (
                  <li key={s.session_id}>
                    <button
                      type="button"
                      onClick={() => onSelectSession(s.session_id)}
                      className="flex w-full items-center gap-3 rounded-lg px-2 py-1.5 text-left hover:bg-muted/40"
                    >
                      <span
                        className={`h-2 w-2 shrink-0 rounded-full ${statusDot(s.status)}`}
                        title={s.status === "running" ? "抓包中" : "已停止"}
                      />
                      <div className="min-w-0 flex-1">
                        <p className="truncate font-mono text-xs text-foreground">
                          {s.session_id}
                        </p>
                        <p className="truncate text-[11px] text-muted-foreground">
                          {s.status === "running" ? "抓包中" : "已停止"} · {fmtTime(s.started_at)}
                        </p>
                      </div>
                      <div className="shrink-0 text-right">
                        <p className="font-mono text-xs text-foreground">
                          {s.events?.toLocaleString() ?? 0}{" "}
                          <span className="text-[10px] text-muted-foreground">events</span>
                        </p>
                        <p className="font-mono text-[11px] text-muted-foreground">
                          {s.raw_packets?.toLocaleString() ?? 0} packets
                        </p>
                      </div>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </section>
      </div>
    </div>
  );
}