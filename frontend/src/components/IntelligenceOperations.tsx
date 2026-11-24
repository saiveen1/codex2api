import { useEffect, useMemo, useState, type ReactNode } from "react";
import { api } from "../api";
import { refreshIntelligenceBars } from "../hooks/useIntelligenceBars";
import { useToast } from "../hooks/useToast";
import {
  intelligenceChannels, intelligenceColor, intelligenceGroupNames, intelligenceLabels,
  intelligencePolicyName, intelligenceTimeRange, type IntelligenceGroup, type IntelligencePage,
} from "../lib/intelligenceCheck";
import { getErrorMessage } from "../utils/error";
import { Button } from "./ui/button";
import { Card, CardContent } from "./ui/card";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "./ui/dialog";
import { Input } from "./ui/input";
import { Select } from "./ui/select";

type Filters = Record<string, string | number | undefined>;
const emptyFilters = { search: "", channel: "", group_id: "", plan: "" };
const emptyHistoryFilters = { ...emptyFilters, status: "", source: "", from: "", before: "", record_only: "" };
const selectionLimit = 200;
const pageSizeLimit = 100;
const options = (values: Record<string, string>) => Object.entries(values).map(([value, label]) => ({ value, label }));
const statuses = ["queued", "running", "normal", "degraded", "error", "interrupted", "skipped"];
const { getIntelligenceRunSummaries, getIntelligenceAccounts } = api;

function usePage<T>(load: (page: number, filters: Filters, signal: AbortSignal) => Promise<IntelligencePage<T>>, page: number, filters: Filters | null) {
  const [revision, setRevision] = useState(0);
  const [result, setResult] = useState<{ data?: IntelligencePage<T>; error?: string; page: number; filters: Filters; revision: number }>();
  useEffect(() => {
    if (!filters) return;
    const controller = new AbortController();
    const request = { page, filters, revision };
    void load(page, filters, controller.signal).then(
      data => { if (!controller.signal.aborted) setResult({ ...request, data }); },
      error => { if (!controller.signal.aborted) setResult({ ...request, error: getErrorMessage(error) }); },
    );
    return () => controller.abort();
  }, [load, page, filters, revision]);
  const current = result?.page === page && result.filters === filters && result.revision === revision ? result : undefined;
  return { ...current, refresh: () => setRevision(n => n + 1) };
}

function FilterSelect({ label, value, onChange, choices, disabled }: {
  label: string; value: string; onChange: (value: string) => void;
  choices: { label: string; value: string }[]; disabled?: boolean;
}) {
  return <div className="flex min-w-0 flex-col gap-2 text-sm">
    <span className="font-medium">{label}</span>
    <Select aria-label={label} value={value} onValueChange={onChange} disabled={disabled}
      options={[{ value: "", label: "全部" }, ...choices]} />
  </div>;
}

function AccountFilters({ filters, groups, disabled, onChange }: {
  filters: typeof emptyFilters; groups: IntelligenceGroup[]; disabled?: boolean;
  onChange: (patch: Partial<typeof emptyFilters>) => void;
}) {
  return <>
    <label className="flex min-w-0 flex-col gap-2 text-sm font-medium">账号搜索
      <Input value={filters.search} onChange={e => onChange({ search: e.target.value })} placeholder="搜索账号" disabled={disabled} />
    </label>
    <FilterSelect label="账号来源" value={filters.channel} onChange={channel => onChange({ channel })} choices={options(intelligenceChannels)} disabled={disabled} />
    <FilterSelect label="分组" value={filters.group_id} onChange={group_id => onChange({ group_id })}
      choices={groups.map(g => ({ value: String(g.id), label: `${g.name} #${g.id}` }))} disabled={disabled} />
    <label className="flex min-w-0 flex-col gap-2 text-sm font-medium">套餐
      <Input value={filters.plan} onChange={e => onChange({ plan: e.target.value })} placeholder="套餐码，如 plus" disabled={disabled} />
    </label>
  </>;
}

function PageFooter({ data, page, onPage, disabled }: {
  data: IntelligencePage<unknown>; page: number; onPage: (page: number) => void; disabled?: boolean;
}) {
  return <div className="flex flex-wrap items-center justify-between gap-2 border-t pt-4 text-xs text-muted-foreground">
    <span>共 {data.total} 条 · 第 {page} 页</span>
    <div className="flex gap-2">
      <Button size="sm" variant="outline" disabled={disabled || page <= 1} onClick={() => onPage(page - 1)}>上一页</Button>
      <Button size="sm" variant="outline" disabled={disabled || page * data.page_size >= data.total} onClick={() => onPage(page + 1)}>下一页</Button>
    </div>
  </div>;
}

function ResultState({ data, error, empty }: { data?: IntelligencePage<unknown>; error?: string; empty: string }) {
  if (error) return <p role="alert" className="py-6 text-sm text-destructive">{error}</p>;
  if (!data || data.items.length === 0) return <p role="status" className="py-8 text-center text-sm text-muted-foreground">{data ? empty : "正在加载…"}</p>;
  return null;
}

function Status({ status }: { status: string }) {
  return <span className="inline-flex items-center gap-1.5 font-medium">
    <span className="size-2 rounded-full bg-muted" style={{ backgroundColor: intelligenceColor(status) }} />
    {intelligenceLabels[status] || status || "暂无记录"}
  </span>;
}

function Row({ children, action }: { children: ReactNode; action: ReactNode }) {
  return <li className="flex flex-wrap items-start justify-between gap-3 border-b py-3 last:border-b-0">
    <div className="min-w-0 flex-1 basis-52 space-y-1 [overflow-wrap:anywhere]">{children}</div>
    {action}
  </li>;
}

export function IntelligenceHistory({ groups, onRun }: { groups: IntelligenceGroup[]; onRun: (id: number) => void }) {
  const [filters, setFilters] = useState(emptyHistoryFilters);
  const [page, setPage] = useState(1);
  const query = useMemo(() => {
    const range = intelligenceTimeRange(filters.from, filters.before);
    return range && { ...filters, ...range, search: filters.search.trim(), plan: filters.plan.trim().toLowerCase() };
  }, [filters]);
  const { data, error, refresh } = usePage(getIntelligenceRunSummaries, page, query);
  const change = (patch: Partial<typeof filters>) => { setPage(1); setFilters(f => ({ ...f, ...patch })); };
  return <Card><CardContent className="space-y-4 py-4">
    <div className="flex flex-wrap items-center justify-between gap-2">
      <p className="text-sm text-muted-foreground">查看所有账号的检测记录。分组、套餐和规则以检测当时的记录为准。</p>
      <Button size="sm" variant="outline" disabled={!query} onClick={refresh}>刷新记录</Button>
    </div>
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <AccountFilters filters={filters} groups={groups} onChange={change} />
      <FilterSelect label="状态" value={filters.status} onChange={status => change({ status })} choices={statuses.map(value => ({ value, label: intelligenceLabels[value] }))} />
      <FilterSelect label="发起方式" value={filters.source} onChange={source => change({ source })} choices={options({ manual: "手动", automatic: "自动" })} />
      <FilterSelect label="检测类型" value={filters.record_only} onChange={record_only => change({ record_only })} choices={options({ true: "试跑（只记录）", false: "常规检测" })} />
      <label className="flex min-w-0 flex-col gap-2 text-sm font-medium">开始时间（含）
        <Input className="min-w-0" type="datetime-local" value={filters.from} onChange={e => change({ from: e.target.value })} />
      </label>
      <label className="flex min-w-0 flex-col gap-2 text-sm font-medium">结束时间（不含）
        <Input className="min-w-0" type="datetime-local" value={filters.before} onChange={e => change({ before: e.target.value })} />
      </label>
    </div>
    {!query ? <p role="alert" className="text-sm text-destructive">请输入有效时间，开始时间必须早于结束时间。</p> : <ResultState data={data} error={error} empty="暂无符合筛选条件的检测记录。" />}
    {data && <>
      <ul className="divide-border border-y text-sm" aria-label="检测历史列表">
        {data.items.map(run => <Row key={run.id} action={<Button size="sm" variant="outline" onClick={() => onRun(run.id)}>查看详情 #{run.id}</Button>}>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <strong>{run.account_name || "未命名账号"} <span className="font-normal text-muted-foreground">#{run.account_id}</span></strong>
            <Status status={run.status} />
            <span className="text-xs text-muted-foreground">{run.source === "manual" ? "手动" : "自动"}</span>
            {run.record_only && <span className="rounded bg-muted px-1.5 py-0.5 text-xs">试跑（只记录）</span>}
          </div>
          <p className="text-xs text-muted-foreground">{run.channel} · 分组：{intelligenceGroupNames(run.groups)} · 套餐：{run.plan || "未知"} · 规则：{intelligencePolicyName(run)}</p>
          <p className="text-xs text-muted-foreground">{run.created_at ? new Date(run.created_at).toLocaleString() : "未记录时间"}{(run.question_name || run.question?.name) && ` · ${run.question_name || run.question?.name}`}{run.duration_ms != null ? ` · ${(run.duration_ms / 1000).toFixed(2)} 秒` : ""}</p>
        </Row>)}
      </ul>
      <PageFooter data={data} page={page} onPage={setPage} />
    </>}
  </CardContent></Card>;
}

const actions = { enable: "开启自动检测", disable: "关闭自动检测", trial: "立即检测（只记录）" };
type Action = keyof typeof actions;

export function IntelligenceBatch({ groups }: { groups: IntelligenceGroup[] }) {
  const [filters, setFilters] = useState(emptyFilters);
  const [page, setPage] = useState(1);
  const [selected, setSelected] = useState<number[]>([]);
  const [loadingSelection, setLoadingSelection] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [confirmation, setConfirmation] = useState<{ action: Action; ids: number[] }>();
  const [feedback, setFeedback] = useState("");
  const { showToast } = useToast();
  const query = useMemo(() => ({ ...filters, search: filters.search.trim(), plan: filters.plan.trim().toLowerCase() }), [filters]);
  const { data, error, refresh } = usePage(getIntelligenceAccounts, page, query);
  const locked = loadingSelection || submitting || !!confirmation;
  const changeFilters = (patch: Partial<typeof filters>) => {
    setSelected([]); setFeedback(""); setPage(1); setFilters(f => ({ ...f, ...patch }));
  };
  const rowIDs = data?.items.map(a => a.account_id) ?? [];
  const pageSelected = rowIDs.length > 0 && rowIDs.every(id => selected.includes(id));
  const setIDs = (ids: number[], check: boolean) => {
    if (check) {
      const next = [...new Set([...selected, ...ids])];
      if (next.length > selectionLimit) { showToast(`每次最多选择 ${selectionLimit} 个账号`, "error"); return; }
      setSelected(next);
    } else setSelected(selected.filter(id => !ids.includes(id)));
  };
  const selectFiltered = async () => {
    if (locked || !data?.total || data.total > selectionLimit) return;
    setLoadingSelection(true);
    try {
      const first = await api.getIntelligenceAccounts(1, { ...query, page_size: pageSizeLimit });
      if (first.total > selectionLimit) throw new Error(`筛选结果超过 ${selectionLimit} 个账号，请缩小范围。`);
      const ids = first.items.map(a => a.account_id);
      if (first.total > pageSizeLimit) {
        const second = await api.getIntelligenceAccounts(2, { ...query, page_size: pageSizeLimit });
        if (second.total !== first.total) throw new Error("筛选结果已变化，请刷新后重试。");
        ids.push(...second.items.map(a => a.account_id));
      }
      if (first.total !== data.total || new Set(ids).size !== first.total || ids.length !== first.total) throw new Error("筛选结果已变化，请刷新后重试。");
      setSelected(ids);
    } catch (e) { showToast(getErrorMessage(e), "error"); }
    finally { setLoadingSelection(false); }
  };
  const submit = async () => {
    if (!confirmation || submitting) return;
    const { action, ids } = confirmation;
    setSubmitting(true);
    setFeedback("");
    try {
      let message: string;
      if (action === "trial") {
        const result = await api.createIntelligenceRuns(ids, true);
        message = `试跑：已创建 ${result.runs.length} 条检测，跳过 ${result.skipped_account_ids?.length ?? 0} 个账号。`;
      } else {
        const result = await api.setIntelligenceAccounts(ids, action === "enable");
        message = `${actions[action]}：已更新 ${result.updated} 个账号。`;
      }
      setFeedback(message);
      showToast(message);
      setSelected([]);
      refreshIntelligenceBars();
      refresh();
    } catch (e) { showToast(getErrorMessage(e), "error"); }
    finally { setSubmitting(false); setConfirmation(undefined); }
  };
  return <Card><CardContent className="space-y-4 py-4">
    <p className="text-sm text-muted-foreground">批量立即检测只记录结果，不触发处置；有活动检测的账号会跳过。开关仅修改账号的自动检测设置，仍受总开关、来源开关与匹配规则约束。</p>
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <AccountFilters filters={filters} groups={groups} disabled={locked} onChange={changeFilters} />
    </div>
    <div className="flex flex-wrap items-center gap-2 border-y py-3">
      <span className="mr-2 text-sm font-medium" role="status">已选 {selected.length} 个账号</span>
      <Button size="sm" variant="outline" disabled={locked || !rowIDs.length} onClick={() => setIDs(rowIDs, !pageSelected)}>{pageSelected ? "取消本页选择" : "选择本页"}</Button>
      <Button size="sm" variant="outline" disabled={locked || !data?.total || data.total > selectionLimit} onClick={() => void selectFiltered()}>{loadingSelection ? "正在选择…" : "选择全部筛选结果（最多 200 个）"}</Button>
      <Button size="sm" variant="ghost" disabled={locked || !selected.length} onClick={() => setSelected([])}>清空选择</Button>
      <Button size="sm" variant="outline" disabled={locked} onClick={refresh}>刷新账号</Button>
      {data && data.total > selectionLimit && <p className="w-full text-xs text-muted-foreground">筛选结果超过 200 个，可缩小范围或逐页选择。</p>}
    </div>
    <div className="flex flex-wrap gap-2">
      {(Object.keys(actions) as Action[]).map(action => <Button key={action} size="sm" variant={action === "trial" ? "default" : "outline"}
        disabled={locked || selected.length === 0} onClick={() => setConfirmation({ action, ids: [...selected] })}>{actions[action]}</Button>)}
    </div>
    {feedback && <p className="text-sm" role="status">{feedback}</p>}
    <ResultState data={data} error={error} empty="暂无符合筛选条件的账号。" />
    {data && <>
      <ul className="text-sm" aria-label="批量账号列表">
        {data.items.map(account => <Row key={account.account_id} action={<Status status={account.latest_status} />}>
          <label className="flex items-start gap-2 font-medium">
            <input type="checkbox" className="mt-1 size-4 shrink-0 accent-primary" checked={selected.includes(account.account_id)}
              disabled={locked} onChange={e => setIDs([account.account_id], e.target.checked)} />
            <span>{account.account_name || "未命名账号"} <span className="font-normal text-muted-foreground">#{account.account_id}</span></span>
          </label>
          <p className="text-xs text-muted-foreground">{account.channel} · 分组：{intelligenceGroupNames(account.groups)} · 套餐：{account.plan || "未知"} · 规则：{intelligencePolicyName(account)}</p>
          <p className="text-xs text-muted-foreground">账号开关：{account.enabled ? "开" : "关"}{account.automatic_effective !== undefined && ` · 实际自动检测：${account.automatic_effective ? "开" : "关"}`}{account.config_error ? ` · ${account.config_error}` : ""}</p>
        </Row>)}
      </ul>
      <PageFooter data={data} page={page} onPage={setPage} disabled={locked} />
    </>}
    <Dialog open={!!confirmation} onOpenChange={open => { if (!open && !submitting) setConfirmation(undefined); }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>确认{confirmation && actions[confirmation.action]}</DialogTitle>
          <DialogDescription>
            将操作已选定的 {confirmation?.ids.length ?? 0} 个账号。{confirmation?.action === "trial" ? "本次试跑不会执行处置；已有活动检测的账号将跳过。" : "仅修改账号自动检测开关，不变更账号当前业务调度。"}
          </DialogDescription>
        </DialogHeader>
        <details className="text-sm text-muted-foreground">
          <summary className="cursor-pointer">查看本次操作的账号 ID</summary>
          <p className="mt-1 max-h-32 overflow-y-auto break-words">{confirmation?.ids.map(id => `#${id}`).join("、")}</p>
        </details>
        <div className="flex justify-end gap-2">
          <Button variant="outline" disabled={submitting} onClick={() => setConfirmation(undefined)}>取消</Button>
          <Button disabled={submitting} onClick={() => void submit()}>{submitting ? "正在提交…" : "确认操作"}</Button>
        </div>
      </DialogContent>
    </Dialog>
  </CardContent></Card>;
}
