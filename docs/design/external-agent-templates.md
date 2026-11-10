# 外部智能体模板、会话实例与任务级队长

状态：设计草案，WORK-267；待用户审阅后另行派发实现。本次不修改运行代码、生产配置或历史数据。

## 目标与已定方向

外部智能体以模板出现在列表中，例如 `Paseo · VM-29-118`；该主机的不同会话展开在模板下。会话 A 发起的小队任务由 A 负责，会话 B 发起的另一条任务链由 B 负责，两者不争用全局队长身份。

- 模板承载名称、头像、skill、可见性、小队成员资格与授权；会话承载标题、在线状态、投递地址及活跃任务。
- 外部执行主体固定为 `模板 + 会话`；小队执行主体固定为 `小队 + 队长主体`，外部队长包含模板与会话；托管执行主体仍为 Agent。
- 发起或显式认领任务链的合格会话成为队长。`squad.leader_id` 只在没有发起会话的入口提供默认值，不覆盖已有任务绑定。
- 身份权限归模板，任务链、回投和 Contract 操作必须验证会话。审计保留 `Agent + 会话`，不能用选择器参数冒充同模板的另一会话。
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
| `external_agent_session` | `last_seen_at`、`connection_state`、`revoked_at`；继续保存展示字段与 daemon 路由 | UUID、Agent、owner、provider、host、宿主 session 身份不可改写。在线是有时效的观测，过期显示 unknown/offline，不等同于有投递权限 |
| `external_agent_token` | 可空 `external_session_id`；新签发必填，旧值空只供 legacy 身份按原边界使用 | 服务端校验会话属于 token 的 Agent/workspace/owner；模板不能签发无会话 token |
| `issue` | 可空 `execution_session_id`；可空 `leadership_root_issue_id` | 外部 assignee 的当前执行会话是单一权威；托管 assignee 为空。小队任务与明确加入任务链的子任务引用同一领导根 |
| `issue_leadership`（新增） | `workspace_id, root_issue_id, squad_id, leader_agent_id, leader_external_session_id, leader_source_task_id, revision, state` | 每根任务一行；external 有 registered session，managed 有真实来源 run（首次默认队长启动前可空），不伪造 external session |
| `issue_leadership_event`（新增） | 根任务、前后 revision/主体、原因、真实操作人和会话、时间、请求 ID | 追加历史；当前领导记录可更新，历史交付与信封不可更新 |
| `issue_contract.data` | 当前 basis 增加执行会话、任务链领导 revision、representative 会话，以及外部 requester/acceptor 会话快照 | executor 的 type/id 继续兼容；delivery、decision 的 author 附会话。旧 JSON 不补造会话作者 |
| 审计、comment、squad activity | 增加可空 `actor_session_id`，由鉴权层赋值 | 旧记录空表示未知，不按当前会话补齐 |

`external_agent_session_binding` 继续用于非执行参与者的显式通知地址；对执行者的绑定只能由分配/交接事务维护，并必须等于 `issue.execution_session_id`，禁止再独立 bind 执行者。这是保留现有路由索引的受约束投影，不允许两个写入口各自决定执行归属。任务级 leader 的外部投递从领导根解析，不借用某个参与者的默认绑定。

模板 key 的权限命名空间包含 workspace 和 owner：同一主机上的不同用户不因主机名相同共享授权。provider 为接入方标识 `paseo`；host 使用该接入方稳定的服务器实例 ID，hostname 和展示别名可随时改。不同 provider、owner 或 workspace 永不自动合并。

- 同 key 并发首次 connect：一个事务创建模板与实例，唯一索引裁决，失败方回读同一模板；重复请求仍返回同一实例。
- 主机改名不换模板；重装导致 host ID 变化则建新模板，显式交接任务。不能据 hostname 相同推断是原主机。
- host ID 被克隆或复用无法仅靠字符串辨别；注册须由已授权插件/owner 与对应 daemon 验证。发现冲突停止注册并提示重新登记，不自动吸收旧会话。
- 模板归档保留 key；再次 connect 提示管理员恢复或明确建立新主机身份，不静默复活授权。权限、skill 和小队资格不从多个旧 Agent 做并集。

表间关系由应用事务校验 workspace、owner、成员资格，无外键和级联。新增普通/唯一索引均用独立单语句 `CREATE [UNIQUE] INDEX CONCURRENTLY` 迁移；新表也不以内联 PRIMARY KEY 绕过索引规则，先并发唯一索引，再独立挂约束。可空旧行不参与模板唯一性。上线前检查重复 key、无效身份和索引有效性；条件对象后续 DDL 使用 `IF EXISTS` / `IF NOT EXISTS`。SQL 改动需 `make sqlc`。

## 主体、任务链与权限

### 会话不能作为可冒充的参数

新插件 connect 返回模板 ID、registered session UUID、宿主 session ID 和会话专属 token。鉴权层清除所有客户端传入的内部身份头，由 token 派生 `Agent + actor_session_id`；CLI 的 `--return-session` 只做一致性校验，不能扩大主体。续签只能续当前会话，新增兄弟会话仍由已授权接入方注册。

模板权限是操作的上限，会话绑定是额外约束。每次请求重查 token 到期/吊销、会话吊销、Agent 归档、owner 成员资格及现有调用权限；投递 claim 也重查。业务中不能只比较 Agent ID。

| 操作 | 外部调用主体必须满足 |
| --- | --- |
| 新建普通任务、发表评论 | 现有模板权限；审计附真实会话。普通评论不授予执行或队长资格 |
| 为外部 Agent 分配执行 | 调用者有调用权限，接收 registered session 属于目标模板；已有不同执行会话返回 conflict |
| 发起小队任务链 | 发起模板是该 squad 的有效成员且有调用权限；使用发起凭据的会话，不接受 body 自报的 leader |
| 认领尚无队长的任务链 | 同上，以 expected revision 做 CAS；两个会话竞争只有一个成功。已有绑定必须走交接 |
| 代表小队 submit、记录队长 activity | 当前任务级 leader 的 Agent 与会话匹配，模板仍有成员资格，Contract 非 stale |
| 外部 executor submit | 当前 executor 模板与执行会话均匹配，并满足当前 Contract |
| revise / accept / return | 现有 requester/acceptor 权限之外，还须匹配该版所钉住的外部会话；同模板不同会话不能绕过禁止自改、自验规则 |
| Mailbox list / ack | 会话凭据只看/处理自己的 frozen target；人类管理视图另按原管理权限授权。传其他 session UUID 不能越权 |
| wait start / get / cancel | 登记者 Agent、会话及本机 daemon 授权均匹配；结果写入使用原登记地址 |

### 绑定顺序与默认回退

1. 有已验证的发起会话且发起模板为小队有效成员：创建领导根并固定该会话；不查全局默认队长。无资格则拒绝，不假扮默认队长继续执行。
2. 没有发起会话的网页或自动化入口：读取 `squad.leader_id`。托管默认队长照旧创建运行，来源 run 由服务器记录；外部默认队长必须由入口明确配置 registered session，缺失就停放并提示选会话。即使只有一个在线会话也不自动选。
3. 新分配给外部模板必须选会话。backlog 可以先仅选模板，但标记“待选会话”，无执行权限和通知义务；从 backlog 启动前补齐。已有精确绑定的恢复/重试复用原绑定，不能偷偷换会话。
4. 创建子任务时显式加入父任务链，服务端验证父链与发起者权限，并记录根 ID。子任务的 assignee 与独立 Contract 不变，leader 不因此获得代交付/代验收权。单纯设置 parent 或 @另一个 squad 不改所属任务链；客串小队仍不能管理父任务状态。
5. 默认队长调整只影响之后没有发起会话的新任务；现有领导根不随之漂移。成员资格撤销、模板归档导致现有 leader 不再有效，任务显示 unavailable，不能自动回退。

同模板 A、B 可分别领导两个根任务；同一根当前只有一个 leader。同一个任务已有 A 的 accepted return 时 B 不可新增第二个“同 Agent 当前返回”，必须显式交接，保留现有 `(issue, dispatcher)` 约束。无需为多会话设计广播或多个共同队长。

### 交接与 stale/revise

失联是路由可用性，不等于权限撤销；A 短暂离线仍拥有任务。需要换 B 时使用专用 handoff 请求，带旧主体、目标 registered UUID、expected leadership/issue revision、原因和幂等请求 ID。

- 有效旧会话可请求交给同模板会话；旧会话不可用时，由具有任务管理与目标调用权限的人类 owner/admin 恢复。跨模板更换还须检查新模板的小队成员资格，普通兄弟会话不得自行接管。
- 锁定领导根、受影响任务、绑定及合同状态，按确定顺序验证版本、权限与目标。事务内更新当前主体与 revision，记录事件、撤销未开始的旧派发、生成新派发义务。消费者在执行前校验该 revision，旧队长后续写入拒绝。
- 相关 Contract 的 basis 引用领导根 revision，交接后自然 stale；有权原请求方 revise 后新会话才 submit/accept。子任务合同中 requester/acceptor 也指向旧会话时同样 stale，但无关合同不批量改版。新 executor/leader 不获得 revise 自己标准的权限。
- 已运行子任务继续保存原交付与回投；不因换队长重跑。已投递或 unknown 信封保留 ID、目标和 receipt；unknown 仍仅核验。已有在途运行须显式取消或收齐结果，不能把“撤销待派发”当作停止执行。
- 交接列出需接收的已有结果和对应 return ID。通过现有显式 result takeover 机制向 B 建新通知，不修改旧返回、不自动继承 B 的其他上游链；未解决的 unknown 状态同时展示，不能宣称旧投递已撤回。
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
| `issue create/assign` | 继续使用 `--recipient-session-id`；小队发起者会话来自鉴权。人类无会话入口必须明确选外部默认队长会话；赋值与绑定原子化 |
| 交接 CLI/API | 增加任务主体 handoff 操作；不把普通 bind 或 assign 当作隐式 takeover。请求含 revision 与 idempotency key，响应含受影响合同和待回收通知 |
| 注册 API | 复用 external 注册入口，新增显式模板模式/能力版本；返回原 Agent shape 加本次 registered session。旧客户端不能在新模板上签发宽泛 token |
| Agent / sessions API | 保留原字段及单数 `external_session` 的兼容展示投影，新增模板标识、session count 与分页会话列表；所有执行入口仅使用显式会话字段 |
| Issue / runs / Contract API | additive 返回 execution session、leadership root、leader 主体与 revision；`executor_representative` 仍保留 type/id，追加会话字段；`accepted_returns` 保持完整冻结快照，缺失不解释为 empty |
| Mailbox / delegation | 目标来自任务主体或明确参与者绑定，入箱后不重解析。结果回投仍使用接受时固定的关系，不读取当前 coordinator/leader。list、ack、activity 增加会话与 revision 校验 |
| squad briefing | 注入“本任务队长是谁、会话、根任务、revision、默认队长仅供新入口”；managed claim 与 external 信封使用同一解析器，客串与 owning 权限仍分开 |
| Web / Desktop | Agent 列表显示模板，展开分页会话与在线状态。任务卡为 `模板 › 会话标题`，小队卡附队长。失联给“交接”操作，选择器不能选无权限/已吊销会话，长标题可截断但保留可访问完整名称 |
| Mobile | 独立数据层与 UI 同步会话选择/不可执行提示；支持前不开放新模板执行变更，仍可读历史，不依赖 web 组件 |

API 响应用共享 zod schema + `parseWithFallback`，移动端使用自己的解析器；缺失新字段显示未知，未知 enum 有 fallback。旧桌面仍可读旧字段，危险写入由服务端能力校验拒绝并给升级提示，不能依赖旧 UI 隐藏按钮。兼容仅在 API 响应与明确 legacy 数据边界；不增加内部双写、按最新会话猜测或长期双调度路径。

并发分两层：managed 的 Agent 级 `max_concurrent_tasks` 原样保留；external 的工作投递按 registered session 串行，Paseo v1 每会话最多一个活动宿主 turn，busy 后原 ID 重试。同模板 A 忙不阻塞 B，不把模板的历史 `max_concurrent_tasks=1` 套到所有会话。活跃任务列表来自任务绑定/运行投影，不拿未验收任务数占用会话运行槽。全局 daemon 工作池容量仍可能限制吞吐，不承诺物理资源无限并行。

## 历史、上线与回滚

建议维护窗口切换新写入，不实施未授权的生产迁移。历史 Agent、session、任务、合同、Mailbox、return 与 wait 记录原样保留。

1. 预检并演练：枚举旧 Agent 的在途任务、未验收合同、pending/leased/unknown 消息、未处理结果、accepted returns、wait 和小队默认队长引用。输出可归档/不可归档及原因；不以 Agent offline 或单个 issue done 判定安全。
2. 部署 additive schema 与兼容读取，保持模板写入关闭。创建索引失败或条件对象缺失时阻断开关，修复对象后重建无效索引，核对迁移记录与实际结构一致。
3. 升级 server、CLI、插件、daemon 和写入 UI 的能力协议，先启用会话凭据验证；在测试 workspace 用两个新会话验证隔离与回投。旧版本写入新模板一律拒绝。
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
| 网页/自动化无发起会话 | managed 默认正常；external 缺显式会话停放，单在线会话也不偷选 | API + CLI |
| 同根同时认领、分配与 handoff 竞态、handoff 与 submit/claim 竞态 | 单一胜者；旧 revision 拒绝；旧 pending 派发撤销不漏事务 | DB 并发测试 |
| 默认 leader 改动、成员移除、leader 失联 | 已绑定任务不漂移；失效可见；只有显式交接改变主体 | 领导解析 + Contract |
| 父/子合同、换会话后 revise、同模板异会话自验 | 正确 stale、独立验收；旧交付/作者/接受历史不改写，无自验逃逸 | Contract 状态矩阵 |
| 投递 busy、ack 丢失、unknown、会话删除、结果 takeover、wait 完成 | 原信封/receipt/固定 return 不变；既有结果保留，显式接收不重跑 | delivery + daemon fake host |
| 旧 Agent/旧 token/旧 Contract 与新模板并存，归档竞态，开关回退 | legacy 不冒充模板；在途引用阻止归档；新写后禁止不安全降级 | 迁移演练 + API |
| 旧桌面响应缺字段/未知 enum；新列表与选择器；移动端未升级 | 可读、写入安全拒绝、无“最新会话” fallback；键盘与长标题可用 | schema + 组件/E2E |

实施按三个可审查切片推进：模板注册及会话凭据隔离；任务绑定/领导解析及 Contract、Mailbox、wait 全调用链；UI/迁移工具和维护窗口演练。第一个切片验证后再开放模板合并写入，避免先共享身份后补鉴权。实现阶段分别跑针对性 Go/DB、CLI/plugin 测试，共享前端检查及必要 E2E；移动端按其独立 AGENTS.md 验证。

设计阶段已定方向没有发现必须推翻的技术障碍。以下边界建议随本文审阅定稿，不代表已获生产执行授权：

- host 的粒度采用 provider 实例 ID 而非物理机名；同一物理机重装后算新模板。若产品需要跨重装连续身份，应另行设计可信主机登记，不用 hostname 合并。
- 不把已登记的旧宿主会话原地搬到模板；旧会话完成责任后用新宿主会话接续。若必须保留同一宿主上下文原地换身份，需要独立的一次性迁移协议，当前不做。
- 默认外部队长入口必须显式配置会话；不提供“最近在线”自动回退。该约束是 WORK-183 显式接收规则的延续。

不做：生产部署/合入、自动归档或删除旧 Agent、改写历史作者、模板之间权限合并、多队长广播、通用消息代理替换、宿主内部子 Agent 管理、跨 provider 自动迁会话、运行中命令搬迁、扩大 merge/deploy 授权、基于任意新会话的自动故障恢复。
