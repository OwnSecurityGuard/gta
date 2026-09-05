@echo off
rem gt-mcp 启动脚本：必须在仓库根目录运行（workdir 探测依赖 cwd）。
rem 最小化窗口常驻，关掉该窗口即停止服务。
cd /d E:/ai_workspace/gta
start "gt-mcp" /min gt-mcp.exe
