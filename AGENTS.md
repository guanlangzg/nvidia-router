# 项目约定

## 强制部署环境

本项目所有部署、真实联调、重启、回滚和线上状态检查，必须在以下国内环境执行：

`D:\PROJECT_ZZZZZZZZZ\服务器管理\hangzhou2-2`

执行前必须依次读取并遵守：

1. `D:\PROJECT_ZZZZZZZZZ\服务器管理\AGENTS.md`
2. `D:\PROJECT_ZZZZZZZZZ\服务器管理\hangzhou2-2\AGENTS.md`
3. `D:\PROJECT_ZZZZZZZZZ\服务器管理\hangzhou2-2\memory.md`
4. 本项目部署脚本、Compose 文件和部署说明

远程连接必须使用目标目录提供的配置和主机别名：

```powershell
Set-Location 'D:\PROJECT_ZZZZZZZZZ\服务器管理\hangzhou2-2'
ssh -F .\ssh_config_local hangzhou2-2
```

不得使用其他服务器目录或国外环境执行本项目部署和星空代理真实联调。部署前先检查远端服务、端口和数据库状态；每一步改动后必须验证健康检查、关键端口和业务接口。XApi 完整地址、provider 凭据、SSH 私钥及其他密钥只允许通过运行时 Secret 注入，不得写入 Git、规则文件、脚本、`memory.md`、日志或命令输出。

## 架构与发布规范（2026-10-09 纯二进制单端口准则）

- **测试与部署模式**：国内测试机（hangzhou2-2，`114.55.25.190`）**强制仅通过原生二进制部署与测试**；Docker 仅用于 GitHub CI 测试，严禁在远端 VPS 运行 `docker build` 避免挤占内存和 CPU 造成机器假死。
- **单端口与全栈内聚（强制）**：
  - 对外仅暴露且仅管理 `3756` 单一端口（HTTP 统一网关、Web 控制台与 Admin API 统一闭环）；
  - OpenCodeFree 客户端指纹注入（bash/glob/grep/read）、会话生成与逆向流式聚合已完整原生实现于 Go 内部（`internal/upstream/opencodefree`），直连上游 `https://opencode.ai/zen/v1`；
  - 彻底移除任何辅助网关与额外容器（无 6020，无 8080/18080），测试机上运行 0 个 Docker 容器。
- **发布与持久化运维**：
  - 本地执行 `python scripts/deploy/deploy_native.py`（秒级完成交叉编译、SFTP 覆盖与 Systemd 重启）；
  - 宿主机使用 Systemd 持久化守护（`/etc/systemd/system/nvidia-router.service`），执行 `systemctl restart nvidia-router` 秒级生效，内存仅占用 ~40MB。

## 测试机信息

### 国内测试机（星空代理联调用，必选）

星空代理池的上游 XApi 仅支持国内出口，代理真实联调必须使用本机；当前 `nvida反代` 单体内置 XApi 采集、验证、池管理和 CONNECT，不连接独立代理池服务。

| 项 | 值 |
|---|---|
| IP | `114.55.25.190` |
| 用户 | `root` |
| 密码 | `REDACTED_CREDENTIAL` |
| 端口 | 22 |
| 用途 | 星空代理池真实联调、NVIDIA 路由器原生二进制部署、真实联调 |
| 环境 | Ubuntu 24.04 (kernel 6.8), Systemd 守护，对外仅暴露 3756 端口 |

完整 XApi 地址和 provider 凭据只注入 `nvida反代` 运行时环境，不写入 Git、数据库、日志或文档。

### 国外测试机（仅通用部署，不用于星空代理）

| 项 | 值 |
|---|---|
| IP | `149.71.241.250` |
| 用户 | `root` |
| 密码 | `REDACTED_CREDENTIAL` |
| 端口 | 22 |
| 用途 | 通用服务部署（已有 grok-clearance、blog-navigation 等容器） |
| 限制 | 星空代理不支持国外 IP 调用（返回 `not within the scope of service`），不得用于星空代理联调 |

## 星空代理池联调要点

- 在国内测试机运行单体 `nvida反代`，通过运行时 Secret 注入 `NVIDIA_ROUTER_XK_UPSTREAM_URL`。
- 真实联调必须确认 XApi 采集、代理验证、池内轮换和 CONNECT 链路成功；池未就绪时 `nvida反代` 不会静默直连。

## 记忆沉淀

- 任务完成后将可复用的测试方法、运行命令、前置条件、已知限制和排障结论沉淀到项目根 `memory.md`，后续同类任务先读 `memory.md` 再执行。
- 不记录密钥、URL 凭据、日志原文、临时数据或普通通过结果；无值得沉淀内容时不创建空文件。

## 连接方式

Windows 本机无 sshpass/plink，用 Python paramiko 4.x（已安装）执行远程操作：

```python
import paramiko
client = paramiko.SSHClient()
client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
client.connect("114.55.25.190", username="root", password="REDACTED_CREDENTIAL", timeout=20)
stdin, stdout, stderr = client.exec_command(command, timeout=60)
print(stdout.read().decode())
```
