# 本地撮合与执行引擎（Go）

一个零第三方依赖的本地交易模拟引擎：内存订单簿 + 内存账户状态，支持限价/市价
订单的提交、部分成交、撤销、改单，支持 **GTC/IOC/FOK 三种下单时效**，支持
账户级/标的级/整体三层风险约束，事件持久化到仅追加（append-only）流水文件，
可在任意时刻依据流水完整、确定地回放与中断恢复；多标的并发提交的结论与按
流水顺序逐一执行完全等价。

- 语言：Go 1.25，仅标准库
- 持久化：二进制分帧（magic + length + CRC32 + gob payload），每条事件
  `write + fsync` 后返回；崩溃产生的残缺尾帧在打开时按 CRC/长度检出并截断
- 并发：每标的一个单写分片协程 + 一把全局锁保护账户/风险/流水；无数据竞争

## 1. 撮合规则

### 1.1 订单类型

| 类型 | 价格 | 默认时效（`TIF` 缺省） | 行为 |
| --- | --- | --- | --- |
| 限价单 `LIMIT` | 必须为正整数 | `GTC` | 能成交立即成交；剩余数量进入订单簿挂单 |
| 市价单 `MARKET` | 不允许携带价格 | `IOC` | 仅与现有对手盘成交，未成交剩余直接作废，**绝不挂单** |

数量必须为正整数；所有金额/数量均为 `int64`，不使用浮点，保证可复现。

### 1.2 下单时效（Time-In-Force）

`OrderRequest.TIF` 决定“成交不完的那部分怎么办”。缺省值 `TIFDefault(0)` 完全
保持历史行为：限价按 `GTC`、市价按 `IOC` 处理，因此**不传该字段的旧调用零
影响**。

| 时效 | 规则 | 成交不完时 | 最终状态 |
| --- | --- | --- | --- |
| `GTC`（Good-Till-Canceled） | 能成交立即成交，剩余挂单 | 剩余进入订单簿，继续占价位/占额度，等待后续成交或撤单 | `NEW` / `PARTIALLY_FILLED` |
| `IOC`（Immediate-Or-Cancel，立即成交剩余撤销） | 按价格-时间优先能吃多少吃多少 | **剩余立即撤销，绝不挂单、绝不继续冻结**资金/持仓；冻结按实际成交量计算（即时成交部分与市价单一样直接结算，不预冻剩余） | 全部成交 `FILLED`；有剩余被撤 `CANCELED`（哪怕一笔都没成交） |
| `FOK`（Fill-Or-Kill，全部成交否则撤销） | 提交时先判断当前可成交量是否覆盖整单 | **不够就整单拒绝（`WOULD_NOT_FILL`）：一笔成交都不会发生**，订单簿、账户资金/持仓/冻结、订单状态均无任何变化；够则一次性全部成交，不挂单 | 成功 `FILLED`，否则 `REJECTED` |

判定与兼容边界（确定性，无“先吃一部分再发现不够”的中间态）：

- IOC/FOK 可用于限价单与市价单；**市价单不允许 `GTC`**（以 `INVALID_TIME_IN_FORCE`
  拒绝）；非法时效值同样以 `INVALID_TIME_IN_FORCE` 拒绝。
- FOK 的“够不够”按提交瞬间对手盘按价格-时间优先**贪心累加**到整单量判断；
  判断与资金/风险校验都先于任何状态变更，所以拒绝时 maker 挂单与全部账户
  状态逐字节不变（`TestFOK_DoesNotConsumeBeforeReject`）。
- 市价 IOC 在完全无对手盘时仍按历史规则拒绝 `NO_LIQUIDITY`（而非“0 成交
  CANCELED”）；限价 IOC 无穿越则以 `CANCELED` 结束且无任何冻结。
- 自成交防护、资金/持仓校验、三层风险限额对三种时效一视同仁：FOK 因流动性
  不足拒绝优先于自成交/资金/风险检查；IOC/市价的资金与风险只按**即时成交/
  实际挂出**的量计算，不把会被撤销的剩余部分计入冻结或挂单敞口。
- 同价位时间优先规则不被时效破坏：IOC/FOK 只是“taker”，撮合仍严格按
  `PrioritySeq` 吃单，未被吃完的 maker 留在原价位原位置。

### 1.3 价格-时间优先

- 买单从最低卖价向上吃，卖单从最高买价向下吃；
- 同一价位严格按“时间优先”成交：时间以该订单被接受时分配的**全局单调递增
  序号 `PrioritySeq`** 定义（序号小者优先），与墙钟时间解耦，保证确定性；
- 部分成交后的订单仍停留在原价位原位置，继续保有既得优先级
  （见 `TestTimePrioritySameLevel`）。

### 1.4 资金/持仓冻结与结算

- 限价买单：提交时冻结 `price × 挂单剩余量` 的现金；成交时从冻结额中扣减，
  未成交部分撤单时解冻。
- 限价卖单：提交时冻结对应数量的标的（必须先有可用持仓）；成交/撤单同理。
- 市价单：不预冻结。按撮合计划逐价位精确计算所需现金/持仓，在提交时一次性
  校验并在成交时直接结算；可用资金不足以买到全部计划数量时整单拒绝。
- 账户状态：`cash / reservedCash / positions / reservedPositions`，
  可用现金 = `cash - reservedCash`，可用持仓 = `positions - reservedShares`。

### 1.5 自成交防护（确定语义）

采用**保守的 cancel-newest 规则**：若来单的撮合计划中出现同账户挂单，则
整笔来单以 `SELF_TRADE` 拒绝，原有挂单优先级与全部账户状态保持不变，不会
产生任何部分自成交。市价单同理。

### 1.6 撤单与改单

- **撤单**：仅活跃挂单可撤；撤单立即解冻并移出订单簿。对已成交/已撤/不存在/
  曾被拒绝的 ID 分别给出可区分原因。
- **改单（同价）**：
  - 缩减数量：立即释放差额冻结，**保留该价位时间优先位置**；
  - 增大数量：对增量重新校验资金/持仓与全部风险限额，通过后方可生效；
    校验失败时原单完全不动。
- **改单（变价）**：语义为“先撤回旧单（释放冻结、放弃旧优先级），再以同 ID
  重新走一遍提交流程”：若越过对手价则立即成交，否则以新序号重新排队。
  - 整个操作在同一个原子提交点内完成，校验（资金、风险、自成交）不过则旧单
    原样保留，不存在“旧单已撤、新单被拒”的中间状态。
  - 流水记录为 `REPLACED(keptPriority=false)` + （可能的）`TRADE*` +
    （剩余挂单时）`ACCEPTED`，回放与增量路径共用同一套状态迁移代码。
- 市价单不可改单；改单价格/数量非法时以 `INVALID_REPLACE` 拒绝。

## 2. 风险与拒单语义

所有检查都是**纯读校验，且全部先于任何状态变更执行**；被拒指令只追加一条
`REJECTED` 事件（审计用），不改动订单簿、现金、持仓、冻结额、风险计数。
校验按确定的优先级进行，同一指令永远命中同一个原因：

1. 字段合法性：`EMPTY_ORDER_ID` → `UNKNOWN_ACCOUNT` → `UNKNOWN_SYMBOL`
   → `INVALID_SIDE` → `INVALID_ORDER_TYPE` → `INVALID_QTY` → `INVALID_PRICE`
   → `INVALID_TIME_IN_FORCE`（含“市价单挂 GTC”）
2. `DUPLICATE_ORDER_ID`（含已完结订单与曾被拒绝的 ID）
3. `UNKNOWN_ACCOUNT`（账户未注册）
4. 市价单无对手盘：`NO_LIQUIDITY`
5. FOK 当前不能全部成交：`WOULD_NOT_FILL`（整单拒绝，零成交、零冻结）
6. `SELF_TRADE`
7. 资金/持仓：`INSUFFICIENT_FUNDS` / `INSUFFICIENT_SHARES`
8. 风险限额（见下）

### 2.1 三层限额（`Limit <= 0` 表示不限制）

| 限额 | 判定语义（最坏情况，可解释） | 拒单码 |
| --- | --- | --- |
| 单笔订单金额上限 `MaxSingleOrderNotional` | `估值价 × 订单原始全量`；限价用限价，市价用当前最优对手价 | `RISK_SINGLE_ORDER_LIMIT` |
| 单账户总挂单敞口 `MaxGrossExposure` | 账户现有全部挂单 `Σ(price×openQty)` + 本单**实际将挂出**部分的名义额；立即成交部分不计入挂单敞口 | `RISK_GROSS_EXPOSURE_LIMIT` |
| 账户×标的净持仓上限 `MaxSymbolPosition[sym]` | `| 当前净持仓 + 现有挂单净数量(买+卖−) + 本单全量带符号数量 |`，按全量最坏情况判定 | `RISK_SYMBOL_POSITION_LIMIT` |
| 全市场挂单敞口 `GlobalMaxOpenNotional` | 所有账户挂单名义额之和 + 本单将挂出部分名义额 | `RISK_GLOBAL_EXPOSURE_LIMIT` |

改单时，被修改旧单的名义额/净数量先从基数中扣除再判定新单，保证“改单不会
因自身存量被重复计算”。全部错误消息均包含判定依据（当前值、需求值、限额）。

## 3. 事件流水、回放与中断恢复

### 3.1 事件类型与顺序

每条指令原子地产生**一组连续序号**的事件，事件先持久化（fsync）再应用到内存：

| 事件 | 含义 |
| --- | --- |
| `ACCEPTED` | 订单被接受；携带订单快照（含有效时效 `TIF`）与冻结额；其序号即时间优先序号 |
| `TRADE` | 一笔成交（价格、数量、maker/taker 的订单与账户） |
| `CANCELED` | 挂单撤销并解冻 |
| `REPLACED` | 改单；`KeptPriority` 区分同价保序与变价重排 |
| `REJECTED` | 指令被拒及稳定原因码；无状态迁移 |
| `EXPIRED` | 即时单（市价/IOC）成交后剩余数量在提交当刻被撤销；该部分从未挂单、从未冻结，故只记录终态（`LeftoverQty`），不改订单簿与账户；用于区分“部分成交后撤单”和“全部成交” |

一次提交的事件组布局（序号连续）：

- GTC 限价：`ACCEPTED(seq=n), TRADE(n+1..)`（有剩余时 `ACCEPTED` 携带剩余量并挂单）；
- IOC / 市价：`ACCEPTED(n), TRADE(n+1..)`，**存在未成交剩余时再追加
  `EXPIRED`**；全部成交则无 `EXPIRED`；
- FOK：要么 `ACCEPTED(n), TRADE*` 全部成交，要么只有一条 `REJECTED`
  （`WOULD_NOT_FILL`），二者必居其一。

回放依据 `EXPIRED`/`CANCELED` 重建订单终态（`CANCELED` vs `FILLED`），因此
“部分成交 + 剩余撤销”和“全部成交”在重启恢复后仍可区分；`EXPIRED` 的
`LeftoverQty` 必须与订单未成交量一致，否则按流水损坏处理。

序号保证全局连续无空洞（回放会强校验，断号即判定流水损坏）。

### 3.2 确定性回放与并发等价性

- 每个标的有独立单写分片，账本/流水只在全局锁下变更。全局锁同时定义了
  “事件提交总顺序”；
- 增量运行与**独立回放使用完全相同的纯状态迁移函数**
  （`replayView.applyEvent`）。因此：
  - 并发提交结束后，把流水顺序重放一遍，账户与订单簿逐字段等于线上状态
    （`TestConcurrentMultiSymbolEquivalentToReplay`，8 账户 × 6 标的 × 数百指令）；
  - 任意时刻停机后重启，先套用配置的创世余额，再在其上应用流水，继续交易时
    序号与挂单无缝衔接（`TestRecoveryContinuesAfterRestart`）；
  - 重复回放是只读、无副作用、幂等且结论一致的
    （`Replay` / `ReplayWithGenesis`）。

### 3.3 崩溃与残缺尾帧

帧格式：`u32 magic | u32 payloadLen | u32 crc32(IEEE) | gob(Event)`。
打开流水时：magic/length/CRC 任一不符即视为崩溃写入的残缺尾帧，**仅截断该
不完整尾帧**，之前完好帧全部保留，并把截断结果写回文件
（`TestTornTailTruncation`、`TestTruncatedPayloadKeepsGoodFrames`）。

## 4. 代码结构

```
engine/
  types.go      订单/成交/事件/账户/风险配置等类型
  errors.go     稳定拒单原因码 RejectReason 与 RejectError
  book.go       单标的订单簿：价位表 + 价格-时间优先遍历/成交/快照
  ledger.go     账户现金、持仓、冻结原语与 maker/taker 结算
  risk.go       单笔/账户总敞口/标的净持仓/全局敞口四层判定
  match.go      指令校验（含 TIF）、只读撮合计划、提交/撤单/改单的原子提交
  applier.go    事件 -> 状态 的纯迁移（增量与回放共用，含 EXPIRED）
  journal.go    分帧持久化、CRC 校验、残缺尾帧截断
  engine.go     引擎：分片协程、全局锁、恢复、快照、生命周期、Replay
cmd/demo/       可运行的端到端演示（含拒单/风险/自成交/回放对账）
```

## 5. 本地验证

```bash
go build ./...
go vet ./...
go test ./... -race -count=1 -v          # 全部单测（含竞态检测），-v 可见每条用例覆盖的业务与结果
go test ./... -race -count=3             # 多轮竞态/抖动回归（既有用例 + 时效/并发/回放用例）
go run ./cmd/demo                        # 端到端演示：跑指令、打印流水、关引擎、回放对账
# 中断恢复能力由 TestRecoveryContinuesAfterRestart 覆盖（重启引擎后继续提交并核对）
```

新增时效的快速手工验证思路：

1. 先挂一笔 `SELL X 5@100`，再下 `BUY X 8@100 TIF=IOC`：应成交 5、剩余 3
   被撤，订单状态 `CANCELED`，订单簿无买单，买入账户只少 500 现金、只多 5 仓，
   `ReservedCash=0`（`TestIOC_LimitPartialFillExpires`）。
2. 同样簿子下 `BUY X 8@100 TIF=FOK`：应得到 `WOULD_NOT_FILL`，挂单、两个
   账户的资金/持仓/冻结完全不变（`TestFOK_ShortLiquidityRejectedWhole`）。
3. 用带 `JournalPath` 的引擎跑完后 `ReplayWithGenesis`：流水里可见
   `ACCEPTED → TRADE* → EXPIRED`（或 `REJECTED`），回放结论与实时一致，
   重复回放结果相同（`TestTIF_JournalReplay`）。

测试覆盖与对应场景：

| 测试 | 覆盖点 |
| --- | --- |
| `TestTimePrioritySameLevel` / `TestPricePriority` | 同价位严格时间优先、部分成交后保序、价格优先 |
| `TestNoCrossRestsAndCancel` | 不穿越挂单、冻结、撤单解冻、重复撤单/未知单 |
| `TestReplaceSamePriceKeepsPriority` / `TestReplacePriceChange` | 改单保序、缩量放量、变价穿越、失败不留中间态 |
| `TestRejectedInstructionsLeaveNoTrace` | 各类非法/资金不足指令拒绝后账户与订单簿零变化 |
| `TestMarketOrderIOC` | 市价 IOC：无流动性拒绝、部分成交不挂单 |
| `TestIOC_LimitPartialFillExpires` / `TestIOC_LimitFullyFilled` / `TestIOC_NoCrossExpiresWhole` / `TestIOC_SellPartialFill` | IOC 限价：部分成交剩余撤销（状态/订单簿/占用三方核对）、全部成交 FILLED、无穿越整笔过期、卖单只交实际成交量 |
| `TestFOK_FullyFilled` / `TestFOK_ShortLiquidityRejectedWhole` / `TestFOK_NoLiquidityAndSellSide` / `TestFOK_DoesNotConsumeBeforeReject` | FOK：跨价位一次全成、量不够整单 `WOULD_NOT_FILL` 零成交零占用、无流动性/卖单侧、拒绝前不先吃掉队首 |
| `TestTIF_InvalidAndMarketCompatibility` / `TestGTCLimitUnchanged` | 非法时效与“市价挂 GTC”拒绝、市价 FOK/默认 IOC 兼容、GTC 限价行为不变 |
| `TestTIF_JournalReplay` | IOC 部分成交+过期、FOK 全成的完整生命周期回放对账、EXPIRED 事件、重启后终态可区分、重复回放幂等 |
| `TestLimitMakerPartialKeepsResting` / `TestLimitMakerPartialThenCancel` | 普通限价 maker 只被吃一部分：剩余留在原价位保有优先级并能继续成交，或被撤；订单状态/订单簿/账户占用每步三方对得上 |
| `TestConcurrentTIFEquivalentToReplay` | 多标的并发 IOC/FOK（360 单 × 6 账户 × 4 标的）与顺序回放逐字段等价、即时单零挂单、FOK 合法终态 |
| `TestSingleOrderNotionalLimit` / `TestGrossExposureLimit` / `TestSymbolPositionLimit` / `TestGlobalExposureLimit` | 三层风险限额边界（等值放行、超限拒绝，错误消息含判定依据） |
| `TestSelfTradeRejectedWithoutFilling` | 自成交整单拒绝且无任何状态变化 |
| `TestInsufficientFundsAndShares` | 现金/持仓不足的边界（恰好足额放行） |
| `TestConcurrentMultiSymbolEquivalentToReplay` | 多标的并发提交 + 撤改单，线上状态与顺序回放逐字段一致 |
| `TestCancelAmendRace` | 同一订单“成交/撤销/改单”三方竞态 200 轮：不重复成交、回放一致、现金守恒 |
| `TestReplayEqualsIncremental` / `TestRecoveryContinuesAfterRestart` | 流水回放对账、重启恢复、重复回放幂等 |
| `TestTornTailTruncation` / `TestTruncatedPayloadKeepsGoodFrames` | 中断残缺尾帧精确截断与恢复后续写 |

所有测试用例通过 `t.Logf` 打印关键订单流水（提交结论、成交 maker/taker、
拒绝原因与判定依据），失败时可用 `go test -v` 直接看到决策依据。
