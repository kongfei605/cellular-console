EC20 Toolbox 1.3.0
=================

已部署到 operator@192.0.2.10（ec20-host）。
访问：http://192.0.2.10:8787/（通过当前 VPN 网络）
用户名：admin

首次获取管理密码，在自己的终端执行：
  ssh -i ~/.ssh/example.pem operator@192.0.2.10 'sudo cat /etc/ec20-console/credentials.json'

密码仅保存在 69 本机的受限文件内；源码和交付包不包含生产密码、短信内容或 SIM 密钥。

功能
----
1. 状态监控：型号、固件、运营商、LTE、信号、SIM、主备出口、VPN、设备动态，每 5 秒同步。
2. 短信会话：收到的完整文本短信自动归档；可新建短信、在会话中回复，发送前确认号码、内容与预计分段。支持中文与 GSM-7 文本，每次最多 3 段；收发记录、搜索、已读状态均保存在 69。
3. 呼叫控制：拨号确认、来电显示、接听、拒接、挂断及当前呼叫状态。
4. 网络保护：只在有线主用且健康、设备和 SIM 就绪、自动主备状态新鲜时开放拨号和接听；EC20 承载备用网络时锁定这些操作。挂断不受此保护限制。
5. 登录保护：随机管理密码、HttpOnly / SameSite 会话、8 小时有效期、CSRF 与来源校验、登录频率限制。

本版尚未接入浏览器双向音频；能拨号、接听不等于可以直接用网页麦克风和扬声器通话。
本版没有删除 SIM 短信、修改 APN、重置模块或断开数据连接的界面。
通话接听经过模拟设备验证；真实呼出已使用用户指定的尾号 5616 号码验证接通和挂断。
没有进行真实来电接听测试，也没有向任何号码发送测试短信。

与自动备用网络的关系
------------------
- Go 直接使用 ModemManager 的系统 D-Bus API；不打开 ttyUSB 串口，不启动另一套 PPP。
- 原 ec20-failover、NetworkManager、ModemManager、OpenVPN 的服务配置和拨号配置保持不变。
- 控制台独立运行、独立重启；主备切换不依赖 Web 页面或此 Go 进程。
- 服务以 ec20-console 专用非 root 账户运行，仅有 ModemManager Voice 与 Messaging 授权；不具备 Device.Control、网络管理或固件操作权限。
- HTTP 仅监听 192.0.2.10:8787。访问依赖 VPN；没有配置公网端口映射。
- 本版使用 VPN 内 HTTP。若以后向公网提供服务，需要另行配置 HTTPS 和访问控制。
- 通话保护会阻止蜂窝主用时新发起或接听电话；已经建立的通话不会被应用因出口变化而主动挂断。

文件与服务
----------
主程序：/opt/ec20-console/current/ec20-console
版本目录：/opt/ec20-console/releases/<SHA256 前 16 位>/
服务：/etc/systemd/system/ec20-console.service
最小授权规则：/etc/polkit-1/rules.d/49-ec20-console.rules
登录凭据：/etc/ec20-console/credentials.json（root / ec20-console，0640）
接收归档：/var/lib/ec20-console/inbox.json（0600）
发送记录：/var/lib/ec20-console/outbox.json（0600，与收件箱独立）
原主备状态：/run/ec20-failover/status.json（只读）

查看服务：
  sudo systemctl status ec20-console --no-pager
查看日志：
  sudo journalctl -u ec20-console -f
重启本应用：
  sudo systemctl restart ec20-console
停用本应用：
  sudo systemctl disable --now ec20-console
停用后短信归档和登录文件保留，现有自动备用网络继续运行。
停止 Web 服务不会自动挂断已建立的电话；请先通过界面挂断再停服务。

修改密码：
  sudoedit /etc/ec20-console/credentials.json
  sudo systemctl restart ec20-console
密码至少 12 个字符，建议继续使用随机强密码；重启会使既有 Web 登录会话失效。

后续更新
--------
用相同架构构建新二进制，上传到临时目录，再运行 deploy/update.py。
更新程序按二进制哈希创建版本目录，原子切换 current 并仅重启 ec20-console。
previous 符号链接保留上一个版本，可在需要时切回后重启本应用。

构建与测试
----------
构建工具：Go 1.26.8；输出 Linux amd64 静态二进制，CGO_ENABLED=0。
依赖：github.com/godbus/dbus/v5 v5.2.4，以及 golang.org/x/sys v0.27.0。
可在有 Go 工具链的开发机上执行：
  go test -race ./...
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o ec20-console-linux-amd64 .
网络无法访问官方 Go 模块代理时，可配置：
  GOPROXY=https://goproxy.cn,direct
依赖完整性由 go.sum 校验。

运行独立演示（不会访问硬件，只允许监听 127.0.0.1）：
  创建仅用于演示的 credentials.json，内容含 username 和至少 12 字符的 password。
  go run . -demo -listen 127.0.0.1:18787 -credentials credentials.json -data-dir ./demo-data
不要将生产凭据复制到演示环境。

验证摘要
--------
- 12 项 Go 测试及 race 检查通过，覆盖鉴权、CSRF、保护状态新鲜度、模拟通话、短信归档去重、跨模块隔离、独立子对象读取和登录限流。
- 桌面与手机布局、短信搜索、已读、模拟接听/挂断/拨号确认经过浏览器验证。
- 真实 ModemManager 读取到 2 条已有短信并归档；未修改这些短信的原始存储或已读标志。
- 实拨进入“通话中”，随后通过 Web API 挂断；独立 mmcli 校验没有遗留通话。
- 原主备程序、主备配置和 EC20 拨号配置哈希一致。
- 真实部署与呼叫期间两段持续 SSH 测试分别收到 180/180、120/120 次心跳，退出码均为 0。
- 安装时设置了仅停用新 Web 服务的超时回滚；成功后已解除。实拨设置了独立挂断保护，成功挂断后已清理定时器。

依赖许可见 THIRD_PARTY_NOTICES.txt。
接口参考：
  https://www.freedesktop.org/software/ModemManager/api/latest/ref-dbus.html
  https://www.freedesktop.org/software/ModemManager/api/latest/gdbus-org.freedesktop.ModemManager1.Modem.Voice.html
  https://www.freedesktop.org/software/ModemManager/api/latest/gdbus-org.freedesktop.ModemManager1.Sms.html

1.1.0 新增（2026-11-06）
----------------------
- 短信改为按号码归并的会话列表、按日期展示的消息气泡、会话搜索与高亮、会话批量已读。
- +86 / 0086 / 中国大陆手机号本地写法归入同一会话，保留原始发送方号码；短服务号不会被误归并。
- 新增网络诊断页，读取现有主备配置、恢复倒计时、VPN 状态与 sysfs 接口计数。
- 展示接收/发送速率和最近 60 个采样点；接口累计值会在接口重建时重置，不表示套餐余额。
- 切换记录保存在 /var/lib/ec20-console/network-history.json，原因标注为根据状态推断；首次运行不会补造旧切换原因。
- 新增所有页面可见的设备/网络状态条和当前呼叫条，可在短信或诊断页直接接听、拒接与挂断。
- 原 inbox.json 格式、现有登录文件、端口及 systemd 服务名称保持兼容。
- 网络诊断只读现有配置与接口计数，不运行额外探测、不改路由、不触发拨号或模块重启。
- 代码仓库：/path/to/ec20-toolbox；Go module 为 github.com/kongfei605/ec20-toolbox。
- Git 仅本地管理，没有自动创建 GitHub 仓库或推送。

开发命令
--------
  make test
  make build
构建产物为 bin/ec20-console-linux-amd64。现有服务路径和部署脚本继续沿用 ec20-console 名称，以兼容原安装。
仓库忽略 .local/、demo-credentials.json、demo-data/、credentials.json 与二进制，不提交生产凭据或短信数据。

独立音频实验室（2026-11-07）
----------------------------
访问 https://192.0.2.10:8788/；使用同一管理员账号，客户端需信任独立服务的服务器证书。
此为浏览器与 69 的 WebRTC 双向音频回环，尚未接入 EC20 通话音频。
提供 440Hz 测试音、麦克风回环、静音、音量、双向包计数、RTT 与返回信号分析。
测试音 20 秒自动停止；服务端每次最长 3 分钟、30 秒无媒体释放资源。
独立 systemd 服务 ec20-audio-loopback，HTTPS 8788 / UDP 8789，仅监听 VPN 地址。
不访问硬件、不改变主备路由、不重启原控制台。详细使用、证书、部署和回退见 AUDIO_LOOPBACK.txt。

真实通话音频预备代码（2026-11-07，未上线启用）
--------------------------------------------
internal/voicepcm 与 audioloop.NewMediaWithPCM 已完成PCM/G.711、受保护ALSA管道和WebRTC双向桥的模拟验证。
声卡识别绑定EC20 USB身份/物理端口，并核实8kHz单声道S16_LE双向能力；默认声卡不会作为替代。
新增 make build-uac-ready，只读检查声卡是否就绪，不打开音频/串口设备或连接D-Bus。
线上仍为既有软件回环版本。现场待办与范围限制见 UAC_READY_FOR_ONSITE.txt。

1.2.0 新增（2026-11-07）
----------------------
- 短信页增加“新建短信”和“回复”，编写与最终确认分两步，显示 GSM-7/UCS-2 及预计分段，最多 3 段，实际收费由运营商决定。暂不支持 emoji 等 BMP 以外字符；不支持群发、附件或自动转发。
- 发送仅在有线主用健康、EC20/SIM/短信服务就绪、蜂窝备用健康、状态新鲜且无正在进行的呼叫时开放；故障切换期间暂停新发送。通话中不发短信，短信处理期间不新拨号或接听，仍可挂断。
- 使用 ModemManager Messaging.Create / Sms.Send，不打开串口、不执行 AT、不改 APN/路由/USB、不重启模块。Messaging 是 ModemManager 的粗粒度授权，理论上还包含删除等动作，但应用不提供删除或 Store 接口。
- 单次操作先持久化发送意图，再向设备提交；同一 request_id 只提交一次。一分钟最多 5 条，短时间相同号码和内容防重复；超时、重启与未知结果均不会自动重发。
- “已提交运营商”表示设备返回 SMS SENT 状态，不等于对方已收到；“结果待核实”时先核实再决定是否重发。未知记录仅查询状态，查询绑定当时的 D-Bus 唯一服务实例，避免重启后路径复用误判。
- 发送记录独立保存在 outbox.json，最多 1000 条；写盘异常时关闭发送，已归档接收短信仍可查看。不要手动删除发送记录作为重发方法。
- 模拟模式覆盖编写、确认、发送、右侧气泡与状态；发送失败、重复请求、超时、重启恢复、鉴权/CSRF、存储异常与字符分段有自动测试。部署验收未发送真实短信。
- 首次真实发送请由管理员在页面确认目标号码与内容；现有通话测试号码没有被用于短信测试。真实发送能力仍需一次用户指定号码、内容的运营商验收。

1.3.0 新增（2026-11-07）
----------------------
- 新增联系人：新建、编辑、按姓名/号码/备注搜索、可恢复的回收站；同一大陆手机号码的 +86 / 0086 / 本地形式去重。最多1000条（含回收站），不修改SIM电话簿。联系人姓名用于短信会话、当前通话条、通话控制与历史。拨号快捷操作只预填号码，仍需原拨号确认。
- 新增持久化通话记录：按 ModemManager 唯一实例与呼叫对象绑定，模块重启后对象路径复用不会覆盖旧记录。保存呼入/呼出、首次/最后观察、是否观察到接通与观测接通时长；最多5000条，不自动删除历史。支持搜索和完整JSON导出；界面显示最近200条匹配记录。
- 通话记录不是运营商账单：5秒采样可能漏掉短通话；首次遇到已结束对象只记录“接通情况未知”，对象消失记录“状态跟踪中断”，不编造历史通话、未接原因或计费时长。应用重启后可接续同一仍存在的呼叫。
- 短信会话增加本地回收站/恢复，不删除 inbox/outbox 或SIM内容；同一号码收到新归档短信或新提交发送记录后自动重新显示。
- 新增设置页：系统/浅色/深色、页面5/10/15/30/60秒刷新、新短信和来电的页面内提醒。设置保存在此浏览器 localStorage，后台采样和自动主备检测频率不变。页面关闭、断网、刷新间隔内的短来电无法保证提醒；没有后台推送或系统通知权限请求。当前语言仍为中文。
- 新增诊断与能力页：固定只读采样、音频实验室入口、诊断ZIP导出。不执行shell、AT、额外ping、端口扫描或网络配置修改。诊断包排除号码、短信正文、联系人、设备唯一标识与凭据，包含IP、固件、接口计数、VPN健康及当前服务版本。
- 定位仅识别当前ModemManager是否公开Location接口，不启用定位、不读取位置；eSTK、VoWiFi、USSD明确标记未验证/未接入。真实UAC通话音频仍待现场支持，预备库未上线启用；没有任意AT终端。
- 新数据文件 /var/lib/ec20-console/workspace.json 使用0600原子持久化，存储联系人、通话记录和回收站元数据。原inbox/outbox/凭据格式不变，不增加任何polkit或系统设备权限。该文件异常时关闭本地管理功能，原自动主备独立运行。
- 浏览器演示覆盖联系人新增/回收/恢复、姓名联动、模拟接听挂断形成历史、设置保存刷新保留、短信回收站恢复及诊断导出。没有用真实号码发短信或打电话。
