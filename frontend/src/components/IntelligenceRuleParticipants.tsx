import { useEffect, useState } from "react";
import { RefreshCw } from "lucide-react";
import { api } from "../api";
import { intelligenceGroupNames, type IntelligenceRuleParticipant, type IntelligencePage, type IntelligenceGroup } from "../lib/intelligenceCheck";
import { getErrorMessage } from "../utils/error";
import { Button } from "./ui/button";

const states: Record<IntelligenceRuleParticipant["state"], string> = {
  due: "当前可排队", scheduled: "尚未到期", queued: "已排队", running: "检测中",
  skipped: "无匹配模型，已跳过", quota_wait: "等待额度恢复", not_dispatchable: "不可调度，暂缓", config_error: "目标分组不可用",
};

export default function IntelligenceRuleParticipants({ ruleID, groups, dirty }: {
  ruleID: string; groups: IntelligenceGroup[]; dirty: boolean;
}) {
  const [page, setPage] = useState(1);
  const [refresh, setRefresh] = useState(0);
  const [result, setResult] = useState<(IntelligencePage<IntelligenceRuleParticipant> & { checked_at: string }) | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    void api.getIntelligenceRuleParticipants(ruleID, page, controller.signal).then(data => {
      if (controller.signal.aborted) return;
      const lastPage = Math.max(1, Math.ceil(data.total / data.page_size));
      if (page > lastPage) setPage(lastPage);
      else setResult(data);
    }).catch(err => {
      if (!controller.signal.aborted) setError(getErrorMessage(err));
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [ruleID, page, refresh]);

  const goTo = (next: number) => { setResult(null); setPage(next); };
  const pages = result ? Math.max(1, Math.ceil(result.total / result.page_size)) : 1;
  return <section aria-label="下轮参与账号名单" className="min-w-0 space-y-3 border-t border-border pt-3" aria-busy={loading}>
    <div className="flex flex-wrap items-center justify-between gap-2">
      <p className="text-sm font-medium" role="status">{loading ? "正在读取名单…" : result && !error ? `${result.total} 个账号 · ${new Date(result.checked_at).toLocaleTimeString()} 更新` : "下轮参与账号"}</p>
      <Button variant="outline" size="sm" disabled={loading} onClick={() => { goTo(1); setRefresh(n => n + 1); }}><RefreshCw className="size-3.5" />刷新名单</Button>
    </div>
    <p className="text-xs text-muted-foreground">{dirty ? "有未保存改动，以下按已保存规则显示。" : "以下按已保存规则显示。"}检测按到期时间滚动排队，发送前还会复核额度和账号状态。</p>
    {error && <p role="alert" className="break-words text-sm text-destructive">名单读取失败：{error}</p>}
    {!loading && !error && result && result.items.length === 0 && <p className="text-sm text-muted-foreground">当前没有符合自动检测条件的账号。请检查检测开关、规则顺序和账号范围。</p>}
    {!error && result && result.items.length > 0 && <ul className="divide-y divide-border rounded-md border px-3">
      {result.items.map(row => <li key={row.account_id} className="grid min-w-0 gap-2 py-2.5 text-sm sm:grid-cols-[minmax(0,1fr)_auto]">
        <div className="min-w-0">
          <p className="flex flex-wrap items-baseline gap-x-2 gap-y-1"><span className="min-w-0 break-all font-medium">{row.account_name || `账号 #${row.account_id}`}</span><span className="text-xs tabular-nums text-muted-foreground">#{row.account_id}</span>{row.recovery && <span className="rounded bg-primary/10 px-1.5 py-0.5 text-xs font-medium text-primary">恢复复测</span>}</p>
          <p className="mt-1 break-words text-xs text-muted-foreground">当前分组：{intelligenceGroupNames(row.groups)}{row.original_group_ids?.length ? ` · 按原分组匹配：${row.original_group_ids.map(id => groups.find(g => g.id === id)?.name || `#${id}`).join("、")}` : ""}</p>
        </div>
        <div className="text-xs sm:text-right">
          <p className={row.state === "due" ? "font-medium text-primary" : "font-medium"}>{states[row.state]}</p>
          {row.state === "scheduled" && row.next_due_at && <p className="mt-1 tabular-nums text-muted-foreground">{new Date(row.next_due_at).toLocaleString()}</p>}
          {row.state === "quota_wait" && row.available_after && <p className="mt-1 tabular-nums text-muted-foreground">预计 {new Date(row.available_after).toLocaleString()}</p>}
        </div>
      </li>)}
    </ul>}
    {!error && result && pages > 1 && <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
      <Button size="sm" variant="outline" disabled={loading || page <= 1} onClick={() => goTo(page - 1)}>上一页</Button><span>第 {page} / {pages} 页</span><Button size="sm" variant="outline" disabled={loading || page >= pages} onClick={() => goTo(page + 1)}>下一页</Button>
    </div>}
  </section>;
}
