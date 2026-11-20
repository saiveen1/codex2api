export type IntelligenceStatus =
  "queued" | "running" | "normal" | "degraded" | "error" | "interrupted";
export type IntelligenceAction = "none" | "pause" | "switch_group" | "pause_and_switch_group";
export const intelligenceActionOptions: Record<IntelligenceAction, string> = {
  none: "不自动处置", pause: "禁用业务调度", switch_group: "转移分组", pause_and_switch_group: "禁用业务调度并转移分组",
};
export const intelligenceActionMovesGroups = (action?: string) => action === "switch_group" || action === "pause_and_switch_group";
export interface IntelligenceQuestion {
  id: string;
  name: string;
  prompt: string;
  expected_answer: string;
  match_mode: "exact" | "contains" | "regex";
  max_duration_ms: number;
  enabled: boolean;
}
export type IntelligenceChannel = "codex" | "claude" | "grok" | "antigravity" | "kiro";
export const intelligenceChannels: Record<IntelligenceChannel, string> = {
  codex: "GPT / Codex", claude: "Claude", grok: "Grok", antigravity: "Antigravity", kiro: "Kiro",
};
export interface IntelligenceRule {
  id: string;
  name: string;
  channel: IntelligenceChannel;
  automatic: boolean;
  group_id?: number;
  plan?: string;
  model?: string;
  reasoning_effort?: string;
  judge_mode?: IntelligenceSettings["judge_mode"];
  interval_seconds?: number;
  retry_seconds?: number;
  timeout_seconds?: number;
  degrade_threshold?: number;
  recover_threshold?: number;
  action?: IntelligenceAction;
  target_group_id?: number;
  question_ids?: string[];
}
export interface IntelligenceGroup { id: number; name: string; channel?: string }
interface IntelligencePolicySnapshot {
  groups?: { id: number; name: string }[];
  plan?: string;
  rule_id?: string;
  rule_name?: string;
}
export function intelligenceGroupNames(groups: IntelligencePolicySnapshot["groups"]) {
  return groups == null ? "未记录" : groups.length ? groups.map(g => g.name || `#${g.id}`).join("、") : "无分组";
}
export function intelligencePolicyName(snapshot: IntelligencePolicySnapshot) {
  return snapshot.rule_name || snapshot.rule_id || (snapshot.groups != null ? "全局配置" : "未记录");
}
export const intelligenceRuleNumbers = [
  ["interval_seconds", "正常间隔（秒）", 86400],
  ["retry_seconds", "异常重试间隔（秒）", 86400],
  ["timeout_seconds", "请求超时（秒）", 3600],
  ["degrade_threshold", "连续未通过次数", 1000],
  ["recover_threshold", "连续恢复次数", 1000],
] as const;

export function intelligenceRuleError(rule: IntelligenceRule, questions: IntelligenceQuestion[], groups: IntelligenceGroup[]) {
  if (!rule.name.trim()) return "请输入规则名称。";
  if (!Object.prototype.hasOwnProperty.call(intelligenceChannels, rule.channel)) return "请选择账号来源。";
  if (!rule.group_id && !rule.plan?.trim()) return "请填写分组或套餐码，至少一项。";
  const sameChannel = (id: number) => groups.some(g => g.id === id && (g.channel || "codex") === rule.channel);
  if (rule.group_id && !sameChannel(rule.group_id)) return "请选择同来源的匹配分组。";
  if (intelligenceActionMovesGroups(rule.action) && !rule.target_group_id) return "转移分组需要选择目标分组。";
  if (rule.target_group_id && !sameChannel(rule.target_group_id)) return "请选择同来源的目标分组。";
  for (const [key, label, max] of intelligenceRuleNumbers) {
    const value = rule[key];
    if (value != null && (!Number.isInteger(value) || value < 1 || value > max)) return `${label}须为 1–${max} 的整数，或留空继承。`;
  }
  if (rule.question_ids?.length) {
    if (rule.question_ids.some(id => !questions.some(q => q.id === id))) return "指定题目包含已删除题目，请重新选择。";
    if (!questions.some(q => q.enabled && rule.question_ids?.includes(q.id))) return "指定题目中至少启用一道。";
  }
  return "";
}

export function normalizeIntelligenceRule(rule: IntelligenceRule) {
  return { ...rule, name: rule.name.trim(), plan: rule.plan?.trim().toLowerCase() || undefined,
    model: rule.model?.trim() || undefined };
}

export interface IntelligenceSettings {
  channels?: Partial<Record<IntelligenceChannel, boolean>>;
  grok_model?: string;
  antigravity_model?: string;
  kiro_model?: string;
  adaptive_enabled: boolean;
  pause_initial_seconds: number;
  pause_max_seconds: number;
  recovery_initial_seconds: number;
  recovery_min_seconds: number;
  revision: number;
  enabled: boolean;
  gpt_model: string;
  claude_model: string;
  reasoning_effort: string;
  judge_mode: "answer" | "latency" | "both";
  interval_seconds: number;
  retry_seconds: number;
  timeout_seconds: number;
  concurrency: number;
  retention: number;
  new_accounts_enabled: boolean;
  action: IntelligenceAction;
  degrade_threshold: number;
  recover_threshold: number;
  target_group_id: number;
  questions: IntelligenceQuestion[];
  rules?: IntelligenceRule[];
  rules_only?: boolean;
}
export interface IntelligenceTargetConflict {
  code: "intelligence_check_target_conflict";
  target_group_id: number;
  target_group_channel: string;
  conflict_count: number;
  conflicts: { account_id: number; account_name: string; channel: string }[];
}

export function intelligenceTargetConflict(error: unknown): IntelligenceTargetConflict | null {
  if (!error || typeof error !== "object" || !("details" in error)) return null;
  const details = error.details as Partial<IntelligenceTargetConflict> | null;
  if (!details || details.code !== "intelligence_check_target_conflict" ||
    !Number.isSafeInteger(details.target_group_id) ||
    typeof details.target_group_channel !== "string" ||
    !Number.isSafeInteger(details.conflict_count) || (details.conflict_count ?? 0) <= 0 ||
    !Array.isArray(details.conflicts) || details.conflicts.length === 0 || details.conflicts.length > 200 ||
    details.conflicts.some(a => !a || !Number.isSafeInteger(a.account_id) || a.account_id <= 0 || typeof a.account_name !== "string" || typeof a.channel !== "string")) return null;
  return details as IntelligenceTargetConflict;
}

export interface IntelligenceRecord {
  id: number;
  status: IntelligenceStatus;
  question_name?: string;
  duration_ms?: number;
  created_at?: string;
  completed_at?: string | null;
}
export interface IntelligenceRun extends IntelligenceRecord, IntelligencePolicySnapshot {
  record_only?: boolean;
  account_id: number;
  account_name: string;
  channel: string;
  source: "manual" | "automatic";
  config_revision: number;
  answer_passed?: boolean;
  latency_passed?: boolean;
  model: string;
  response_model?: string;
  reasoning_effort: string;
  judge_mode: IntelligenceSettings["judge_mode"];
  question: IntelligenceQuestion;
  output: string;
  error: string;
  input_tokens: number;
  output_tokens: number;
  reasoning_tokens: number;
  started_at?: string | null;
  action: string;
  action_message?: string;
  effective_context?: Record<string, unknown>;
}
export type IntelligenceRunSummary = Omit<IntelligenceRun, "question" | "output" | "error" | "effective_context"> & {
  question?: Omit<IntelligenceQuestion, "prompt" | "expected_answer">;
};
export interface IntelligenceAccount extends IntelligencePolicySnapshot {
  automatic_effective?: boolean;
  account_id: number;
  account_name: string;
  channel: string;
  enabled: boolean;
  latest_status: IntelligenceStatus | "";
  config_error?: string;
  failed_streak: number;
  passed_streak: number;
  action: IntelligenceAction;
  next_due_at?: string | null;
}
export interface IntelligenceBar {
  probe_phase?: string;
  next_interval_seconds?: number;
  next_due_at?: string | null;
  passed_streak?: number;
  active_run_id?: number;
  active_status?: "queued" | "running";
  enabled: boolean;
  action: IntelligenceAction;
  records: IntelligenceRecord[];
}
export interface IntelligencePage<T> {
  items: T[];
  total: number;
  page: number;
  page_size: number;
}
export const intelligenceLabels: Record<string, string> = {
  queued: "等待执行",
  running: "检测中",
  normal: "正常",
  degraded: "未通过",
  error: "上游错误",
  interrupted: "已中断",
  resume: "恢复调度",
  restore_groups: "还原分组",
  resume_and_restore_groups: "恢复调度并还原分组",
  none: "无处置",
  pause: "检测暂停调度",
  switch_group: "检测转移分组",
  pause_and_switch_group: "检测禁用调度并转移分组",
};
export function intelligenceColor(status: string): string | undefined {
  return (
    { normal: "#22c55e", degraded: "#ef4444", error: "#991b1b" } as Record<
      string,
      string
    >
  )[status];
}
export function intelligenceWindow<
  T extends { id: number; status: string; created_at?: string },
>(records: T[]) {
  const recent = records
    .filter((r) => !["queued", "running"].includes(r.status))
    .slice()
    .sort(
      (a, b) =>
        (a.created_at ?? "").localeCompare(b.created_at ?? "") || a.id - b.id,
    )
    .slice(-20);
  const normal = recent.filter((r) => r.status === "normal").length;
  const degraded = recent.filter((r) => r.status === "degraded").length;
  return {
    records: recent,
    rate: normal + degraded ? (normal / (normal + degraded)) * 100 : null,
    errors: recent.filter((r) => r.status === "error").length,
  };
}
export function intelligenceQuery(
  page = 1,
  filters: Record<string, string | number | undefined> = {},
) {
  const params = new URLSearchParams({ page: String(page), page_size: "20" });
  for (const [key, value] of Object.entries(filters))
    if (value !== undefined && value !== "") params.set(key, String(value));
  return params.toString();
}

export function intelligenceTimeRange(from: string, before: string) {
  const start = from ? new Date(from) : undefined;
  const end = before ? new Date(before) : undefined;
  if ((start && !Number.isFinite(start.getTime())) || (end && !Number.isFinite(end.getTime())) || (start && end && start >= end)) return null;
  return { from: start?.toISOString(), before: end?.toISOString() };
}
export function latestRequestGuard() {
  let revision = 0;
  return {
    next: () => ++revision,
    isCurrent: (value: number) => value === revision,
  };
}

export function intelligenceScheduleLabel(bar: IntelligenceBar): string {
  const labels: Record<string, string> = {
    normal: '正常检测', backoff: '退避中', recovering: '恢复确认',
    retrying: '异常重试', credential_blocked: '等待凭证修复',
    quota_wait: '等待额度恢复',
  };
  const label = labels[bar.probe_phase || ''];
  if (!label) return '';
  if (bar.probe_phase === 'credential_blocked') return label;
  const progress = bar.probe_phase === 'recovering' ? ` · 连续通过 ${bar.passed_streak ?? 0} 次` : '';
  const due = bar.enabled && bar.next_due_at ? ` · 下次 ${new Date(bar.next_due_at).toLocaleString()}` : '';
  return label + progress + due;
}
