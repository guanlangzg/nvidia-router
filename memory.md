# 记忆 — 可复用约束、部署方法与排障结论

> 本文件只保留长期有效信息。不得记录密钥、完整 URL 凭据、Cookie、Access Key、日志原文、临时数据或普通测试流水账。

## 1. 项目边界与安全

- 星空代理真实联调、部署、重启和线上检查只在国内目标 hangzhou2-2 执行；国外机器不用于星空代理。
- 处理服务器前依次读取：项目 AGENTS.md、服务器管理/AGENTS.md、目标目录 AGENTS.md 与 memory.md、部署脚本、Compose 和部署说明。
- 单体路由器内置 XApi 采集、验证、池管理和 CONNECT；池未就绪时必须失败，不得静默直连。
- OpenCodeFree 候选发现和 `SyncOpenCodeFreeModels` 均只把 `-free` 后缀模型视为可用；同步时非 free 网关条目不会保持本地模型启用，网关请求失败仍不改库。
- 能力探测的判定必须能被公平重算：歧义提问和鉴权/策略类 4xx 都不得写成永久 `unsupported`；探测改动后必须用 `POST /admin/api/model-test-jobs` 强制重探测核对，而不是等周期探测。
- XApi 完整地址、provider 凭据、主密钥、管理员密码、NVIDIA Key 和 SSH 私钥只通过运行时 Secret 注入；命令输出、日志、Git、文档和记忆中只允许出现脱敏值。
- 当前公网入口为 HTTP；管理员密码、Cookie、Access Key、请求和响应存在明文传输风险。生产 HTTPS 需要受信反向代理、Secure Cookie、External Origin 和 Trusted Proxy CIDR。

## 2. 镜像与发布版本

- 版本格式：YYYYMMDD-变更主题-git短SHA；每次发布必须唯一，禁止生产使用 latest、local、dev 或其他漂移标签。
- 同一版本号必须同时对应 Git HEAD、Release 目录、镜像和回滚记录：
  - /opt/nvidia-router-releases/<version>
  - nvidia-router:deploy-<version>
- 标准发布命令：python scripts/deploy/deploy_remote.py <version>。脚本使用 git archive HEAD，继承现网 .env 与 deploy override，使用 goproxy.cn 构建，切换前由旧镜像备份数据库。
- 同一 Release 已存在备份时不得覆盖；源码或配置变化必须生成新版本号。部署后核对容器实际镜像、工作目录、Git SHA、备份路径和回滚版本。
- 版本、备份、回滚点和未完成验证项要更新到本节“当前线上状态”；详细历史放在目标服务器记忆或专项文档。

## 3. 架构与关键配置

- 监听：应用容器 0.0.0.0:3756；默认 Compose 绑定回环，公网部署 override 绑定 0.0.0.0:3756。
- XApi：当前国内套餐 qty=2；qty>2 曾返回 506，NVIDIA_ROUTER_XK_EXPECTED_QTY 必须先与实际套餐确认，不能凭旧记录调高。
- 常用默认：采集间隔 5s、代理 TTL 120s、验证期望状态 404、验证并发 2；慢推理模型客户端首字节超时建议至少 120s，流式 idle 超时建议 180s。
- 数据卷：生产使用外部 Docker volume nvr-data；不得执行 docker compose down -v。
- SQLite 迁移：新增或升级索引必须核对旧索引名；CREATE INDEX IF NOT EXISTS 同名时不会重建索引。

## 4. 标准部署流程

### 发布前

~~~powershell
Set-Location 'D:\PROJECT_ZZZZZZZZZ\服务器管理\hangzhou2-2'
ssh -F .\ssh_config_local hangzhou2-2
~~~

- 先检查远端 app、端口、数据库卷、健康接口和近期错误，再开始构建。
- 本地确认工作树和目标提交：git status --short、git diff --check、git rev-parse --short HEAD。
- 有新增迁移时，必须确认旧镜像备份成功后再切换；不要把 .env、key/、data/、依赖或本地二进制放进发布包。

### 切换

~~~powershell
python scripts/deploy/deploy_remote.py 20260823-redeploy-cfcaecf
~~~

- 脚本流程：读取现网 release/image → 创建唯一 Release → git archive HEAD 上传 → 继承 .env/deploy override → 构建版本镜像 → 停 app → 旧镜像备份 nvr-data → 启动新 app → live/ready 校验。
- Compose 生产覆盖文件通过 NVIDIA_ROUTER_IMAGE 注入版本镜像；不要手工改回 nvidia-router:local，不要使用 --remove-orphans 删除预期存在的容器。

### 回滚

- 使用带版本号的旧 Release、旧镜像和同一组基础 Compose + deploy override；不得依赖默认镜像标签。
- 回滚前确认数据库迁移兼容性；数据库恢复只能使用明确的备份文件，且备份权限保持 600。
- 管理员密码重置必须先停 app 释放 SQLite 锁，密码仅通过 stdin 注入；不得写入 argv、文件、日志或记忆。

## 5. 最小充分验证

### 本地

~~~bash
go vet ./...
go test ./...
pnpm --dir web run typecheck
pnpm --dir web run test
pnpm --dir web run build
git diff --check
~~~

前端改动必须同步 internal/web/dist（go:embed），并运行 scripts/check-web-dist.sh；Go race 在无 CGO/GCC 的本机无法验证时，交给 CI。

### 远端免认证

- 容器：running/healthy、重启次数为 0、OOM 为 false。
- 接口：/health/live、/health/ready、代理池和 OpenCodeFree 健康端点返回 200。
- 端口：3756 以及本次涉及的 18080、18081、6020 监听正常。
- 根页及其 JS/CSS 资源返回 200；匿名 /v1/models、/metrics 应返回 401。
- 读取 schema_migrations 最大版本、enabled 模型数量和部署后错误签名；不输出数据库内容或日志原文。

### 真实联调

- 只在 hangzhou2-2，通过运行时 Secret 执行 scripts/test/live-nvidia.sh、live-xk-proxy.sh 或专项探针。
- 重启后先等待代理池预热，再判断 NVIDIA 渠道；优先看鉴权 metrics 中的池健康 gauge。
- 逐 case PASS 才能宣称通过；SKIP、BLOCKED、缺凭据或仅健康检查都不能宣称完整 live/E2E。

## 6. 高频排障结论

- 池健康数高但 latency_samples 为 0：优先检查 validator 和请求 transport 是否启用 ForceAttemptHTTP2；Grace 上限必须锚定 ValidatedAt，不能锚定当前时间，也不能续命从未验证的出口。
- validation_all_failed：先看池 gauge 和实际请求，不要只依赖 INFO 日志；XApi TXT 随机性需要在 fetch 内有界重试，最终失败必须触发退避。
- 代理快速 502：ReasonTransportFailed 必须映射为可重试的全局上游故障，让请求换 Key；共享租约并发上限可能造成瞬时无健康出口。
- OpenCodeFree 638/502：内网 gateway 只走直连，不要经外部 XApi；非 2xx、空响应和协议错误必须映射为明确的上游错误，不能回退成泛化 500。
- 大量 499：通常是客户端 60 秒超时与 NVIDIA 慢首字节冲突；监控中将 canceled 单列，不要直接归因于路由器故障。
- 管理 API 401/403：先确认运行时密码是否为当前值；变更请求需要匹配 Host 的 Origin。管理员登录页面验证应等待 URL 离开 /admin/login。
- 前后端契约漂移：后端迁移、DTO 或结构体加字段时，同步前端类型、contract spec 和 embed 产物；模型能力或 context_length 由运营数据维护时不要在候选发现中编造默认值。

## 7. Windows 与发布工具坑

- Windows 无 sshpass/plink；使用目标目录的 SSH 配置或部署脚本内 Paramiko 配置。
- PowerShell 管道传 gzip/归档可能破坏二进制；优先用 git archive + SFTP。
- PowerShell 读写中文文件可能改变编码；批量替换使用 apply_patch 或明确 UTF-8 的工具。
- 本地 3756 可能被旧 nvidia-router.exe 占用；运行 E2E/视觉探针前确认实际端口和嵌入资源版本。
- 一次性诊断脚本放临时目录，含 Secret 时使用 umask 077/权限 600，验证后清理。

## 8. 当前线上状态（最后核验：2026-10-02）

- 源码：运行版本为 `00c75d2`（分支 `codex/optimize-executor-20260828`，含 4fca032/d54a2d5）；其后 `eea3556` 只改文档与测试脚本，未重新发布。
- Release：`/opt/nvidia-router-releases/20261002-ocf-provider-retry-00c75d2`。
- 镜像：`nvidia-router:deploy-20261002-ocf-provider-retry-00c75d2`。
- 回滚点：`20261002-ocf-tools-probe-4fca032` / `nvidia-router:deploy-20261002-ocf-tools-probe-4fca032`（再往前 `20261002-reasoning-probes-5f944db`）。
- 切换前数据库备份：两版各一份 `backups/predeploy-<版本号>/router.db`，约 19,349,504 字节，`600`，属主 `10001:10001`。
- 已核验：app `running/healthy`、重启 0、OOM false；`/health/live`、`/health/ready`、根页 200；匿名 `/v1/models`、`/metrics`、`/admin/api/models/candidates` 均 401；3756 与 6020 监听（代理池按设计只走内网，无宿主端口）；近 20 分钟 panic/fatal/ERROR 签名 0。
- 启用模型 3：NVIDIA `nvidia/nemotron-3-ultra-550b-a55b`、OCF `opencodefree/longcat-2.5-preview-free`、OCF `opencodefree/space-bunny-free`；两个 OCF 模型 `supports_tools=true`/`tools_status=supported`（见 §43）。
- 未执行 CONNECT 矩阵与 NVIDIA 渠道真实请求；公网 HTTP 明文风险保持不变。

## 9. 价格功能移除（2026-08-23）

- 价格字段、模型单价编辑、成本统计接口和成本面板已从运行时代码移除；迁移 043 负责从现有 `models` 表删除遗留价格列。历史迁移 019/027/029/031 保持不变，以免破坏已应用迁移的 checksum 和升级链。
- 前端构建配置直接把 Vite 输出写入 `internal/web/dist`；Windows 下 pnpm 非交互安装可能被 `ERR_PNPM_IGNORED_BUILDS` 拦截，可直接调用 `web/node_modules/.bin/vue-tsc.cmd`、`vitest.cmd` 和 `vite.cmd` 完成本地校验。`scripts/check-web-dist.sh` 需要可用的 POSIX bash。

## 10. 渠道状态页卡片重设计（2026-08-23）

- 渠道状态卡片应将成功率、探测次数、连续失败和最近延迟作为首屏固定信息；时间段只保留紧凑时间线和单段标题提示，不再用可展开的长详情列表，避免卡片高度随交互跳变。
- 这类前端改动的最小验证组合：`web/node_modules/.bin/eslint.cmd`、`vitest.cmd run`、`vue-tsc.cmd --noEmit`、`vite.cmd build`，再用 Playwright 在桌面三列和 390px 单列检查无展开入口、无横向溢出；构建后同步检查 `internal/web/dist` 的静态资源引用闭包。

## 11. 本地统一启动（2026-08-23）

- 本地唯一启动入口为 `start.bat`，它只包装 `scripts/start-local.ps1`；脚本从当前源码启动 `go run ./cmd/nvidia-router serve`，再启动 `web/node_modules/.bin/vite.cmd --host 127.0.0.1 --strictPort`。
- 前端开发地址固定为 `http://127.0.0.1:5173`，Vite 的 `/admin/api` 请求代理到 `3756`；`3756` 在本地开发中只作为 API 服务，不作为前端页面入口。
- `internal/web/dist` 是 Docker/生产 Go 内嵌前端产物，不能作为本地实时开发入口删除或替代 Vite。
- 启动器在 `tmp/local-start-state.json` 记录自己拉起的进程及启动时间；无登记的端口占用会安全失败，不得按通用 `node`/`go` 进程名误杀其他服务。启动失败时要清理本次已拉起的进程。
- 本地 `.env` 的主密钥必须与现有 `data` 匹配；不匹配会触发 AES-GCM authentication failure。密钥只通过本机运行时环境注入，不能写入启动脚本、日志或记忆；不得删除或重建 `data`。
- 启动自检：`powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/test/start-local_test.ps1`；它检查 `3756/health/live=200`、`5173/=200`、`5173/@vite/client=200` 和 `/admin/api/models=401` 代理边界，前端改动只在 `5173` 页面验证 HMR。

## 12. Flat Outline 遮罩与响应式浮层（2026-08-23）

- 前端浮层验收应覆盖 1440px 与 390px：基础页、移动侧栏、命令面板、快捷键帮助、Modal、菜单和 tooltip 均检查 `scrollWidth - clientWidth`、可见浮层矩形是否在视口内、活动焦点中心是否仍可命中；完整页面检查用 Playwright，认证只从本地运行时配置读取，不回显凭据。
- UnoCSS 下移动抽屉的显示/隐藏位移类必须互斥；基础类同时保留 `-translate-x-full`、再动态追加 `translate-x-0` 可能因生成顺序继续应用隐藏变换。使用条件类只输出一个移动位移状态，并保留 `lg:translate-x-0` 桌面覆盖。
- 单列 CSS Grid 的卡片子项默认 `min-width:auto`，内部 flex 标题与固定操作按钮可能把轨道撑出视口；卡片 grid item 与可收缩的标题内容容器都应按需加 `min-w-0`，不能只依赖 `body { overflow-x: hidden }`。
- Flat Outline 视觉回归同时做源码扫描和浏览器计算样式检查：生产源码不得出现阴影、文字阴影、drop-shadow、backdrop-filter、backdrop-blur、mask-image、animate-ping 或 shadow 工具类；功能性半透明 scrim 保留，面板层级使用底色与描边。
- Windows 本机没有可用 POSIX bash 时，`scripts/check-web-dist.sh` 用等价 Python 闭包检查：从 `internal/web/dist/index.html` 递归跟踪静态引用与指纹文件名，确认无缺失资源和孤立旧 hash；随后用 `web/node_modules/.bin/vite.cmd build` 重建嵌入产物。

## 13. Flat Outline 交互与移动布局补充（2026-08-23）

- 移动抽屉打开时，后渲染的 `aside` 若与顶部 Header 同为 `z-40`，会拦截菜单关闭按钮；打开态 Header 应提升到 `z-50`，并用真实点击回归验证关闭路径。
- 移动端 Playwright 几何检查要等待抽屉 300ms 位移动画完成；登录跳转要等待明确的 `/admin/`，不能用会匹配 `/admin/login` 的宽泛 glob。
- 代理池配置的全宽 Grid 子项若包含长说明和输入框，必须加 `min-w-0`；数据表自身可以保留在 `overflow-auto` 容器内，但不能依赖 `body { overflow-x: hidden }` 掩盖父级 Grid 外溢。
- 本地统一启动器清理应以 `tmp/local-start-state.json` 的进程路径、启动时间和父 PID 做精确核验；子进程退出可能连带启动器消失，确认 3756/5173 已无监听后再清理状态和本轮日志。

## 14. 全站视觉 QA 环境与基线（2026-08-24）

### 隔离 QA 环境（不碰本地 data 与 3756/5173）

- `node scripts/test/web_qa_env.mjs [--port 5175] [--no-seed]`：编译并拉起 `tests/e2e/harness`（临时目录 SQLite + mock NVIDIA 上游），播种数据，再起一个独立 Vite，把 `/admin/api` 代理到 harness；连接信息写 `tmp/web-qa-env.json`。用它而不是直接对本地实例做 QA：本地库的管理员口令存在库里且可能已改，隔离库既能稳定播种又不会写坏数据。
- `node scripts/test/web_visual_qa.mjs [--out dir] [--only substr]`：13 个页面 × 4 视口（390/768/**1024**/1440）× 双主题 × 6 种浮层态截图 + 几何审计。1024 是 lg 断点第一格，侧栏刚占 240px 而正文比 768 更窄，是全站最容易溢出的宽度，必须单独测。
- `Vite` 的代理目标由 `VITE_PROXY_ORIGIN` 决定，可指向任意后端；spawn Vite 用 `node node_modules/vite/bin/vite.js` 而非 `.bin/vite.cmd`，避免 shell 拼参与进程树回收不确定。
- 管理端登录限流是**每 IP+用户名 1 分钟 5 次**，多视口各自登录必然被拒；探针必须登录一次后用 `context.addCookies` 复用会话。

### 播种契约（易踩）

- `POST /admin/api/auth/change-password` 只收 `current_password`/`new_password`（多传字段会 400），且会换发 cookie，jar 必须跟着更新。所有写请求都要带与 baseURL 完全一致的 `Origin`。
- `POST /admin/api/nvidia-keys/batch` 的 `keys` 是**换行分隔字符串**，不是数组。
- `POST /admin/api/models` 不校验候选是否来自上游发现，可离线批量播种；但 `selectionDTO` 不含 `context_length`/`stream_*`（只能后续 PATCH），且 `kind=asr/tts` 直接 `enabled:true` 会被能力校验拒绝。
- `PATCH /admin/api/proxy-pool` 的 `upstream_url` 必须带 provider 查询凭据，QA 播种应留空而不是伪造凭据串。
- 请求日志只有请求热路径一个写入口，且 `BufferRecorder` 默认 **30s** 才 flush；打完流量要等够时间，否则监控/统计页是空的。

### 几何审计的假阳性规则（缺了会被噪声埋掉真缺陷）

- 判定必须**祖先感知**：滚动容器内的宽内容、收起抽屉（`[inert]` 或整体位于画布外）的后代、`.sr-only` 子树都要排除。
- `overflow:visible` 的"撑破"不丢内容，属于越界范畴；只报真正裁切的容器，且带 `text-overflow:ellipsis` + `nowrap` 的单行截断是设计意图。
- 命中测试要跳过铺满视口的 scrim，以及被打开的 `[role=menu/dialog/tooltip/listbox]` 浮层正常覆盖的底层控件。
- 触控尺寸按最近的 `<label>` 命中区算，而不是 16px 的 `input` 本体。

### 本轮定位到的系统性根因（都已修，勿回退）

- **项目没有任何 CSS reset**，UA 默认值曾在每页泄漏：`<a>` 全站带下划线；169 个 `<p>` 带 13–14px 非网格外边距（`mt-*` 只覆盖 top，bottom 全残留）；`<ul>/<ol>` 带 disc 与 40px 内缩，让状态行出现"圆点 + 状态点"双点；`<dd>` 有 40px 缩进；表单控件不继承字体回落 **Arial**。reset 现集中在 `web/src/styles/theme.css`，改动前先确认不是在重新引入这些。
- **UnoCSS 的 `font-[...]` 编译成 `font-family`**，而 `--text-display/title/heading/label` 是 `font` **简写**值，赋给 font-family 是无效声明会被整条丢弃——四层字体阶梯从未生效，标题一路回落到 UA（h1 30px/700、h2 22.5px/700、眉题 15px）。现改为 `uno.config.ts` 的 rule 显式输出 `font` 简写；`type-*` 不要再与 `text-base` 等字号工具类混用（简写会重置字号）。
- 表格默认 `border-spacing: 2px`，9 列表格凭空多 20px 足以逼出横向滚动条；`data-table` 已加 `border-collapse`，同时让 1px 行分隔连成整线。
- `table-layout: auto` 下"不能换行"的列先吃满 max-content，可换行的模型列只分到剩渣（实测 115px，长模型名折 8 行），声明宽度被整体忽略。密集表用 `table-fixed` + 显式列宽 + `min-w`，宽度分配才可控；给单元格加 `whitespace-nowrap` 会让该列变刚性并饿死邻列，是有代价的。
- `ring-*` 工具类编译成 box-shadow，与 Flat Outline 冲突，且会和 `theme.css` 的 `:focus-visible` outline 叠成多层焦点指示。焦点统一只用 outline。
- 颜色只允许来自 theme.css 语义 token：`dark:` 变体走 `.dark` 选择器，与本项目的 `[data-theme='dark']` 永不相交，写了就是死代码（组件会停在亮色配色上）。
- 以上三条已固化为回归守卫：`web/src/styles/flat-visual.spec.ts` 断言无 `ring-\d`、无 `dark:` 变体、无原始调色板色阶。

### 最小验证组合

~~~bash
web/node_modules/.bin/eslint.cmd .
web/node_modules/.bin/vue-tsc.cmd --noEmit
web/node_modules/.bin/vitest.cmd run
web/node_modules/.bin/vite.cmd build
python scripts/test/check_web_dist_closure.py   # dist 静态资源闭包（无 POSIX bash 时替代 check-web-dist.sh）
~~~

## 15. 2026-08-24 main 提交与确认性重新部署（9f45568）

- 发布时工作树 `HEAD`、本地 `main` 和 GitHub `origin/main` 均为 `9f45568`；执行 `git push origin HEAD:refs/heads/main` 返回 `Everything up-to-date`。发布前工作树保持干净。
- 本地门禁已通过：`go test ./...`、`go vet ./...`、前端 lint、typecheck、284 个前端单测、前端生产构建、`git diff --check`；Windows 使用 `python -X utf8 scripts/test/check_web_dist_closure.py`，结果为引用 40、磁盘 40、缺失 0、孤立 0。
- 使用标准 `python scripts/deploy/deploy_remote.py 20260824-redeploy-9f45568` 发布到 `/opt/nvidia-router-releases/20260824-redeploy-9f45568`，镜像为 `nvidia-router:deploy-20260824-redeploy-9f45568`；继承线上 `.env` 和 deploy override，复用外部 `nvr-data`，切换前由旧镜像生成数据库备份。
- 切换前备份为 `/opt/nvidia-router-releases/20260824-redeploy-9f45568/backups/predeploy-20260824-redeploy-9f45568/router.db`，大小 8,806,400 字节，权限 `600`，属主 `10001:10001`，SHA-256 为 `4b40da6a7197ebd86f16b5bc07a99190086335401a586be2852291e61ee1d885`；回滚点为 `20260823-remove-pricing-59abdc9` / `nvidia-router:deploy-20260823-remove-pricing-59abdc9`。
- 部署后 app 使用新镜像，`running/healthy`、重启 0、OOM false；`/health/live`、`/health/ready`、代理池 `18080/healthz`、OpenCodeFree `6020/healthz` 均 200；公网 3756 `/health/live`、根页和当前 JS/CSS 资源均 200；匿名 `/v1/models` 与 `/metrics` 均 401。
- 管理烟测通过：管理员登录 200，鉴权 `/metrics`、模型、设置和 Access Key 列表均 200，模型总数 11、启用 10，注销 204。未执行真实模型请求、代理轮换或 CONNECT 矩阵；部署后代理验证日志仍出现有限的 `validation_all_failed` 预热告警，因此不能将本轮结果宣称为完整 live/E2E。公网 HTTP 明文风险保持不变。

## 16. 2026-08-24 vibe 场景多维复测

- 使用 `scripts/test/vibe_eval_remote.py` 和一次性补充探针，在 `hangzhou2-2` 的当前 release 上完成模型思考强度、thinking 开关、工具、流式、长输入、context needle、JSON、长输出和重复稳定性测试；未修改源码、运行配置或模型白名单。
- 模型目录共 11 个，其中 10 个启用 Chat 模型；10 个基础请求中 7 个有效 HTTP 200，3 个 NVIDIA 模型为 `502 upstream_proxy_unavailable`：`deepseek-ai/deepseek-v4-flash-0731`、`meta/llama-3.2-90b-vision-instruct`、`minimaxai/minimax-m3`。
- OpenCodeFree 深测覆盖 `opencode-free/nemotron-3-ultra-free` 与 `opencodefree/x-preview-f-free`，思考、工具、长输入主路径大部分为 200；Nemotron 完成流式样本无 malformed event，x-preview 出现一次流式 503。
- 综合矩阵耗时约 127 秒，选中 `nvidia/nemotron-3-ultra-550b-a55b`、`stepfun-ai/step-3.7-flash`、`opencodefree/x-preview-f-free`、`opencode-free/hy3-free`；x-preview 完成双工具调用和 follow-up，另外三个选中模型的工具用例观察到 501 `not_implemented`。工具能力必须按模型判断。
- 临时测试 Key 已删除，清理后管理注销 204、metrics 200、健康代理 27；最终容器为 `running/healthy`、重启 0、OOM false，live/ready、18080/healthz、6020/healthz 均 200。评测报告见 `docs/plans/2026-08-24-vibe场景多维复测报告.md`。
- 限制：5 次重复不是长压测；context needle/长输入不等于已证明 32K/128K 上限；501/502/503 分别按能力未实现、代理出口不可用、上游瞬态失败区分，不能合并为单一故障。

## 17. 2026-08-24 vibe 优化提交与重新部署

- GitHub `main` 已推送到 `b9704f3`（`feat: tighten vibe model capability validation`）；发布前 worktree 干净，`go test ./...` 通过。
- 标准发布版本为 `20260824-vibe-optimization-b9704f3`，release 为 `/opt/nvidia-router-releases/20260824-vibe-optimization-b9704f3`，镜像为 `nvidia-router:deploy-20260824-vibe-optimization-b9704f3`。
- 切换前数据库备份位于 `backups/predeploy-20260824-vibe-optimization-b9704f3/router.db`，大小 9,023,488 字节，权限 `600`，SHA-256 为 `47336e880e4a290b4c2d9d85b1eb8d0b27aa68619bf23f0429eaeface52769c6`；回滚点为 `20260824-redeploy-9f45568` / `nvidia-router:deploy-20260824-redeploy-9f45568`。
- 发布后容器 `running/healthy`、重启 0、OOM false；3756 live/ready、18080/6020 healthz、根页均 200；匿名 `/v1/models` 与 `/metrics` 均 401；3756/18080/18081/6020 端口监听正常；release 含 migration 044。
- 本轮没有重复执行管理员会话、真实模型、代理轮换或 CONNECT 矩阵；不据此宣称完整 live/E2E，公网 HTTP 明文风险保持不变。

## 18. 2026-08-24 前端整体美学一致性收敛

- 视觉一致性守卫集中在 `web/src/styles/flat-visual.spec.ts` 与 `shortcuts.spec.ts`：组件源码不得直接写状态 hex、引用未定义 surface token、使用未命名的宏观圆角或把运行时值插入 UnoCSS 任意颜色 class；数据热力格、时间线刻度和状态点等数据形状可保留局部圆角。
- `ModelHealthCard` 的动态 `border-[color-mix(...${token}...)]` 与 `bg-[...${token}]` 会让 Vite/esbuild 产生 CSS 语法警告；状态边框改为静态 token class 映射，时间线颜色改为原生 `:style` 的 `backgroundColor`，不要回退到运行时拼 UnoCSS class。
- 全站视觉 QA 必须先启动隔离 `node scripts/test/web_qa_env.mjs --port 5175`，等待 `tmp/web-qa-env.json` 出现后再运行 `node scripts/test/web_visual_qa.mjs http://127.0.0.1:5175 --out <dir>`；环境未就绪时探针会误连本地实例并以错误登录失败，不能据此判断页面代码。

## 19. 2026-08-24 前端一致性发布与重新部署

- 运行时代码发布基线为 GitHub `main` 的 `08eb1c9`；最终发布版本为 `20260824-ui-consistency-08eb1c9`，release 为 `/opt/nvidia-router-releases/20260824-ui-consistency-08eb1c9`，镜像为 `nvidia-router:deploy-20260824-ui-consistency-08eb1c9`。随后仅追加本记录的文档提交已推送到 `main`，不改变运行时代码。
- 切换前数据库备份位于 `backups/predeploy-20260824-ui-consistency-08eb1c9/router.db`，大小 9,445,376 字节，权限 `600`，属主 `10001:10001`，SHA-256 为 `95816c45052bd783995e09be7ab9c827cfdd3cac226f24807c456be8e3695b4d`；回滚点为 `20260824-vibe-optimization-b9704f3` / `nvidia-router:deploy-20260824-vibe-optimization-b9704f3`。
- 发布后容器实际为目标镜像，`running/healthy`、重启 0、OOM false；release 工作目录与版本一致，迁移 `044_model_tools_status.sql` 存在；3756/18080/18081/6020 端口监听正常，3756 live/ready、18080/6020 healthz 均 200，根页和 index 引用的 2 个静态资源均 200。
- 匿名 `/v1/models` 与 `/metrics` 均 401；管理员登录 200，会话、模型列表、运行时摘要均 200，注销 204，注销后的会话为 401。未执行真实模型请求、代理轮换或 CONNECT 矩阵；公网 HTTP 明文风险保持不变。
- 远端 loopback 静态探针应使用禁用代理的 opener，并将正则提取的 bytes 路径先 decode 成字符串；否则 Python 探针会把类型错误吞成资源失败，而 `curl` 与实际服务均正常。

## 20. 2026-08-24 Teleport 测试隔离

- 共享浮层组件真实 Teleport 到 `body` 后，业务视图单测中不要继续用 `wrapper.get/find` 查询菜单、Modal、确认按钮或 tooltip；用 `document.body.querySelector` 查询，并用原生 `click`/`change` 事件驱动交互，保留对真实 DOM 拓扑的覆盖。
- 每个会打开浮层的测试套件在 `afterEach` 清理 `document.body`，避免未销毁的 Teleport 节点让后续用例命中旧菜单或旧弹窗；使用全局 `config.global.stubs` 的旧测试必须在 `afterEach` 恢复为空，不能污染其他套件。
- `v-model.lazy` 的 Teleport 输入控件测试要设置 `HTMLInputElement.value` 后派发冒泡 `change`，仅派发 `input` 不会触发保存逻辑。

## 21. 2026-08-24 Teleport 菜单无障碍与层级

- `UiMenu` Teleport 到 `body` 后，打开时必须把焦点移入首个可操作项；菜单内使用 `ArrowUp/ArrowDown/Home/End` 移动焦点，关闭时再归还触发按钮。仅依赖 DOM 顺序会让键盘焦点跳过 body 末尾的 Teleport 节点。
- Overlay token 的数值顺序必须与语义一致：popover 低于 modal，modal 低于 toast，tooltip 位于最上层；共享 shortcut 不得用 `z-50` 这类硬编码覆盖 token。
- 当前机器默认 Vitest 文件并行时，路由/命令面板/AppShell 的 5 秒用例可能被 worker 竞争拖超时；遇到无失败堆栈但有超时，使用 `vitest run --maxWorkers=1 --no-file-parallelism` 做确定性全量复核，再结合定向并行测试判断是否为环境抖动。

## 22. 2026-08-25 UI 浮层视觉重构提交与重新部署（772fce2）

- 部署前 GitHub `main` 与源码 `HEAD` 均为 `772fce2`（`feat: refine frontend overlays and visual tokens`）；本次只包含前端浮层、焦点无障碍、层级 token、回归测试和嵌入式资源更新，无数据库迁移。
- 标准版本为 `20260825-ui-overlays-772fce2`，release 为 `/opt/nvidia-router-releases/20260825-ui-overlays-772fce2`，镜像为 `nvidia-router:deploy-20260825-ui-overlays-772fce2`。
- 回滚点为 `/opt/nvidia-router-releases/20260824-ui-consistency-08eb1c9` / `nvidia-router:deploy-20260824-ui-consistency-08eb1c9`；切换前备份为 `/opt/nvidia-router-releases/20260825-ui-overlays-772fce2/backups/predeploy-20260825-ui-overlays-772fce2/router.db`，大小 9,494,528 字节，权限 `600`，属主 `10001:10001`，SHA-256 为 `9d363345872d8c101863832178d0068638db52a83d713a2b31b68d4fd022f495`。
- 发布后 app 使用目标镜像，`running/healthy`、重启 0、OOM false；3756 live/ready、代理池 `18080/healthz`、OpenCodeFree `6020/healthz`、根页和公网 live 均 200；新静态资源 `/assets/index-CMOvBKXD.js`、`/assets/index-D5M_XNC2.css` 均 200；匿名 `/v1/models` 与 `/metrics` 均 401；3756/18080/18081/6020 监听正常；部署后错误签名计数为 0。
- 管理员烟测通过：登录 200，会话访问 models/settings/runtime summary/access-keys 均 200，注销 204。未执行真实模型请求、代理轮换或 CONNECT 矩阵；公网 HTTP 明文风险保持不变。

## 18. 2026-08-25 模型能力评测与修复

- 新增可复用探针 `scripts/test/capability_eval_remote.py`（经 `remote_exec.py --stdin-env` 注入密码，在 hangzhou2-2 机内跑），判分全部程序化：沙箱执行生成代码、约束正则、needle 回取；本地 Windows 无 spawn 沙箱时走 `_sandbox_inline` 回退。
- 管理员密码：环境变量 `NVIDIA_ROUTER_ADMIN_PASSWORD` 已失效（401）；当前有效值在项目根 `.env` 的 `NVIDIA_ROUTER_INITIAL_ADMIN_PASSWORD`。
- 工具 501 根因是路由器门控（`tools_status != supported` 即拒，internal/modelcatalog/capabilities.go:179）。生产 detailed 探测入口：`POST /admin/api/model-test-jobs {"model_ids":[...],"mode":"concurrent"}`，会自动写回 reasoning/tools 状态。实测 llama tools=unsupported（真实不支持）；nemotron-free 与 x-preview 反复探测均 unknown——`service_probe.go` 的 `marshalProbeToolsBody` 用 `tool_choice=required`，这两个网关模型疑似只响应 auto+强提示，属探针局限，不要据此断言模型不支持工具。
- 元数据修复：llama(id=35) PATCH `reasoning_zero_allowed=true` 后 `reasoning_effort=none` 从 501 变 200（levels=["none"] 且 zero_allowed=false 时 nearestLevel 必失败）。
- 探针假阴性已修：reasoning 模型在小 max_tokens 下会吃光预算产出空回复——stability 32→512、json_mode 256→1024、tools_parallel 512→1024；多函数代码块需执行式函数选择（同名 arity 的 helper 会遮蔽目标函数）。
- 第二轮复测要点：kimi-k3 恢复可用且 needle/IF/推理/补测代码全对（偶发 502）；hy3 稳定性 3/3 exact OK；nemotron json_mode 仍输出超长非精确 JSON（真实弱点）。

## 19. 2026-08-25 设计令牌提交与重新部署（ccc725d）

- GitHub `main` 已推送至 `ccc725d`（`feat: refine design tokens and add capability eval probe`）：前端五层字体刻度（新增 caption/metric/mono）、间距半步与 data 圆角 token、panel-inset 描边、模型表截断提示、侧栏排版修正；含能力评测探针、2026-08-24 复测报告与设计文档；发布前门禁全过（go vet/test、typecheck、291 前端单测、dist 闭包 41/41）。
- 标准版本 `20260825-design-tokens-ccc725d`，release `/opt/nvidia-router-releases/20260825-design-tokens-ccc725d`，镜像 `nvidia-router:deploy-20260825-design-tokens-ccc725d`；切换前备份 `backups/predeploy-20260825-design-tokens-ccc725d/router.db`（10,059,776 字节，600）；回滚点 `20260825-ui-overlays-772fce2`。
- 首次构建失败：镜像加速器瞬时无法解析 `golang:1.24.0-bookworm` 元数据；远端直接 pull 精确 tag 后重试即成功。教训：加速器抖动先补拉精确 tag 重试，不改 Dockerfile、不换版本号。
- 发布后验证：app `running/healthy`、重启 0、OOM false；live/ready、18080/6020 healthz、根页与新资源 `index-CweVNl6N.js`/`index-CIPy-lLW.css`、公网 live 均 200；匿名 `/v1/models` 与 `/metrics` 401；schema 44、enabled 模型 10；近 5 分钟错误签名 0。管理烟测：登录 200、受保护 API 全 200、注销 204。

## 20. 2026-08-25 P0-P3 全量优化提交（5da18ad）

- 提交 `5da18ad`（`fix: resolve tool-gate deadlock, reasoning starvation, and proxy exit stickiness`），基于 2026-08-25 能力评测与四份测试报告的根因修复：
  - **工具门控死锁（P0）**：`capabilities.go` validateRequirements 放行 `tools_status=inferred`（运营/能力注册表显式声明；unsupported 与 unknown 仍拒绝）；探针改两段式（required → auto+强提示，tools 探针 max_tokens 16→256），unsupported 需两种形态都明确阴性。kimi-k3/minimax-m3/nemotron-free/x-preview 恢复 Agent 可用。
  - **小预算思考饥饿（P0）**：`AutoReasoningSpec(profile, outputLimit)` 注入阶梯——≤128 token 注入 none（无 none 可表达则跳过注入，绝不落入正档位）、129–511 最便宜正档位、≥512 维持 medium 上限；`capThinkingBudget` 增加绝对保留 `limit−32`（limit≤32 不封顶）。两个协议包均接线，Responses 的 max_output_tokens 已在 Parse 时改名所以直接读 chat["max_tokens"]。
  - **proxy_rejected 出口粘滞（P1）**：CONNECT 被拒的出口现在 `Retire(RetireReasonProxyRejected)` 计入池失败计数并丢弃缓存 transport，消除同会话反复拨同一坏出口导致的成簇快速 502；router 层短路语义保持不变。
  - **启动期 profile 一致性检查（P1）**：`CountUnexpressibleReasoningProfiles` 启动时告警 llama 形态（levels=[none]+zero_allowed=false）模型。
  - **OCF 流式瞬态重试（P2）**：首字节未写出前允许一次 500ms 重试（firstWriteTracker 跟踪）；owned_by 按 provider 区分；capability 501 写入专用 error_code。
  - **常态化探测（P3）**：迁移 045 加 `capability_probe_enabled`（默认关）+ `capability_probe_interval_hours`（默认 24），CapabilityProbeRunner 串行跑 detailed probe 自动回写。
- 测试要点：探针 fake 用 `chatResponses []string` 按调用重放；runner 测试必须给 discoverer 设 `chatResponse` 兜底否则第二个 chat 模型的 base+reasoning 探针会因空 choices 失败导致调用数不符；runner 已改为纯串行（无并发），测试确定性依赖 List 的 public_id 排序。

## 21. 2026-08-25 P0-P3 确认性重新部署（227061b）

- GitHub `main` 与部署源码基线均为 `227061b`；发布前工作树干净，`go test ./...`、`go vet ./...`、`git diff --check` 通过。
- 使用标准 `python scripts/deploy/deploy_remote.py 20260825-redeploy-227061b` 发布到 `/opt/nvidia-router-releases/20260825-redeploy-227061b`，镜像为 `nvidia-router:deploy-20260825-redeploy-227061b`；运行时 `.env` 与 deploy override 从现网 Release 继承。
- 首次构建因远端 Docker 镜像加速器对 `node:22.12.0-bookworm-slim` 返回 `not found` 失败，期间未停止 app、未生成备份、未切换现网；通过远端精确拉取该 tag 后重试成功。不要因此修改 Dockerfile、换基础版本或复用失败版本。
- 回滚点为 `/opt/nvidia-router-releases/20260825-design-tokens-ccc725d` / `nvidia-router:deploy-20260825-design-tokens-ccc725d`。切换前备份为 `/opt/nvidia-router-releases/20260825-redeploy-227061b/backups/predeploy-20260825-redeploy-227061b/router.db`，大小 10,391,552 字节，权限 `600`，UID/GID `10001:10001`，SHA-256 为 `87a7872b1bde68253100dec58ceab328a0ba7cf385882471c2ce12b82c56e878`。
- 发布后 app 使用目标镜像，`running/healthy`、重启 0、OOM false；schema 45、enabled 模型 11 个；3756 live/ready、18080/6020 healthz、根页均 200；匿名 `/v1/models` 与 `/metrics` 均 401；3756/18080/18081/6020 监听正常；临时归档已清理。
- 管理烟测通过：登录 200、鉴权 metrics 200、reasoning 接受请求 2/2 返回 200、预算协调请求 200、注销和临时 Key 清理由脚本 finally 执行。未执行完整模型矩阵、代理轮换或 CONNECT 矩阵；公网 HTTP 明文风险保持不变。

## 22. 2026-08-25 渠道状态美学重构与重新部署（b8e83c2）

- GitHub `main` 与部署源码基线均为 `b8e83c2`（`feat: redesign channel status with Claude and Codex aesthetics`）：
  - 前端融合 Claude 官方温润排版（暖白卡片、微光呼吸状态胶囊、清晰标题层级）与 Codex 官方精密遥测（4 联 KPI 态势概览看板、交互式状态过滤胶囊条、即时搜索框、4 列对齐核心指标网格与 Uptime Bar 悬停时间线）。
  - 补齐前端 `capability_probe_enabled` 运行时设置契约与测试用例，全量 44 套件 292 个前端单测全部 PASS，88/88 对比度配对合规，vue-tsc/eslint 0 错误。
- 使用标准脚本 `python scripts/deploy/deploy_remote.py 20260825-channel-status-b8e83c2` 发布到 `/opt/nvidia-router-releases/20260825-channel-status-b8e83c2`，镜像为 `nvidia-router:deploy-20260825-channel-status-b8e83c2`。
- 回滚点为 `/opt/nvidia-router-releases/20260825-redeploy-227061b` / `nvidia-router:deploy-20260825-redeploy-227061b`。切换前备份为 `/opt/nvidia-router-releases/20260825-channel-status-b8e83c2/backups/predeploy-20260825-channel-status-b8e83c2/router.db`，大小 10,518,528 字节，权限 `600`，属主 `10001:10001`。

## 23. 2026-08-25 渠道状态精修重构与重新部署（3f3ceae）

- GitHub `main` 与部署源码基线均为 `3f3ceae`（`feat: optimize channel status card with Claude and Codex telemetry aesthetics`）：
  - 优化卡面结构：双同心呼吸光晕指示灯、SLA 与延迟速度评级微标签、成功/异常分项拆解、60-slot Uptime Bar 跟随 Tooltip 悬浮提示与自适应对齐。
  - 新增 `/admin/model-health` 至 `/admin/channel-status` 的平滑路由别名重定向。
  - 前端 44 套件 292 个测试 100% PASS，vue-tsc/eslint 0 错误，静态闭包 41/41 完整。
- 使用标准脚本 `python scripts/deploy/deploy_remote.py 20260825-channel-status-3f3ceae` 发布到 `/opt/nvidia-router-releases/20260825-channel-status-3f3ceae`，镜像为 `nvidia-router:deploy-20260825-channel-status-3f3ceae`。
- 回滚点为 `/opt/nvidia-router-releases/20260825-channel-status-b8e83c2` / `nvidia-router:deploy-20260825-channel-status-b8e83c2`。切换前备份为 `/opt/nvidia-router-releases/20260825-channel-status-3f3ceae/backups/predeploy-20260825-channel-status-3f3ceae/router.db`，大小 10,559,488 字节，权限 `600`，属主 `10001:10001`。
- 远端按指令通过离线 CLI 完成管理员密码重置（密码未写入文件或日志）。
- 部署后验证：
  - 容器 `nvidia-router-app-1` 状态 `Up (healthy)`、重启 0；
  - 端点 `http://127.0.0.1:3756/health/live` 与 `http://127.0.0.1:3756/health/ready` 返回 200；
  - 静态资源 `/admin/` 正常返回 200；
  - 管理员认证：使用配置管理员密码 `POST /admin/api/auth/login` 返回 200 `authenticated: true`，获取 Session Cookie 请求 `/admin/api/model-health/summary` 正常返回 11 个白名单模型遥测数据。

## 24. 2026-08-25 15 分钟 vibe 诊断与健康探测竞态修复

- 本轮限定在国内目标机，未重启、未部署、未修改远端运行配置；远端仍使用 `nvidia-router:deploy-20260825-channel-status-3f3ceae`。管理员登录、匿名 metrics `401`、鉴权 metrics `200`、容器健康与版本化 Release 均复核通过。
- 日志证据分为两类：`validation_all_failed`、`proxy_rejected`、`proxy_transport_retired`、`transport_failed` 是当前上游代理出口波动；公网监听产生的明文 HTTP 安全告警是已知配置风险；另有一次定时模型健康探测因 `model_health_probes.model_id` 外键失败，涉及已被删除的 `z-ai/glm-5.2` 与 `deepseek-ai/deepseek-v4-flash-0731`。
- 根因：model-health 使用 reader 快照列出模型，探测并发执行期间模型可被管理端删除；writer 随后记录事件时目标模型行已经不存在，SQLite 合法触发外键约束，但服务把该预期竞态升级成整轮告警。修复在 `internal/modelhealth`：将 SQLite 外键错误归类为 `ErrModelDeleted`，健康循环跳过该过期快照；其他数据库错误保持失败。新增回归测试覆盖“探测期间删除模型”。
- 本地验证：`go test ./...`、`go vet ./internal/modelhealth ./internal/modelcatalog`、`python -m unittest scripts/test/test_vibe_eval_remote.py`（14 tests）和 `git diff --check` 通过。相关模型健康/目录单测也单独通过。
- 模型验证：管理员探针中 Nemotron reasoning 接受与高预算内容保留均返回 `200`；StepFun 返回 `502 upstream_proxy_unavailable`，按“只测稳定模型”排除。缩短矩阵脚本因其门控阶段仍使用全目录默认重复/预算，运行 244 秒达到硬超时，未产生可用完整矩阵结果，不得据此宣称本轮工具、长上下文和稳定性全覆盖；此前 2026-08-22/24 的已验证 vibe 结果继续以对应报告为准。
- 临时测试 Key 清理探针确认测试标签残留数为 0；本轮未生成发布版本号、数据库备份或回滚点。后续若需要让远端日志告警消失，必须按标准唯一版本发布此本地修复后再做一次短健康探测。

## 25. 2026-08-25 15 分钟 vibe 多维评测与日志排障

- 稳定模型筛选应先对当前启用 Chat 目录逐模型执行两次最小请求，再只对连续 200 的模型进入矩阵；`stepfun-ai/step-3.7-flash` 的两次 35 秒客户端超时应在门控阶段排除，不能把它混入后续能力结论。
- Windows 上 `remote_exec.py --arg` 传 JSON 模型过滤器时，PowerShell 会吞掉未转义的双引号；优先使用支持 `--models` 的 `live-model-matrix.py`，或确保参数中的引号经过 PowerShell 保留。全目录门控会吞掉 15 分钟预算。
- 15 分钟内的可复用组合是：门控 2 次、思考强度、thinking 开关、stream、tools、约 8K 长输入、64/512/2048 输出、5 次重复，再对单个稳定模型补 2 个 AST-only 代码任务。结果只保留状态码、错误类别、finish、TTFT、长度、工具数量和 AST 布尔值，不保存响应正文或生成代码。
- 错误归类要分开：`upstream_proxy_unavailable`/`upstream_unavailable` 对应代理或 provider 瞬态；`upstream_protocol_error` 对应上游协议异常；客户端超时对应慢或无响应；`validation_all_failed`、`proxy_rejected`、`proxy_transport_retired` 是出口池证据，不能合并成模型协议失败。
- 模型健康探测的删除竞态根因是快照读取与事件写入之间模型行被删除；`internal/modelhealth/repository.go` 将 SQLite 外键错误映射为 `ErrModelDeleted`，`service.go` 跳过过期快照。`TestRunOnceIgnoresModelDeletedDuringProbe`、`go test ./...` 和 `go vet ./...` 是最小回归组合。当前修复仍在本地未提交状态，远端旧镜像未包含它。
- 模型目录未声明 `none` 时，评测脚本不要强行发送 `reasoning_effort=none` 后再把小预算空回复判成上下文失败；应按 `reasoning_levels` 选择档位或跳过显式 reasoning 字段。

## 26. 2026-08-26 模型健康探测竞态修复提交与重新部署（6d4b782）

- GitHub `main` 与部署源码 `HEAD` 均为 `6d4b782`（`fix: handle deleted model health probes`）；修复模型健康探测快照与管理端删除模型之间的 SQLite 外键竞态，补充删除确认、`ErrModelDeleted` 归类及其他数据库错误不吞掉的回归测试；测试报告已纳入 `docs/plans/2026-08-25-vibe模型思考工具上下文稳定性测试报告.md`。
- 使用标准 `python scripts/deploy/deploy_remote.py 20260826-model-health-race-6d4b782` 发布到 `/opt/nvidia-router-releases/20260826-model-health-race-6d4b782`，镜像为 `nvidia-router:deploy-20260826-model-health-race-6d4b782`；回滚点为 `/opt/nvidia-router-releases/20260825-channel-status-3f3ceae` / `nvidia-router:deploy-20260825-channel-status-3f3ceae`。
- 切换前数据库备份为 `/opt/nvidia-router-releases/20260826-model-health-race-6d4b782/backups/predeploy-20260826-model-health-race-6d4b782/router.db`，权限 `600`、属主 `10001:10001`，SHA-256 为 `6758b87e256ed21fa3a604c8b669589f567a730743c15b876bedc02832537087`。
- 发布后 app 实际使用目标镜像，容器 `running/healthy`、重启 0、OOM `false`；`/health/live`、`/health/ready`、代理池 `18080/healthz`、OpenCodeFree `6020/healthz`、根页和管理员页均返回 200；匿名 `/v1/models` 与 `/metrics` 均返回 401；`3756/18080/18081/6020` 监听正常。
- 远端只读数据库核验：`schema_migrations` 最大版本 45，enabled 模型 5 个。发布后 5 分钟日志未发现 panic、fatal、migration 或 foreign-key 错误；管理员 reasoning 与预算请求均返回 200，临时测试 Key 已由清理逻辑删除。
- 本地门禁通过：`go test ./...`、`go vet ./...`、19 个 Python 探针测试和 `git diff --check`；两轮代码审查无阻断问题。未将免认证健康检查或单模型结果宣称为完整模型矩阵、代理轮换或 CONNECT E2E。

## 27. 2026-08-26 hy3 稳定模型短测与评测器修复

- 远端真实短测严格限定 `opencode-free/hy3-free`，总耗时约 130 秒；门控两次连续 200，reasoning low/high、JSON、长输出均 200，20/20 stability 样本成功，TTFT 约 1.7-7.4 秒。
- 工具用例返回 501 `not_implemented`，模型目录为 `supports_tools=false` 且 `tools_status=unknown`，归类为模型能力限制而非路由器故障；context needle 因目录 `context_length=0` 跳过，未编造上下文窗口结论。
- 根因修复：`scripts/test/vibe_eval_remote.py` 的 `run_matrix` 仅在日志副本附加 `case`，返回记录缺失该字段，导致真实 stability 样本被汇总为 `samples=0`；现在普通、预算跳过和重试记录均保留 case。新增模型白名单、reasoning zero_allowed、inferred tools 与两条 case 归因回归测试。
- 本地服务修复仍未发布到远端：reasoning 空答案返回可重试的上游空响应，OpenCodeFree 首字节前瞬态 500/空 reasoning 自动重试，最终 500 映射 502；相关 Go 回归测试已通过。远端当前仍为 `nvidia-router:deploy-20260826-model-health-race-6d4b782`。
- 远端复核：容器 healthy、重启 0、OOM false，live/ready 均 200；`vibe-eval` 临时 Key 匹配残留 0。近期 `validation_all_failed`、`proxy_rejected`、`proxy_transport_retired` 仍是上游代理出口波动，未发现 panic、foreign-key 或协议错误。
- 验证组合：`go test ./...`、`go vet ./...`、21 个 Python 单测、`git diff --check` 均通过；未执行本地修复的生产部署、全模型矩阵、完整代理轮换或 CONNECT E2E。

## 28. 2026-08-26 Vibe 评测语义与流式空终答修复

- 根因一：两套评测器把 reasoning 字符当成用户答案，reasoning-only + `finish_reason=length` 被记为 `empty_response=false/ok=true`；修复为 content/tool call 才算可见输出，稳定性还必须精确匹配 `OK`。
- 根因二：`vibe_eval_remote.py` 的稳定性门控使用 16-token 输出预算，先前会把稳定 reasoning 模型在门控阶段误排除；门控改为 512，context needle 改为 512，并在未知 `context_length` 时执行命名的 8K lower-bound 探测，不写回或猜测窗口值。
- 根因三：Chat SSE 首事件快速路径把空 `content` 当语义事件，且 `[DONE]` 前无终态校验；新增空 content 拒绝、终态前的 reasoning-only/length 检查和 `upstream_empty_response` 流内错误。普通非流式、OpenCodeFree 重试和最终 500 映射修复保持不变。
- 最终短测只使用 `opencode-free/hy3-free`，约 125 秒：门控 2/2 200，reasoning low/high、JSON、长输出 200，8K lower-bound needle 命中；稳定性 17/20（85%），3 次为空终答且 `finish_reason=length`。工具 501 是目录 `supports_tools=false`/`tools_status=unknown` 的能力限制。
- 远端仍运行 `nvidia-router:deploy-20260826-model-health-race-6d4b782`，本地服务修复未部署、未重启；24 小时日志签名为 validation_all_failed 172、proxy_rejected 11、proxy_transport_retired 74、transport_failed 10，panic/fatal/foreign-key/upstream_protocol_error 均 0。代理签名继续按出口/provider 波动处理。
- 本地验证：`go test ./...`、`go vet ./...`、34 个 Python 单测、`gofmt -d`、`git diff --check` 通过。未完成全模型矩阵、Codex/Responses E2E、完整代理轮换或 CONNECT 专项验收。

## 29. 2026-08-26 稳定模型复现与评测器边界修复

- 远端只读核验仍为 `nvidia-router:deploy-20260826-model-health-race-6d4b782`，容器 `running/healthy`、重启 0、OOM false，Release 与版本化镜像完整；本轮没有部署、重启或改远端配置。
- 近 24 小时应用日志脱敏计数：`validation_all_failed=249`、`proxy_rejected=11`、`proxy_transport_retired=95`、`transport_failed=10`、`reasoning_starved_response=4`；未发现 panic、fatal、migration、foreign-key 或 `upstream_protocol_error`。前四类继续归为代理出口/provider 波动，思考饥饿是旧版本仍只告警未重试的证据。
- 仅对 `opencode-free/hy3-free` 执行一次有界短测，矩阵耗时 121.4 秒：门控 2/2 HTTP 200；25/26 请求 HTTP 200；8K lower-bound context needle 命中；工具 501 `not_implemented` 与目录 `supports_tools=false/tools_status=unknown` 一致；稳定性 16/20，4 次为 `finish_reason=length` 且无可见 content。该结果复现旧版本问题，不代表全模型通过。
- 评测器新增边界：声明 `reasoning_zero_allowed=false` 时过滤 `none`，声明 levels 无可表达档位时跳过显式 reasoning；稳定性要求完整匹配 `OK`；生成代码结果比较改用 `ast.literal_eval`，不再执行模型返回的表达式。服务端空终答、SSE 终态、OpenCodeFree 瞬态重试修复仍未发布到远端。
- 本地门禁：`go test ./...`、`go vet ./...`、31 个 vibe 单测、8 个 capability 单测、`gofmt -d`、`git diff --check` 均通过。若要验证服务端修复，必须先处理未提交改动，生成唯一版本号并按 `scripts/deploy/deploy_remote.py` 发布；发布会重启测试机 app，需遵守目标机确认规则。
- 当前 Windows 未安装 `gcc`，`go test -race` 在 `CGO_ENABLED=1` 下无法构建；未为本轮临时安装工具或改环境配置，race 结果保持未验证。

## 30. 2026-08-26 vibe 修复发布与稳定性确认

- 提交 `e3729c8` 已按标准脚本发布为 `/opt/nvidia-router-releases/20260826-vibe-debug-e3729c8`，镜像为 `nvidia-router:deploy-20260826-vibe-debug-e3729c8`；回滚点为 `20260826-model-health-race-6d4b782` / `nvidia-router:deploy-20260826-model-health-race-6d4b782`。
- 切换前备份为 `/opt/nvidia-router-releases/20260826-vibe-debug-e3729c8/backups/predeploy-20260826-vibe-debug-e3729c8/router.db`，权限 `600`、属主 `10001:10001`、大小 10,575,872 字节，SHA-256 为 `907e12992ad01aae0bca16d4b3ad119166df666d844929c6ce1635ab6eaa1ac8`。
- 发布后实际镜像与 Release 一致，容器 `running/healthy`、重启 0、OOM false；3756 live/ready、18080/6020 healthz 均 200，3756/18080/18081/6020 监听正常。
- 仅对稳定模型 `opencode-free/hy3-free` 做线上矩阵：服务修复上线后旧的 32-token 稳定性样本为 17/20，3 个 reasoning-only 流被正确转成 `upstream_empty_response`；评测稳定性预算提升到 512 后，矩阵耗时 145.9 秒，20/20 稳定性成功，reasoning low/high、JSON、8K lower-bound needle、长输出均 200，工具 501 仍为该模型能力限制。总测试时长未超过 15 分钟。
- 评测器的 512-token 稳定性修复在本地工作树中已验证红绿，线上通过 `remote_exec.py` 直接执行；它不改变已运行服务镜像。发布后 5 分钟应用日志未发现 panic、fatal、migration、foreign-key、`upstream_protocol_error` 或 `upstream_empty_response`，代理池仍有正常的 `validation_all_failed` 波动。

## 31. 2026-08-26 Cherry 全局 MCP 工具导致 501

- `AI_APICallError` 的直接根因不是 `reasoning_effort: none`：Cherry Studio 请求会附带全局 MCP 工具集合（本次 44 个工具）和 `tool_choice: auto`。解析后 `Requirements.Tools=true`，而 hy3 与 NVIDIA Nemotron 目录均声明 `supports_tools=false`，能力门控正确返回 501；不得为了消除错误静默删除工具字段或伪造工具成功。
- 同一时间窗口内的 501 均归为该能力不匹配；hy3 另有上游 429，OpenCodeFree 的其他探针出现 503 `Endpoint is unavailable`，分别属于上游限流和 provider 可用性问题，不应合并为能力门控故障。
- `reasoning_effort: none` 的语义是关闭思考，`ReasoningSpec.RequiresReasoning()` 不应要求 reasoning 能力；相关解析回归测试已覆盖。工具调用应关闭 Cherry 的全局 MCP 注入，或选择经过稳定性验证且明确支持工具的模型。
- 本地分支新增能力拒绝的具体能力类型和 API 可操作错误：工具请求返回 `model_capability_unsupported`、`param=tools`，提示移除 `tools/tool_choice` 或换用支持工具的模型；`errors.Is(ErrCapabilityUnsupported)` 兼容性保持。该改动尚未提交或部署，生产仍使用上一版本镜像。

## 32. 2026-08-26 请求日志持久化与生命周期修复发布（37c81bc）

- GitHub `main` 已推送到 `37c81bc8b3da9121065cecb420f936a5d25439c4`；本次增加请求能力摘要、日志缓冲深度/丢弃/flush 失败指标和迁移 046，并修复失败批次保留、ForceFlush context 传递、Stop 并发关闭及 context 取消后的写入门禁。
- 本地门禁通过：`go test ./...`、`go vet ./...`、前端 lint、typecheck、44 个 Vitest 套件/292 个测试、生产构建和 diff 校验；race 仍受 Windows 缺少 C 编译器限制，未安装工具或改环境。
- 按标准脚本发布到 `/opt/nvidia-router-releases/20260826-observability-37c81bc`，实际镜像为 `nvidia-router:deploy-20260826-observability-37c81bc`；旧回滚点为 `/opt/nvidia-router-releases/20260826-vibe-debug-e3729c8` / `nvidia-router:deploy-20260826-vibe-debug-e3729c8`。
- 切换前数据库备份为 `/opt/nvidia-router-releases/20260826-observability-37c81bc/backups/predeploy-20260826-observability-37c81bc/router.db`，大小 10,993,664 字节，权限 `600`，属主 `10001:10001`，SHA-256 为 `080676e98b913e8153390b198584964765060336352b34f302e291bab4747112`。
- 发布后 app 实际 `running/healthy`、重启 0、OOM false；schema 最大版本 46，`request_logs.requested_capabilities` 存在；3756 live/ready、18080/18081/6020 healthz、根页及新 JS/CSS、公网 live 均 200；匿名 models/metrics 401；管理登录、只读 API、鉴权 metrics 200，登出 204；三项日志缓冲 Prometheus 指标已出现。
- 本轮未执行真实模型请求、全模型矩阵、代理轮换或 CONNECT E2E；仅做部署和管理/健康验证，未超过 15 分钟模型测试约束。公网 HTTP 明文风险及代理出口波动告警保持既有状态。
## 33. 2026-08-27 Codex/Cherry 兼容性与项目精简审计

- 本轮只读分析与真实矩阵均限定在国内 `hangzhou2-2`，线上版本仍为 `20260826-vibe-codex-cherry-288be38`；未部署、未重启、未改远端配置、白名单或数据库。详细结论见 `docs/项目精简与优化分析报告-2026-08-27.md`。
- 直接根因不是单纯 HTTP 转发失败，而是三类问题叠加：模型工具能力元数据的 `false/unknown` 硬门控会在上游调用前返回 501；Responses 是 stateless Responses→Chat 转换器，按设计拒绝 hosted/stateful/background 能力；代理出口/provider 瞬态会产生 502/503/超时和流截断。三类必须分开统计。
- 当前稳定模型筛选必须先做两次最小请求；StepFun 两次 35 秒超时应排除深测。24 小时模型健康仅用于筛选，不等价于所有能力通过。Chat 矩阵和 Responses 矩阵需分别统计，不能合并成一个成功率。
- 2026-08-27 Luna 矩阵安全标量：Chat 65 条中 200=50、501=2、502=12、timeout=1，稳定性子样本 10/15；Responses 41 条中 200=39、502=2，稳定性 14/15。OCF Nemotron 仍有 `upstream_protocol_error`/`upstream_stream_truncated`、JSON 不精确；NVIDIA Nemotron 的高档 reasoning、JSON/context 与长输出受出口波动影响；hy3 基础/Responses 稳定但工具被目录状态阻断。
- 所有启用模型的 `context_length` 仍为 0/未声明；不能从一次约 8K 输入成功推断 32K 或 128K。小 `max_tokens` 下思考吃光预算导致 `finish=length` 或空可见内容时，应按模型 reasoning profile 解释，不得直接判作上下文失败。
- Cherry 本机 MCP 初始化错误（参数缺失、OAuth/session 失效、连接拒绝）与路由器 501/502 是不同来源；排障时按客户端 MCP、路由器能力门控、上游代理/provider 三层对齐 request id 和错误类别。
- Codex 普通 Responses、function tool、混合工具过滤路径已验证可用；非 function hosted tools、stateful response、background 等仍按设计返回结构化 400。不得把 custom provider 宣称为完整 Codex hosted tool 实现。
- 精简方向：保留国内 XApi/CONNECT 代理池、Key 池、Responses 转换、鉴权审计；合并 Chat/Responses 共同执行器、OCF 两套 retry/status 生命周期和多后台 writer/scheduler。能力状态收敛为单一 `CapabilitySnapshot`，未知、推断、已验证、明确不支持必须可区分。
- 复测方法：使用现有 `model_whitelist_audit_remote.py`、`live-model-matrix.py` 和 Responses 探针；PowerShell 传模型过滤器优先使用 `--models`，避免 JSON 引号丢失；每项控制在 15 分钟内，报告只保留状态码、错误类别、finish、TTFT、长度、工具数量/参数合法性和 AST 布尔值。
- 本轮没有新增发布版本、数据库备份或回滚点；后续若实施重构或部署，必须重新生成唯一版本号，并记录镜像、Release、Git SHA、备份、回滚点和未完成验证项。继续禁止在 memory、日志、脚本或报告中记录任何凭据、完整上游 URL、响应正文或生成代码。

## 34. 2026-08-28 数据面执行核心两阶段收敛（未部署）

- 分支 `codex/optimize-executor-20260828` 仅包含本地代码/测试/设计文档，未提交、未部署、未重启远端服务；不得把本地通过当作线上验证。
- 第一阶段新增 `internal/httpapi/v1/opencodefree_execution.go`，Chat 与 Responses 的 OpenCodeFree 路径共用一次重试、500ms 等待、404/429/5xx/436 状态映射、首字节提交门控和幂等 body 关闭；删除两套旧循环与重复状态映射。为保持旧 OCF stream 空流/[DONE] 公开契约，handler 不新增 `primeSSE`；真实流错误仍由原 `streamResponse*` 负责。
- 第二阶段新增 `internal/httpapi/v1/request_prepare.go`，Chat/Responses 共用模型/stream 观测、Resolve、能力错误码、MarshalForWithOptions、reasoning source/effective wire 观测；协议解析、Responses 转换、provider 分支和模型门控保持各自边界。准备器直接借用协议 marshaller 返回的 body，不额外复制大请求。
- 可复用门禁：`go test ./internal/httpapi/v1 -run 'TestOpenCodeFreeExecution' -count=1`、`go test ./internal/httpapi/v1 -run 'Test(PrepareModelRequest|Chat|Responses)' -count=1`、`go test ./internal/httpapi/v1 ./internal/router ./internal/upstream/opencodefree -count=1`、`go test ./...`、`go vet ./...`、`git diff --check`；本轮均通过。
- 第二阶段新增测试应避免格式化包含 `sync.Mutex` 的 `observability.RequestState`（会触发 vet copylocks）；只断言并打印标量观测字段。远端下一步仍需按国内 `hangzhou2-2` 流程，先健康/端口/数据库检查，再稳定模型两次门控和短 Chat/Responses 矩阵；不得复用旧版本号。

## 35. 2026-08-28 Responses OCF 流写入降级与最终门禁

- Responses OCF 流经 `firstWriteTracker` 包装后必须提供 `Unwrap`，以透传 `ResponseWriter` 的可选能力；SSE 遇 `ErrWriteDeadlineUnsupported` 时应保留 watchdog 并降级为普通 `Flush`，不能把能力缺失误报为 500。
- 本轮 `go test ./... -count=1`、`go vet ./...`、`git diff --check` 均通过；未部署、未重启。不得记录凭据、完整 URL 或响应正文。

## 36. 2026-08-29 执行核心收敛发布

- 本次部署源码提交为 `7d6bbb3`；标准 Release 为 `/opt/nvidia-router-releases/20260829-executor-consolidation-7d6bbb3`，镜像为 `nvidia-router:deploy-20260829-executor-consolidation-7d6bbb3`。
- 回滚点为上一版本 `20260826-vibe-codex-cherry-288be38`；切换前数据库备份位于 `/opt/nvidia-router-releases/20260829-executor-consolidation-7d6bbb3/backups/predeploy-20260829-executor-consolidation-7d6bbb3/router.db`，SHA-256 为 `78218e062322d99524365bcc625a15ef0362613df43fb5951276c7a0aac58fb4`，权限/属主为 `600/10001:10001`。
- 发布后健康检查、关键端口、静态资源和匿名鉴权均通过；管理员登录烟测返回 401，未重置密码。真实模型/代理矩阵未执行，公网 HTTP 明文警告保持。

## 37. 2026-10-01 OCF 候选只保留 free 发布（eadee8a）

- 提交 `eadee8a`（`feat: surface only free OpenCodeFree models in candidate discovery`）在分支 `codex/optimize-executor-20260828`，未推送 GitHub。`DiscoverCandidates` 过滤非 `-free` 后缀的 OpenCodeFree 模型；`SyncOpenCodeFreeModels` 过期同步仍用全量列表。本地门禁：`go test ./...`、`go vet`、`gofmt`、`git diff --check` 全过。
- 标准版本 `20261001-ocf-free-candidates-eadee8a`，Release `/opt/nvidia-router-releases/20261001-ocf-free-candidates-eadee8a`，镜像 `nvidia-router:deploy-20261001-ocf-free-candidates-eadee8a`；回滚点 `20260829-executor-consolidation-7d6bbb3`。
- 切换前备份 `backups/predeploy-20261001-ocf-free-candidates-eadee8a/router.db`（19,349,504 字节，600，10001:10001）。
- 线上真实验证：网关 `/v1/models` 共 83 个模型（71 个非 free）；admin 候选接口返回 11 个 OCF 候选全部 `-free`（与网关 free 清单一致）+ 81 个 NVIDIA 候选，`nonfree-leak none`；管理员登录/注销 200/204（本地 `.env` 的 `NVIDIA_ROUTER_INITIAL_ADMIN_PASSWORD` 当前有效）；schema 46、白名单 8 条、启用 1 条（运营状态，未改动）；免认证健康/鉴权边界、静态资源、panic/fatal=0 全过。
- 部署事故与教训：首次 `deploy_remote.py` 卡死在 docker build 步骤——脚本 `run()` 先 `stdout.read()` 再读 stderr，BuildKit 冷构建时大量进度写 stderr 塞满 SSH 通道缓冲造成 paramiko 死锁，而镜像实际已构建成功。处置：先 SSH 只读核对远端实际步骤（镜像已存在、app 未切换、无备份目录），确认后停本地任务同版本重跑（构建全缓存、stderr 极小）顺利完成。同类卡死先查远端真相再决定重跑，不要先动现网。
- 未执行真实模型请求、代理轮换或 CONNECT 矩阵；公网 HTTP 明文风险保持不变。

## 38. 2026-10-01 候选页直接测试模型发布与 OCF 探测预算修复（12bd9ad → af5b566）

- 功能：候选列表行级"测试"按钮 + "测试选中候选"批量（前端并发 3，可停止）；后端 `POST /admin/api/models/candidates/test`（`{provider, upstream_id}` 同步返回 `{status, duration_ms, error}`），`Service.TestCandidate` 复用 `TestModelAuto` 只读探测路径（NVIDIA 随机多 Key、OCF 直连网关），零持久化；端点并发闸 4。提交 `12bd9ad`（含 dist 重建），本地门禁 go test/vet、前端 295 vitest、vue-tsc、build、eslint 全过。
- 两轮部署：`20261001-candidate-probe-12bd9ad`（回滚点 `20261001-ocf-free-candidates-eadee8a`）→ 首轮真实验证发现 OCF 探测误杀 → 修复 `af5b566` → `20261001-candidate-probe-fix-af5b566`（回滚点 `20261001-candidate-probe-12bd9ad`）。两轮切换前备份均约 19,349,504 字节、600、10001:10001，健康/边界检查全过。
- **根因教训（max_tokens=1 空内容误判）**：`testOpenCodeFreeModel` 用 `max_tokens:1`，free 档模型（如 space-bunny-free）把唯一 token 耗尽后返回 200 空内容（finish=length），`ValidateNonstreamChat` 判 ErrEmptyResponse → 上报"上游多次未返回可用响应"，**实际可用的模型被误判不可用**；同文件 NVIDIA 基础探测与 OCF detailed 探测都用 `modelProbeMaxTokens`(16)。修复：OCF 基础探针预算对齐 16。同类"HTTP 200 但空内容"必须看 content 而非状态码。
- 线上真实判定（修复后）：`space-bunny-free` → success 11.8s（当前 11 个 free 候选中唯一可用）；`mimo-v2.5-free`/`nemotron-3-ultra-free` → 模型测试失败 403 FreeTierError（网关直连证实）；`deepseek-v4-flash-free` → 模型测试失败 400 Model is unavailable（已下线）；NVIDIA `z-ai/glm-5.3` → 90s 不可达（3×30s 探测窗口内无首字节，候选无 per-model 超时 override，慢模型会误报——已知语义限制，与白名单只读测试一致）。免认证边界、管理员登录/注销、非法输入 400 全过；容器 0 重启、panic/fatal 0。
- 上游环境事实（非本次改动回归）：网关 `/api/monitor` 24h 成功率 1.29%（49/3788），网关→OpenCode 经内部星空池基本不可用已超 24h；路由器内置池 `healthy=26` 全靠 TTL 宽限维持，`validation_all_failed` 持续。OCF 网关主机名为单标签容器名 → OCF client `local=true` 直连不走池；远端 `.env` 的 `NVIDIA_ROUTER_INITIAL_ADMIN_PASSWORD` 是陈旧值（登录 401），有效值在本地 `.env`。

## 39. 2026-10-01 OCF 全组不可用根因诊断（上游政策拒绝，非本地故障）

- 现象：OCF 网关 24h monitor 成功率 1.44%（3755 请求仅 54 成功），失败分类 `upstream_http_error=3492`（94%）、`client_disconnected=190`；网关/代理池/路由器容器与健康端点全部正常。网关唯一调用方为路由器所用 key（`key:890117e0`）。
- 根因是上游 OpenCode 收紧 free tier 准入，网关链路本身是通的（`space-bunny-free` 实测 200/1.7s）。11 个 free 候选逐个实测分类：
  - **6 个 FreeTierError（403）**：`OpenCode's free tier can only be used from within OpenCode`——上游按客户端来源策略拒绝，覆盖 mimo-v2.5/mimo-v2.6-flash/longcat-2.5/ling-3.0/nemotron-3-ultra/nemotron-3.5-lightning；
  - **2 个 RegionError（403）**：muse-spark-1.2/1.3-contributor `This model is not available in your country`（中国出口被封）；
  - **1 个已下线（400）**：deepseek-v4-flash-free `Model is unavailable`；**1 个网关内部错误（500）**：jev-1.13-free；
  - **1 个可用**：`space-bunny-free`。
- 路由器白名单 4 个 OCF 模型（deepseek-v4-flash-free/jev-1.13-free/ling-3.0-flash-fin-free/longcat-2.5-preview-free，id 38-41）**全部 enabled=false**；渠道状态页 OCF 全红的直接输入是 modelhealth 对这些模型的探测全部撞上 FreeTierError。
- 旁证：monitor 中 nemotron-3-ultra-free/hy3-free/x-preview-f-free 各 ~1013 次/24h（每 ~85s 一轮）全失败，但三者已不在网关模型列表且非当前白名单——疑似 9router 残留配置在用同一网关 key 轮询；hy3/x-preview 已下线，继续轮询纯浪费。
- 诊断方法（可复用）：网关侧带 key 逐模型 `POST /6020/v1/chat/completions`（max_tokens=32）拿错误分类；admin API `/admin/api/models` 响应包在 `data` 键下、模型行用 `upstream_id` 字段（非 model_id）；paramiko 下发 `python3 -` 时 stdin 会把注入的密码行当脚本执行，必须 SFTP 上传脚本+stdin 只传密码。
- 结论：除非上游恢复或改用"从 OpenCode 客户端内"的真实来源，OCF 分组无法恢复；运营上仅 `space-bunny-free` 值得保留测试入口。
- **longcat 复核（同日应户质疑重测）**：`longcat-2.5-preview-free` 以 5 种形态（非流式 32t/512t、流式、带 system、重试）复测全部 403 FreeTierError（~400ms 稳定快速失败），非偶发非请求形态问题；该拒绝是上游服务端按模型执行的策略（公开讨论见 Reddit r/opencode "Opencode free tier can only from within Opencode"），官方客户端内可用≠API 可用。space-bunny 同为 free 但上游未对它执行该策略（网关/出口/key 全同，唯一变量是模型）。网关已带 `x-opencode-client: desktop` 仍被拒，说明上游校验更完整客户端身份；错误前缀 "Error from provider (Console)" 表明拒绝发生在上游 provider 控制台侧。
- **9router 实际状态（2026-10-01 只读核查）**：进程 Up 6 weeks、`/api/health` OK，但 266 个 NVIDIA 连接全部 `isActive=0`，唯一启用的 `opencode-go` 连接 key 自 2026-08-05 起 401（`Invalid API key`，testStatus=unavailable），usageHistory 最后一条成功请求停在 2026-08-05——**实际不可用**。上一轮"疑似 9router 轮询 3 个已删 OCF 模型"的归因有误：9router 无指向 6020 的连接，该 ~1013 次/24h/模型流量来自持有网关 key 的路由器侧（`capability_probe_enabled=false` 已排除该候选，具体组件未定位）；路由器 modelhealth 正活跃探测（last_probe 分钟级新鲜），longcat 连续失败 173 次 `probe_failed`。
- **OCF 403 精确根因（2026-10-01 经 9router GitHub issues 逆向 + 线上对照实验确证）**：上游 Zen free tier 自 2026-09-16 起做**四层客户端指纹校验**，缺一即 403：(1) UA 必须 `opencode/<ver>` 且 ≥1.17.0；(2) 会话头 `x-opencode-session` 必须匹配 `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`（30 字符）；(3) 请求必须携带 `bash/glob/grep/read` 四件套 no-op 工具声明；(4) 必须 `stream:true` 上游（非流式可强制 SSE 上游后聚合回 JSON）。对照实验（同模型同出口）：longcat 带完整指纹 → HTTP 200 SSE 正常；裸请求 → 403。space-bunny 特例可用是因上游对它不执行该校验。RegionError（muse-spark）是独立的 CN 出口地区门，与指纹无关。9router master 已内置四层伪装（社区 PR #4105/#4132 被 close 未合并，维护者自实现）；#4182 有自称 OpenCode 团队的 takedown 要求（身份未证实）。网关侧只发 `Authorization: Bearer public` + `x-opencode-client: desktop` 即缺全部四层；星空池入站认证格式为 `Basic base64("proxy:"+authKey)`（user 固定 `proxy`）。
- **OCF 指纹修复已部署（2026-10-01）**：把四层客户端指纹移植进 `opencode-free-proxy` 网关（改 `src/upstream.js` + `src/config.js`，改动已上传远端；修改前源码备份在 `/opt/opencode-free-proxy/src.bak-pre-fingerprint-20261001`）：(1) UA `opencode/1.18.31`（env `OPENCODE_USER_AGENT` 可覆盖）；(2) chat 请求生成 `x-opencode-session: ses_<12hex><14base62>`；(3) 无四件套 `bash/glob/grep/read` 声明时注入 no-op 工具（调用者工具保留在前）；(4) 上游强制 `stream:true`，非流式调用方在网关内把 SSE 聚合为单个 `chat.completion` JSON（含 content/reasoning/tool_calls 按 index 聚合/usage/finish_reason；无帧 SSE 抛 `UPSTREAM_ERROR`→502；上游非 2xx 原样透传）。`observeResponse` 的 JSON usage 路径与聚合后响应兼容，monitor/路由器侧无需改动。
- 部署方式：scp 覆盖远端 `src/` 两文件 → `docker compose -f docker-compose.yml -f docker-compose.proxy.yml build && up -d`（镜像重建、容器 healthy）。本地 mock fetch 冒烟 6 用例全过（归档于远端 `scripts/test/fingerprint-smoke.mjs`；容器镜像不含 scripts/，须在仓库 checkout 上跑）。
- 线上真实验证（部署后）：**8/11 free 模型恢复**——longcat 非流式+流式、nemotron-3-ultra、mimo-v2.5、nemotron-3.5-lightning、mimo-v2.6、space-bunny（回归）、longcat+调用者工具全部 200；剩余 3 个为独立模型级问题：ling-3.0 上游 `Endpoint is unavailable`(400)、deepseek-v4-flash-free 已下线(400)、muse-spark CN 出口 RegionError(403)。路由器→网关全链路（候选测试端点）longcat/nemotron success；mimo-v2.5 在路由器 16-token 探测下仍报"上游多次未返回可用响应"（探测窗口对慢响应模型的已知语义限制，网关直连 200）。回滚：恢复 `src.bak-pre-fingerprint-20261001` 后重建镜像即可。
- 遗留风险：上游可能轮换指纹特征（届时 403 回归，改 `opencodeUserAgent`/`FINGERPRINT_TOOL_NAMES` 即可）；#4182 takedown 风险未解除；ling-3.0/deepseek-v4-flash-free/muse-spark 为上游侧问题，无法本地修复。

## 40. 2026-10-01 OCF 探测预算修复发布与端到端验证（fdbe7bf）

- 提交 `fdbe7bf`（`fix: give OpenCodeFree base probes a template-token-proof budget`，分支 `codex/optimize-executor-20260828`）：OCF 基础探测与 detailed base 探测的 max_tokens 由 `modelProbeMaxTokens`(16) 改为新增 `ocfProbeMaxTokens`(256)；NVIDIA 探测保持 16。根因：mimo-v2.5-free 在 16-token 窗口下输出被不可见 chat 模板 token 吃光，200 + 空 content 被判"上游多次未返回可用响应"（可用模型误杀）；nemotron 对照组 16 tokens 可吐可见文本所以通过。门禁：go vet、`go test ./...` 全过。
- 标准发布版本 `20261001-ocf-probe-budget-fdbe7bf`，Release `/opt/nvidia-router-releases/20261001-ocf-probe-budget-fdbe7bf`，镜像 `nvidia-router:deploy-20261001-ocf-probe-budget-fdbe7bf`；回滚点 `20261001-candidate-probe-fix-af5b566`。切换前备份 `backups/predeploy-20261001-ocf-probe-budget-fdbe7bf/router.db`（19,349,504 字节，600，10001:10001）。
- 部署后候选测试 3/3 success（mimo 3.5s / longcat 3.6s / nemotron 14s）。启动日志有既有 WARN：`reasoning models with unexpressible profiles count=1 public_ids=opencodefree/space-bunny-free`（profile 一致性检查告警，非本次回归）。
- 白名单现状（运营在本轮会话间自行整理过）：共 3 条启用——NVIDIA `nvidia/nemotron-3-ultra-550b-a55b`、OCF `opencodefree/longcat-2.5-preview-free`、OCF `opencodefree/space-bunny-free`；旧的 deepseek-v4-flash-free/jev/ling 白名单行已被删除。mimo-v2.5-free、nemotron-3-ultra-free 未登记（候选已验证可用，登记与否待运营决定）。
- 端到端真实验证（临时 Access Key，测试后已删除）：`/v1/models` 200 返回 3 模型；`opencodefree/longcat-2.5-preview-free` 非流式 200/4.5s、流式 200/24 chunks 带 [DONE]；`opencodefree/space-bunny-free` 非流式 200/1.3s。**调用 OCF 模型必须用带 provider 前缀的公开 id（`opencodefree/<model>`），裸模型名 404 model_not_found**。
- 回滚：恢复 `/opt/opencode-free-proxy/src.bak-pre-fingerprint-20261001`（网关）+ 镜像 `deploy-20261001-candidate-probe-fix-af5b566`（路由器）。

## 41. 2026-10-01 思考参数全盘透传重构发布（370dbc3）

- 用户决策：9router 式纯透传，终结 per-model reasoning 元数据维护成本。提交 `370dbc3`（`refactor: make reasoning parameters pure pass-through`，净删约 1200 行）。
- 行为变化：`reasoning_effort`/`reasoning`/`thinking` 别名原样转发——删除 `ResolveReasoning`（nearestLevel 档位映射、未知档位 400）、`ApplyReasoning`（Strip+wire format 重注入）、`capThinkingBudget`（预算封顶）、`AutoReasoningSpec`（客户端未发时的注入阶梯）和 validateRequirements 的 reasoning 501 门控；Parse 阶段 reasoning 参数错误/冲突不再拒绝请求（仅观测）。**保留**：Responses→Chat 的参数名映射（mapReasoning）、响应侧 reasoning 归一化读取、观测记录（requested/effective/wire fields，source 恒为 client）、reasoning 501→`AutoReasoningEnabled` 运行时设置保留读写但不再有任何运行时效果（API/前端兼容，前端开关已是摆设——后续可清）。
- 已知行为变化：off-profile 档位（如 kimi-k3 的 medium）不再本地映射，直接到上游由上游 400 说明支持档位；小预算+思考的空回复风险回到客户端侧（vibe 评测脚本如遇 16-token 空回复需自查 max_tokens）。
- 门禁：`go test ./...` 全过（删除/改写 9 个测试文件）、go vet、gofmt、git diff --check。提交时误 `git add -A` 带入 `.worktrees/` embedded repo，已 `git rm --cached` + amend 修正并加入 .gitignore——`git add -A` 前先看 status 里未跟踪目录。
- 标准发布版本 `20261001-reasoning-passthrough-370dbc3`，Release/镜像同名；回滚点 `20261001-ocf-probe-budget-fdbe7bf`；切换前备份 `backups/predeploy-20261001-reasoning-passthrough-370dbc3/router.db`（19,349,504 字节，600，10001:10001）。
- 线上真实验证（临时 Key，已删）：`opencodefree/longcat-2.5-preview-free` 五场景全 200——effort=high（off-profile，旧版本地 501，现到达上游且返回真实 reasoning_content）、effort=none、无参数回归、thinking 对象、流式+effort=high（17 chunks 带 [DONE]）。启动日志 `unexpressible profiles count=2`（longcat/space-bunny）仍为 advisory 告警，不影响请求。

## 2026-10-02 杭州服务器 reasoning-probes 部署已完成

- GitHub `main` 部署目标 `5f944db4ba4620ab69a8b7aa03d063db5db97be9`（`fix: bound probes and preserve reasoning aliases`）已按强制流程部署。
- Release `/opt/nvidia-router-releases/20261002-reasoning-probes-5f944db`，镜像 `nvidia-router:deploy-20261002-reasoning-probes-5f944db`；回滚点 `20261001-reasoning-passthrough-370dbc3`。切换前数据库备份与验证结果见对应既有发布记录。
- 后续 2026-10-02 OpenCodeFree 代理依赖恢复记录见本文件上方同日条目；代理池使用独立版本化镜像，不代表 Router 源码重新发布。

## 2026-10-02 OpenCodeFree 代理依赖恢复

- 根因：`/opt/star-proxy-pool` 旧构建目录中 `internal/forwarder/transport.go` 使用 `config.Runtime.DisableKeepAlives`，但远端 `internal/config/config.go` / `runtime.go` 文件不匹配，Dockerfile 的 `go test ./...` 编译失败；目录无可核验 Git HEAD。本地星空代理池干净提交 `c20c0e0ae6d02757fbf7219dfa68f2c638fe7503` 的全量 `go test ./...` 通过。
- 未覆盖远端 `.env`、`data/` 或 runtime-config；从提交归档部署到 `/opt/star-proxy-pool/releases/20261002-ocf-pool-c20c0e0`，关键源码与归档字节校验一致。构建镜像 `star-proxy-pool:deploy-20261002-ocf-pool-c20c0e0`（image ID `sha256:81fca0f9181c9fb5f40859a8c5ba31959233631c02614331d26a6a46d7d768b3`），Docker build 中 Go 全量测试通过。
- 持久化 Compose 覆盖：`/opt/star-proxy-pool/compose.ocf-internal.yaml`（权限 600），只连接外部 `router-internal`、Docker DNS alias `proxy-pool`，不发布宿主端口；数据卷仍挂载 `/opt/star-proxy-pool/data:/data`。切勿改用 `/root/star-proxy-pool` 公网服务。
- 验证：代理池 `running/healthy`、重启 0、OOM false；`/healthz=200`；Router 与 OCF 网关容器内访问 `http://proxy-pool:8080/healthz` 均 200；代理池无宿主机端口绑定，数据目录 owner `100:101`。Router 候选测试 `space-bunny-free` 返回 success。使用公开模型 ID `opencodefree/space-bunny-free` 的真实 Router Chat 请求 HTTP 200、1 choice、回复匹配，临时 Access Key 已删除；裸 ID 会得到 `model_not_found`。
- 遗留：既有 OpenCodeFree 网关镜像仍标为 `opencode-free-proxy-opencode-free-proxy:latest`，image ID `sha256:7c5e52faefa289bec18edbbb05509973effb87e8142dc1e2b7124c0053891daf`，无 OCI 源码 revision/version 标签；本轮未重建或重启它，避免在源码版本不可确认时伪造版本标签。后续需先从可核验源码/提交构建，再固定不可漂移版本。
- 教训：Windows `core.autocrlf=true` 时 `git archive` 成员可能与 `git show HEAD:path` 的原始 blob 哈希不同；核验发布归档需比较归档成员与远端释放文件，不能直接比较工作树/原始 blob 哈希。

## 43. 2026-10-02 OpenCodeFree 编程可用性修复（4fca032 + 00c75d2）

- **结论**：修复前两个启用 OCF 模型**完全无法用于编程任务**——任何带 `tools` 的请求都被本地能力门控 501 `model_capability_unsupported`；修复后两个模型在 `/v1/chat/completions`（非流式与流式）和 `/v1/responses` 三条路径上都能真正完成 agent 编程循环。
- **根因一（探测提问歧义）**：`internal/modelcatalog/service_probe.go` 的 tools 探测第二形态用 "You must call the weather tool."，这类模型会用散文回答（"Which city?"）而不是发出 tool_call；两形态都"沉默"就写入永久 `tools_status=unsupported`，此后所有 agent 请求 501。上游实测（`scripts/test/opencodefree_tools_probe_remote.py`）：space-bunny/longcat/nemotron/mimo 四个 free 模型在具体任务（带工具必填参数）下都能产出合法 tool_calls，只有歧义提问失败。修复：提示改成携带工具参数的具体任务。
- **根因二（4xx 当能力证据）**：`readProbeChat` 把所有 4xx 归为 `probeHTTPUnsupported`，`attemptToolsProbe` 直接判 negative。OCF 在 403 FreeTierError 窗口会给全部模型盖上永久 unsupported，未知模型则是 401 `Model X is not supported`。修复：4xx 只有 body 明确提到该能力（tools / reasoning 关键词）才算能力否定，鉴权、免费额度策略、地区和模型不存在一律视为 unknown、不写库（reasoning 探测同样处理）。
- **根因三（上游 provider 错误当终态）**：OpenCode provider 层把自身失败包成 400 `invalid_request_error` + "Error from provider (Console): ..."，网关原样透传，`classifyOpenCodeFreeStatus` 视为终态 → 多轮循环中途 502 掉线（实测约每 12 个循环 turn 一次；同一会话重放几秒后即成功）。修复：仅此类 400 在首字节前重放一次，公开错误码仍为 `upstream_error`（不谎称可重试）；普通 400/422 保持终态。
- 修复后能力探测结果（`POST /admin/api/model-test-jobs` 强制重探测，两轮一致）：两个 OCF 模型 `base=success`、`tools=supported`、`supports_tools=true`。
- **真实编程任务验证**（`scripts/test/ocf_programming_probe_remote.py`，临时工作区预置 `moving_average` 越界 bug + 4 个 unittest，模型只能用 list_files/read_file/write_file/run_tests 工具，最后以 `python3 -m unittest` 判定）：两个模型 × chat/stream/responses 共 10 次循环全部 `solved=true`（2 失败 → OK），工具参数 JSON 全部合法，无 501/502。
- 遗留/限制：
  - OCF 网关 `ensureFingerprintTools` 在调用方未声明时追加 no-op `bash/glob/grep/read`，模型可能调用调用方根本没有的工具（探针里表现为凭空调用 `glob`/`bash` 并浪费轮次）。真实编码 Agent（opencode/Codex）自带这四件套，不受影响；探针用 `--arg TOOLSET=standard` 复现真实 Agent 形态。网关源码无可核验版本，本轮未改。
  - `nvidia/nemotron-3-ultra-550b-a55b` 仍 `tools_status=unsupported`；重探测时它在 **base** 阶段就失败（NVIDIA 基础探测仍是 `modelProbeMaxTokens=16`），因此能力数据无法刷新。属 NVIDIA 渠道独立问题，未在本轮处理。
  - OCF 免费档本身不稳（历史上 503/流截断/1% 级成功率）；重放只覆盖单次瞬态，不解决持续故障。
- 可复用方法：
  - 上游真值直连网关：`python scripts/test/remote_exec.py scripts/test/opencodefree_tools_probe_remote.py --arg MODELS=<逗号分隔上游ID>`（在网关容器内用路由器同一把 key，含复刻路由器探测形态的 case）。
  - 编程任务闭环：`... ocf_programming_probe_remote.py --arg MODE=<state|reprobe|chat|stream|responses|all> [--arg TOOLSET=standard] --stdin-env NVIDIA_ROUTER_ADMIN_PASSWORD`；`reprobe` 走 model-test-jobs 强制刷新能力判定，不必等周期探测。
  - 发布后只读验收：`scripts/test/post_deploy_accept_remote.py`（容器状态/版本/备份/健康/匿名 401/端口/错误签名）。CLI 无 `db verify`；app 运行时 `db backup` 会因进程锁失败，完整性只能靠部署期备份与 `/health/ready`（含 ping + VerifyMigrations）。
  - `.gitignore` 里的 `".worktrees/"` 带字面引号，等于没生效，`git add -A` 会再次把嵌套 worktree 当普通目录纳入；已改为无引号写法。

## 44. 2026-10-09 架构提纯：纯二进制原生部署、单端口 3756 与零容器收敛

- **架构决策（用户强制指令）**：彻底取消国内测试机（hangzhou2-2）复杂 Docker 容器与虚拟网桥结构；对外有且仅管理 `3756` 单一端口。Docker 仅用于 GitHub CI 测试，严禁在远端 VPS 运行 `docker build` 耗尽资源。
- **单端口管理与全栈自闭环**：
  - 核心路由器 `nvidia-router` 直接以单体原生二进制运行在宿主机，唯一监听 `0.0.0.0:3756`；
  - 辅助网关 `opencode-free-proxy` 已彻底从宿主机移除（`docker rm` 销毁容器，彻底关停 6020 端口）；
  - OpenCodeFree 客户端指纹注入（`bash/glob/grep/read` 四件套）、Session ID 注入及逆向 SSE 流式聚合（针对非流式调用自动聚合为 chat.completion JSON）已全量原生内嵌于 Go 模块 `internal/upstream/opencodefree` 中，直连上游 `https://opencode.ai/zen/v1`；
  - 全机公网仅暴露 3756 一个业务管理端口，0 业务容器，内存占用从数百兆大幅降低至 ~40MB，CPU 0%。
- **极速部署链路（`scripts/deploy/deploy_native.py`）**：
  - 本地执行交叉编译生成 14.5MB 静态二进制（内置前端 `go:embed`）；
  - SFTP 上传覆盖 `/opt/nvidia-router/nvidia-router`；
  - 宿主机配置 Systemd 守护进程 `/etc/systemd/system/nvidia-router.service`，执行 `systemctl restart nvidia-router`（0.1s 重启生效，支持开机自启）。
	- **step-5-preview-free 能力解封与验证闭环**：
	  - 修复 `internal/modelcatalog/capability_hints.go`：将 `step-` 前缀纳入推理与工具提示列表，默认赋予 `inferred` 能力状态；
	  - 修复 `internal/modelcatalog/repository.go`：修复 `r.Patch` 的 SQL UPDATE 遗漏更新 `tools_status` 与 `tools_verified_at` 导致管理员 PATCH 工具能力不生效的陈年 Bug；
	  - 真实长编程闭环实测（`ocf_programming_probe_remote.py`）：`step-5-preview-free`、`space-bunny-free` 在直连 `https://opencode.ai/zen/v1` 的原生架构下，在 `chat`、`stream`、`responses` 三协议下真实 Coding-agent 测试全量 `solved=true` 100% 达成，工具调用零 501 拦截。

## 45. 2026-10-09 全量深度代码审查与核心性能/健壮性优化

- **P0 逆向 SSE 聚合 Usage 逃逸修复**：`internal/upstream/opencodefree/aggregate.go` 的 `absorb` 对 `chunk.Usage` 使用 `bytes.Clone` 深拷贝，彻底杜绝 `bufio.Scanner` 缓冲区复用在后续帧（`data: [DONE]` 等）中污染覆盖 `usage` 导致反序列化畸变的风险；补充大流（32MB）上限防护 `maxAggregatePayloadBytes`。
- **P1 代理 Transport 缓存读写锁解耦**：`internal/xkproxy/manager.go` 将 `clock` 与 `cachedTransport.lastUsed` 改为 `atomic.Uint64`；快路径（命中且代理未过期/健康）在 `RLock` 保护下原子更新时钟并直接返回 Handle，彻底消灭每个请求释放读锁后争抢独占写锁造成的 Lock Churn 性能瓶颈。
- **P1 NVIDIA Base 探针 Token 预算调优**：`internal/modelcatalog/service.go` 将 `modelProbeMaxTokens` 从 16 提升至 128，解决 Nemotron 等具有长 chat 模板或 reasoning header 的大模型因 16 tokens 耗尽被误判为 empty response / unreachable 的陈年假阴性缺陷。
- **P2 app.go 组装生命周期解耦**：将 `app.New` 中多线程 Worker（清理器、健康探针、OpenCodeFree 同步器等）解耦提取至 `startBackgroundWorkers`，提升装配内聚性。
- **P3 前端测试夹具静音**：`AccessKeysView.spec.ts` 与 `NvidiaKeysView.spec.ts` 补充 `vue-router` mock，消除控制台未捕获的 router injection 警告。
- **杭州服务器真实部署与双模式联调**：
  - 重新编译 Linux 原生二进制并 SFTP 部署到国内主机（114.55.25.190），Systemd 重启生效；
  - 3756 `/health/live` 与 `/health/ready` 返回 200；
  - 真实 Coding-agent 编程测试（`ocf_programming_probe_remote.py`）：非流式（MODE=chat）与流式（MODE=stream）在 `space-bunny-free` 与 `step-5-preview-free` 上驱动 5-7 轮完整工具交互（list/read/write/run_tests）全部达成 `solved=true`（unittest 2 失败 -> 全部通过 OK），代码修改真实生效且参数 JSON 零畸变。

## 46. 2026-10-09 全量深度代码审查（只读，未改代码）

- 报告：`docs/2026-10-09-深度代码审查与架构优化报告.md`。审查 HEAD `0df2768`，规模 370 个 Go 源文件（后端非测试约 34.3k 行）+ 197 个 Go 测试 + 160 个前端文件。方式为 6 路并行子代理扫描 + 关键结论人工逐行复核。
- **方法论教训（重要）**：子代理报告必须抽样人工复核后再据以排期。本轮已实证存在 **误报**（其报的“代理池上游凭据经 `/admin/api/proxy-pool/refresh` 回显”不成立：refresh 返回固定文案、status 只暴露白名单 `LastErrorCode`、`xkproxy.ErrorCode()` 对非 ProviderError 返回空、`writeInternalError` 不外发 cause）与 **多处严重度偏高**（全局 body 预算信号量实为注释充分的设计取舍；前端桌面/移动双份挂载属常见取舍）。复核规则：先看被引用的那一行是否恰是脱敏/正常代码。
- **已逐行复核的高优先级缺陷（未修复，待排期）**：
  - `scripts/deploy/deploy_native.py:74,82,92`：覆盖生产二进制前不备份旧二进制、从不备份 router.db、校验失败仍 `return 0`；`run()` 先读 stdout 再读 stderr（§37 记载过的 paramiko 死锁成因仍在）。无发布前门禁。**本轮风险最高项**。
  - `internal/app/app.go:346,363-366` + `internal/app/shutdown.go:89-151`：`StartOpenCodeFreeSync` 与 `capabilityProbe` 无 done 通道，`finishShutdown` 不等待即关库；`rootCancel()` 仅发信号不等待退出，存在“关库后写库”竞态。
  - `internal/xkproxy/pool.go:574-616`（`ReportFailure`）对比 `656-719`（`ReportHTTPFailure`）：传输层失败**无时间窗防抖、无“全池近期有 2xx”门控、无计数饱和**，而 HTTP 侧三者齐备。池仅 2~4 出口时并发抖动可瞬时击穿 `MaxEjections` 永久清空池。
  - `internal/xkproxy/manager.go:305-327`：Rebuild 路径无条件覆盖 `m.transports[key]`，并发 rebuild 时先前 transport 被孤儿化且永不关闭（cache-miss 路径 356-359 有正确放弃逻辑，Rebuild 漏了）。
  - `internal/upstream/opencodefree/aggregate.go:33-38,102-104`：非流式聚合只认 `reasoning_content`，而 `internal/protocol/responses/delta.go:31-33` 把 `reasoning_content`/`reasoning`/`thinking` 当等价别名——同上游两种调用形态不一致。
  - `internal/observability/http.go:237-251`：`appendTail` 超 64KB 后**每个 chunk** 分配+拷贝 64KB，长流产生数十 MB 短命垃圾。
  - `internal/xkproxy/collector.go:159,171,425-517`：`Close()` 只在两次 fetch 之间看 `c.done`，进行中的 fetch/validate 只监听传入 ctx，最坏阻塞约 27s（3×4s + 3×5s + 2×0.5s）。
  - `internal/httpapi/v1/opencodefree_execution.go:212-223`：上游错误原文（≤512 字符）未经清洗回显客户端；NVIDIA 侧有 `fault` 白名单脱敏而 OCF 侧缺失。
  - `internal/app/app.go:169-172` 与 `internal/upstream/opencodefree/client.go:290`：装配层 `WithProxy(proxy)` 并注释“走同一出口池”，客户端却因 `baseURL` 含 `opencode.ai` 静默直连——意图与实现冲突，须二选一并统一。
  - `internal/httpapi/admin/models.go:71-74,92-95,119-122` 与 `web/src/features/models/types.ts`：后端仍序列化 `reasoning_levels`/`reasoning_min_budget`/`reasoning_max_budget`/`reasoning_zero_allowed`/`reasoning_dynamic_allowed`，前端零引用；结合 §41 纯透传重构后这些字段运行时已无效果，属前后端共同死字段。
	- **确认健康、无需动的部分**：Go 直接依赖仅 2 个；SQLite 写库 `MaxOpenConns(1)`+`_txlock=immediate`、读库 `mode=ro` 独立池；`BufferRecorder` 队列满即丢且计数、不阻塞热路径；Prometheus 无动态 label；领域层不反向依赖 httpapi/app；管理 API 错误路径脱敏完整。

## 47. 2026-10-10 深度审查缺陷全量修复与核心健壮性优化

- **OpenCodeFree 请求级代理会话隔离与 429 故障转移**：
  - `internal/upstream/opencodefree/client.go`：废除全局唯一单例 `c.session` 导致的单出口挤占，代理池租约 session 改为由请求上下文的 `SessionFrom(ctx)`（即 `x-opencode-session`）动态派生；不同用户的会话分散到代理池的多个出口 IP，同一会话保持 IP 连贯性。
  - 通过 `WithRetryAttempt(parent, attempt)` 注入重试轮次；在遭遇 429 或瞬态错误重试时，代理 session 自动派生重试后缀（`%s_try%d`），强制向代理池索取新出口 IP 发起重试，消灭盲目向原出口重试导致的重复 429。
- **优雅停机并发竞态修复（Shutdown Race）**：
  - `internal/modelcatalog/service.go` 与 `capability_probe.go`：`StartOpenCodeFreeSync` 与 `CapabilityProbeRunner.Start` 改为返回 `<-chan struct{}` 信号通道，循环退出时关闭；
  - `internal/app/app.go` 与 `shutdown.go`：在 `finishShutdown` 关闭 `dbReader` 与写库连接前，显式等待 `ocfSyncDone` 与 `capabilityProbeDone` 排空退出，彻底杜绝停机期数据库被提前关闭引发的 `sql: database is closed` 错误。
- **代理池 Transport Rebuild 并发连接泄漏修复**：
  - `internal/xkproxy/manager.go`：Rebuild 路径在获取写锁后，若发现其他 goroutine 已抢先更新了 entry，立即对新建的 `transport2` 调用 `CloseIdleConnections()` 并复用已有 entry，与 cache-miss 路径保持严格一致，杜绝 Transport 孤儿化与底层的 TCP 连接泄漏。
- **长流 trackingWriter 堆内存分配平摊优化**：
  - `internal/observability/http.go`：引入两倍阈值修剪（`usageTailCaptureThreshold = 128KB`）。仅当缓冲区超过 128KB 时才执行裁剪回 64KB，彻底消灭每个 chunk 写入后均触发 64KB make+copy+Reset 的高频堆内存开销（平摊到 O(1)）。
- **代理池 ReportFailure 传输失败时间窗防抖与计数饱和**：
  - `internal/xkproxy/pool.go`：补充 `transportFailureWindow`（60s）防抖与计数饱和保护，防止偶发网络抖动导致 `HealthFails` 瞬间击穿 `FailureLimit` 与 `MaxEjections` 清空出口池。
- **全量门禁**：`go vet ./...` 0 警告，全量 `go test ./...` 100% 通过，`git diff --check` 0 格式缺陷。


