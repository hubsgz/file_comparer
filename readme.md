# file_comparer

一个用 Go 写的**目录重复文件比对 / 清理工具**。

它按 **「文件大小 + SHA-256」** 判定两个文件是否真的相同 —— 文件名、所在路径、修改时间一概不作数。支持千万级文件、TB 级目录的比对，纯标准库、零依赖、单文件主程序。

同时附带一个辅助工具 `delEmptyDir`，用于删除文件清理后残留的空目录。

---

## 目录

- [为什么需要它](#为什么需要它)
- [判定规则](#判定规则)
- [安装与编译](#安装与编译)
- [快速开始](#快速开始)
- [参数详解](#参数详解)
- [两种分组模式](#两种分组模式)
- [输出说明](#输出说明)
- [删除安全性](#删除安全性)
- [推荐工作流](#推荐工作流)
- [辅助工具 delEmptyDir](#辅助工具-delemptydir)
- [性能与内存](#性能与内存)
- [常见问题](#常见问题)
- [注意事项](#注意事项)

---

## 为什么需要它

常见的「重复文件清理」工具要么只看文件名，要么只看大小，都会误判：

| 判断依据 | 问题 |
| --- | --- |
| 文件名相同 | `A\1.mp4` 和 `B\1.mp4` 名字一样，内容可能完全不同 |
| 路径相同 | 两个盘上的备份，路径结构往往早就不一样了 |
| 修改时间 | 复制、同步、解压都会改变时间 |
| 文件大小 | 同尺寸的视频/图片极其常见 |

本工具只用 **内容 SHA-256** 作为最终判据，宁可慢一点，也绝不误删。

---

## 判定规则

**两个文件被判定为「相同」，当且仅当：它们的 SHA-256 完全相同。**

在此之前会用「分组键」做一轮低成本预筛，减少需要读内容的文件数量：

```
遍历两个目录（只读元数据，不读内容）
        │
        ▼
按 -group-by 的分组键分组   （size = 文件大小 / name = 文件名）
        │
        ▼
只保留「两边都出现」的分组键 ──► 候选文件（大部分文件在这里被淘汰）
        │
        ▼
流式计算 SHA-256（1MB 缓冲，不整文件载入内存）
        │
        ▼
按哈希求交集 ──► 输出 SAME / 执行删除
```

- `size` 模式下，分组键就是文件大小，因此**隐含要求大小相同**；
- `name` 模式下，分组键是文件名（**不区分大小写**），大小可以不同，最终仍由哈希决定。

---

## 安装与编译

要求 **Go 1.21+**，无需任何第三方依赖。

```bash
git clone https://github.com/<your-name>/file_comparer.git
cd file_comparer

# 主工具
go build -o file_comparer .

# 辅助工具
go build -o delEmptyDir ./AdditionalAuxiliaryTools/delEmptyDir
```

Windows 下可直接：

```cmd
go build -o file_comparer.exe .
go build -o delEmptyDir.exe ./AdditionalAuxiliaryTools/delEmptyDir
```

交叉编译：

```bash
GOOS=linux   GOARCH=amd64 go build -o file_comparer .
GOOS=darwin  GOARCH=arm64 go build -o file_comparer .
```

> 仓库自带 `.gitignore`，编译产物、IDE 配置和运行时生成的 `*.csv` 报告都不会被提交。

---

## 快速开始

```bash
# 1）只对比，不删任何东西，报告打到终端
file_comparer -current F:\2022 -target F:\202201

# 2）只比对视频/图片，结果写入 CSV
file_comparer -current F:\2022 -target F:\202201 -ext .mp4,.jpeg -report dup.csv

# 3）先试运行，看看会删掉哪些
file_comparer -current F:\2022 -target F:\202201 -delete-mode current -dry-run

# 4）确认无误后真删
file_comparer -current F:\2022 -target F:\202201 -delete-mode current -yes

# 5）删完之后清理残留空目录
delEmptyDir -target F:\2022 -dry-run
delEmptyDir -target F:\2022
```

---

## 参数详解

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-target` | **必传** | 目标目录路径 |
| `-current` | `.` | 当前目录（即"源"目录），默认当前工作目录 |
| `-group-by` | `size` | 分组方式：`size` / `name`，见[两种分组模式](#两种分组模式) |
| `-ext` | 空（不限制） | 只处理这些后缀，逗号分隔，如 `-ext .mp4,.jpeg` |
| `-exclude-name` | 空（不排除） | 跳过这些文件名，逗号分隔，如 `-exclude-name tmp.jpeg,tmp.mp4` |
| `-min-size` | `0` | 忽略小于该字节数的文件 |
| `-delete-mode` | `notdelete` | `notdelete` / `current` / `target`，见[删除安全性](#删除安全性) |
| `-dry-run` | `false` | 试运行，只报告不删除 |
| `-yes` | `false` | 跳过删除前的交互确认 |
| `-report` | 空（stdout） | CSV 报告输出文件；人读日志始终走 stderr |
| `-max-report` | `20` | 每个重复组最多输出的配对行数，`0` 表示不限制 |
| `-report-unmatched` | `true` | 同组但哈希不同时也输出 `DIFF` 行，便于排查 |
| `-quiet` | `false` | 不打印周期进度 |

---

## 两种分组模式

### `-group-by size`（默认）

按**文件大小**分组。只有两边都存在同一大小的文件，才会去读内容算哈希。

- ✅ 能找出**改过名**的重复文件（最常见的备份重复场景）
- ⚠️ 候选集最大：同尺寸文件多时要读的内容也多，速度取决于磁盘

```bash
file_comparer -current A -target B -group-by size -report dup.csv
```

### `-group-by name`

按**文件名**分组（转小写，不区分大小写）。只对两边同名的文件做哈希。

- ✅ 极快：候选集最小，几乎不读盘
- ⚠️ **发现不了改名后的重复**；两边命名规则不同的话，结果可能是 0

```bash
file_comparer -current A -target B -group-by name -report dup.csv
```

> 两种模式最终都靠 SHA-256 确认，**都不会误判**。区别只在于"能找到多少"和"要读多少盘"。

---

## 输出说明

### CSV 列

```
action,size,sha256,current_path,target_path,note
```

### `action` 取值

| action | 含义 |
| --- | --- |
| `SAME` | 两边文件哈希相同，判定为重复 |
| `SAME_TRUNCATED` | 该组配对数超过 `-max-report`，只输出了前 N 条 |
| `DIFF` | 分到同一组但**内容不同**，`note` 里带对方的 size 与 sha256，用于排查 |
| `DIFF_TRUNCATED` | DIFF 行数超过 `-max-report` |
| `DELETE_CURRENT` / `DELETE_TARGET` | 已删除（当前目录 / 目标目录侧） |
| `DRYRUN_DELETE_CURRENT` / `DRYRUN_DELETE_TARGET` | 试运行，将会删除 |
| `DELETE_FAILED` | 删除失败，原因见 `note` |

### 汇总信息（stderr）

```
分组方式       : size
当前目录文件数 : 24066
目标目录文件数 : 16424
跳过非普通文件 : 0
分组键数量     : 当前 24066 组，目标 16424 组
候选分组(交集) : 1 组
参与哈希文件数 : 2
哈希不一致     : 1 组（同组但内容不同，详见 CSV 的 DIFF 行）
重复配对数     : 0
当前目录重复   : 0 个文件
目标目录重复   : 0 个文件
重复占用空间   : 0 B (按当前目录侧计)
耗时           : 117ms
```

其中 **`候选分组(交集)` / `参与哈希文件数` / `哈希不一致`** 是排查"为什么没找到重复"的关键指标，见[常见问题](#常见问题)。

---

## 删除安全性

- 默认 `notdelete`，**绝不删任何东西**；
- 只有显式指定 `-delete-mode current` 或 `target` 才会删除；
- 真实删除前有**交互确认**（输入 `yes` 才继续），可用 `-yes` 跳过；
- 强烈建议先跑 `-dry-run`；
- 删除侧的文件被删的前提是：**另一侧存在 SHA-256 完全相同的副本**，因此数据不会整体丢失；
- 报告文件本身会自动排除，不会被自己扫描进去；
- 目录互相嵌套时会打印警告。

---

## 推荐工作流

```bash
# ① 试运行，先看 CSV
file_comparer -current F:\2022 -target F:\202201 -report dup.csv -delete-mode current -dry-run

# ② 人工抽查 dup.csv 里的 SAME 行

# ③ 真删
file_comparer -current F:\2022 -target F:\202201 -report dup.csv -delete-mode current -yes

# ④ 清理空目录
delEmptyDir -target F:\2022 -dry-run
delEmptyDir -target F:\2022
```

---

## 辅助工具 delEmptyDir

删除文件后往往会留下一堆空文件夹，这个工具用来递归清理它们。

### 用法

```bash
delEmptyDir -target <目录> [-dry-run]
```

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-target` | **必传** | 要清理的目录 |
| `-dry-run` | `false` | 只列出将要删除的空目录，不实际删除 |

```bash
delEmptyDir -target F:\2022 -dry-run   # 预览
delEmptyDir -target F:\2022            # 执行
```

### 行为说明

- **自底向上**递归：先清空子目录，再判断父目录是否变空；
- **根目录本身永远不会被删除**；
- **符号链接不跟随**，且会被当作普通条目占位 —— 避免误删链接指向的目标，也保证父目录不会被误判为空；
- 读取失败的目录视为"非空"，**保守不删**，只计入 `failed`；
- 输出 `scanned / removed / failed` 三个计数。

```
[clean] target dir: F:\2022
Removed empty dir: F:\2022\2022012915\sub
Cleanup completed. scanned: 128, removed: 37, failed: 0
```

---

## 性能与内存

- **遍历阶段**用 `filepath.WalkDir`，只取元数据（`d.Info()`），不读文件内容，速度快；
- **哈希阶段**用 1MB 缓冲流式读取，单个大文件不会整块载入内存；
- 内存主要消耗在"路径索引"上，量级约为 **文件总数 × 平均路径长度**（约 150~200 字节/文件）。百万级文件约几百 MB，属于可接受范围；千万级文件建议：
  - 用 `-min-size` 先过滤掉小文件；
  - 用 `-ext` 缩小扫描范围；
  - 或按子目录分批跑；
- **瓶颈在磁盘 I/O**：`size` 模式需要真实读取所有同尺寸候选文件的内容，机械盘会比较慢；`name` 模式几乎不读盘。

---

## 常见问题

### Q：跑出来「重复配对数 0」，是不是程序有问题？

按顺序看汇总里这三个指标：

1. **`候选分组(交集) = 0`** —— 两边分组键完全没有交集，一个文件都没进哈希阶段。
   - `name` 模式：说明两边没有同名文件（很可能文件被改过名了）→ 改用 `-group-by size`。
   - `size` 模式：说明两边没有任何大小相同的文件，比较少见。
   - 程序会自动打印两边的示例分组键，方便对比。

2. **`候选分组 > 0` 但 `参与哈希文件数` 很小** —— 候选太少，同样考虑换分组方式。

3. **`参与哈希文件数 > 0` 但 `哈希不一致 > 0`** —— 文件确实被读了，但 **SHA-256 不一样，内容就是不同**。看 CSV 里的 `DIFF` 行，对比双方的 `size` 和 `sha256`：
   - size 不同 → 本来就是两个不同的文件（重新转码 / 剪辑过）；
   - size 相同但 sha256 不同 → 可能只是元数据（moov atom、封面、创建时间）不同，也可能是数据损坏。

   可用系统工具独立复核：
   ```cmd
   certutil -hashfile "F:\a.mp4" SHA256
   certutil -hashfile "F:\b.mp4" SHA256
   ```

### Q：文件太多、目录太大，怎么缩小范围？

用 `-ext` 限定后缀、`-exclude-name` 排除特定文件名、`-min-size` 跳过小文件，或按子目录分批跑。

### Q：`-report` 不指定会怎样？

CSV 打到 stdout，人读日志打到 stderr，方便管道处理：
```bash
file_comparer -target B 2>log.txt >dup.csv
```

### Q：会删掉两个目录下的所有副本吗？

不会。只会删 `-delete-mode` 指定那一侧的文件，且前提是另一侧存在相同哈希的副本。

---

## 注意事项

- **删除不可逆**，请务必先 `-dry-run` 并抽查 CSV；
- 比对过程中文件若被其他程序修改，哈希结果可能不一致，属正常现象；
- 目录互相嵌套（一个目录是另一个的子目录）会导致重复扫描，程序会警告，请避免；
- 符号链接、快捷方式等非普通文件会被跳过并计入 `跳过非普通文件`；
- 作者对使用本工具造成的数据损失不承担责任 —— 请先备份。
