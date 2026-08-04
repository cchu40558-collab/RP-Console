# RP Console 部署与运维改造方案

> 文档状态：部署脚本和本机运维命令已在本地实现，尚未完成最终校验、发布或部署。
>
> 本文描述的是 RP Console 总站自身的安装、升级、回退和日常运维方式，不包含通过总站远程修改子站线路或远程升级子站的功能。

## 1. 背景与目标

RP Console 是 Relay Panel 的总站，用于从一个统一入口查看已经登记的子站服务器、读取线路状态和累计流量，并查看子站线路摘要。

当前已经完成的内容：

- RP Console 程序源代码已经在 GitHub 仓库 `cchu40558-collab/RP-Console` 中。
- 当前已发布版本为 `v0.1.0`；本次部署自动化版本为 `v2.0.18`，与子站版本统一。
- 已有管理员登录、加密保存子站总站只读令牌、子站状态汇总、子站线路只读查看和审计记录。
- 子站需要升级到 Relay Panel `v2.0.18` 或更高版本，才能生成总站只读令牌并接入。
- 总站的安装脚本、升级脚本、回退脚本和本机命令包含在 `v2.0.18` 中。

本次改造的目标是让总站具备与子站类似的运维体验：

```text
指定版本安装
    -> rp-console version
    -> rp-console status
    -> rp-console logs
    -> sudo rp-console check
    -> rp-console restart
    -> rp-console update vX.Y.Z
    -> rp-console rollback
```

所有安装和升级都必须指定 Git tag，不能直接使用可变的 `main` 分支。

## 2. 适用服务器与域名

当前准备部署总站的服务器：

```text
公网 IPv4：35.212.217.103
系统：Ubuntu 22.04.5 LTS
云厂商：Google Compute Engine
SSH 用户：vultr-Los-ub22.04
```

总站管理域名：

```text
rp-console.wakeup-ai.top
```

域名建议始终使用小写。DNS 本身不区分大小写，但统一小写可以避免证书、配置和日志出现两种写法。

这台服务器此前运行过旧的 Xray + Nginx 线路。旧线路的 Xray、Nginx 站点、Origin Certificate、私钥和 `8443` 防火墙规则已经删除；Nginx 软件和 SSH `22` 端口被保留，以便部署总站。

## 3. 明确边界

### 3.1 本次要做的内容

- 安装和运行 RP Console 本身。
- 用 Nginx 将公网 HTTPS 请求反向代理到总站本机服务。
- 保存和升级总站程序。
- 备份、检查和回退总站程序及总站数据。
- 通过网页添加子站的总站只读令牌。

### 3.2 本次不做的内容

- 不安装 Xray，不部署代理线路，不提供用户代理流量。
- 不保存任何子站 SSH 密码或私钥。
- 不提供网页终端或任意 Shell 命令。
- 不通过总站只读令牌修改子站线路。
- 不通过总站升级子站。该功能必须等待子站另行设计受控写接口后才可实施。
- 不因为子站 VPS 的人工有效期到达而停止子站流量。总站的有效期仅作提醒。

## 4. 目标网络结构

```text
管理员浏览器
    -> Cloudflare HTTPS
    -> rp-console.wakeup-ai.top
    -> Google Cloud 防火墙 TCP 443
    -> 服务器 UFW TCP 443
    -> Nginx :443
    -> RP Console 127.0.0.1:2053

RP Console
    -> HTTPS + 子站总站只读令牌
    -> 子站 Relay Panel v2.0.18+
```

RP Console 不会成为 Cloudflare 主线路、Reality 线路或住宅出口流量的一部分。

## 5. 首次部署前准备

首次部署前必须完成以下准备。顺序不能颠倒。

### 5.1 确认旧线路已退出

在服务器上确认以下旧组件不存在：

```bash
systemctl status xray --no-pager
sudo ss -lntp
sudo ufw status numbered
```

预期结果：

- `xray.service` 不存在或不是运行状态。
- 不再监听旧线路端口 `8443`。
- UFW 只保留 SSH `22`，或者仅保留与服务器实际用途有关的规则。

### 5.2 Cloudflare DNS

在 Cloudflare 的 `wakeup-ai.top` 域名中添加 DNS 记录：

| 字段 | 值 |
|---|---|
| 类型 | `A` |
| 名称 | `rp-console` |
| IPv4 地址 | `35.212.217.103` |
| 代理状态 | 已代理，橙色云 |
| TTL | Auto |

保存后，等待 DNS 生效。安装脚本不会自动创建 Cloudflare DNS 记录，因为脚本不保存 Cloudflare API Token。

### 5.3 Cloudflare Origin Certificate

在 Cloudflare 控制台中：

1. 打开 `SSL/TLS`。
2. 打开 `Origin Server`。
3. 点击 `Create Certificate`。
4. 选择由 Cloudflare 生成私钥。
5. 主机名填写 `rp-console.wakeup-ai.top`。也可额外包含 `*.wakeup-ai.top`，但只为总站单独签发更容易管理。
6. 生成后分别保存证书内容和私钥内容。
7. 在 FinalShell 中把它们上传到服务器临时目录，例如：

```text
/root/rp-console-origin.crt
/root/rp-console-origin.key
```

私钥不能粘贴到聊天、Git 仓库、Shell 历史记录或截图中。

安装脚本会校验证书和私钥能否配对，然后复制到总站专用目录。脚本不会把私钥写进 Git，也不会在安装输出中显示私钥。

### 5.4 Cloudflare TLS 模式

在 Cloudflare `SSL/TLS` 页面设置：

```text
Full (strict)
```

不要使用 `Flexible`。Flexible 会导致 Cloudflare 到源站之间没有可靠的 HTTPS 保护，且可能造成重定向循环。

### 5.5 Google Cloud 防火墙

Google Cloud 的 VPC 防火墙独立于服务器内的 UFW。即使 UFW 放行 `443`，Google Cloud 未放行时，公网仍无法访问。

在 Google Cloud 控制台中创建或确认入站规则：

| 协议 | 端口 | 来源 | 用途 |
|---|---|---|---|
| TCP | 22 | 仅自己的管理出口 IP，优先；必要时临时 `0.0.0.0/0` | SSH 管理 |
| TCP | 80 | `0.0.0.0/0` | HTTP 跳转到 HTTPS 或 Cloudflare 验证 |
| TCP | 443 | `0.0.0.0/0` | 总站 HTTPS |

后续可进一步把 `443` 源地址限制为 Cloudflare 官方 IP 段，但这是第二阶段加固。首次上线先保证 HTTPS 可用并通过 Cloudflare Access 保护管理页。

### 5.6 服务器内 UFW

首次安装脚本会保留 `22/tcp`，并在 Nginx 配置和健康检查成功后放行：

```text
80/tcp
443/tcp
```

不要开放 `2053/tcp` 到公网。它只允许监听在 `127.0.0.1:2053`。

## 6. 目录、账户和权限设计

部署完成后的目录约定如下：

| 路径 | 用途 | 权限要求 |
|---|---|---|
| `/usr/local/rp-console/rp-console` | 编译后的总站二进制 | root 所有，`0755` |
| `/usr/local/rp-console/VERSION` | 已部署版本号 | root 所有，`0644` |
| `/opt/rp-console-src` | 指定 tag 的源码和构建目录 | root 所有 |
| `/etc/rp-console/rp-console.env` | 管理员密码、数据主密钥、监听参数 | root 所有，`0600` |
| `/etc/rp-console/tls/origin.crt` | Cloudflare Origin Certificate | root 所有，`0644` |
| `/etc/rp-console/tls/origin.key` | Cloudflare Origin 私钥 | root 所有，`0600` |
| `/var/lib/rp-console` | 加密后的子站记录和审计数据 | `rp-console` 用户所有，`0700` |
| `/var/backups/rp-console` | 升级备份 | root 所有，`0700` |
| `/etc/systemd/system/rp-console.service` | systemd 服务定义 | root 所有 |
| `/etc/nginx/sites-available/rp-console.conf` | Nginx 站点配置 | root 所有 |
| `/usr/local/bin/rp-console` | 运维命令包装器 | root 所有，`0755` |

安装脚本会创建专用系统用户 `rp-console`，总站程序以该用户运行。Nginx 和 systemd 配置仍由 root 管理。

## 7. systemd 服务设计

服务名固定为：

```text
rp-console.service
```

运行环境至少包含：

```text
CENTRAL_ADMIN_PASSWORD=<管理员密码>
CENTRAL_MASTER_KEY=<32 字节 Base64 主密钥>
CENTRAL_DATA_DIR=/var/lib/rp-console
CENTRAL_LISTEN_ADDR=127.0.0.1:2053
CENTRAL_ALLOW_PRIVATE_NODES=false
```

设计要求：

1. 首次安装自动生成随机管理员密码和随机主密钥，或允许管理员显式传入。
2. 后续更新绝不重新生成主密钥。
3. 主密钥若丢失或被替换，已保存的子站总站只读令牌将无法解密。
4. 服务失败时由 systemd 自动重启。
5. 服务启动后由安装脚本访问 `http://127.0.0.1:2053/healthz` 验证。
6. 健康接口应返回总站状态和版本号，例如：

```json
{"status":"ok","version":"2.0.18"}
```

## 8. Nginx 与 HTTPS 设计

Nginx 使用两个 server 块：

1. `80` 端口仅把同域名请求重定向到 HTTPS。
2. `443` 端口加载 Cloudflare Origin Certificate，并代理到 `127.0.0.1:2053`。

代理必须传递以下请求头：

```text
Host
X-Real-IP
X-Forwarded-For
X-Forwarded-Proto
```

写入配置后的固定流程：

1. `nginx -t`。
2. 只有测试成功时才创建或更新 `sites-enabled` 符号链接。
3. `systemctl reload nginx`。
4. 用本机 HTTPS 请求确认反向代理成功。
5. 任一步失败，恢复旧 Nginx 配置并保留原服务。

首次部署时必须验证证书和私钥是否匹配。由于 Cloudflare Origin Certificate 的签发链不是普通公网浏览器信任链，脚本不能把普通 `openssl verify` 作为唯一依据；应检查证书可读取、私钥可读取、二者公钥一致，并由 Nginx 实际加载成功作为最终判断。

## 9. 一键安装脚本设计

计划新增文件：

```text
scripts/install-server.sh
```

首个带部署能力的版本计划为：

```text
v2.0.18
```

### 9.1 安装参数

脚本必须要求明确的版本 tag：

```text
CONSOLE_REPO_REF=vX.Y.Z
```

首次安装还必须提供：

```text
CONSOLE_DOMAIN=rp-console.wakeup-ai.top
CONSOLE_TLS_CERT_FILE=/root/rp-console-origin.crt
CONSOLE_TLS_KEY_FILE=/root/rp-console-origin.key
```

管理员密码可以显式提供，也可以自动生成：

```text
CONSOLE_ADMIN_PASSWORD=<可选>
```

主密钥不建议人工输入。脚本默认生成 32 字节随机值，并仅写入 `/etc/rp-console/rp-console.env`。

### 9.2 未来首次安装命令

首次部署时使用以下指定版本命令：

```bash
sudo env \
  CONSOLE_REPO_REF=v2.0.18 \
  CONSOLE_DOMAIN=rp-console.wakeup-ai.top \
  CONSOLE_TLS_CERT_FILE=/root/rp-console-origin.crt \
  CONSOLE_TLS_KEY_FILE=/root/rp-console-origin.key \
  bash <(curl -fsSL https://raw.githubusercontent.com/cchu40558-collab/RP-Console/v2.0.18/scripts/install-server.sh)
```

它是一条 Shell 命令，只是为了阅读而换行显示。实际粘贴时可以写成单行。

### 9.3 安装脚本执行顺序

1. 检查是否以 root 执行。
2. 校验 tag 格式必须是 `vX.Y.Z`。
3. 用 `git ls-remote` 校验远端 tag 存在。
4. 校验目标服务器是支持的 Linux 系统和 CPU 架构。
5. 安装或确认 Git、curl、OpenSSL、编译工具、Go、Nginx 已存在。
6. 下载指定 tag 的源码并 checkout 到该 tag。
7. 读取源码中的版本文件，确认版本号与 tag 一致。
8. 校验证书、私钥、域名和文件权限。
9. 创建专用用户、目录、环境文件、数据目录和 TLS 目录。
10. 编译 `./cmd/relay-central`，将临时二进制写入安装目录。
11. 写入 systemd 服务并启动总站。
12. 轮询本机健康接口，确认服务返回期望版本。
13. 写入 Nginx 配置，执行 `nginx -t`，成功后重载 Nginx。
14. 放行 UFW `80/443`，不开放 `2053`。
15. 写入 root 专用安装结果文件，显示总站 URL、版本和初始管理员密码。当前总站登录不使用管理员用户名。
16. 删除临时上传目录中由脚本复制后的证书源文件，或明确提示管理员手工删除。

如果第 10 至 14 步任一步失败，脚本必须停止并输出明确原因；不能留下已启动但未通过健康检查的半成品服务。

## 10. 本机运维命令设计

安装脚本会生成：

```text
/usr/local/bin/rp-console
```

命令接口如下。

| 命令 | 权限 | 行为 |
|---|---|---|
| `rp-console version` | 普通用户可用 | 显示已部署总站版本 |
| `rp-console status` | 普通用户可用 | 显示版本和 `rp-console.service` 状态 |
| `rp-console logs` | 普通用户可用 | 显示最近 100 条总站日志 |
| `sudo rp-console check` | root | 检查总站、Nginx、证书、监听端口、UFW 和版本一致性 |
| `sudo rp-console restart` | root | 重启总站并确认运行 |
| `sudo rp-console update vX.Y.Z` | root | 升级总站到指定 tag |
| `sudo rp-console rollback` | root | 回退到最近一个旧备份 |
| `sudo rp-console backups` | root | 列出可回退备份 |
| `sudo rp-console password` | root | 交互式修改管理员密码 |

`status`、`logs`、`check` 不显示管理员密码、主密钥、子站令牌或证书私钥。

### 10.1 `check` 的检查项目

`sudo rp-console check` 至少输出：

1. 程序版本和安装目录版本是否一致。
2. `rp-console.service` 是否为 `active (running)`。
3. `http://127.0.0.1:2053/healthz` 是否成功并返回期望版本。
4. `nginx -t` 是否通过。
5. `127.0.0.1:2053` 是否只监听 loopback。
6. `443` 是否由 Nginx 监听。
7. Origin Certificate 和私钥是否存在且权限正确。
8. UFW 是否保留 `22`、`80`、`443`，且不存在公开 `2053`。

子站离线、子站某条线路到期或子站线路异常不应该让总站的 `check` 失败。它们属于总站网页中服务器状态灯显示的业务状态，而非总站自身部署故障。

## 11. 升级机制设计

升级命令形式：

```bash
sudo rp-console update v2.0.19
```

升级流程：

1. 校验 tag 格式。
2. 校验远端确实存在该 tag。
3. 在 `/var/backups/rp-console/<时间戳>/` 创建升级前备份。
4. 备份当前二进制、`VERSION`、环境文件、systemd 服务、Nginx 配置、TLS 文件和整个数据目录。
5. 将备份权限设置为 root 专用。
6. 拉取并编译指定版本到临时文件，不能先覆盖当前可运行二进制。
7. 校验编译版本与目标 tag 一致。
8. 原子替换二进制和版本文件。
9. 保留原有环境文件和主密钥，禁止在升级中重新生成密码或主密钥。
10. 重启服务。
11. 检查本机健康接口、服务状态和 `nginx -t`。
12. 成功后保留最近两个旧备份。

### 11.1 自动恢复

从第 6 步开始，如果构建、启动、健康检查或 Nginx 校验失败：

1. 停止失败的新服务。
2. 恢复本次升级前的二进制、版本文件、环境文件、systemd、Nginx、TLS 和数据目录。
3. `systemctl daemon-reload`。
4. 恢复 Nginx 配置并重载。
5. 启动旧总站服务。
6. 再次检查旧版本的健康接口。
7. 输出失败原因和已恢复版本。

不能因为某台子站暂时连接失败而自动回退总站。自动恢复仅以总站自身能否启动、健康接口是否正确、Nginx 是否有效为依据。

## 12. 回退机制设计

回退命令：

```bash
sudo rp-console rollback
```

回退必须：

1. 找到最近一个完整旧备份。
2. 先备份当前正在运行的版本，避免回退失败后无法恢复。
3. 停止总站服务。
4. 恢复旧二进制、版本、环境、systemd、Nginx、TLS 和数据目录。
5. 启动服务并检查健康接口。
6. 回退失败时，自动恢复第 2 步保存的当前版本。
7. 成功后输出当前版本。

重要数据影响：回退会把总站数据目录恢复到旧备份创建时的状态。因此升级后新增的子站、修改的人工有效期或新增的审计记录会回到旧状态。回退前自动保存的“当前版本备份”仍可用于再次恢复。

## 13. 版本发布纪律

从 `v2.0.18` 开始，总站每个版本必须遵守以下顺序：

1. 修改单一版本源文件。
2. 程序健康接口、安装目录 `VERSION` 和 `rp-console version` 都读取或验证同一版本。
3. 运行 `go test ./...`。
4. 运行 `go vet ./...`。
5. 提交并推送 `main`。
6. 确认 CI 成功。
7. 创建并推送相同名称的 `vX.Y.Z` tag。
8. 只有 tag、程序版本文件和 GitHub CI 都一致，才称为正式版本。

安装器和 `rp-console update` 都必须读取 tag 中的脚本：

```text
https://raw.githubusercontent.com/cchu40558-collab/RP-Console/vX.Y.Z/scripts/install-server.sh
```

不得在生产服务器上执行来自 `main` 的安装脚本。

## 14. 子站接入顺序

总站安装完成后，按以下顺序接入子站：

1. 确认子站已升级到 Relay Panel `v2.0.18` 或更高版本。
2. 确认子站自身网页和线路仍正常。
3. 在子站打开 `设置 > 安全 > 总站接入`。
4. 创建一个“总站只读令牌”。
5. 立即保存明文令牌。该令牌只显示一次，不能从页面重新查看。
6. 打开 `https://rp-console.wakeup-ai.top`，使用总站管理员密码登录。
7. 点击添加服务器。
8. 填写名称、子站管理地址、HTTPS 端口、管理基础路径、服务器人工有效期和总站只读令牌。
9. 保存并执行检测。
10. 确认总站读取到子站身份、子站版本、Xray 状态、线路数量、异常数量和累计流量。

子站总站只读令牌只能访问子站的三类总站读取接口，不能访问普通子站 API；普通子站 API Token 也不能访问总站读取接口。

## 15. 上线验收清单

### 15.1 服务器验收

- [ ] SSH 登录仍正常。
- [ ] Google Cloud VPC 防火墙已经放行 80/443。
- [ ] UFW 仅放行所需端口，未公开 2053。
- [ ] `rp-console.service` 是运行状态。
- [ ] `sudo rp-console check` 全部通过。
- [ ] `nginx -t` 通过。
- [ ] `https://rp-console.wakeup-ai.top/healthz` 正常返回状态和版本。
- [ ] 直接访问 `http://35.212.217.103:2053` 不可用。

### 15.2 Cloudflare 验收

- [ ] DNS A 记录指向正确 IP。
- [ ] 记录为橙色云代理状态。
- [ ] SSL/TLS 为 `Full (strict)`。
- [ ] Cloudflare 能正常访问源站的 Origin Certificate。
- [ ] 建议为总站域名配置 Cloudflare Access，仅允许管理员身份登录。

### 15.3 业务验收

- [ ] 浏览器能够登录 RP Console。
- [ ] 能添加一台 `v2.0.18+` 子站。
- [ ] 子站正常时显示绿灯。
- [ ] 子站人工有效期不足 7 天时显示黄灯。
- [ ] 子站无法连接、Xray 未运行或报告线路异常时显示红灯。
- [ ] 点击服务器名称在新标签页打开总站内部的只读线路页面，而不是暴露子站 IP 或后台地址。
- [ ] `rp-console update <指定版本>` 能创建备份、升级并通过健康检查。
- [ ] `rp-console rollback` 能恢复最近旧版本。

## 16. 实施顺序

当前本地实施状态：

1. 已完成：新增总站唯一版本源文件，并让健康接口和程序日志使用它。
2. 已完成：编写 `scripts/install-server.sh`。
3. 已完成：编写 systemd、Nginx、TLS、环境文件和 UFW 处理逻辑。
4. 已完成：编写 `/usr/local/bin/rp-console` 命令包装器。
5. 已完成：实现升级备份、失败自动恢复、备份清理和手动回退。
6. 已完成：增加健康接口版本测试和脚本语法校验。
7. 待完成：在临时或正式 VPS 做首次安装验证。
8. 本次发布：发布 `v2.0.18`，确认 tag 与版本一致。
9. 待完成：在 `35.212.217.103` 部署 `v2.0.18`。
10. 待完成：配置 Cloudflare DNS、Origin Certificate、Google Cloud 防火墙和 UFW。
11. 待完成：接入第一台已升级到 `v2.0.18` 的子站。
12. 待完成：验证总站自身升级和回退后，再接入其余子站。

在第 8 步完成前，不应在生产服务器上尝试从 GitHub 克隆后手工拼装服务，也不应执行文中标记为“未来”的安装命令。
