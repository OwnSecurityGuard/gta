import { type ChangeEvent, type KeyboardEvent, type Ref, useEffect, useMemo, useRef, useState } from "react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { useCaptureSchema } from "@/hooks/use-mcp";
import { Search, X } from "lucide-react";

interface FilterBarProps {
  sessionId: string | null;
  filter: string;
  onFilterChange: (filter: string) => void;
  /** 由父组件传入，用于 "/" 快捷键聚焦输入框 */
  inputRef?: Ref<HTMLInputElement>;
}

export function FilterBar({ sessionId, filter, onFilterChange, inputRef }: FilterBarProps) {
  const { data: schemaData } = useCaptureSchema(sessionId);
  const [local, setLocal] = useState(filter);
  const debounceRef = useRef<number | null>(null);

  // 外部 filter 变化（切换会话清空、快捷标签）同步到本地输入
  useEffect(() => {
    setLocal(filter);
  }, [filter]);

  // 本地输入变化后防抖提交（300ms），避免每次按键都触发后端查询
  useEffect(() => {
    if (local === filter) return;
    if (debounceRef.current) window.clearTimeout(debounceRef.current);
    debounceRef.current = window.setTimeout(() => {
      onFilterChange(local);
    }, 300);
    return () => {
      if (debounceRef.current) window.clearTimeout(debounceRef.current);
    };
  }, [local, filter, onFilterChange]);

  /** 从 schema 中提取 data.* 字段用于快捷标签 */
  const dataFields = useMemo(() => {
    if (!schemaData?.query_fields) return [];
    return schemaData.query_fields
      .filter((f) => f.name.startsWith("data."))
      .map((f) => f.name);
  }, [schemaData]);

  function flush() {
    if (debounceRef.current) window.clearTimeout(debounceRef.current);
    onFilterChange(local);
  }

  function handleKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter") {
      flush();
    } else if (e.key === "Escape") {
      if (debounceRef.current) window.clearTimeout(debounceRef.current);
      setLocal("");
      onFilterChange("");
    }
  }

  function handleInputChange(e: ChangeEvent<HTMLInputElement>) {
    setLocal(e.target.value);
  }

  function handleQuickFilter(field: string) {
    const value = field.includes("method") ? '"GET"' : '""';
    const newFilter = `${field} == ${value}`;
    if (debounceRef.current) window.clearTimeout(debounceRef.current);
    setLocal(newFilter);
    onFilterChange(newFilter);
  }

  function handleClear() {
    if (debounceRef.current) window.clearTimeout(debounceRef.current);
    setLocal("");
    onFilterChange("");
  }

  return (
    <div className="space-y-2.5">
      {/* 输入行 */}
      <div className="flex items-center gap-2">
        <div className="relative flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            ref={inputRef}
            value={local}
            onChange={handleInputChange}
            onKeyDown={handleKeyDown}
            aria-label="筛选表达式"
            placeholder='输入筛选表达式，如 data.method == "GET"（按 / 聚焦，Esc 清除）'
            className="pl-9 font-mono"
          />
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={handleClear}
          disabled={!local}
          aria-label="清除筛选"
        >
          <X className="h-3 w-3" />
          清除
        </Button>
      </div>

      {/* 快捷标签 */}
      {dataFields.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="text-xs text-muted-foreground">快捷字段：</span>
          {dataFields.map((field) => (
            <button
              key={field}
              type="button"
              onClick={() => handleQuickFilter(field)}
              className="cursor-pointer rounded-md border border-border bg-background px-2 py-0.5 font-mono text-xs text-muted-foreground transition-colors hover:border-primary/40 hover:bg-primary-muted hover:text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40"
            >
              {field}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
