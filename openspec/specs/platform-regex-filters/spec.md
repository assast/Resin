## Requirements

### Requirement: 正则过滤支持 ! 前缀排除

系统在平台 `regex_filters` 中 SHALL 将以 `!` 开头的条目解释为排除规则：剥离行首 `!` 后对剩余字符串编译为正则；不以 `!` 开头的条目为正选规则。

#### Scenario: 正选与排除混用

- **WHEN** 平台配置 `regex_filters` 为 `["^Provider/.*", "!.*试用.*"]`
- **THEN** 仅当节点存在 candidate tag 匹配 `^Provider/.*`，且该节点所有 candidate tag 均不匹配 `.*试用.*` 时，该节点通过正则过滤阶段

#### Scenario: 仅配置排除规则

- **WHEN** 平台配置 `regex_filters` 为 `["!.*过期.*"]` 且无正选规则
- **THEN** 除 candidate tag 命中 `.*过期.*` 的节点外，其余具备启用订阅的节点通过正则过滤阶段

#### Scenario: 行首 ! 不参与正则编译

- **WHEN** 系统编译条目 `"!foo.*"`
- **THEN** 实际编译的正则模式为 `foo.*`，且该条目被标记为排除规则

### Requirement: 排除为节点级硬排除

当任一启用订阅下的 candidate tag（格式 `<订阅名>/<tag>`）匹配任一排除正则时，系统 SHALL 判定该节点未通过正则过滤，不得因另一 tag 满足正选而放行。

#### Scenario: 多 tag 节点命中排除

- **WHEN** 节点同时拥有 candidate `SubA/正常` 与 `SubB/试用-01`，正选为空，排除为 `.*试用.*`
- **THEN** 该节点未通过正则过滤

#### Scenario: 排除未命中时正选 AND 仍生效

- **WHEN** 正选为 `["^SubA/.*", ".*香港.*"]`，排除为 `["!.*试用.*"]`，节点唯一 candidate 为 `SubA/香港-01`
- **THEN** 该节点通过正则过滤

#### Scenario: 正选未同时满足

- **WHEN** 正选为 `["^SubA/.*", ".*香港.*"]`，节点唯一 candidate 为 `SubA/日本-01`
- **THEN** 该节点未通过正则过滤

### Requirement: 正则过滤编译与校验

系统在创建/更新平台以及从持久化加载平台时，SHALL 校验每条 `regex_filters`：剥离可选的行首 `!` 后模式非空且可被正则引擎编译；失败时返回带条目下标的明确错误。

#### Scenario: 非法正则被拒绝

- **WHEN** 客户端提交 `regex_filters` 含 `"(broken"`
- **THEN** 请求失败，错误信息指出对应 `regex_filters` 下标与无效正则原因

#### Scenario: 单独的感叹号被拒绝

- **WHEN** 客户端提交 `regex_filters` 含 `"!"`
- **THEN** 请求失败，错误信息指出该排除规则在剥离 `!` 后模式为空

#### Scenario: 空列表保持兼容

- **WHEN** 平台 `regex_filters` 为空列表
- **THEN** 正则过滤阶段不因正选/排除失败而拦截节点（仍受启用订阅等既有条件约束）

### Requirement: 配置界面提供规则说明与样例

WebUI 在创建平台与编辑平台的「节点名正则过滤规则」表单中，SHALL 展示匹配对象说明、正选 AND 语义、`!` 排除语义，并提供可照抄的配置样例（placeholder 与/或底注）。

#### Scenario: 编辑页可见排除说明

- **WHEN** 用户打开平台详情配置页的正则过滤输入框区域
- **THEN** 界面说明包含以 `!` 开头可排除节点名，并给出至少一条排除样例

#### Scenario: 创建页与编辑页说明一致

- **WHEN** 用户分别查看新建平台弹窗与平台详情配置中的正则过滤字段
- **THEN** 两处均传达正选 AND、`!` 排除、以及 `<订阅名>/.*` 类用法，不出现互相矛盾的文案
