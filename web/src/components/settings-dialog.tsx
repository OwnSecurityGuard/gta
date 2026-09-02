import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { mcpClient } from "@/lib/mcp-client";
import { getToken, setToken } from "@/lib/auth";
import { Settings, Check, X, KeyRound } from "lucide-react";

interface SettingsDialogProps {
  open: boolean;
  onClose: () => void;
}

export function SettingsDialog({ open, onClose }: SettingsDialogProps) {
  const [url, setUrl] = useState(mcpClient.getBaseUrl());
  // 空输入 = 清除 token（回到匿名模式）。
  const [token, setTokenInput] = useState(getToken() ?? "");
  const [saved, setSaved] = useState(false);
  const queryClient = useQueryClient();

  // 关闭时组件仍挂载（Dialog 渲染 null），useState 初值只在首次执行；
  // 每次打开时重新同步本地状态，保证「取消」能丢弃未保存的半截输入。
  useEffect(() => {
    if (open) {
      setUrl(mcpClient.getBaseUrl());
      setTokenInput(getToken() ?? "");
      setSaved(false);
    }
  }, [open]);

  function handleSave() {
    mcpClient.setBaseUrl(url);
    setToken(token || null);
    // 凭证/地址可能变了：立即重刷全部查询，让身份回显与会话列表马上生效。
    void queryClient.invalidateQueries();
    setSaved(true);
    setTimeout(() => {
      setSaved(false);
      onClose();
    }, 800);
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
          <Input
            type="password"
            value={token}
            onChange={(e) => setTokenInput(e.target.value)}
            aria-label="访问令牌"
            placeholder="服务器未开启令牌校验时留空"
            autoComplete="new-password"
            className="mt-1.5 font-mono"
          />
          <p className="mt-1 text-xs text-muted-foreground">
            团队共享服务端开启令牌校验时，向管理员领取你的 token（形如
            <code className="font-mono"> gta_…</code>）填入；留空则按匿名/单机模式访问。保存后立即生效。令牌仅保存在本机浏览器（localStorage），不会上传到其他设备。
          </p>
        </div>
      </div>
    </Dialog>
  );
}
