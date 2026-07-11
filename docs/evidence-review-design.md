# 证据式人工审查体系设计方案（Evidence-Based Review）v2

> 基于 no-mistakes 框架改造，融合 qa-changes 与 cr 两个 skill。
> 目标：AI 生成代码后，人工 review 的对象从「代码 diff」变为「经可信 Agent 生产、独立核验、风险筛选后的证据卷宗」；只有过了 Agent 筛选的东西才到人面前。
> 本文只谈设计，不谈实现周期。框架与 no-mistakes 保持一致（步骤状态机、findings/park/respond、axi 协议、PR 落地），在其上做大改造。
> v2 说明：初稿经过一轮对着 no-mistakes 源码的对抗性评审，修正了三处骨架级问题与一批严重死角；修订对照见附录 B。

---

## 1. 目标与六条设计原则

**目标状态**：一个 PR/MR 到人面前时，人默认看到的是一页「证据卷宗」——结论、风险级、覆盖账本、每条断言及其可点开的证据、未决问题清单。人 10 秒内能做出放行/深究的判断；深究时按证据下钻，最后才是读代码。代码永远可读，但不再是 review 的默认入口。

**原则 1：生成者-验证者分离。**
写代码的 Agent、造证据的 Agent、核验证据的 Agent 是三个独立实例（可配不同 harness/模型）。任何 Agent 不为自己的产出做最终裁决。现状缺口：no-mistakes 的 test 步骤让同一条流水线的 agent 自己造证据、自己写 testing_summary、fix 后自己判 "no issues remain"。

**原则 2：证据的可信度来自采集方式，且只声明「采集真实性」。**
由流水线自有的、Agent 无法冒充的受控采集器捕获并**签名**的产物是 `captured` 级证据；Agent 转述/粘贴的内容一律是 `attested` 级（自述）。必须明确区分两件事：
- **采集真实性**（这条输出/截图确实由这条命令、在这个 commit、这个 URL 上产生）——可以机器背书；
- **语义有效性**（这条证据确实支撑它所挂的断言）——**永远是判断**，由核验层裁决、由校准体系兜底，机器背书不了。
卷宗上的 🔒 标记只声明前者，渲染层与文案不得暗示后者。把 Agent 可操纵的产物洗成「机器事实」的外观，比没有标记更危险。

**原则 3：断言必须绑定证据，且绑定只是语法约束。**
每一句「修复有效」「行为符合预期」都必须挂证据 ID；无证据的结论自动降级为自述并单独列出。但「挂了证据」只保证链接存在，不保证链接有效（截图可能截的是无关页面）——语义有效性由 verify 层专职裁决（原则 2）。两层分工必须在文档与卷宗上都讲清，防止读者把绑定当成验证。

**原则 4：覆盖率显式记账，账本的地面真值尽量来自插桩而非自述。**
「验证了什么」和「没验证什么、为什么」同等重要。每个 diff hunk、每条用例都落在覆盖账本里，取值封闭：运行时已验证 / 静态已验证 / 仅自述 / 未验证（附原因）。其中「运行时已验证」由**代码覆盖率插桩数据**回填（该 hunk 在证据采集运行中确实被执行，是可测量的机器事实）；「静态已验证」必须绑定可执行的静态证据（类型检查输出、AST 等价性工具输出），不接受一句自然语言声明。覆盖率是硬 gate 信号，不是结论行里的装饰。

**原则 5：可信是校准出来的，不是声明出来的。**
每个验证 Agent（gate）都带评测集，有可度量的精度/召回；已放行的 PR 被**有编制、有配额、盲审式**的抽样人工复核；人在卷宗上的每次纠错回流为规则与评测用例。「可信 Agent」的可信度是一个持续测量的数字，不是一个头衔。核验层（verify）自己也在被校准之列。

**原则 6：凡标注「确定性/机器事实」之处，其输入不得含 Agent 主观标签。**
确定性的函数吃进主观的输入，输出依然是主观的——只是套了一层确定性的壳。风险评分、覆盖审计、复现比对这些「机器裁决点」的输入必须是客观量（路径规则、插桩数据、签名 manifest、历史缺陷密度）；Agent 的主观评级只能作为展示项或**单向阀**（只能调高风险，不能调低）。

---

## 2. 总体架构

### 2.1 保持 no-mistakes 骨架

保留不动的框架件：

- **入口**：`git push no-mistakes` / TUI / agent skill 三入口，post-receive hook 通知 daemon，一次性 worktree 隔离执行。
- **执行模型**：`Step` 接口 + executor **顺序**状态机 + 步内 round 轮次 + `awaiting_approval`/`fix_review` park 机制 + `axi run/respond/status` 阻塞式 agent 协议。本方案尊重「executor 是顺序的、round 机制封闭在单步之内、跨 run 重放（re-run + 已解决 findings 去重）是既有的回退通道」这一事实，所有回退语义都建立在这三个既有原语上（§4.4），不假设跨步自动回跳。
- **findings 流转**：`auto-fix / ask-user / no-op` 三分类，ask-user 原样转达给人，fix 归流水线所有（agent 不许在 run 中自己改代码）。
- **落地面**：PR/MR body 确定性渲染（prsummary 机制），证据随分支/证据 ref 提交。

### 2.2 四层验证体系

```
L1 确定性层    build / typecheck / lint / 既有测试 / 覆盖率插桩 / 可执行规则   —— 脚本，机器事实
L2 静态语义层  static-review（cr 改造）                                      —— Agent 判断 + 规则库
L3 运行时行为层 qa（qa-changes 改造）+ test（证据生产）                        —— 真实运行，受控采集证据
L4 对抗核验层  verify（纯新增 gate）                                          —— 独立怀疑者核验 L2/L3 的断言与证据
```

L1–L3 是「生产层」：产出 findings、claims、evidence。L4 是「核验层」：反驳断言、复现证据、审计账本。人只消费 L4 之后的结果。

如实声明：L4 不是从任何现有资产「吸收」来的——qa-changes 与 cr 都没有独立核验机制，verify 是从零新造的部件（附录 B M4）。同时要正视：verify 的 CONFIRMED/PLAUSIBLE/REFUTED 裁决驱动路由、决定卷宗结论，**它是体系内最强的单点权威**。方案对它的约束是：裁决全程留痕入库、裁决自身进评测集并测「与盲审人的一致率」（§8）、对安全敏感类改动保留强制人工下限（§6）——而不是假装它「只核验不裁决」。

### 2.3 流水线序列（改造后）

```
intent → rebase → static-review → test → qa → verify → document → lint → push → PR(卷宗渲染) → CI
```

- `static-review`：吸收 cr 的规则库与路由机制，并入原 review 步骤的通用正确性审查。
- `test`：保留 no-mistakes 原 test 定位，证据采集全部改走受控采集器（§4.2）。
- `qa`：新 gate，qa-changes 的四阶段流程进 pipeline，负责端到端行为验证。
- `verify`：新 gate，对抗核验（§4.4）。
- `PR`：prsummary 升级为卷宗渲染器（§7）。

---

## 3. 可信证据基础设施（Evidence Vault）

这是全方案最核心的新子系统。没有它，「审证据」就是「审 Agent 写的作文」。

### 3.1 信任根：采集器是独立信任域，manifest 带签名

采集器不是 worktree 里的一个普通 CLI，而是 **daemon 侧的服务**（借用 in-toto/SLSA 的 attestation 思路）：

- Agent 在 worktree 里调用 `no-mistakes evidence ...` 只是**发起采集请求**；实际执行、产物落盘、manifest 写入都发生在 daemon 进程侧。
- daemon 持有**Agent 进程拿不到的签名密钥**，对每条 manifest 条目（含产物 hash）签名；密钥与签名日志存在 worktree 之外（daemon 的私有目录/系统 keychain）。
- **卷宗渲染器只认签名验签通过的 manifest 条目**。Agent 直接往证据目录写文件、或手改 manifest，产物一律不进 captured 池——不是靠路径约定，是靠验签失败。
- Agent 经 `evidence attach` 提交的自述产物同样入库登记（好处是防丢、可链接），但 provenance 永远是 `attested`，且**签名声明的内容只是「Agent 在时刻 T 提交了这个文件」**，不为其内容背书。

这把初稿里「纪律级」的信任根升级为「机制级」：captured 与 attested 之分由密码学边界维护，不由 Agent 的自觉维护。

### 3.2 受控采集器（trusted collectors）

```
no-mistakes evidence exec   -- <cmd...>       # pty 包裹执行：完整 argv、cwd、env 指纹、stdout/stderr、exit code、时长
no-mistakes evidence coverage -- <cmd...>     # exec 的插桩变体：同时产出行级覆盖率数据（istanbul/coverage.py/go -cover 等），是覆盖账本的地面真值来源
no-mistakes evidence screenshot --url <u> [--flow <脚本>]   # 流水线自驱浏览器(CDP)截图，元数据盖 commit SHA / URL / 时间戳
no-mistakes evidence http --request <file>    # 真实 HTTP 往返，记录请求与响应原文
no-mistakes evidence diff --before <ref> --after <ref> -- <cmd...>   # bugfix 前后对比：同一命令在基准与变更两个 checkout 各跑一次，双输出并排登记
no-mistakes evidence attach --file <p> --label <l>   # Agent 自备产物入库，强制 provenance=attested
```

要点与边界：

- **exec/coverage 是命令类 captured 证据的唯一来源**。Agent 粘贴的「我跑了 X，输出是 Y」不进 captured 池。
- **screenshot 的诚实边界**：URL、交互 flow 脚本由 Agent 提供，采集器忠实地截下 Agent 让它去的地方。所以 screenshot 的 captured 语义仅是「这张图确实在该 commit、该 URL、该交互序列下产生」；**它证明不了这个 flow 走到了被改的代码、也证明不了 Agent 没有规避出错场景**。补强手段有二：(a) 前端场景下 dev server 以插桩构建运行，截图采集时同步收集覆盖率，flow 是否执行了改动 hunk 由插桩数据回答；(b) 语义有效性交 verify 裁决。内网场景沿用 ego-browser + bifrost 链路，并把 qa-changes 的流量归因自检固化进采集器（自检不过则拒绝入库，自检记录随图登记）——注意这个自检同样只证「页面来自本地 dev、API 走目标环境」，不证语义。
- **diff 采集器是新造原语**（qa-changes 里的前后对比是 prompt 叙事约定，不是机制；此处将其机制化）：基准复现与修复验证由同一命令、同一环境产出，机器可比。
- **可复现执行是复跑的前置基础设施**（没有它，§4.4b 的抽样复现必然被非确定性输出淹没）：
  - 采集器默认注入可复现环境：固定随机种子、mock 时钟（可行时）、固定端口分配、锁依赖版本；
  - manifest 存两个指纹：原始输出 sha256 + **归一化指纹**（经过滤器剥离时间戳、UUID、端口、耗时、内存地址等已知非确定字段后再 hash），复跑比对用后者；归一化规则是仓库配置的确定性 filter，版本化、可审计；
  - 对外部 I/O 重的验证支持录制/回放（VCR 式）：首采录制，复跑回放，把「外界变了」与「证据假了」分开。
- **attach 是逃生门，但有硬闸**：卷宗里 attested 占比超过仓库阈值（默认：关键断言中 attested 占比 > 30%）时，路由禁止绿色直通（§6）。否则现实压力下 Agent 会把一切走 attach，视觉降级挡不住实质回到作文。

### 3.3 证据 manifest

每条证据一个 JSON 条目，daemon 侧写入并签名：

```json
{
  "id": "ev-7f3a",
  "kind": "command-output | coverage | screenshot | http | diff-pair | file",
  "provenance": "captured | attested",
  "collector": "evidence exec",
  "argv": ["pnpm", "test", "--filter", "@app/login"],
  "cwd": "apps/space",
  "commit": "a1b2c3d",
  "run_id": "run-2026-07-07-xx",
  "exit_code": 0,
  "duration_ms": 41200,
  "sha256": "…",
  "normalized_sha256": "…",
  "normalizer_version": "norm-filters@v3",
  "replay": { "mode": "live | recorded", "cassette": "…" },
  "env_fingerprint": { "os": "…", "node": "22.x", "branch": "fm/fix-login" },
  "paths": [".no-mistakes/evidence/fm/fix-login/ev-7f3a/stdout.txt"],
  "claims": ["c-12", "c-15"],
  "signature": "…daemon 签名，覆盖以上全部字段与产物 hash…"
}
```

manifest 解决四个问题：**溯源**、**防挪用**（hash + run_id + commit 绑定）、**防伪造**（签名）、**可复核**（verify 和人都能按 argv + 归一化规则重跑重比）。

### 3.4 存储与呈现策略

- **证据必须随 PR 可见，这是本方案的立场，不跟随上游默认**。上游 `store_in_repo` 默认 false 的理由（分支历史污染、体积）由下述「证据专用 ref」方案化解，而非靠牺牲可见性回避；若未来上游给出不同的托管终态，迁移的是存储位置，不是「默认可见」这条立场。
- **存储两档，按仓库配置选**：
  1. **分支内提交**（上游现有 opt-in 模式）：`.no-mistakes/evidence/<branch-slug>/`，简单、PR 原生渲染；代价是污染分支历史。作为过渡档。
  2. **证据专用 ref**（推荐终态）：证据提交到 `refs/no-mistakes/evidence/<branch>` 孤儿链，push 步骤一并推送，PR body 用该 ref 的 raw 链接内联。主分支历史零污染，证据不可变、可链接、可按保留策略 GC。
  内网 Codebase/MR 场景复用 qa-changes 的既有通道：截图上 TOS 取内网直链，manifest 记 TOS URL 与 hash。
- **截断问题**：PR body 只放摘要 + 链接（每条证据一行：label、判定、provenance 标记、点开看全文），完整产物在证据 ref/TOS 上永不截断；内联仅限截图与关键小片段。完整 HTML 卷宗（§7）承载全量。

---

## 4. 各 gate 详细设计

### 4.1 static-review（cr 改造）

**沿用的现有资产**：规则库驱动（`cr-rules.md` 的 `## CR-NNN` 结构）、glob/npm-scope 过滤、委托型规则路由、`capture-cr-rule` 沉淀机制、evals 护栏。

**改造（以下均为新增能力，cr 现状没有）**：

1. **findings 结构对齐并扩展 no-mistakes 的 `Finding`**：
   - `confidence: deterministic | semantic`——工具硬命中 vs 模型语义判断，分开呈现；
   - `rule_id` / `delegated_from`——finding 来源可溯；
   - `severity: error | warning | info`（cr 现状是二元「违规/通过」）；
   - `evidence`——命中锚点（diff hunk 原文 + 上下文 + 定位）作为静态证据入库。
2. **规则库二分为「可执行规则」与「语义规则」**：可执行规则必须附一个独立的 deterministic checker（lint 规则或脚本，作为 L1 资产维护、有自己的测试），由 L1 跑出机器结论——这才配 `deterministic` 标记；cr 现状「agent 现场手拼 grep 再语义判断」的做法只能标 `semantic`。沉淀新规则时优先写成可执行。
3. **语义类 finding 一律送 verify 反驳**，反驳存活的才进卷宗。
4. **原 review 步骤的通用正确性审查并入本 gate**；风险评级（low/medium/high + rationale）保留，但按原则 6 只作为展示项与单向升险阀进入路由（§6）。
5. **ask-user / auto-fix 三分类照旧**，review 类 auto_fix 默认仍为 0（park 给人）——no-mistakes 已做对的强关口，不动。

### 4.2 test（收紧证据纪律）

保留 no-mistakes test 步骤的定位（基线测试命令 + 为用户意图造端到端证据），三处收紧：

1. **证据采集全部改走受控采集器**；基线测试跑 `evidence coverage` 变体，覆盖率数据直接回填账本。
2. **证据缺失不再可被写成 info 混过去**：intent 声明的每个可观测行为必须映射到 ≥1 条 captured 证据；映射不全时**记入覆盖账本的缺口区并继续**，是否打断人交给 §6 的统一路由判断（初稿此处设计为步内强制 park，对抗评审指出这与「按风险筛选后才给人」冲突——低风险但难取证的改动会中途频繁捞人。修正为：步内 park 只保留给真正阻断执行的情况，如环境不可达无法继续；证据缺口是路由的输入，不是各 gate 各自的打断理由）。
3. `tested[]` 废除自由文本语义，改为 manifest 条目引用——「跑了哪些」由签名采集记录背书。

### 4.3 qa（qa-changes 进流水线）

> **实现状态（与本节设计不同）**：独立的 `qa` step 已从流水线删除。它跑的是一个 agent，与 `test` 步骤的 evidence agent 共用同一个 worktree、同一份 base..head diff、同一套证据命令和同一份 findings schema，只多出可达性分诊与覆盖账本四态标注这两项记账纪律——这两项已并入 `test` 的 evidence agent prompt。本节其余内容作为设计记录保留；流水线现状以 `docs/src/content/docs/reference/pipeline-steps.md` 为准。

qa-changes 的四阶段流程整体保留，从「手动 skill」变为 pipeline gate；SKILL.md 转为该 step 的 agent prompt 与操作手册（dev-proxy 手册原样保留）。

**沿用的现有资产**：四阶段结构、用例台账四态判定、「代码级佐证不计入通过」纪律、「三次实质不同尝试才可放弃、放弃必须附真实失败输出」规则、代理链路自检（升级进采集器，§3.2）。

**如实标注现状并升级**：

1. **可达性分诊现状是 Agent 读用例标注做的判断，没有确定性探测**。升级：端可达性（dev server 能否起、端能否在本环境运行）改由确定性探测脚本回答，数据/账号可达性保留 Agent 判断但标注为 semantic。分诊结果作为「范围声明」落库；预估覆盖率低于阈值时**作为路由输入**提前呈现给人（在花钱的 L3/L4 之前给人一次「接受低覆盖 / 补数据 / 转人工」的决策点——这是唯一保留的前置决策 park，由路由策略而非 gate 自身触发）。
2. **用例台账机器可读**：每条用例一个 JSON 条目（ID、优先级、四态判定、证据 ID 列表、未执行原因），构成覆盖账本的运行时部分；判「运行时通过」必须挂 captured 证据 + 对应覆盖率数据。
3. **bugfix 前后对比从叙事约定升级为机制**：统一走 `evidence diff` 采集器（§3.2），基准复现失败（bug 本来就不存在）会被机器暴露。
4. **产出对象从 MR 评论改为 findings + 证据库**；MR 评论由卷宗渲染器统一生成，qa gate 不再直接发评论。
5. 用例来源保留 `cozetest-e2ecase-generate-spec` 联动；不可用时 Agent 自拟并在台账里标注来源。

### 4.4 verify（纯新增：对抗核验 gate）——「结果筛选」的把关人

由**独立 Agent 实例**执行（与生产层不同实例；支持跨 harness/模型配置）。职责三项：

**（a）断言反驳（adversarial refutation）。**
对生产层每条 claim 与语义类 finding，起 N 个（默认 3）互相独立的怀疑者，目标是**反驳**：证据是否支撑断言？截图内容与声称是否一致（对照插桩数据：flow 是否真执行了改动 hunk）？有没有更简单的反例？判定三值 `CONFIRMED / PLAUSIBLE / REFUTED`，多数决。
关于共模失误的诚实立场：多模型多样性**只部分缓解**共模盲区（前沿模型共享训练分布，同一个微妙 bug 可能一起漏），所以多数决给出的置信有上限——这正是 §6 保留人工下限、§8 用盲审一致率校准 verify 的原因，不指望怀疑者数量堆出真理。

**REFUTED 的处置（与 executor 顺序状态机一致，不假设跨步回跳）**：

no-mistakes 的 executor 是顺序的，round/fix 机制封闭在单步之内，`Respond` 硬性拒绝定向到非当前步——因此「verify 自动打回 qa 重跑」在既有框架内不成立。本方案的回退语义完全建立在框架既有原语上：

- **verify 步内 park**：REFUTED 项作为 verify 的 findings（error 级，携带反驳理由与涉事证据 ID）触发 park。
- **两条恢复路径**，由驱动 agent（经 `axi respond`）或人选择：
  1. `respond --action fix`：走 verify **步内** round——由流水线派**新的 fix agent 实例**改代码/补证据请求，然后 verify 重新执行（重新反驳）。fix agent 与怀疑者是不同实例，生成者-验证者分离不破；步内 fix 轮受 `auto_fix.verify` 限次。适合小缺口（断言改措辞、补一条采集）。
  2. **跨 run 重放**：缺口需要重造运行时证据或返工实现时，abort 当前 run，修复 commit 后 `axi run` 重跑整条流水线——这是 no-mistakes 既有的「run 之间回退」通道，已解决 findings 的指纹去重避免重复审理，rebase/replay 机制现成。代价是重跑 L1–L3，按本方案前提（机器成本换人成本）接受；已通过且输入未变的 step 结果可按内容指纹缓存跳过（这是对 executor 的增量优化项，标注在 §9，属可选）。
- 若确需「引擎原生支持有向回边」（verify 直接重入 qa），那是 executor 核心重构，§9 中如实标注为**引擎级改造、高风险、非必需**——上面两条路径已覆盖需求。

**（b）抽样复现（spot-check reproduction）。**
从 manifest 抽样（默认：全部 error 级相关证据 + 随机 20% 其余 captured 证据），按登记 argv 在干净环境重跑，**比对归一化指纹**（§3.2 的可复现执行设施是本机制的前置条件）。判定规则：
- 归一化指纹一致 → 复现成功；
- 不一致 → **先重跑 3 次取多数**：多数与原指纹一致则标记 flaky（finding 级 warning，计入卷宗，不 park）；多数不一致才判复现失败（高严重，park）。
- 归一化过滤器覆盖不了的输出（如内容本质随机）在采集时就标 `replay: not-applicable`，不进抽样池，卷宗上如实显示「不可复现类证据」占比。
杜绝两个坑：拿原始 sha256 比对非确定输出造成假失败洪水；用 LLM 模糊比对冒充确定性复现（若某类证据只能语义比对，那就是 verify (a) 的判断题，不得计入「复现」这个机器背书项）。

**（c）覆盖审计（coverage audit）。**
机器比对三方数据：diff hunk 集合 × 覆盖账本 × **插桩覆盖率数据**。审计规则：
- 每个改动 hunk 必须有账本条目（完整性，机器可判）；
- 账本标「运行时已验证」的 hunk 必须被至少一条 captured 证据的插桩数据覆盖（真伪，机器可判——这是对「Agent 批量自贴标签」的直接反制）；
- 标「静态已验证」的 hunk 必须挂可执行静态证据（typecheck 输出、AST 等价工具输出），纯自然语言声明不合格；
- 审计产出最终覆盖率与未覆盖清单，供 §6 路由。

verify 的裁决记录（谁反驳了什么、复跑结果、审计明细）全部入证据库，人可查；verify 自身的裁决质量由 §8 的评测与盲审一致率持续校准。

---

## 5. 断言-证据模型（Claim Model）

全流水线统一的最小语义单元：

```json
{
  "id": "c-12",
  "step": "qa",
  "text": "登录页在移动端视口不再溢出（修复 #1234）",
  "kind": "behavior | regression-fixed | rule-compliance | non-goal",
  "evidence": ["ev-7f3a", "ev-8b21"],
  "verdict": "CONFIRMED",
  "verdict_by": "verify/skeptic-2of3",
  "hunks": ["apps/space/src/pages/login.tsx:40-88"]
}
```

三条明确的语义边界：

- `evidence` 为空的 claim 自动降级为自述区条目，永不出现在卷宗结论区（语法约束，机器强制）。
- claim→evidence 的**语义有效性**（证据真的支撑断言）由 verify 裁决，不由绑定本身保证。
- claim→hunks 映射由 Agent 填写，但「运行时已验证」的真值由插桩数据校验（§4.4c）；插桩不可用的场景（纯配置、文档）在账本里如实落入「静态已验证/仅自述」档。

覆盖账本 = 以 hunk 与用例为行、四态为值的两张表，由 test/qa/static-review 填写、插桩数据回填、verify 对账合并。人不需要读代码来发现「有个改动没人验过」——账本直接列出来。

---

## 6. 风险分级与人工介入路由

「筛选后才给人」不等于「全绿才给人」，而是**按风险决定人看多深**。全流水线只有这一个统一的「要不要打断人」闸口（各 gate 不再各自捞人，见 §4.2 修正）。

**评分输入（按原则 6 全部为客观量）**：
- diff 触面：安全敏感路径、依赖/lockfile、公共 API、迁移脚本（路径规则，仓库配置）；
- 插桩回填后的运行时覆盖率、静态验证占比、未验证 hunk 数；
- verify 裁决分布：REFUTED/PLAUSIBLE 占比、复现失败/flaky 数；
- attested 证据占比（§3.2 硬闸）;
- intent 状态：缺失或低置信；
- 历史信号:该路径的历史缺陷密度、该 gate 近期的审计分歧率（§8）。

**Agent 的主观风险评级（static-review 的 low/medium/high）是单向阀**：只能把路由级别调高，永远不能调低。它出现在卷宗上供人参考，但一个谄媚或被注入的 review agent 把一切判 low，不会因此打开绿色通道。

**路由表**：

| 级别 | 条件（示意） | 人看到什么 |
|---|---|---|
| **绿色直通** | 低风险触面 + 全部 claim CONFIRMED + 运行时覆盖 ≥ 阈值 + attested 占比 ≤ 阈值 + intent 完整 + 无 ask-user + 无 flaky 高发 | 一行摘要 + 卷宗链接；`yolo=on` 项目可授权自动合入（破坏性/不可逆/安全敏感永远除外） |
| **黄色·审卷宗** | 中风险，或存在 PLAUSIBLE，或覆盖率灰区，或 attested 占比偏高 | 完整卷宗（§7），人审证据放行 |
| **红色·深审** | 高风险触面 / REFUTED / 复现失败 / 覆盖审计失败 / ask-user / intent 缺失 | 卷宗置顶未决项，ask-user 逐字转达，人按需下钻代码 |

**四条不变式**：

1. **ask-user 永远直达人**，任何评分不得吸收。
2. **intent 缺失或低置信 → 禁止绿色直通**（最低黄色）。intent 是全部证据评判的对照根，根不可靠时，一路 CONFIRMED 的高置信外观恰恰是被放大的假信心——此时必须有人看一眼「它到底在验证什么目标」。
3. **安全敏感触面与全新代码路径设人工下限**：即使全绿也至少一次人过目（可以只读卷宗）。这是对 verify 共模盲区（§4.4a）的结构性对冲。
4. **人永远可以越过卷宗直接读代码**。本方案降低「必须读代码」的默认成本，不取消这个权利。

---

## 7. 证据卷宗（Evidence Dossier）——人的 review 界面

由 PR 步骤**确定性渲染**（prsummary 的升级替代物；模板化拼装 DB、claim 表与签名 manifest，不经 Agent 之笔。fix 轮次叙事保留 agent 摘要原文，但每轮附 verify 复检裁决与 diff 链接，agent 自报的摘要不再是唯一叙事来源）。

**结构（自上而下 = 10 秒 → 10 分钟的阅读路径）**：

1. **结论横幅**：verdict（PASS / PASS WITH ISSUES / FAIL / PARTIAL）+ 路由级别 + 运行时覆盖率（插桩口径，如 `改动 hunk 运行时覆盖 41/47`）+ attested 占比。
2. **需要你决定的事**（红色路由置顶）：ask-user 逐字转达、REFUTED 项、复现失败项、覆盖缺口决策。
3. **覆盖账本**：hunk 表与用例台账，四态着色；「未验证」行必须带原因；插桩回填的行标机器背书记号。
4. **断言与证据表**：每条 claim 一行——文本、verdict、置信档、证据链接（截图内联、命令输出点开、diff-pair 并排）。provenance 视觉区分：🔒 captured（并注明「机器背书的是采集真实性」）/ 💬 attested；attested 类统一沉到独立小节。
5. **流水线叙事**（折叠）：每步状态、每轮「发现 → 修复 → verify 复检」记录、flaky 清单。
6. **溯源脚注**：run ID、commit、环境指纹、归一化过滤器版本、各 gate 评测版本号与当前通过率（§8）、test/lint 命令来源 ref（与分支不一致时提示）、证据 ref 链接。

**双载体**：PR/MR body（GitHub 与内网 Codebase 各自适配，复用 scm 抽象与 qa-changes 的 6000 字符拆分经验）+ **完整 HTML 卷宗**。HTML 卷宗托管在证据 ref（GitHub raw / Pages）或 TOS（内网），不受评论长度限制，承载全量台账与内联媒体。托管终态与 §3.4 的存储选型是同一个决策，绑定推进，不再各自悬空。

**防盲信设计**：绿色直通的「一行摘要」刻意不使用权威化措辞（不写「已验证无问题」，写「N 条断言经核验存活，覆盖 x/y，点开卷宗」）；卷宗脚注常驻显示该仓库 gate 的最近审计分歧率——提醒人这套体系的出错率是一个非零的、公开的数字。

---

## 8. Agent 可信度体系与校准闭环

「可信 Agent」的可信来自四个持续运转的机制，其中人工审计是唯一的地面真值来源，必须按岗位而非按口号设计：

1. **每个 gate 带评测集**。cr 的 `evals/` 是现成范式，推广到 static-review / test / qa / **verify 自身**：评测用例 = 带已知答案的历史 diff/MR；gate 升级（prompt、模型、规则）必须先过评测。verify 的评测额外测两项：误反驳率（false REFUTED）与**与盲审人的一致率**——核验层不能只校准别人不校准自己。卷宗脚注标注各 gate 评测版本与通过率。
   **冷启动**：初期评测集的「已知答案」来自上线前的影子期——体系并行跑但不放行，人照旧全量 review，人的结论即标注；影子期同时校准路由阈值。这笔标注成本是体系的入场费，如实列出，不含糊。
2. **抽样人工审计是编制内的岗位，不是善意**。设计要点：
   - **有名有姓有配额**：审计职责轮值到具体工程师，计入工作量（如每人每周 M 个），不是「有空看看」；
   - **风险加权抽样**：绿色直通按比例抽，安全触面与新路径加权，黄色偶抽；
   - **盲审**：审计者先做传统全量 code review、后看卷宗对账，避免被卷宗结论锚定；
   - **分歧率是核心指标**：审计结论与卷宗结论的分歧率按 gate 归因，公开在卷宗脚注，分歧率超阈值的 gate 自动收紧其路由权（该 gate 参与的 PR 不再允许绿色直通，直到评测修复）。
3. **纠错回流**。审计与日常纠正（误报、漏报、证据不实）走 `capture-cr-rule` 的泛化版 `capture-gate-rule`：沉淀为规则库条目或评测用例，归属到具体 gate。人不看绿色 PR 就没有纠错信号——所以回流的主渠道是第 2 条的编制化审计，不指望日常自发。
4. **多样性与分权，如实评估**。三层不同实例、verify 多怀疑者多数决、可配跨模型——结构性冗余能压低单点失误，但**压不掉前沿模型的共模盲区**。承认这个上限，才有 §6 不变式 3 的人工下限存在的理由。

---

## 9. 对 no-mistakes 的具体改造清单

逐项对接调研确认的扩展点与硬编码位置，并如实标注改造性质与风险：

| # | 改造 | 落点 | 性质与风险 |
|---|---|---|---|
| 1 | 新增 `static-review`、`qa`、`verify` 三个 step | 实现 `pipeline.Step` 接口，加入 `steps/AllSteps()` 与 `types.AllSteps()`（两处硬编码序列） | 新增 step，框架内成立；**回退语义只用步内 round + 跨 run 重放两个既有原语（§4.4a），不改 executor 顺序模型** |
| 2 | （可选）executor 增量重放：跨 run 重跑时按内容指纹跳过输入未变的已过 step | `internal/pipeline/executor.go` | **引擎级优化，高风险，非必需**——不做则跨 run 重放全量重跑，只是更贵 |
| 3 | `Finding` 扩展 `confidence`/`rule_id`/`delegated_from`/`claims` | `internal/types/findings.go` + `steps/common.go` 各 schema | 改 schema，向后兼容（参考既有 legacy 字段处理） |
| 4 | Claim 模型与覆盖账本 | 新增 `internal/claims/`，DB 加 `claim`/`coverage_entry` 表 | 新增子系统；跨步读 DB 有先例（pr.go 读全部 step 结果） |
| 5 | Evidence Vault：daemon 侧采集服务 + 签名 manifest + provenance | 新增 `internal/evidence/` + `no-mistakes evidence` CLI（worktree 侧薄客户端 → daemon 服务） | 新增子系统，**信任根，须自带测试套件与签名验签的独立审计** |
| 6 | 可复现执行设施：归一化过滤器、种子/时钟注入、录制回放 | `internal/evidence/` 内 | 新增，**是 verify 复现机制的前置条件，缺它则 §4.4b 不可用** |
| 7 | 覆盖率插桩集成（istanbul/coverage.py/go -cover 等，按语言适配） | `evidence coverage` 采集器 + 账本回填 | 新增，**是覆盖账本地面真值的来源**；插桩不可用的技术栈退化为「静态已验证/自述」档并在卷宗如实显示 |
| 8 | 证据可见默认 + 证据专用 ref | `config.go` 默认值翻转；`push.go` 增 evidence-ref 推送模式 | 改默认值 + 扩展 push；与上游默认不同是**有论证的立场**（§3.4） |
| 9 | test 步骤证据纪律收紧 | `steps/test.go` 内联 prompt 与判定 | 改硬编码 prompt/逻辑 |
| 10 | 卷宗渲染器 | 替代 `steps/prsummary.go` 全部渲染规则 | 重写渲染层（原本就是硬编码区） |
| 11 | 风险路由 | 新增 `internal/policy/`；`.no-mistakes.yaml` 增 `routing` 段 | 新增 + 配置；输入全客观量，agent 评级仅单向升险 |
| 12 | verify 配置 | `.no-mistakes.yaml` 增 `verify: {skeptics: 3, agents: [...], replay_sample: 0.2, auto_fix.verify: N}` | 配置点 |
| 13 | 内网 SCM（Codebase MR）适配 | `internal/scm/` 增 codebase host；评论走 bytedcli | 新增 adapter（能力声明机制现成） |
| 14 | qa gate 浏览器/代理/TOS 集成 | qa step prompt 引用 dev-proxy 手册；采集器集成 ego-browser/bifrost/TOS | 新增（内网栈） |
| 15 | intent 状态进路由 | intent 缺失/低置信 → 路由不变式 2（禁绿） | 小改 |
| 16 | commands 来源透明化 | 卷宗脚注标注 test/lint 命令来源 ref（保留上游「默认分支读取」的供应链权衡） | 小改 |

---

## 10. qa-changes / cr 两个 skill 的归宿

- **skill 本体转为 gate 的知识载体**：SKILL.md 的流程与纪律成为对应 step 的内联 prompt 与 agent 侧协议（no-mistakes 的 SKILL.md ↔ axi 协议是现成先例）；references 原样保留为 step 执行手册。
- **手动入口保留为薄包装**：`/qa-changes <MR>`、`/cr [...]` 仍可独立触发，内部调用同一 gate 实现（单 gate 模式），产出同一种 findings/claims/证据格式——手动用和流水线用同一套判定与证据标准，杜绝两套真相。
- **cr 的 evals 与 capture-cr-rule 升格为全体系机制**（§8.1、§8.3）。
- 两者均为 beta、无兼容负担，上述改造不做旧格式兼容。

---

## 11. 设计权衡与未决问题

1. **证据存储与 HTML 卷宗托管是同一个决策**：分支内提交（过渡）vs 证据专用 ref（推荐终态），内网走 TOS。本方案立场是「默认可见」，托管位置可随上游演进迁移（§3.4）。
2. **verify 与复现的成本**：N 怀疑者 + 抽样复跑 + 跨 run 重放显著增加每单机器成本。这正是「用机器成本换人成本」的交换本身；`skeptics`/`replay_sample`/增量重放缓存是未来降档旋钮。
3. **误反驳（false REFUTED）**：多数决 + flaky 三连跑缓解；误反驳率是 verify 评测的一级指标（§8.1），审计时统计并回流。
4. **不可运行时验证的改动**（纯重构、构建配置、文档）：账本允许「静态已验证」为主的形态，但每个此类 hunk 必须挂可执行静态证据（§4.4c），自然语言声明不算——「重构」标签不再是免验通道，因为标签本身不被采信，被采信的是 typecheck/AST 工具的输出。
5. **双栈（GitHub / 内网 Codebase）**：证据托管、评论长度、approve 语义不同；scm 能力声明可承载，渲染两套适配需持续维护。
6. **采集器与签名链自身的可信**：它是新信任根，靠三件事背书——自身测试套件、签名验签逻辑的独立审计、以及「验签失败不渲染」这条渲染层铁律。残余风险：daemon 与 agent 同机同用户运行时，操作系统层面的密钥隔离强度有限（agent 理论上可读 daemon 私有目录）；更强的隔离（独立 uid / 远程签名服务）列为部署选项。
7. **人的行为风险是长期主要风险**：automation bias 不可能被界面设计完全消除，只能被 §7 的防盲信措辞、§8 的编制化盲审、§6 的人工下限持续对冲。这三件事里任何一件退化成口号，体系就会滑回「横幅上点绿」。

---

## 附录 A：调研输入

- no-mistakes 深挖报告（架构 / 9 步流水线 / findings / evidence / SKILL 协议 / 扩展点 / 7 缺口），源码 `/Users/bytedance/Documents/repos/no-mistakes`
- qa-changes 与 cr skill 调研报告（流程 / 证据能力评估 / 可改造点），源 `/Users/bytedance/Documents/workrepo/coze-monorepo-long-task/.agents/skills/{qa-changes,cr}` 与 `.agents/rules/cr-rules.md`

## 附录 B：对抗评审修订记录（v1 → v2）

v1 初稿经一轮对着 no-mistakes 源码的对抗性评审（独立 Agent 执行），确认问题并修订如下：

| 编号 | 评审发现 | v2 处置 |
|---|---|---|
| B1 | 「verify 打回 qa 重跑」与 executor 顺序状态机根本冲突（round 机制封闭在单步内，Respond 拒绝跨步定向） | §4.4a 重写：回退只用「verify 步内 round」与「跨 run 重放」两个既有原语；引擎回边降级为明确标注的非必需选项（§9 #2） |
| B2 | 「确定性风险评分」吃 agent 主观风险评级，是假确定性 | 原则 6 新增；§6 评分输入全客观量，agent 评级改为单向升险阀 + 展示项 |
| B3 | 信任根是路径约定非密码学 attestation；截图场景由 agent 编排，captured 被当语义背书用 | §3.1 采集器升级为 daemon 侧签名服务，渲染层只认验签条目；原则 2 明确「采集真实性 ≠ 语义有效性」；截图配插桩覆盖率佐证（§3.2） |
| S1 | sha256 比对非确定输出 → 假失败洪水；flaky 单次判死 | §3.2 可复现执行设施（归一化指纹、种子/时钟、录制回放）；§4.4b 三连跑多数决 + flaky 分类 + not-applicable 池 |
| S2 | hunk 覆盖与「静态已验证」标签由 agent 自贴，反滥用循环论证 | 原则 4 + §4.4c：运行时真值来自插桩数据，静态档必须挂可执行静态证据 |
| S3 | 绿色直通+yolo 下多数 PR 无人看；抽样审计无岗位/激励设计，冷启动标注无来源 | §8.2 编制化盲审岗位设计 + 分歧率指标 + gate 降权机制；§6 不变式 3 人工下限；§8.1 影子期冷启动 |
| S4 | 中段强制 park 与「按风险筛选后才给人」冲突 | §4.2/§4.3 修正：证据缺口记账进路由统一决策，步内 park 只留真正阻断 |
| M1 | verify 是最强单点权威却不被单次核验；共模盲区被低估 | §2.2 如实声明其权威地位；§8.1 verify 自身进评测 + 盲审一致率；§6 人工下限 |
| M2 | 遗漏覆盖率插桩、mutation/metamorphic、record-replay 等机器地面真值 | 插桩为账本真值（§3.2/§4.4c/§9 #7）；录制回放进复现设施；mutation/metamorphic 列为 gate 信号候选（未强制，评测先行） |
| M3 | intent 是评判根却被降为脚注 | §6 不变式 2：intent 缺失/低置信禁止绿色直通 |
| M4 | 对 qa-changes/cr 现状多处夸大（分诊无确定性探测、前后对比是叙事约定、checker 是新造、verify 是纯新增） | §2.2/§3.2/§4.1/§4.3 全部改为如实标注「沿用 vs 新造」 |
| M5 | claim-evidence 绑定是语法非语义 | 原则 3 与 §5 明确两层分工 |
| m1-m4 | 托管悬空、默认值自相矛盾、attach 逃生门无硬闸、「不发证」措辞失实 | §3.4 托管与卷宗绑定决策且立场明确；§3.2 attested 占比硬闸；§2.2 改写 verify 定位措辞 |
