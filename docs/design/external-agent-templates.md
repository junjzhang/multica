# 外部智能体模板、会话实例与任务级队长

状态：设计草案，WORK-267；已落实 2026-11-10 用户对 D1、D2、D3 的定案，待文档验收后另行派发实现。本次不修改运行代码、生产配置或历史数据。

## 目标与已定方向

外部智能体以模板出现在列表中，例如 `Paseo · VM-29-118`；该主机的不同会话展开在模板下。会话 A 发起的小队任务由 A 负责，会话 B 发起的另一条任务链由 B 负责，两者不争用全局队长身份。

- 模板承载名称、头像、skill、可见性、小队成员资格与授权；会话承载标题、在线状态、投递地址及活跃任务。
- 外部执行主体固定为 `模板 + 会话`；小队执行主体固定为 `小队 + 队长主体`，外部队长包含模板与会话；托管执行主体仍为 Agent。
- 外部发起或显式认领任务链的合格会话成为队长。`squad.leader_id` 只在没有发起会话的入口提供默认值，不覆盖已有任务绑定。
- 身份权限归模板，任务链、回投和 Contract 操作必须验证会话。审计保留 `Agent + 会话`，不能用选择器参数冒充同模板的另一会话。
- 托管 Agent 不得新建或改派任务给 squad，也不得通过评论 @squad 新发起任务链；已有任务中的成员执行、提交、普通评论及分配给 Agent 的子任务不受影响。
- 失联后显式交接，复用 stale/revise 语义；不改写既有交付作者、已接受回投或 Mailbox 信封。

下文“现状”是代码事实；“建议模型”及后续章节是待实现方案。在线规模“31 个 Agent、其中 19 个会话式 Agent”来自任务背景，本文没有重新盘点生产数据，也不将数量用作迁移条件。

## 现状与证据

核对基线为生产分支 `feat/task-session-delivery` 的 `20c43e5d153aef6f37f55a9d4c3b1957eadbd885`。以下链接固定到该 SHA，行号不随文档分支漂移。

| 路径 | 已核实的行为及设计影响 |
| --- | --- |
| [CLI connect](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/cmd/multica/cmd_session_connect.go#L32)；[插件注册](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/integrations/paseo-multica/server/service.ts#L186)；[名称生成](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/integrations/paseo-multica/server/service.ts#L363) | CLI 调插件 RPC，插件使用宿主标题生成 Agent 名并注册、签发 token。托管运行中禁止 connect。插件注册输入没有指定已有模板。 |
| [注册 handler](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/agent_external.go#L270) | API 已允许 `agent_id` 附加会话。按 workspace、owner、provider、host、宿主 session 查重；重复 connect 同一会话返回原 Agent。仅新会话且未指定 Agent 时创建新身份。因此“每次 connect 都创建”需要收窄为“每个新会话默认创建”。 |
| [会话查询](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/pkg/db/queries/external_agent.sql#L19)；[单会话唯一索引移除](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/migrations/613_drop_external_agent_session_agent_unique.up.sql#L1) | registered session 是 `external_agent_session` 的 UUID；`session_id` 是 Paseo 会话 ID，`host_id` 是连接/服务器 ID，不是 hostname。已支持一 Agent 多会话；单数 `external_session` DTO 是展示投影，不能当路由权威。 |
| [任务绑定](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/sessionroute/sessionroute.go#L104)；[显式分配](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/issue_external_assignment.go#L54) | `(workspace, agent, issue)` 只有一个活动接收会话。已有绑定可复用，冲突须 takeover；无绑定的可执行分配必须给 registered UUID，backlog 可暂不绑定。外部执行不创建 managed run。 |
| [Mailbox 选取与锁](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/pkg/db/queries/agent_mailbox.sql#L109)；[投递状态](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/delivery/claim.go#L66) | 信封冻结 `target_session_id`；候选和 advisory lock 按会话串行。worker 的 daemon 可更换，目标不变。busy 可重试；unknown 只查原 receipt，不能盲目重发。 |
| [delegation 地址](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/delegation/return.go#L213)；[回投约束](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/docs/design/task-session-delivery.md#L20) | 回投解析显式当前会话，拒绝缺失或歧义；已接受关系按任务与 dispatcher 固定，运行关联其实际承接的请求。coordinator 不是回投地址。同 Agent 在同任务换会话会冲突，不能静默替换。 |
| [wait 授权与完成](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/background_wait.go#L25) | external wait 验证 Agent、return session 与本机 daemon owner；完成事件使用登记的 registered session。更换模板选择不能改变原等待返回地。 |
| [小队目标解析](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/issue_external_assignment.go#L24)；[外部评价](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/squad_external_evaluation.go#L39) | 分配目标读取 `squad.leader_id`；外部 squad activity 也检查当前全局 leader。仅改变 briefing 文案不足以支持任务级队长。 |
| [Contract basis](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/migrations/654_squad_contract_representative.up.sql#L1)；[权限计算](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/issue_contract.go#L82) | 父合同 executor 保持 squad，representative 从未归档的小队全局队长及成员资格计算。basis 变化则 stale，当前比较主体只有 type/id。 |
| [briefing](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/squadbriefing/briefing.go#L113)；[外部分配注入](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/issue_external_assignment.go#L101) | 外部信封携带 squad briefing，拥有父任务与客串小队权限分开；父合同要求队长以真实身份 submit。 |
| [托管容量检查](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/task.go#L3861)；[外部初值](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/pkg/db/queries/external_agent.sql#L1) | managed claim 用 Agent 级 `CountRunningTasks` 比较 `MaxConcurrentTasks`；外部 Agent 初值虽为 1，但外部通过 Mailbox，不走此检查。不能宣称现有外部会话被这个字段串行化；实际投递隔离已按会话。 |
| [token 鉴权](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/middleware/auth.go#L156)；[token 存储](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/pkg/db/queries/external_agent.sql#L95) | token 绑定 Agent、workspace、owner，没有会话字段；return session 是另行传入的引用。推断：直接合并模板会让同模板凭据可选择兄弟会话，必须先补会话凭据边界。此为代码推导，未做生产攻击验证。 |

## 建议模型：复用现有实体

不另建一套 Agent、运行或消息系统。模板仍是 `agent(execution_kind=external)`，实例仍是 `external_agent_session`。以下字段名是建议接口，不表示已上线。

| 存储 | 新增或调整 | 不变量 |
| --- | --- | --- |
| `agent` | 可空 `external_provider`、`external_host_id`，两者都存在表示新模板；旧 Agent 保持空 | 模板唯一键为 `(workspace_id, owner_id, external_provider, external_host_id)`，仅约束模板行；不把会话标题放进 key |
| `external_agent_session` | 新增 `revoked_at`；继续保存展示字段与 daemon 路由 | UUID、Agent、owner、provider、host、宿主 session 身份不可改写。在线状态由已授权 daemon 的宿主会话探测与观测时间推导，过期为 unknown；daemon 心跳或 claim 仅证明 worker 存活，不能单独证明宿主会话在线。不持久化第二份 connection state |
| `external_agent_token` | 可空 `external_session_id`；新签发必填，legacy 空值受下述单会话门禁限制 | 服务端校验会话属于 token 的 Agent/workspace/owner；模板不能签发无会话 token |
| `external_agent_session_binding` | 保留现有键，执行者写入口收窄到分配/交接 | 外部执行会话只存此处，Issue API 的 `execution_session` 从当前 assignee 的活动绑定投影，不再加重复列 |
| `issue` | 可空 `leadership_root_issue_id`；根行增加 `leadership_squad_id, leader_agent_id, leader_external_session_id, leadership_revision` | 小队任务与明确加入任务链的子任务引用同一根。根指针一跳且同 workspace，无环；external leader 有 registered session，managed leader 只绑定 Agent，不引入托管会话队长模型 |
| 现有 activity | 增加 leadership handoff 事件 payload：根任务、前后 revision/主体、原因、真实操作人、时间、请求 ID | 追加历史，不另建 1:1 领导表或专用事件表。信封 ID、目标、payload、receipt 不改；pending 可撤销为终态 |
| `issue_contract.data` | 当前 basis 增加执行会话、任务链领导 revision、representative 会话，以及外部 requester/acceptor 会话快照 | executor 的 type/id 继续兼容；delivery、decision 的 author 附会话。旧 JSON 不补造会话作者 |
| 审计、comment、squad activity | 增加可空 `actor_session_id`，由鉴权层赋值 | 旧记录空表示未知，不按当前会话补齐 |

`external_agent_session_binding` 同时保存非执行参与者的显式通知地址。对当前执行者的绑定禁止通过普通 bind 修改，只有分配/交接可写；所有执行者读取都查该绑定。同一任务内同模板仍只有一个活动会话地址，这是用户已选择的 D1 乙；不扩展为按会话区分的多 dispatcher 键。任务级 leader 的外部投递从领导根解析，不借用参与者的默认绑定。

模板 key 的权限命名空间包含 workspace 和 owner：同一主机上的不同用户不因主机名相同共享授权。provider 为接入方标识 `paseo`；host 使用接入方服务器实例 ID，hostname 和展示别名可随时改。不同 provider、owner 或 workspace 永不自动合并。实例 ID 跨普通重启稳定是待实现阶段验证的假设，不是本文已证实的 Paseo 保证；若验证失败，暂停模板开关，先补稳定主机登记协议。

- 同 key 并发首次 connect：一个事务创建模板与实例，唯一索引裁决，失败方回读同一模板；重复请求仍返回同一实例。
- 主机改名不换模板；重装导致 host ID 变化则建新模板，显式交接任务。不能据 hostname 相同推断是原主机。
- host ID 被克隆或复用无法仅靠字符串辨别；注册须由已授权插件/owner 与对应 daemon 验证。发现冲突停止注册并提示重新登记，不自动吸收旧会话。
- 模板归档保留 key；再次 connect 提示管理员恢复或明确建立新主机身份，不静默复活授权。权限、skill 和小队资格不从多个旧 Agent 做并集。
- 展示名必须满足现有 workspace 内名称唯一约束。默认 `Paseo · <host label>`，冲突时追加模板 ID 的短后缀并逐步加长直至唯一；名称和后缀不参与模板 key。主机改名不自动覆盖用户名称，显式改名发生冲突时返回可操作提示。

表间关系由应用事务校验 workspace、owner、成员资格，无外键和级联。新增普通/唯一索引均用独立单语句 `CREATE [UNIQUE] INDEX CONCURRENTLY` 迁移；新表也不以内联 PRIMARY KEY 绕过索引规则，先并发唯一索引，再独立挂约束。可空旧行不参与模板唯一性。上线前检查重复 key、无效身份和索引有效性；条件对象后续 DDL 使用 `IF EXISTS` / `IF NOT EXISTS`。SQL 改动需 `make sqlc`。

## 主体、任务链与权限

### 会话不能作为可冒充的参数

新插件 connect 返回模板 ID、registered session UUID、宿主 session ID 和会话专属 token。鉴权层清除所有客户端传入的内部身份头，由 token 派生 `Agent + actor_session_id`；CLI 的 `--return-session` 只做一致性校验，不能扩大主体。续签只能续当前会话，新增兄弟会话仍由已授权接入方注册。

模板权限是操作的上限，会话绑定是额外约束。每次请求重查 token 到期/吊销、会话吊销、Agent 归档、owner 成员资格及现有调用权限；投递 claim 也重查。业务中不能只比较 Agent ID。

本方案的 API 隔离保证以“会话凭据未泄露”为前提，覆盖伪造 session 参数、误路由及使用 A token 操作 B 的情况。它不防持有 B token 的进程：当前插件将凭据写入同 uid 可读的 0600 文件（[store.ts:108](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/integrations/paseo-multica/server/store.ts#L108)），CLI 可通过会话环境选择文件（[session_context.go:109](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/cmd/multica/session_context.go#L109)）。同账户恶意或失控进程可偷读兄弟 token，0600 不是会话沙箱。用户已选择 D2 甲：接受凭据未泄露前提下的 API/会话隔离，同 uid 宿主属于共同信任域。本期不做 OS 级沙箱、跨 uid 凭据隔离或防窃取 broker，也不将这些能力设为上线门禁；不能把 API 隔离宣传成进程隔离。

legacy 也不能笼统沿用宽 token：预检发现一个旧 Agent 注册多会话时，保持 Agent/session ID 和历史不变，为每个实例重新签发 session-bound token，确认所有在途会话已更新后吊销旧宽 token。切换门禁拒绝此类宽 token 的会话敏感操作，不替用户选会话；任一在途会话无法更新则停放该 Agent，不能开启它的多会话新写入。单会话 legacy 空值 token 只允许服务器证明恰好一个原注册会话后按该身份使用；禁止继续追加第二会话，先升级凭据再注册。新模板始终无此 legacy 例外。

| 操作 | 外部调用主体必须满足 |
| --- | --- |
| 新建普通任务、发表评论 | 现有模板权限；审计附真实会话。B 可在 A 负责的任务卡上查看和普通评论，但不改变负责人、执行绑定、派发义务或回投；评论内容不是执行或队长授权 |
| 为外部 Agent 分配执行 | 调用者有调用权限，接收 registered session 属于目标模板；已有不同执行会话返回 conflict |
| 发起小队任务链 | 发起模板是该 squad 的有效成员且有调用权限；使用发起凭据的会话，不接受 body 自报的 leader |
| 认领尚无队长的任务链 | 同上，以 expected revision 做 CAS；两个会话竞争只有一个成功。已有绑定必须走交接 |
| 代表小队 submit、记录队长 activity | 当前任务级 leader 的 Agent 与会话匹配，模板仍有成员资格，Contract 非 stale |
| 外部 executor submit | 当前 executor 模板与执行会话均匹配，并满足当前 Contract |
| revise / accept / return | 现有 requester/acceptor 权限之外，还须匹配该版所钉住的外部会话；同模板不同会话不能绕过禁止自改、自验规则 |
| Mailbox list / ack | 会话凭据只看/处理自己的 frozen target；人类管理视图另按原管理权限授权。传其他 session UUID 不能越权 |
| wait start / get / cancel | 登记者 Agent、会话及本机 daemon 授权均匹配；结果写入使用原登记地址 |

### 绑定顺序与默认回退

1. 有已验证的外部发起会话且发起模板为小队有效成员：创建领导根并固定该会话；不查全局默认队长。无资格则拒绝，不假扮默认队长继续执行。
2. 没有发起会话的网页或自动化入口：读取 `squad.leader_id`。托管默认队长照旧创建运行，来源 run 由服务器记录；外部默认队长必须由入口明确配置 registered session，缺失就停放并提示选会话。即使只有一个在线会话也不自动选。
3. 新分配给外部模板必须选会话。backlog 可以先仅选模板，但标记“待选会话”，无执行权限和通知义务；从 backlog 启动前补齐。已有精确绑定的恢复/重试复用原绑定，不能偷偷换会话。
4. 创建子任务时显式加入父任务链，服务端验证父链与发起者权限，并记录根 ID。子任务的 assignee 与独立 Contract 不变，leader 不因此获得代交付/代验收权。单纯设置 parent 或 @另一个 squad 不改所属任务链；客串小队仍不能管理父任务状态。
5. 默认队长调整只影响之后没有发起会话的新任务；现有领导根不随之漂移。成员资格撤销、模板归档导致现有 leader 不再有效，任务显示 unavailable，不能自动回退。

托管 task token 新建或改派任务给 squad 一律拒绝，包含根任务、子任务、backlog、suppress_run、批量改派及工作流解析后实际移交给 squad 的操作；托管队长也没有例外。不能回退到默认队长把拒绝变成成功，不能用原始人类 originator 把 task token 当成人类请求。网页用户与服务端 Autopilot 调度属于无会话入口，仍按规则 2 解析默认队长；Autopilot 启动后的托管运行持有 task token，同样受禁令约束。

已有小队任务中的托管成员仍可按原权限被派活、提交 Contract、普通评论及创建分配给 Agent 的子任务；这些操作不得隐式改成新的 squad 分配。小队的托管默认队长仍可执行已有小队任务、向成员 Agent 派发和代表本小队提交；禁止的是它再发起小队任务，而非禁止托管 Agent 当现有队长。系统恢复已有绑定、原运行续接、冻结回投、child_done 和已授权的既有唤醒不因此被禁止。鉴权应判定本次真实调用者和操作意图，不按“当前任务 assignee 是 squad”一刀切。

同模板 A、B 可分别领导两个根任务；同一根当前只有一个 leader。保留 `(issue, dispatcher)` 与 `(workspace, agent, issue)` 约束：在 A 负责的任务上，B 不能独立委派或改变负责人，尝试派发返回 409，不能静默复用 A 的身份或回投。B 可按模板权限查看任务卡和发表普通评论；评论仅记录真实 `Agent + B 会话`，不改负责人/执行绑定，不创建派发、自动 assignee 唤醒或新的 accepted return，不改 A 的既有回投。评论创建、编辑和预览都遵守此边界：B 使用可执行的 @agent/@squad 请求派发时拒绝并提示改为普通评论或请求显式交接，不能借评论副作用绕过 D1。需要承担原责任时由人类显式交接；独立工作可另建任务或子任务。旧“一会话一 Agent”下同任务多会话独立委派不再无缝，这是用户已接受的 D1 乙取舍。

### 托管发起门禁及现有流程冲突

新增门禁位于服务端业务写入与 delegation/queue/Mailbox 义务创建之前，并在事务内按鉴权主体及解析后的目标复核。CLI 只提供前置提示，不能作为唯一防线。新建/改派以及新链 @squad 请求返回 HTTP 403、稳定错误码 `managed_squad_initiation_forbidden`，提示“托管智能体不能发起小队任务；请分配给具体智能体，或请人类/已接入的外部会话发起小队任务”。不建议换凭据重试。批量接口按现有逐项语义返回失败项与相同原因，拒绝项不产生任务、状态、负责人、关系、事件或派发变更；允许项不受影响。

普通评论可以保存；真正的 @squad 新派发（含对已有任务另开小队协作链）在保存前拒绝整次评论写入，提示去掉可执行提及后再评论，避免部分派发。按已接受关系返回结果或已有成员汇报不等于新建任务链，不从 mention 文本重新推导授权；旧线程身份、源运行与冻结 return 是续接依据。评论预览/编辑沿用同一门禁，不能先保存再改出提及。不得通过直接 @默认队长并让其建 squad 子任务绕过门禁：目标队长后续的创建请求仍是 task token。

以下冲突均以固定基线源码核对；表中的拒绝和改造是拟议实现，本文未更改代码或运行配置：

| 入口及基线事实 | 与用户定案的冲突及处理 |
| --- | --- |
| [CreateIssue 的 assignee 检查，issue.go:3411](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/issue.go#L3411)；[共享权限检查:4540](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/issue.go#L4540) | 当前按 originator 的调用权限放行 squad，未禁止 task token。新增、工作流解析后的创建及托管队长创建 squad 子任务均需明确拒绝；Agent 子任务照旧 |
| [UpdateIssue:4238](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/issue.go#L4238)；[批量更新:5140](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/issue.go#L5140) | 当前明确改派及工作流 handoff 都复用调用权限。检查最终目标及操作意图，不能靠 suppress_run、先建无主任务再改派或批量路径绕过；普通更新已有 squad 任务不算新改派 |
| [评论 squad 解析，comment.go:3175](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/comment.go#L3175) | 当前解析默认队长后通过调用权限可创建新派发。托管新链 @squad 必须在新义务产生前拒绝；既有成员普通评论和已冻结结果回投保留 |
| [小队触发，issue_trigger.go:152](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/issue_trigger.go#L152)；[成员派发 briefing:49](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/squadbriefing/briefing.go#L49) | 基线明确把“Agent 给自己领导的 squad 分配”视为有效 group handoff。该行为与 D3 冲突，必须禁止；托管队长改为给具体成员 Agent 派活。briefing/CLI 帮助须同步此边界，不能再指导层层分派给 squad |
| [网页 QuickCreateIssue，issue.go:2823](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/issue.go#L2823) | 当前网页选 squad 后先唤起默认托管队长，再由它创建 squad 任务，第二步会违反 D3。后续实现须将实际 squad 任务创建与绑定移至原人类请求授权下的服务端操作，再启动队长处理已存在任务；不能给 quick-create task token 开例外。切换前遗留此类尚未建任务的运行需停放并由人类通过新入口重新发起，幂等核对防止重复建单 |
| [Autopilot create_issue 入队，autopilot.go:831](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/autopilot.go#L831) | 服务端调度创建并绑定小队任务可继续使用默认队长；若 run_only 或任务提示要求托管运行再建 squad 任务，后者受禁令约束，改用服务端 create_issue 模式或分配给具体 Agent。不得把 task token 的人类来源当豁免 |
| [现有 Agent 创建小队任务回归，issue_agent_create_e2e_test.go:91](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/issue_agent_create_e2e_test.go#L91) | 测试显式要求 task token 创建 squad 任务返回 201，与新规则冲突。实施时将该场景改为拒绝回归，另保留“人类创建小队任务 → 托管队长向成员 Agent 派发”的合法链路覆盖；不删除 originator 权限验证 |

上述冲突按用户已定 D3 处理，无需重开默认队长方案。上线前需完成这些入口改造与拒绝/保留路径的成对验证，不能只改 UI 或文案就开放门禁。

### 统一队长解析的消费者清单

建议统一 `ResolveIssueLeader(issue, squad)`：该 squad 与既有领导根相符时，只返回根绑定，失效则 unavailable；没有适用根才允许新入口解析默认队长并保存绑定。@客串 squad 不改原根，只建该次授权范围内的显式客串投递；其通知不授予父任务管理权限。以下是基线仍使用全局队长的读取点，必须连同前述分配、activity、Contract 一起改造，不能只改 briefing：

| 消费者及基线证据 | 设计要求 |
| --- | --- |
| [评论 @小队投递，mailbox.go:491](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/mailbox.go#L491) | 先拒绝托管新链 @squad，再对允许入口按任务 leader 解析、授权及 enqueue；普通评论不派发，不能回退默认队长绕过门禁 |
| [wakeup 权限，issue_wakeup.go:1450](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/issue_wakeup.go#L1450) | 建规则与执行时校验任务 leader 和外部 session，旧队长不能靠模板相同修改规则 |
| [系统 wakeup，issue_wakeup_system.go:343](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/issue_wakeup_system.go#L343)；[父任务/stage 候选 SQL:60](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/pkg/db/queries/workspace_wakeup.sql#L60) | 查询候选与最终派发采用同一领导根；child_done、stage 屏障完成通知不得回到全局默认值 |
| [rerun，task_lifecycle.go:386](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/handler/task_lifecycle.go#L386)；[触发判断，issue_trigger.go:165](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/issue_trigger.go#L165) | 系统恢复和既有执行续接使用原绑定，returns 保持快照；托管新建/改派的触发在前置门禁拒绝，不能转为默认队长 rerun |
| [队长入队，issue.go:1166](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/issue.go#L1166)；[task.go:6221](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/task.go#L6221) | 区分被派活的 managed 执行者与发起方；拒绝托管新发起后，合法入口才解析主体并分 managed queue/external Mailbox；claim 重验 revision |
| [自动化，autopilot.go:1477](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/service/autopilot.go#L1477) | 服务端自动化新任务使用默认值，配置外部接收会话并保存绑定；启动后的 task token 不得再发起 squad 任务，恢复已有任务不覆盖根 |
| [名册，briefing.go:229](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/internal/squadbriefing/briefing.go#L229) | “Leader (you)” 指当前任务 leader；默认队长单独标识。托管队长只能给具体 Agent 派活，不能建立 squad 子任务；worker roster 不混淆兄弟会话 |

实现审查需搜索全部 `leader_id` / `LeaderID` 消费者，将剩余读取分类为任务级解析或纯默认配置展示；列表不是允许保留其他隐式回退的白名单。

### 交接与 stale/revise

失联是路由可用性，不等于权限撤销；A 短暂离线仍拥有任务。需要换 B 时使用专用 handoff 请求，带旧主体、目标 registered UUID、expected leadership/issue revision、原因和幂等请求 ID。

- handoff 的生效操作限定人类 owner/admin，且须具有任务管理与目标调用权限；Agent 只能提出交接建议，不能调用生效接口。旧会话仍在线也不例外，避免新增机器凭据接管授权。跨模板更换还须检查新模板的小队成员资格，普通兄弟会话不得自行接管。
- 锁定领导根、受影响任务、绑定及合同状态，按确定顺序验证版本、权限与目标。事务内更新当前主体与 revision，记录事件、撤销未开始的旧派发、生成新派发义务。消费者在执行前校验该 revision，旧队长后续写入拒绝。
- 相关 Contract 的 basis 引用领导根 revision，交接后自然 stale；有权请求方 revise 后新会话才 submit/accept。子任务合同中 requester/acceptor 也指向旧会话时同样 stale，但无关合同不批量改版。原外部 requester 会话已失联时，由现有 Contract 维护权限允许的人类维护者恢复；不要求已失联者上线。新 executor/leader 不获得 revise 自己标准的权限。
- 已运行子任务继续保存原交付与回投；不因换队长重跑。已投递或 unknown 信封保留 ID、目标和 receipt；unknown 仍仅核验。已有在途运行须显式取消或收齐结果，不能把“撤销待派发”当作停止执行。
- 交接列出需接收的已有结果和对应 return ID。现有 [result takeover 路由](https://github.com/junjzhang/multica/blob/20c43e5d153aef6f37f55a9d4c3b1957eadbd885/server/cmd/server/router.go#L2089) 仅限人类；因此本方案 handoff 也保留此边界。同模板交接事务更新所选 dispatcher 的当前 return 版本，旧版本仍供已承接运行引用；已有结果通过同权限的 takeover 操作建新通知。两步分别幂等，UI 在结果通知完成前显示“交接已生效，历史结果待接收”，不能误报全部收齐。unknown 同时展示，不修改原信封，不继承 B 的其他上游链。
- 跨模板（包括旧 Agent → 新模板）不能直接套用现有同 dispatcher takeover，需明确扩展人类结果接收操作：请求给出旧 return、精确结果 run、新模板/session、expected 版本及请求 ID；服务端检查人类对原/目标 Agent 的管理权限、任务访问及目标可用性。在事务中创建目标 dispatcher 的独立 return 关系与一次性结果通知，记录旧 return 为来源，不重写旧 dispatcher/作者，也不伪造新 Agent 曾经委派的历史。目标已有不同当前 return 则 conflict；不自动汇集其上游。后续新委派由新主体接受新关系；旧在途运行仍先履行原冻结回投，再由人类按具体结果显式接收。该 API 扩展和迁移演练完成前，不允许归档仍有旧义务的 Agent。
- wait 的本地命令继续由原 daemon 持有；交接不搬进程，也不改已登记地址。新负责人可经授权显式取回已有结果，需取消时请求原 owner/daemon 操作，不能从兄弟会话借凭据取消。

| 当前状态 | 事件 | 结果 |
| --- | --- | --- |
| 未绑定 | 合法分配/认领 | 绑定且可执行；业务写与派发义务同事务 |
| 已绑定 | 重复同请求 | 返回原结果，不新增通知 |
| 已绑定 | 会话离线 | 显示不可达；保留绑定与可核验的等待/投递状态 |
| 已绑定/不可达 | 合法 handoff | 新当前主体，旧历史不变；相关合同 stale |
| stale | 有权 requester revise | 新版可继续，旧版接受项不继承 |
| 任意 | 旧 revision 或伪造会话写入 | conflict/forbidden，无部分业务写、无幽灵派发 |

## 接口、投递及界面

| 入口 | 变更 |
| --- | --- |
| `session connect/status` | connect 按模板 key 解析或创建模板并注册实例；status 同时显示模板、会话及凭据绑定状态。保留禁止 managed connect、跨 profile 覆盖和 foreign context 覆盖的检查 |
| `issue create/assign` | 继续使用 `--recipient-session-id`；小队发起者会话来自鉴权。managed create/assign 到 squad 返回可操作拒绝，批量逐项报因；人类无会话入口明确选外部默认队长会话；合法赋值与绑定原子化 |
| 交接 CLI/API | 增加任务主体 handoff 操作；不把普通 bind 或 assign 当作隐式 takeover。请求含 revision 与 idempotency key，响应含受影响合同和待回收通知 |
| 注册 API | 复用 external 注册入口，新增显式模板模式/能力版本；返回原 Agent shape 加本次 registered session。旧客户端不能在新模板上签发宽泛 token |
| Agent / sessions API | 保留原字段及单数 `external_session` 的兼容展示投影，新增模板标识、session count 与分页会话列表；所有执行入口仅使用显式会话字段 |
| Issue / runs / Contract API | additive 返回 execution session、leadership root、leader 主体与 revision；`executor_representative` 仍保留 type/id，追加会话字段；`accepted_returns` 保持完整冻结快照，缺失不解释为 empty |
| Mailbox / delegation | B 普通评论不入执行投递队列、不建立返回关系；托管新链 @squad 不产生义务。允许投递的目标来自任务主体或明确参与者绑定，入箱后不重解析；结果回投用冻结关系，list/ack/activity 校验会话与 revision |
| squad briefing | 注入“本任务队长是谁、会话、根任务、revision、默认队长仅供新入口”；managed claim 与 external 信封使用同一解析器，客串与 owning 权限仍分开 |
| Web / Desktop | Agent 列表展开会话，任务卡为 `模板 › 会话标题`，小队卡附队长。B 仍可查看并普通评论，负责人始终显示 A；其无派发权限时保留评论入口，禁用派发/接管控件并提示显式交接，评论预览显示不会派发或改变回投。失联给“交接”操作，选择器排除无权限/吊销会话，长标题保留可访问完整名称 |
| Mobile | 独立数据层与 UI 同步会话选择及 B 可读/可评论、不可派发提示；支持前不开放新模板执行变更，仍可读历史，不依赖 web 组件 |

API 响应用共享 zod schema + `parseWithFallback`，移动端使用自己的解析器；缺失新字段显示未知，未知 enum 有 fallback。旧桌面仍可读旧字段，危险写入由服务端能力校验拒绝并给升级提示，不能依赖旧 UI 隐藏按钮。兼容仅在 API 响应与明确 legacy 数据边界；不增加内部双写、按最新会话猜测或长期双调度路径。

并发分两层：managed 的 Agent 级 `max_concurrent_tasks` 原样保留；external 的工作投递按 registered session 串行，Paseo v1 每会话最多一个活动宿主 turn，busy 后原 ID 重试。同模板 A 忙不阻塞 B，不把模板的历史 `max_concurrent_tasks=1` 套到所有会话。活跃任务列表来自任务绑定/运行投影，不拿未验收任务数占用会话运行槽。全局 daemon 工作池容量仍可能限制吞吐，不承诺物理资源无限并行。

## 历史、上线与回滚

建议维护窗口切换新写入，不实施未授权的生产迁移。历史 Agent、session、任务、合同、Mailbox、return 与 wait 记录原样保留。

1. 预检并演练：枚举旧 Agent 的在途任务、未验收合同、pending/leased/unknown 消息、未处理结果、accepted returns、wait 和小队默认队长引用。输出可归档/不可归档及原因；不以 Agent offline 或单个 issue done 判定安全。
2. 部署 additive schema 与兼容读取，保持模板写入关闭。创建索引失败或条件对象缺失时阻断开关，修复对象后重建无效索引，核对迁移记录与实际结构一致。
3. 升级 server、CLI、插件、daemon 和写入 UI 的能力协议，先完成 legacy 多会话凭据更换、宽 token 吊销与会话校验；在测试 workspace 用两个新会话验证隔离与回投。不能更新的旧在途会话明确停放，当前活跃会话最后处理；旧版本写入新模板一律拒绝。
4. 暂停新 external 注册/派发，备份数据库与原部署产物。核对旧在途责任后打开模板 connect；此前从未注册的新宿主会话挂新模板。模板初始授权须由 owner 明确选择，不自动汇总旧权限。
5. 已有宿主会话重复 connect 仍返回它的旧 Agent，不能将同一 immutable session 改挂模板。完成旧责任后，由 owner 在宿主创建新会话挂模板，再显式交接需要延续的未结任务。这保留“当前有在途会话最后处理”，也避免一个物理会话同时拥有两个可投递身份。
6. 旧 Agent 无在途责任且已处理默认队长/成员引用后，提供归档预览，逐个由 owner 确认归档并吊销其旧 token。不删除历史或改作者。归档扫描与最终写入之间再次加锁复核，防止新派发插入；保留可追溯的旧→新模板提示关系只用于导航，不作权限或路由别名。

旧 Contract 不批量重写：未交接的 legacy 任务按原 basis 版本继续；第一次显式交接进入新主体语义并 stale/revise。basis 生成需由任务是否已采用新绑定决定，不能仅新增 JSON 键就让所有历史合同突然 stale。legacy token 只能访问其旧 Agent，不能通过新 session 选择器进入模板任务；新模板无 legacy 宽权限回退。

回滚边界：首次新模板业务写之前，可关闭开关并退回旧二进制，保留 additive 表。首次新写之后禁止直接回旧写路径或执行 down 删除会话字段；停止新注册和派发、保留数据库及消息凭据，使用支持新模型的修复版本前滚，或在完整导出新增记录并经另行批准后进行恢复演练。历史备份恢复会丢失新增交付，不能作为“无损回滚”。当前文档没有授权执行任何一种生产恢复。

## 验收与实施拆分

测试全部在隔离数据库与测试宿主会话执行；不以真实用户会话做默认测试，不启动真实 Agent CLI。以下是后续实现验收计划，本设计 PR 只做文档检查。

| 场景 | 必须证明的结果 | 主要层次 |
| --- | --- | --- |
| 同主机两个新会话并发 connect、同会话重试、host 改名/重建、跨 owner/workspace | 复用正确模板，实例不重复，身份冲突不自动迁移 | 注册 handler + DB |
| A token 携带 B 的 header、return/session UUID、Mailbox ID | list/ack、Contract、wait、activity、handoff 均拒绝越权；无副作用 | 鉴权与各边界回归 |
| A/B 同模板分别发起两个 squad 根任务 | 各自成为队长、各自回投/提交；默认队长无权抢交；并发 claim 不互相占 Agent 槽 | 服务事务 + 投递 DB |
| B 领导任务后 @小队、定时/系统 wakeup、rerun、子任务/stage 完成 | 所有入口解析 B，失联时明确失败；默认队长变更不接管；客串不改根 | trigger + wakeup + 队列回归 |
| 同模板 A/B 在同任务独立委派，人类同模板/跨模板交接 | B 独立委派固定返回 409；跨模板结果接收保留原作者/return，仅产生一次新通知；Agent handoff 被拒绝 | delegation + handoff DB |
| legacy 一 Agent 多会话、宽 token 轮换、同 uid 凭据暴露 | 未轮换禁止敏感写；轮换后 A token 不能操作 B；同 uid 共同信任域不承诺抵御凭据窃取，OS 沙箱不作为验收门禁 | 迁移 + 鉴权/宿主验收 |
| 网页/自动化无发起会话 | managed 默认正常；external 缺显式会话停放，单在线会话也不偷选 | API + CLI |
| managed task token 新建/改派 squad（根/子任务、backlog、suppress_run、批量、工作流最终目标） | 403 或逐项失败原因明确；无业务/绑定/return/queue/Mailbox 副作用，默认队长不作为回退；托管队长也拒绝 | handler + managed 分配 + DB |
| 托管评论创建/编辑/预览中新链 @squad，普通成员汇报及合法 @Agent 派活 | 新链提及返回可操作拒绝，普通评论可保存，既有回投不变；成员执行/提交、Agent 子任务、已授权恢复继续工作 | comment + delegation + Contract |
| B 在 A 任务卡查看、普通评论、编辑评论和试图添加派发提及 | 卡片及 API 均允许普通评论，审计作者为 B；负责人/执行绑定/accepted returns 不变，零派发/自动唤醒；可执行提及被拒绝 | comment DB + UI/E2E |
| 网页快速创建、Autopilot create_issue 与启动后 task token 再建 squad | 人类/服务端先建并绑定，再唤起默认队长；后续托管新发起被拒绝，run_only 不豁免；旧 quick-create 停放后幂等恢复 | 服务端入口 + E2E |
| 同根同时认领、分配与 handoff 竞态、handoff 与 submit/claim 竞态 | 单一胜者；旧 revision 拒绝；旧 pending 派发撤销不漏事务 | DB 并发测试 |
| 默认 leader 改动、成员移除、leader 失联 | 已绑定任务不漂移；失效可见；只有显式交接改变主体 | 领导解析 + Contract |
| 父/子合同、换会话后 revise、同模板异会话自验 | 正确 stale、独立验收；旧交付/作者/接受历史不改写，无自验逃逸 | Contract 状态矩阵 |
| 投递 busy、ack 丢失、unknown、会话删除、结果 takeover、wait 完成 | 原信封/receipt/固定 return 不变；既有结果保留，显式接收不重跑 | delivery + daemon fake host |
| 旧 Agent/旧 token/旧 Contract 与新模板并存，归档竞态，开关回退 | legacy 不冒充模板；在途引用阻止归档；新写后禁止不安全降级 | 迁移演练 + API |
| 旧桌面响应缺字段/未知 enum；新列表与选择器；移动端未升级 | 可读、写入安全拒绝、无“最新会话” fallback；键盘与长标题可用 | schema + 组件/E2E |

实施按三个可审查切片推进：模板注册及会话凭据隔离；任务绑定/领导解析及 Contract、Mailbox、wait 全调用链；UI/迁移工具和维护窗口演练。第一个切片验证后再开放模板合并写入，避免先共享身份后补鉴权。实现阶段分别跑针对性 Go/DB、CLI/plugin 测试，共享前端检查及必要 E2E；移动端按其独立 AGENTS.md 验证。

## 用户已定决策与实施边界

2026-11-10 用户定案已纳入正文、接口和验收矩阵，不再作为待选项：

| 决策 | 本期规则 |
| --- | --- |
| D1 乙 | 每任务每模板一个活动地址。B 可按模板权限查看/普通评论，但不改变 A 的负责人、派发或回投；独立委派返回 409，接管须显式交接 |
| D2 甲 | 接受凭据未泄露前提下的 API/会话隔离；同 uid 为共同信任域，不做 OS 级沙箱，不将强进程隔离作为上线门禁 |
| D3 新方案 | 托管 Agent 不得发起小队任务，包括新建、改派及评论 @squad 新链；现有任务成员活动和 Agent 子任务保留，网页与 Autopilot 服务端入口仍用默认队长 |

本轮没有新增待用户决策项。以下仍是实施边界或待验证事实，不表示已获实现、合入或生产操作授权：

- host 的粒度采用 provider 实例 ID 而非物理机名；同一物理机重装后算新模板。若产品需要跨重装连续身份，应另行设计可信主机登记，不用 hostname 合并。
- 不把已登记的旧宿主会话原地搬到模板；旧会话完成责任后用新宿主会话接续。若必须保留同一宿主上下文原地换身份，需要独立的一次性迁移协议，当前不做。
- 默认外部队长入口必须显式配置会话；不提供“最近在线”自动回退。该约束是 WORK-183 显式接收规则的延续。

不做：OS 级会话沙箱、抵御同 uid 凭据窃取的强进程隔离、同任务同模板多活动派发会话、托管 Agent 发起小队任务、生产部署/合入、自动归档或删除旧 Agent、改写历史作者、模板之间权限合并、多队长广播、通用消息代理替换、宿主内部子 Agent 管理、跨 provider 自动迁会话、运行中命令搬迁、扩大 merge/deploy 授权、基于任意新会话的自动故障恢复。
