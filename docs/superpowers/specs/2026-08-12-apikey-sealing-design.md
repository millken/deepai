# API Key 密封（apikey sealing）设计

日期：2026-08-12

## 问题

API key 目前以明文存放在 `~/.deepai/.env`（`pkg/commands/root.go:23` 在启动时把它加载进进程环境变量）。任何 CLI agent —— 包括 deepai 自己 —— 都能用 `read_file`、`grep` 或 `bash: printenv` 读到明文，然后把它送进远端模型的上下文、日志和会话记录。这是一条真实且高频的泄露路径。

本设计把 `.env` 里的密钥值替换成绑定本机硬件的密文，使读取该文件的收益归零，同时不改变任何现有的密钥消费路径。

## 威胁模型

按优先级排列，明确哪些防得住、哪些防不住。

| 威胁 | 是否防护 | 机制 |
| --- | --- | --- |
| agent 意外读取 `.env` 并把明文送进模型上下文 | ✅ 完全阻断 | 文件里只有密文 |
| `.env` 被提交进 git、贴给他人、录屏泄露 | ✅ 阻断 | 密文跨机不可用 |
| 进程环境变量泄露（`printenv`、`/proc/<pid>/environ`） | ✅ 附带获得 | 环境变量里存的就是密文，解密发生在使用点 |
| 同机同用户的主动提取 | ❌ **不防护** | 见下 |

### 非目标（必须明确，避免日后误信）

**这不是抗本地攻击者的密码学保护。** deepai 二进制以用户自己的身份运行，因此凡是它能无交互拿到的密钥材料，同用户的任何进程（包括 agent 的 bash 工具）原理上都能拿到。`buildSecret` 是公开常量，硬件序列号是本机可读的。

它提供的是**成本壁垒**：意外泄露被完全阻断，而拿到明文需要刻意的、多步的越狱行为 —— 这不是 LLM 会顺手做的事。想要真正抗本地提取只有引入人工交互（口令 / keychain 弹窗），本设计明确不走这条路，因为它会破坏非交互使用。

## 决策记录

| 决策 | 选择 | 理由 |
| --- | --- | --- |
| 主密钥来源 | 内置 `buildSecret` 常量 + 硬件指纹 HKDF 派生 | 零交互、零依赖；密文跨机失效 |
| `buildSecret` 取值 | 源码固定常量，`-ldflags` 可覆盖 | 重编译不失效；机密性不依赖它 |
| 密文存储位置 | 继续用 `~/.deepai/.env`，值换成 `enc:v1:…` | 改动最小；`export FOO=enc:v1:…` 自动兼容；明文向后兼容 |
| 指纹变化容错 | N-of-M 多重包裹（每块盘一个 wrap） | 换掉一块盘不锁死 |
| 指纹来源 | ghw 磁盘序列号，**无 build tag** | 一种源覆盖三平台；ghw 内部处理平台差异 |
| 无磁盘序列号时 | 三级阶梯降级，不退回明文 | 云主机 / WSL2 / VM 仍受保护 |
| 明文导出命令 | **不提供** | `bash: deepai key show` 比 `read_file .env` 更省事，等于递上越狱工具 |

## 架构

```
                      ~/.deepai/.env
                ANTHROPIC_API_KEY=enc:v1:…
                            │
              root.go 原样载入进程环境变量（仍是密文）
                            │
        registry.go resolveAPIKey() ──► secret.Reveal()
                            │                   │
                            │          pkg/secret/fingerprint.go
                            │                   │
                     明文（仅内存）        磁盘序列号 / 机器 ID / 常量
                            │
                      provider HTTP 请求
```

新增包 `pkg/secret`，两个职责严格分离：

- `secret.go` —— 密封格式与 AEAD 运算。不知道硬件的存在。
- `fingerprint.go` —— 发现绑定材料。不知道加密的存在。

二者通过 `[]source` 通信。`fingerprint` 的采集入口是可注入的，因此全部加密测试都不依赖真实硬件。

## `pkg/secret` API

```go
// Seal 加密 plaintext，绑定到本机。
func Seal(plaintext string) (string, error)

// Reveal 解密带 enc: 前缀的值；无前缀的值原样返回（明文向后兼容）。
func Reveal(raw string) (string, error)

// IsSealed 报告一个值是否为密封形式。
func IsSealed(raw string) bool

// Fingerprint 返回本机绑定材料的诊断信息：只含层级与源名，
// 以及每个源值的 sha256 前 8 位十六进制 —— 绝不含原值。
func Fingerprint() Info
```

刻意没有 `Export`、`Show`、`Dump` 之类的导出入口。

## 线格式

```
"enc:v1:" + base64.RawURLEncoding(blob)

blob:
  magic      [4]byte   "DPK1"
  version    uint8     = 1
  mode       uint8     1=硬件绑定 2=安装绑定 3=混淆
  nWraps     uint8
  wraps      nWraps × {
      nonce  [12]byte
      ct     [48]byte        AES-256-GCM(32 字节 DEK) + 16 字节 tag
  }
  dataNonce  [12]byte
  dataCT     变长            AES-256-GCM(plaintext) + 16 字节 tag
```

- 数据层 AAD：`"deepai-secret-v1"`
- wrap 层 AAD：`"deepai-kek-v1"`

108 字符的 Anthropic key、2 个 wrap = 263 字节 → base64 352 字符，`.env` 里一行。

### 硬性约束：blob 内不得含任何源值提示

序列号是唯一的机密材料 —— `buildSecret` 公开，格式公开。一旦把序列号（哪怕只是前缀或哈希）写进 blob，`.env` 就成了解密它自己的钥匙，整个绑定失效。

因此 blob 里没有 srcID、没有源名、没有序列号摘要。`mode` 字节只标明层级（三个值之一），不泄露源值，用于诊断与 `key check` 的显示。

### 解密：全试

解密时枚举本机**所有三个层级**的候选 KEK，对每个 wrap 逐一尝试 GCM open。两边都 ≤ 6 个，最多 36 次廉价运算。

这比"按源类型配对"少一整类 bug —— GCM 的认证标签自然告出哪个对了。

注意加解密的**非对称性**，这是有意的：

- **密封时**只取可用的最高层级（发现真磁盘序列号就绝不掺入低层级材料）
- **解密时**全部层级都试

若反过来在密封时也把低层级材料一并包进去，每个硬件绑定的密文都会额外带一个"万能 wrap"，N-of-M 的最弱环效应会让所有机器的绑定同时失效。

## KEK 派生

```go
KEK = HKDF-SHA256(
    secret: buildSecret,                              // 源码常量，-ldflags 可覆盖
    salt:   sha256(source.value + "\x00" + userID),
    info:   "deepai-kek-v1/" + source.tier,
)
```

`userID` 取 `user.Current().Uid` —— Windows 上返回 SID，Unix 上返回数字 uid，天然跨平台。不能用 `os.Getuid()`，它在 Windows 上返回 -1。取值失败时退化为空串（不阻断）。

`source.tier` 是三个固定字符串之一，与 `mode` 字节一一对应：

| `mode` | `tier` | 含义 |
| --- | --- | --- |
| 1 | `disk-serial` | 硬件绑定 |
| 2 | `machine-id` | 安装绑定 |
| 3 | `constant` | 混淆 |

`tier` 进入 HKDF 的 `info`，因此同一个源值在不同层级下派生出不同的 KEK —— 层级之间不会意外互通。

## 指纹层级

密封时自上而下取**第一个**有可用值的层级。

### 层级 1 —— 磁盘序列号（硬件绑定）

`ghw.Block()` 枚举块设备，取 `Disk.SerialNumber`。ghw 在 Linux 读 sysfs/udev、macOS 走 ioreg+plist、Windows 走 WMI，三者均不需要 root/admin。

排除规则：

- `IsRemovable` 为真 —— 否则密封时插着的 U 盘会变成一条额外解密路径
- 退化值：空、纯空白、字面量 `"unknown"`（**ghw 在读不到时返回这个字符串**，不过滤就会变成一把所有机器共享的万能钥匙）、全零、长度 < 6，以及占位符 `"None"`、`"N/A"`、`"To Be Filled By O.E.M."`、`"Not Specified"`（大小写不敏感）

**不使用 WWN**：廉价 NVMe 固件常报无意义的全零 WWN，而 `SerialNumber` 是好的。开发机上实测：

```
nvme0n1  serial=2425130401001      wwid=eui.0000000000000002   ← WWN 全零垃圾
nvme1n1  serial=S7U4NU0Y444140F    wwid=eui.0025385451a19572
```

序列号排序后，**每块盘各出一个 wrap**。开发机上是 2 个，因此换掉其中一块不锁死。

不使用主板 / 机箱序列号：`/sys/class/dmi/id/product_uuid`、`product_serial`、`board_serial` 在现代 Linux 内核上是 `0400` root-only（正是为防指纹追踪），实测确认不可读。

不使用 MAC 地址：需要 `addr_assign_type == 0` 才能区分永久 MAC 和随机化 MAC（开发机的 WiFi 网卡 `addr_assign_type=3`，NetworkManager 每换热点变一次），而 ghw 不暴露这个判断。磁盘序列号已足够，不值得为此引入平台代码。

### 层级 2 —— OS 机器 ID（安装绑定）

仅当层级 1 采到 0 个源时启用。适用于云主机、WSL2、虚拟机 —— 虚拟磁盘常报 `"unknown"` 或空序列号。

- Linux：`/etc/machine-id`，退化到 `/var/lib/dbus/machine-id`
- Windows：注册表 `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`，经 `golang.org/x/sys/windows/registry`（`x/sys` 已是现有依赖）
- 其他平台：无，落到层级 3

这是本设计中唯一的 build-tag 代码：两个小文件加一个 stub，约 40 行。不引入 `denisbrodbeck/machineid` —— 它 2019 年后停止维护、连 `go.mod` 都没有，而这里需要的逻辑只有几行。

机器 ID 是一个**文件/注册表值**，可以被复制，因此它不足以承担层级 1 的角色。但在层级 2 的位置上它相比常量是纯增益：它让云主机上的密文提交进 git 后，别的机器仍然打不开。

### 层级 3 —— 固定常量（混淆）

前两级都不可用时的兜底。此时**没有任何机器绑定** —— 拿到密文和二进制的任何人都能解开。

保留它的理由：它仍然 100% 阻断首要威胁（agent 读到密文而非明文），严格优于退回明文。

## 模式必须可见

降级是静默的安全性损失，因此模式必须一眼可见，否则用户会以为有硬件绑定、其实没有：

- `key set` 在降级到层级 2 或 3 时当场打印警告
- `key list` 和 `key check` 明确打印「硬件绑定（2 块磁盘）」/「安装绑定（机器 ID）」/「混淆（无机器绑定）」

## 失败必须响亮

解密失败（硬件已变化、密文被篡改、版本号不认识）必须返回明确错误并中止 provider 初始化。

绝不能静默降级成空 key 去发请求 —— 那样用户收到的是一个莫名的 401，而真因是指纹不匹配。

错误信息要可诊断且不泄密：

```
无法解密 ANTHROPIC_API_KEY：密文封装时绑定了 2 个源，
当前机器找到 1 个（硬件绑定模式），均不匹配。
磁盘可能已更换。请运行 `deepai key set anthropic` 重新录入。
```

数量来自 `nWraps`，模式来自 `mode` 字节，当前源来自本机采集 —— 都不需要回读被封装的源值。错误信息中永不包含密钥内容。

未知的 `version` 字节必须报错，**不能**静默当作明文透传 —— 否则未来的格式升级会让旧版二进制把密文当 API key 发出去。

## 集成点

只有三处需要动。

### 1. `pkg/llm/registry.go:299` `resolveAPIKey()`

结果过一遍 `secret.Reveal`，签名改为 `(string, error)`。

### 2. `pkg/llm/registry.go:52` `resolveConfig()`

`overrides.APIKey`（来自 `ProviderConfig`）也可能是密文，同样处理。

### 3. `pkg/commands/setup.go:324`

`saveEnvValue` 之前先 `secret.Seal`。

同时改掉 `setup.go:308` 的 `loadEnvValue` 回填：不再把已存值填回输入框，改为提示「(已加密，留空保持不变)」。

### 顺带修复的两个现存问题

这两处都在本次改动的直接路径上：

- `providerCacheKey()`（`registry.go:276`）把**明文 API key 拼进 map 键**。改成拼 `sha256(key)` 的十六进制前缀 —— 缓存键没必要持有明文。
- `ProviderFor()` 路径上 `resolveAPIKey()` 被调用**两次**（`providerCacheKey` 一次、`buildProviderFromDef` 一次）。改成解析一次往下传，既去重又避免解密跑两遍。

## CLI

```
deepai key set [provider]   交互录入（EchoModePassword）→ Seal → 写 .env
deepai key list             每个 provider 的状态：sealed / plaintext / absent + 掩码 + 模式
deepai key seal             把 .env 里现存明文 key 就地加密
deepai key check            打印指纹层级与源（名 + 值的 sha256 前 8 位）+ 每个密钥能否解开
```

`key check` 在首次运行 macOS / Windows 构建时尤其关键：ghw 的 Darwin product/serial 支持在其文档中带 caveat，而项目开发环境只有 Linux。`key check` 让用户一眼看到实际采到几个源、处于哪个层级。

不提供 `key show` / `key export`。要更换密钥只能重新录入。

### `key seal` 不留明文备份

「加密前先备份 `.env.bak`」是本能反应，但那会在磁盘上留下一份明文副本，正好抵消整个功能的目的 —— 而且 `.env.bak` 不在 `.gitignore` 里，比原文件更危险。

改用**先验证后写入**，全程不落明文：

1. 逐个 `Seal` 明文值，对每个结果立即 `Reveal` 并与原文比对
2. 任一条不匹配 → 中止，原文件不动
3. 全部通过 → 写临时文件（0600，与 `.env` 同目录）后 `rename` 原子替换

同目录是 `rename` 原子性的前提（跨文件系统的 rename 会退化成复制）。0600 在创建时就设定，不能先创建再 `chmod` —— 那之间存在一个可被读取的窗口。

## 测试策略

全部加密测试通过注入假源运行，不依赖真实硬件。

`pkg/secret`：

- roundtrip：Seal → Reveal 得回原文
- 明文透传：无 `enc:` 前缀的值原样返回，`Reveal` 不报错
- 篡改检测：翻转密文任一字节 → GCM 认证失败
- N-of-M：3 源密封，解密时只喂 1 源 → 成功
- 跨机模拟：替换全部源值 → 失败，错误信息含 wrap 数量与模式，且**不含**密钥内容
- 未知 version 字节 → 报错，不透传
- 空明文、空源列表的边界行为
- `mode` 字节随密封时的层级正确落值

`fingerprint`：

- 退化值过滤：`""`、`"unknown"`、`"0000000000"`、`"N/A"`、`"To Be Filled By O.E.M."`、短值全部被剔除
- `IsRemovable` 的盘被剔除
- 层级选择：有磁盘序列号时不掺机器 ID；无磁盘序列号时用机器 ID；都无时用常量
- 序列号排序后 wrap 顺序稳定

`pkg/llm`：

- `resolveAPIKey` 对密文和明文都返回正确结果
- 解密失败时 provider 初始化返回错误而非空 key
- `providerCacheKey` 不含明文密钥

`key seal`：

- 加密后目录中不存在任何含明文的文件（遍历目录断言，覆盖 `.env.bak` 这类残留）
- 注入一个使 roundtrip 失败的假源 → 原 `.env` 内容逐字节不变
- 写出的 `.env` 权限为 0600
- 已是密文的条目被跳过，不会二次封装

## 依赖变更

新增 `github.com/jaypipes/ghw`，净新增 4 个模块：`jaypipes/pcidb`、`yusufpapurcu/wmi`、`howett.net/plist`、`go-ole/go-ole`。`gopkg.in/yaml.v3` 与 `golang.org/x/sys` 已是现有依赖。

只调用 `ghw.Block()`，因此 ghw 的 `ethtool` shellout 路径（网卡相关）不会被触发。

## 迁移

明文永久向后兼容 —— `Reveal` 对无前缀的值原样返回。现有安装无需任何动作即可继续工作。

用户可在任意时刻运行 `deepai key seal` 就地加密。CI 与容器环境可以继续用明文环境变量。
