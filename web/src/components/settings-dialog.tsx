import { useState } from "react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Dialog } from "@/components/ui/dialog";
import { mcpClient } from "@/lib/mcp-client";
import { Settings, Check, X } from "lucide-react";

interface SettingsDialogProps {
  open: boolean;
  onClose: () => void;
}

export function SettingsDialog({ open, onClose }: SettingsDialogProps) {
  const [url, setUrl] = useState(mcpClient.getBaseUrl());
  const [saved, setSaved] = useState(false);

  function handleSave() {
    mcpClient.setBaseUrl(url);
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
          默认 /mcp 通过 Vite dev server 代理到 localhost:8087。如需直连其他地址则填完整 URL。
        </p>
      </div>
    </Dialog>
  );
}
