"""
Game Traffic Analysis Python API
提供脚本访问游戏流量数据的接口
"""

import json
import os
import sys
from typing import Any, Dict, List, Optional
from datetime import datetime


def query_events(
    filter: str = "",
    limit: int = 100,
    offset: int = 0,
    session_id: str = ""
) -> Dict[str, Any]:
    """
    查询解码事件

    Args:
        filter: expr 表达式过滤条件，如 'data.entity == "buff" && data.hp > 5'
        limit: 返回最大数量
        offset: 偏移量
        session_id: 会话 ID（可选）

    Returns:
        包含 total_matched, count, events 的字典
    """
    # 通过 HTTP 调用 MCP 接口
    import urllib.request
    import urllib.parse

    mcp_url = os.environ.get("GTA_MCP_URL", "http://localhost:8090/mcp")

    params = {
        "limit": limit,
        "offset": offset,
    }
    if filter:
        params["filter"] = filter
    if session_id:
        params["session_id"] = session_id

    # 构建 JSON-RPC 请求
    request = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {
            "name": "list_decoded_data",
            "arguments": params
        }
    }

    req_data = json.dumps(request).encode("utf-8")
    req = urllib.request.Request(
        mcp_url,
        data=req_data,
        headers={"Content-Type": "application/json"},
        method="POST"
    )

    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            result = json.loads(resp.read().decode("utf-8"))
            if "result" in result and "content" in result["result"]:
                content = result["result"]["content"][0]
                if content["type"] == "text":
                    return json.loads(content["text"])
            return {"error": "Invalid response format"}
    except Exception as e:
        return {"error": str(e)}


def query_metrics(
    name: str = "",
    filter: str = "",
    limit: int = 100
) -> Dict[str, Any]:
    """
    查询聚合指标

    Args:
        name: 指标名称
        filter: expr 表达式过滤条件
        limit: 返回最大数量

    Returns:
        包含 count, metrics 的字典
    """
    import urllib.request

    mcp_url = os.environ.get("GTA_MCP_URL", "http://localhost:8090/mcp")

    params = {"limit": limit}
    if name:
        params["name"] = name
    if filter:
        params["filter"] = filter

    request = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {
            "name": "aggregate_query",
            "arguments": params
        }
    }

    req_data = json.dumps(request).encode("utf-8")
    req = urllib.request.Request(
        mcp_url,
        data=req_data,
        headers={"Content-Type": "application/json"},
        method="POST"
    )

    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            result = json.loads(resp.read().decode("utf-8"))
            if "result" in result and "content" in result["result"]:
                content = result["result"]["content"][0]
                if content["type"] == "text":
                    return json.loads(content["text"])
            return {"error": "Invalid response format"}
    except Exception as e:
        return {"error": str(e)}


def save_result(
    data: Any,
    format: str = "json",
    filename: str = "output.json"
) -> str:
    """
    保存结果到文件

    Args:
        data: 要保存的数据
        format: 输出格式 (json/csv)
        filename: 文件名

    Returns:
        保存的文件路径
    """
    # 获取输出目录
    output_dir = os.environ.get("GTA_OUTPUT_DIR", ".")
    filepath = os.path.join(output_dir, filename)

    if format == "json":
        with open(filepath, "w", encoding="utf-8") as f:
            json.dump(data, f, indent=2, ensure_ascii=False)
    elif format == "csv":
        import csv
        if not isinstance(data, list) or len(data) == 0:
            raise ValueError("CSV format requires a non-empty list of dicts")

        with open(filepath, "w", encoding="utf-8", newline="") as f:
            writer = csv.DictWriter(f, fieldnames=data[0].keys())
            writer.writeheader()
            writer.writerows(data)
    else:
        raise ValueError(f"Unsupported format: {format}")

    return filepath


def log(message: str) -> None:
    """
    输出日志到 stderr

    Args:
        message: 日志消息
    """
    timestamp = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    print(f"[{timestamp}] {message}", file=sys.stderr)


def get_arg(name: str, default: Any = None) -> Any:
    """
    获取脚本参数

    Args:
        name: 参数名
        default: 默认值

    Returns:
        参数值
    """
    args_json = os.environ.get("GTA_ARGS", "{}")
    try:
        args = json.loads(args_json)
        return args.get(name, default)
    except json.JSONDecodeError:
        return default


def get_session_id() -> str:
    """
    获取当前会话 ID

    Returns:
        会话 ID
    """
    return os.environ.get("GTA_SESSION_ID", "")


def get_work_dir() -> str:
    """
    获取工作目录

    Returns:
        工作目录路径
    """
    return os.environ.get("GTA_WORK_DIR", ".")


# 便捷函数
def print_summary(title: str, data: Any) -> None:
    """
    打印数据摘要

    Args:
        title: 标题
        data: 数据
    """
    log(f"=== {title} ===")
    if isinstance(data, list):
        log(f"Count: {len(data)}")
        if len(data) > 0:
            log(f"First item: {json.dumps(data[0], ensure_ascii=False)[:200]}")
    elif isinstance(data, dict):
        log(f"Keys: {list(data.keys())}")
    else:
        log(f"Value: {data}")
