import { useEffect, useState } from "react";
import { CheckCircle2, AlertCircle, Info, X } from "lucide-react";
import { cn } from "@/lib/utils";

type ToastKind = "success" | "error" | "info";

interface ToastItem {
  id: number;
  kind: ToastKind;
  title: string;
  description?: string;
}

type Listener = (items: ToastItem[]) => void;

// 单例事件总线：任何组件（含非 React 代码）都能直接调用 toast.xxx，无需 Provider。
let items: ToastItem[] = [];
const listeners = new Set<Listener>();
let nextId = 1;

function emit() {
  const snapshot = [...items];
  listeners.forEach((l) => l(snapshot));
}

function push(kind: ToastKind, title: string, description?: string) {
  const id = nextId++;
  items = [...items, { id, kind, title, description }];
  emit();
  const ttl = kind === "error" ? 6000 : 3500;
  window.setTimeout(() => dismiss(id), ttl);
}

function dismiss(id: number) {
  const before = items.length;
  items = items.filter((t) => t.id !== id);
  if (before !== items.length) emit();
}

export const toast = {
  success: (title: string, description?: string) => push("success", title, description),
  error: (title: string, description?: string) => push("error", title, description),
  info: (title: string, description?: string) => push("info", title, description),
};

/** 全局通知视口：固定在右下角，可访问（aria-live，屏幕阅读器自动播报新增项）。 */
export function ToastViewport() {
  const [list, setList] = useState<ToastItem[]>(items);

  useEffect(() => {
    listeners.add(setList);
    return () => {
      listeners.delete(setList);
    };
  }, []);

  return (
    <div className="pointer-events-none fixed inset-x-0 bottom-4 z-[60] flex flex-col items-center gap-2 px-4 sm:items-end sm:pr-6">
      <div
        className="flex w-full max-w-sm flex-col gap-2"
        role="status"
        aria-live="polite"
        aria-relevant="additions"
      >
        {list.map((t) => (
          <ToastCard key={t.id} item={t} onDismiss={() => dismiss(t.id)} />
        ))}
      </div>
    </div>
  );
}

function ToastCard({ item, onDismiss }: { item: ToastItem; onDismiss: () => void }) {
  const Icon =
    item.kind === "success" ? CheckCircle2 : item.kind === "error" ? AlertCircle : Info;
  const tone =
    item.kind === "success"
      ? "text-success"
      : item.kind === "error"
        ? "text-destructive"
        : "text-info";

  return (
    <div className="gt-toast-in pointer-events-auto flex items-start gap-2.5 rounded-lg border border-border bg-popover px-3.5 py-3 text-popover-foreground shadow-lg">
      <Icon className={cn("mt-0.5 h-4 w-4 shrink-0", tone)} />
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium leading-snug">{item.title}</p>
        {item.description && (
          <p className="mt-0.5 text-xs leading-relaxed text-muted-foreground">{item.description}</p>
        )}
      </div>
      <button
        type="button"
        aria-label="关闭通知"
        onClick={onDismiss}
        className="-mr-1 -mt-1 flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
      >
        <X className="h-3.5 w-3.5" />
      </button>
    </div>
  );
}
