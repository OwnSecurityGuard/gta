import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { mcpClient } from "@/lib/mcp-client";
import { getToken, setToken } from "@/lib/auth";
import { Settings, Check, X, KeyRound, UserPlus, Eye, EyeOff } from "lucide-react";

interface SettingsDialogProps {
  open: boolean;
  onClose: () => void;
}

/** 与后端 validOwnerName 同规则的即时预校验。 */
const NAME_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/;

export function SettingsDialog({ open, onClose }: SettingsDialogProps) {
  const [url, setUrl] = useState(mcpClient.getBaseUrl());
  // 空输入 = 清除 token（回到匿名模式）。
  const [token, setTokenInput] = useState(getToken() ?? "");
  // 已保存 token 的明文查看开关：token 本就明文存在本机 localStorage，
  // 支持查看是为了退出/换浏览器前能复制带走，避免「只显示一次」把人锁死。
  const [showToken, setShowToken] = useState(false);
  const [saved, setSaved] = useState(false);
  const queryClient = useQueryClient();

  // 自助注册（新用户免邀请获取身份）
  const [regName, setRegName] = useState("");
  const [regBusy, setRegBusy] = useState(false);
  const [regError, setRegError] = useState("");
  const [regDone, setRegDone] = useState("");

  // 关闭时组件仍挂载（Dialog 渲染 null），useState 初值只在首次执行；
  // 每次打开时重新同步本地状态，保证「取消」能丢弃未保存的半截输入。
  useEffect(() => {
    if (open) {
      setUrl(mcpClient.getBaseUrl());
      setTokenInput(getToken() ?? "");
      setShowToken(false);
      setSaved(false);
      setRegName("");
      setRegError("");
      setRegDone("");
    }
  }, [open]);

  function applyToken(t: string) {
    setTokenInput(t);
    // 空串语义 = 清除凭证（回到匿名模式），与原保存逻辑一致。
    setToken(t || null);
    // 凭证/地址可能变了：立即重刷全部查询，让身份回显与会话列表马上生效。
    void queryClient.invalidateQueries();
  }

  function handleSave() {
    mcpClient.setBaseUrl(url);
    applyToken(token || "");
    setSaved(true);
    setTimeout(() => {
      setSaved(false);
      onClose();
    }, 800);
  }

  async function handleRegister() {
    const name = regName.trim();
    if (!NAME_RE.test(name)) {
      setRegError("用户名格式：字母或数字开头，可含 . _ -，最长 64 字符");
      return;
    }
    setRegBusy(true);
    setRegError("");
    try {
      const { owner, token: newToken } = await mcpClient.register(name);
      applyToken(newToken);
      setRegDone(`身份 ${owner} 已创建并自动保存凭证`);
      setTimeout(() => {
        setRegDone("");
        onClose();
      }, 1600);
    } catch (e) {
      setRegError(e instanceof Error ? e.message : String(e));
    } finally {
      setRegBusy(false);
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      icon={<Settings className="h-5 w-5" />}
      title="设置"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            <X className="h-4 w-4" />
            取消
          </Button>
          <Button onClick={handleSave}>
            {saved ? (
              <>
                <Check className="h-4 w-4" />
                已保存
              </>
            ) : (
              "保存"
            )}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div>
          <label className="text-sm font-medium">MCP Server 地址</label>
          <Input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            aria-label="MCP Server 地址"
            placeholder="/mcp（通过 Vite 代理）或 http://其他地址/mcp"
            className="mt-1.5 font-mono"
          />
          <p className="mt-1 text-xs text-muted-foreground">
            默认 /mcp 通过 Vite dev server 代理到 localhost:8781。如需直连其他地址则填完整 URL。
          </p>
        </div>
        <div>
          <label className="flex items-center gap-1.5 text-sm font-medium">
            <KeyRound className="h-3.5 w-3.5 text-muted-foreground" />
            访问令牌（可选）
          </label>
          <div className="relative mt-1.5">
            <Input
              type={showToken ? "text" : "password"}
              value={token}
              onChange={(e) => setTokenInput(e.target.value)}
              aria-label="访问令牌"
              placeholder="服务器未开启令牌校验时留空"
              autoComplete="new-password"
              className="pr-9 font-mono"
            />
            {token && (
              <button
                type="button"
                onClick={() => setShowToken((v) => !v)}
                className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-1 text-muted-foreground hover:text-foreground"
                aria-label={showToken ? "隐藏令牌" : "查看令牌"}
                title={showToken ? "隐藏令牌" : "查看令牌（可复制备份）"}
              >
                {showToken ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
              </button>
            )}
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            团队共享服务端开启令牌校验时，向管理员领取你的 token（形如
            <code className="font-mono"> gt_…</code>）填入；留空则按匿名/单机模式访问。保存后立即生效。令牌仅保存在本机浏览器（localStorage），不会上传到其他设备。点右侧眼睛可查看已保存的令牌——清除前建议先复制备份，否则只能找管理员重置。
          </p>
        </div>

        <div className="rounded-md border border-dashed border-border p-3">
          <label className="flex items-center gap-1.5 text-sm font-medium">
            <UserPlus className="h-3.5 w-3.5 text-muted-foreground" />
            没有令牌？快速开始
          </label>
          <div className="mt-1.5 flex gap-2">
            <Input
              value={regName}
              onChange={(e) => setRegName(e.target.value)}
              aria-label="新用户名"
              placeholder="起一个用户名，如 carol"
              className="font-mono"
              onKeyDown={(e) => {
                if (e.key === "Enter" && !regBusy) void handleRegister();
              }}
            />
            <Button variant="outline" onClick={() => void handleRegister()} disabled={regBusy || !!regDone}>
              {regBusy ? "创建中…" : "创建我的身份"}
            </Button>
          </div>
          {regError && <p className="mt-1.5 text-xs text-destructive">{regError}</p>}
          {regDone && (
            <p className="mt-1.5 text-xs text-primary">
              {regDone}——之后即可创建自己的项目；用别人项目的解码插件需要该项目把你加为成员。
            </p>
          )}
          <p className="mt-1.5 text-xs text-muted-foreground">
            注册即获得个人独立身份（token 自动保存在本机）。项目创建人人可用；项目插件仅项目成员可解析。
          </p>
        </div>
      </div>
    </Dialog>
  );
}
