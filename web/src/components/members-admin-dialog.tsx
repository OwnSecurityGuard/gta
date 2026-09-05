import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Dialog } from "@/components/ui/dialog";
import {
  Copy,
  Check,
  Users,
  UserPlus,
  Trash2,
  KeyRound,
  ShieldCheck,
} from "lucide-react";
import {
  useCreateAccessCode,
  useListUsers,
  useRevokeUser,
} from "@/hooks/use-mcp";
import { toast } from "@/components/ui/toast";
import { getIdentity } from "@/lib/auth";
import type { CreateAccessCodeResult, GtaUser } from "@/types/access-code";

/** 与后端 validOwnerName 同规则的即时预校验。 */
const NAME_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/;

function formatTime(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleString();
}

/**
 * 「成员管理」对话框：新成员加入的两条路径 + 成员账号列表。
 *
 *  - 自助注册（主路径）：对方在「设置 → 快速开始」起个用户名即可，无需任何操作；
 *  - 邀请码（辅助）：为指定用户名生成一次性码，对方 curl claim 直接拿 token，
 *    免去自己注册一步；同名已存在则认领失败。
 *
 * 成员账号列表区分来源：env bootstrap（本机配置，不可撤销）/
 * 自助注册（created_by 为空）/ 邀请（created_by 为邀请人）。
 */
export function MembersAdminDialog({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}) {
  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Users className="h-5 w-5" />}
      title="成员管理"
      footer={
        <Button variant="outline" onClick={onClose}>
          关闭
        </Button>
      }
    >
      <div className="space-y-5">
        <InviteCodeSection />
        <AccountsSection />
      </div>
    </Dialog>
  );
}

/** 邀请码：为还没注册的新成员预创建身份，对方 claim 后直接拿到 token。 */
function InviteCodeSection() {
  const createCode = useCreateAccessCode();
  const [newOwner, setNewOwner] = useState("");
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<CreateAccessCodeResult | null>(null);
  const [copiedField, setCopiedField] = useState<"code" | "cmd" | null>(null);

  async function handleGenerate() {
    const name = newOwner.trim();
    if (!NAME_RE.test(name)) {
      toast.error("请填写有效的新成员用户名", "字母或数字开头，可含 . _ -，最长 64 字符");
      return;
    }
    setBusy(true);
    try {
      const res = await createCode.mutateAsync({ newOwner: name });
      setCreated(res);
      toast.success("邀请码已生成", `把码交给 ${res.new_owner}，认领后将创建其独立身份`);
    } catch (e) {
      toast.error("生成失败", e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function copyText(field: "code" | "cmd", text: string) {
    try {
      await navigator.clipboard.writeText(text);
      setCopiedField(field);
      toast.success("已复制");
      setTimeout(() => setCopiedField(null), 1500);
    } catch {
      toast.error("复制失败", "请手动选择复制");
    }
  }

  const base = typeof window !== "undefined" ? window.location.origin : "";
  const claimCmd = created ? `curl -fsSL "${base}/access/claim?code=${created.code}"` : "";

  return (
    <section>
      <label className="flex items-center gap-1.5 text-sm font-medium">
        <UserPlus className="h-3.5 w-3.5 text-muted-foreground" />
        邀请新成员（可选）
      </label>
      <p className="mt-1 text-xs text-muted-foreground">
        对方也可以直接在「设置 → 没有令牌？快速开始」自助注册同名账号，效果相同；
        邀请码只是替对方省掉这一步——生成一次性码，对方执行一条命令即拿到 token。
      </p>
      <div className="mt-2 flex gap-2">
        <Input
          value={newOwner}
          onChange={(e) => setNewOwner(e.target.value)}
          placeholder="新成员用户名，如 carol"
          aria-label="新成员用户名"
          className="font-mono"
          onKeyDown={(e) => {
            if (e.key === "Enter" && !busy) void handleGenerate();
          }}
        />
        <Button variant="outline" onClick={() => void handleGenerate()} disabled={busy}>
          {busy ? "生成中…" : "生成邀请码"}
        </Button>
      </div>
      {created && (
        <div className="mt-3 space-y-2.5 rounded-md border border-border bg-muted/30 p-3">
          <div>
            <div className="flex items-center gap-2">
              <span className="text-sm font-medium">
                邀请码（为 <span className="font-mono">{created.new_owner}</span> 创建身份，一次性，24 小时有效）
              </span>
              <Button
                variant="outline"
                size="sm"
                className="ml-auto"
                onClick={() => copyText("code", created.code)}
              >
                {copiedField === "code" ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                复制
              </Button>
            </div>
            <code className="mt-2 block rounded-md border border-border bg-background px-3 py-2 font-mono text-lg tracking-widest">
              {created.code}
            </code>
          </div>
          <div>
            <div className="flex items-center gap-2">
              <span className="text-sm font-medium">新成员执行以下命令认领身份</span>
              <Button
                variant="outline"
                size="sm"
                className="ml-auto"
                onClick={() => copyText("cmd", claimCmd)}
              >
                {copiedField === "cmd" ? <Check className="h-4 w-4" /> : <Copy className="h-4 w-4" />}
                复制命令
              </Button>
            </div>
            <pre className="mt-2 overflow-auto whitespace-pre-wrap rounded-md border border-border bg-background p-3 font-mono text-[11.5px] leading-relaxed text-foreground">
              {claimCmd}
            </pre>
            <p className="mt-1.5 text-xs text-muted-foreground">
              认领成功后返回的 JSON 中 <code className="font-mono">token</code>{" "}
              字段即为新成员的独立凭证，请提醒其妥善保存（仅此一次展示）。
            </p>
          </div>
        </div>
      )}
    </section>
  );
}

/** 成员账号列表：bootstrap（只读）+ users 表（自助注册 / 邀请），admin 可撤销。 */
function AccountsSection() {
  const { data, error, isLoading } = useListUsers();
  const revokeUser = useRevokeUser();
  const [busy, setBusy] = useState<string | null>(null);

  // 非 admin：list_users 返回 ok=false → callTool 抛错 → query error。静默隐藏。
  if (error) return null;
  const users = data?.users ?? [];
  const bootstrap = data?.bootstrap_owners ?? [];
  const self = getIdentity()?.owner ?? "";

  async function handleRevoke(u: GtaUser) {
    if (!window.confirm(`撤销用户 ${u.owner}？其 token 将立即失效，且无法恢复。`)) return;
    setBusy(u.owner);
    try {
      await revokeUser.mutateAsync({ owner: u.owner });
      toast.success("已撤销", `${u.owner} 的 token 已失效`);
    } catch (e) {
      toast.error("撤销失败", e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  }

  return (
    <section>
      <label className="flex items-center gap-1.5 text-sm font-medium">
        <KeyRound className="h-3.5 w-3.5 text-muted-foreground" />
        成员账号（{users.length + bootstrap.length}）
      </label>
      {isLoading ? (
        <p className="mt-2 text-sm text-muted-foreground">加载中…</p>
      ) : (
        <div className="mt-2 divide-y rounded-md border border-border">
          {bootstrap.map((owner) => (
            <div key={owner} className="flex items-center gap-2 px-3 py-2 text-sm">
              <span className="font-mono">{owner}</span>
              <span className="rounded bg-primary/10 px-1.5 py-0.5 text-[11px] text-primary">admin</span>
              <span className="rounded bg-muted px-1.5 py-0.5 text-[11px] text-muted-foreground">
                本机配置（GT_AUTH_TOKENS）
              </span>
              <span className="ml-auto text-[11px] text-muted-foreground">不可撤销</span>
            </div>
          ))}
          {users.map((u) => (
            <div key={u.owner} className="flex items-center gap-2 px-3 py-2 text-sm">
              <span className="font-mono">{u.owner}</span>
              {u.is_admin && (
                <span className="flex items-center gap-0.5 rounded bg-primary/10 px-1.5 py-0.5 text-[11px] text-primary">
                  <ShieldCheck className="h-3 w-3" />
                  admin
                </span>
              )}
              <span className="rounded bg-muted px-1.5 py-0.5 text-[11px] text-muted-foreground">
                {u.created_by ? `由 ${u.created_by} 邀请` : "自助注册"}
              </span>
              <span className="ml-auto text-[11px] text-muted-foreground">{formatTime(u.created_at)}</span>
              {u.owner !== self && (
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy === u.owner}
                  onClick={() => handleRevoke(u)}
                >
                  <Trash2 className="h-3.5 w-3.5" />
                  {busy === u.owner ? "撤销中…" : "撤销"}
                </Button>
              )}
            </div>
          ))}
        </div>
      )}
      <p className="mt-1.5 text-xs text-muted-foreground">
        撤销仅使其 token 立即失效；该用户名随即释放，可被重新注册（项目归属按用户名自动恢复）。
      </p>
    </section>
  );
}
