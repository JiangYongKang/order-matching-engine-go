# 本地撮合与执行引擎（Go）

一个零第三方依赖、单进程内运行的本地交易撮合与执行引擎。用内存中的订单簿与
账户状态模拟真实交易流程，支持限价/市价订单、部分成交、撤销与改单、账户级与
标的级风险约束、自成交防护，并通过持久化的事件流水实现**确定性回放与中断恢复**。

- 模块路径：`github.com/highcumontoa/order-matching-engine-go`
- 仅使用 Go 标准库；金额为最小货币单位的 `int64` 整数，数量为 `int64` 整数。

---

## 1. 快速开始

```go
import "github.com/highcumontoa/order-matching-engine-go"

// JournalPath 为空时为纯内存引擎；给路径则在启动时自动回放恢复并继续追加流水。
e, err := matching.NewEngine(matching.Config{
    JournalPath: "data/events.log",
    STP:         matching.STPRejectTaker,
    Limits: matching.RiskLimits{
        MaxAccountBuyNotional: 1_000_000,
        MaxSymbolPosition:     10_000,
        MaxGlobalExposure:     100_000_000,
    },
})

e.OpenAccount("A", 1_000_000)            // 开户 + 初始现金
e.Deposit("A", "AAA", 100)               // 入券（持仓）

r := e.Submit(matching.SubmitOrder{      // 下限价单
    ID: "o1", Account: "A", Symbol: "AAA",
    Side: matching.Buy, Type: matching.Limit, Price: 100, Qty: 10,
})

e.Cancel(matching.CancelOrder{ID: "o1"}) // 撤单
e.Amend(matching.AmendOrder{ID: "o1", NewPrice: 100, NewQty: 8}) // 同价改量
```

本地验证：

```bash
go build ./...
go vet ./...
go test -v ./...                 # 运行全部单测（含关键流水日志）
go test -race -count=3 ./...     # 竞态检测 + 重复运行
```

---

## 2. 撮合规则

### 2.1 价格-时间优先

- 买单按价格**从高到低**、卖单按价格**从低到高**成交（价格优先）。
- 同一价位严格按进入订单簿的先后顺序成交。先后顺序由引擎在临界区内分配的
  **全局单调递增 `Seq`** 决定（而不是墙钟时间），因此回放后优先级完全一致。
- 一笔主动单可与多个价位、多个被动单连续成交，每次成交生成一个成交事件。

### 2.2 限价单（Limit）

- 买入按 `价格 × 数量` **全额冻结现金**，卖出按数量**全额冻结持仓**，
  必须足额，否则整单拒绝（`INSUFFICIENT_FUNDS` / `INSUFFICIENT_POSITION`）。
- 能成交的部分立即成交，剩余部分挂入订单簿等待。
- 成交结算时先按挂单价释放对应冻结，再按实际成交价交割；买单成交价优于
  挂单价时，差额自然留在可用现金中。
- 不允许卖空：卖出数量不得超过账户可用持仓。

### 2.3 市价单（Market）

- 市价单为 **IOC（Immediate-or-Cancel）**：只立即成交，不进入订单簿。
- 买入可成交量还受「可用现金」与单标的持仓上限约束，卖出受「可用持仓」约束：
  沿对手盘逐档计算可承受数量，承受量为 0 时停止。
- 零成交时按原因**整单拒绝**：无对手盘为 `NO_LIQUIDITY`，
  现金为 0 为 `INSUFFICIENT_FUNDS`，持仓为 0 为 `INSUFFICIENT_POSITION`。
- 部分成交后剩余数量立即撤销，订单终态为 `CANCELLED`（事件中注明
  `NO_LIQUIDITY` 及剩余数量）。

### 2.4 撤销（Cancel）

- 仅 `NEW` / `PARTIALLY_FILLED` 状态的挂单可撤销。
- 撤单释放剩余部分对应的全部冻结现金/持仓并从订单簿移除。
- 撤不存在的订单 → `UNKNOWN_ORDER`；撤已终结订单 → `ORDER_CLOSED`。

### 2.5 改单（Amend）——不丢失优先级、不重复成交

- **仅允许同价改量**：携带不同价格的改单被明确拒绝
  （`AMEND_PRICE_CHANGE_FORBIDDEN`）。因为改价必然需要重新排队、丢失时间
  优先级，本引擎不提供"悄悄丢失优先级"的语义，调用方应先撤再以新价格重挂。
- 同价改量**原地修改订单数量，不改变其在价位队列中的元素位置**，因此严格
  保留原时间优先级。
- 缩量：立即释放多余冻结；增量：必须重新通过资金/持仓与风险校验，任一不满足
  则整单拒绝且状态不变（不产生部分生效）。
- 改单数量非法（≤ 已成交数量）→ `AMEND_INVALID_QUANTITY`。
- 成交、冻结与改单在同一条状态迁移路径上，改单不会造成重复成交。

### 2.6 自成交防护（STP）

通过 `Config.STP` 配置，语义确定：

| 策略 | 行为 |
| --- | --- |
| `STPRejectTaker`（默认） | 主动单一旦会与同账户挂单成交，**主动单整体拒绝**，此前规划的成交一并作废，订单簿与账户零改动 |
| `STPCancelMaker` | 撤销同账户的被动挂单（释放其冻结，原因记为 `SELF_TRADE`），主动单继续与后续挂单撮合 |

---

## 3. 风险限额与拒单语义

限额字段为 `0` 表示不限制。所有判定都在指令改变订单簿/账户**之前**完成；
触发即整体拒绝，**不留下部分生效的中间状态**。

| 限额 | 判定语义 | 拒绝原因 |
| --- | --- | --- |
| `MaxAccountBuyNotional` | 单账户当前在簿买单冻结名义金额 + 本单全额 ≤ 上限。仅对买入限价单生效（市价即时成交不挂簿、卖出不占买单额度） | `RISK_ACCOUNT_LIMIT` |
| `MaxSymbolPosition` | 买单按「账户该标的净持仓（可用−卖单冻结） + 本单数量」 ≤ 上限；市价买单沿对手盘逐档用剩余额度截断可成交量 | `RISK_SYMBOL_LIMIT` |
| `MaxGlobalExposure` | 全引擎全部订单簿上买卖双边在簿名义金额 + 本单全额 ≤ 上限 | `RISK_GLOBAL_LIMIT` |

完整的可区分拒绝原因（`RejectReason`）：

```
INVALID_PARAM            指令字段非法（空账户/标的/方向/类型）
INVALID_QUANTITY         数量非正或名义额溢出
INVALID_PRICE            限价非正，或市价单携带了价格
UNKNOWN_ACCOUNT          账户不存在
DUPLICATE_ORDER_ID       订单 ID 重复（订单 ID 全引擎唯一）
UNKNOWN_ORDER            撤/改的订单不存在
ORDER_CLOSED             订单已成交/撤销/拒绝，不可再操作
INSUFFICIENT_FUNDS       可用现金不足
INSUFFICIENT_POSITION    可用持仓不足
RISK_ACCOUNT_LIMIT       触发单账户买单名义额上限
RISK_SYMBOL_LIMIT        触发单标的持仓上限
RISK_GLOBAL_LIMIT        触发全引擎敞口上限
SELF_TRADE               触发自成交防护
AMEND_PRICE_CHANGE_FORBIDDEN  改单尝试改价
AMEND_INVALID_QUANTITY   改单数量非法
NO_LIQUIDITY             市价单无对手盘
```

被拒绝的指令会以 `ORDER_REJECTED` 事件进入流水（便于审计与复现），但
**不改变任何资金、持仓与订单簿**；订单实体以 `REJECTED` 状态留档供查询。

---

## 4. 事件流水（Journal）

### 4.1 存储格式

- JSON Lines：每条记录一行，形如 `<crc32-hex>|<json>\n`。
  - 前缀为 payload 的 IEEE CRC32 校验和（8 位十六进制）。
  - payload 为一条 `JournalRecord`：`{seq, time, events:[...]}`。
- 一条**指令**（含开户、入金/入券、下单、撤单、改单、拒单）产生的全部事件
  打包进同一条记录原子追加；`append` 后立即 `flush + fsync`。
- `Seq` 为全引擎单调递增序号，既是时间优先令牌，也是回放的幂等边界。

事件类型：`ACCOUNT_OPENED`、`DEPOSIT`、`ORDER_ACCEPTED`、`ORDER_REJECTED`、
`ORDER_FILLED`、`ORDER_CANCEL`（撤单/STP 撤被动方/IOC 剩余）、`ORDER_AMENDED`。

### 4.2 唯一状态迁移路径（实时与回放一致）

指令处理分两阶段，保证回放结论与增量运行严格一致：

1. **纯规划（不修改状态）**：校验、风控、可行性判定、撮合匹配，产出一批事件。
   自成交整体拒绝、资金不足等都在这一阶段决定，因此天然没有中间状态。
2. **先落盘，后施加**：事件记录先 fsync 持久化，成功后由唯一入口
   `applyRecord` 依次施加到内存状态（冻结/结算/入簿/离簿/改量）。

崩溃回放时同样调用 `applyRecord`，与实时执行共用同一套代码，因此不存在
"实时逻辑"与"回放逻辑"两条路径漂移的可能。

### 4.3 回放与中断恢复

```go
// 全新只读回放（不追加、不 fsync），重复执行无副作用
e, _ := matching.ReplayFromJournal("data/events.log", cfg)

// 崩溃后显式恢复：截断末尾半截写入，返回截断字节数
n, _ := matching.RecoverJournal("data/events.log")
```

- **已提交记录**：以换行结束且 CRC 校验通过，回放时全部重放。
- **半截写入**：崩溃导致的末尾不完整行（无换行）在 `NewEngine`/
  `RecoverJournal`/`ReplayFromJournal` 时被截断，绝不施加状态。
- **损坏检测**：完整但 CRC 不符的记录视为流水损坏，返回错误（含行号/偏移），
  **绝不静默丢弃**。
- **幂等**：重复回放只读打开文件，字节不变、结论一致；恢复后可继续追加，
  新的增量与再次回放结论一致。

---

## 5. 并发与确定性

- 引擎内部为每个标的维护独立订单簿；所有指令（提交/撤/改/查询）在一把
  `sync.Mutex` 的临界区内按**调用到达顺序**分配 `Seq` 并串行撮合。
- 因此多标的、多账户的并发提交是**无数据竞争**的，且任意并发交错的结果都
  严格等价于"指令实际到达顺序"的逐一串行执行。
- 时间优先使用逻辑 `Seq` 而非墙钟时间，配合事件流水保证结果可复现。
- 测试中通过以下方式验证（见 `concurrent_test.go`、`race_test.go`）：
  - 固定顺序下并发与串行快照逐字段一致；
  - 多个随机种子下 goroutine 同时提交，按实际生效 `Seq` 还原到达序列后
    串行重放，逐指令结论与最终快照完全一致；
  - 同一挂单"吃单 vs 撤单"、"改单 vs 吃单"高并发竞态，校验成交/撤销互斥、
    冻结额恒等于订单剩余名义额、资金与持仓守恒（以 `-race` 重复运行）。

---

## 6. 代码结构

| 文件 | 职责 |
| --- | --- |
| `types.go` | 方向/类型/状态/拒绝原因枚举、订单与指令结构体 |
| `engine.go` | 引擎入口、指令处理、校验、拒绝持久化、查询与快照 |
| `match.go` | 纯规划式撮合（价格-时间优先、市价预算、STP） |
| `orderbook.go` | 单标的订单簿（有序价位 + 时间优先队列） |
| `ledger.go` | 账户资金/持仓的冻结、释放、成交结算（含溢出检测） |
| `risk.go` | 三类限额判定与可解释的判定描述 |
| `event.go` / `journal.go` | 事件/记录定义、CRC 校验的 JSONL 流水读写 |
| `replay.go` | 回放、`applyRecord` 状态迁移、半截写入截断恢复 |
| `snapshot.go` | 账户/订单簿/全引擎快照（深拷贝，供对账与测试） |
| `*_test.go` | 撮合、拒单、市价、撤改、风控、自成交、并发竞态、回放恢复测试 |

---

## 7. 测试覆盖一览

- 价格优先、同价位严格时间优先、部分成交与挂簿（`matching_test.go`）。
- 各类非法/不足指令的可区分拒绝，以及拒绝后账户与订单簿不变（`orders_test.go`）。
- 市价单即时成交、预算截断、部分成交后 IOC 撤销（`orders_test.go`）。
- 撤单释放冻结、重复撤单/撤不存在订单（`orders_test.go`）。
- 同价改量保留优先级、改价被拒、增量资金校验、缩量释放冻结（`orders_test.go`）。
- 三类风险限额的边界触发、成交后额度释放（`risk_test.go`）。
- 两种 STP 策略及"拒绝零副作用 / 撤被动方继续撮合"（`risk_test.go`）。
- 多标的并发与串行等价、多种随机交错可复现、`-race` 压测与资金守恒
  （`concurrent_test.go`、`race_test.go`）。
- 事件回放与增量一致、重复回放幂等、半截写入中断恢复、CRC 篡改检测
  （`journal_test.go`）。

单测使用 `-v` 时会打印关键订单流水（下单/成交/拒单原因/账户与簿快照），
便于失败时区分根因。
