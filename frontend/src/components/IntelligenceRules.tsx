import { useState, type ReactNode } from "react";
import { ArrowDown, ArrowUp, Plus, Trash2 } from "lucide-react";
import {
  intelligenceActionMovesGroups, intelligenceActionOptions, intelligenceChannels, intelligenceRuleError, intelligenceRuleNumbers,
  type IntelligenceGroup, type IntelligenceRule, type IntelligenceSettings,
} from "../lib/intelligenceCheck";
import { createUUID } from "../lib/uuid";
import { Button } from "./ui/button";
import { Card, CardContent } from "./ui/card";
import { Input } from "./ui/input";
import { Select } from "./ui/select";
import { Switch } from "./ui/switch";

const choices = (values: Record<string, string>) => Object.entries(values).map(([value, label]) => ({ value, label }));
const inherit = { value: "", label: "继承全局" };

function Field({ label, children }: { label: string; children: ReactNode }) {
  return <label className="flex min-w-0 flex-col gap-2 text-sm">
    <span className="font-medium">{label}</span>{children}
  </label>;
}

export default function IntelligenceRules({ settings, groups, onChange, disabled }: {
  settings: IntelligenceSettings; groups: IntelligenceGroup[]; onChange: (rules: IntelligenceRule[]) => void; disabled: boolean;
}) {
  const [editor, setEditor] = useState<string>();
  const rules = settings.rules ?? [];
  const edit = (id: string, patch: Partial<IntelligenceRule>) => onChange(rules.map(r => r.id === id ? { ...r, ...patch } : r));
  const move = (index: number, direction: -1 | 1) => {
    const next = [...rules];
    [next[index], next[index + direction]] = [next[index + direction], next[index]];
    onChange(next);
  };
  return <section aria-labelledby="intelligence-rules-title" className="min-w-0 space-y-4">
    <h2 id="intelligence-rules-title" className="text-lg font-semibold">分组 / 套餐规则</h2>
    <div className="flex flex-wrap items-start justify-between gap-3">
      <p className="max-w-3xl flex-1 basis-64 text-sm text-muted-foreground">
        从上到下采用首条匹配策略；分组和套餐都填写时须同时匹配。未覆盖的参数继承全局。自动检测仍受总开关、来源开关和账号开关限制。
      </p>
      <Button variant="outline" disabled={disabled} onClick={() => {
        const id = createUUID();
        onChange([...rules, { id, name: "新规则", channel: "codex", automatic: true }]);
        setEditor(id);
      }}><Plus className="size-4" />新增规则</Button>
    </div>
    {!rules.length && <p className="text-sm text-muted-foreground">{settings.rules_only ? "暂无策略，不会启动新的自动检测；已处置账号仍可检测恢复。" : "暂无规则，未受其他开关限制的账号使用全局配置。"}</p>}
    <ol className="space-y-3">
      {rules.map((rule, index) => {
        const name = rule.name || "未命名规则";
        const patch = (value: Partial<IntelligenceRule>) => edit(rule.id, value);
        const groupChoices = groups.filter(g => (g.channel || "codex") === rule.channel).map(g => ({ value: String(g.id), label: `${g.name} #${g.id}` }));
        const groupOptions = (id: number | undefined, empty: string) => [
          { value: "", label: empty }, ...groupChoices,
          ...(id && !groupChoices.some(g => g.value === String(id)) ? [{ value: String(id), label: `#${id}（不可用）` }] : []),
        ];
        const error = intelligenceRuleError(rule, settings.questions, groups);
        const questionIDs = rule.question_ids ?? [];
        const missingIDs = questionIDs.filter(id => !settings.questions.some(q => q.id === id));
        return <li key={rule.id}><Card><CardContent className="space-y-3 py-4">
          <div className="flex flex-wrap items-center gap-3">
            <span className="text-xs tabular-nums text-muted-foreground">{index + 1}</span>
            <div className="min-w-0 flex-1 basis-44 [overflow-wrap:anywhere]">
              <h3 className="font-medium">{name}</h3>
              <p className="text-xs text-muted-foreground">{intelligenceChannels[rule.channel]}{rule.group_id ? ` · 分组：${groups.find(g => g.id === rule.group_id)?.name || `#${rule.group_id}`}` : ""}{rule.plan?.trim() ? ` · 套餐：${rule.plan.trim().toLowerCase()}` : ""} · {rule.automatic ? "允许自动检测" : "不参与新自动检测"}</p>
              {rule.automatic && rule.normal_only && <p className="text-xs text-muted-foreground">仅检测正常账号</p>}
              <p className="mt-1 text-xs text-muted-foreground">{rule.action ? intelligenceActionOptions[rule.action] : "处置继承全局"}{rule.target_group_id ? ` → ${groups.find(g => g.id === rule.target_group_id)?.name || `#${rule.target_group_id}`}` : ""}</p>
            </div>
            <div className="flex gap-1">
              <Button variant="ghost" size="icon" aria-label={`上移 ${name}`} disabled={disabled || index === 0} onClick={() => move(index, -1)}><ArrowUp className="size-4" /></Button>
              <Button variant="ghost" size="icon" aria-label={`下移 ${name}`} disabled={disabled || index === rules.length - 1} onClick={() => move(index, 1)}><ArrowDown className="size-4" /></Button>
              <Button variant="outline" size="sm" aria-expanded={editor === rule.id} disabled={disabled} onClick={() => setEditor(editor === rule.id ? undefined : rule.id)}>编辑</Button>
              <Button variant="ghost" size="icon" aria-label={`删除 ${name}`} disabled={disabled} onClick={() => {
                onChange(rules.filter(r => r.id !== rule.id));
                if (editor === rule.id) setEditor(undefined);
              }}><Trash2 className="size-4" /></Button>
            </div>
          </div>
          {error && <p className="text-sm text-destructive">{error}</p>}
          {editor === rule.id && <fieldset aria-label="规则编辑" disabled={disabled} className="grid min-w-0 gap-4 border-t pt-4 sm:grid-cols-2 lg:grid-cols-3">
            <Field label="规则名称"><Input value={rule.name} onChange={e => patch({ name: e.target.value })} /></Field>
            <Field label="规则来源"><Select aria-label="规则来源" value={rule.channel} options={choices(intelligenceChannels)} onValueChange={v => patch({ channel: v as IntelligenceRule["channel"], reasoning_effort: v === "codex" ? rule.reasoning_effort : undefined })} /></Field>
            <Field label="匹配分组"><Select aria-label="匹配分组" value={String(rule.group_id || "")} options={groupOptions(rule.group_id, "不限分组")} onValueChange={v => patch({ group_id: Number(v) || undefined })} /></Field>
            <Field label="匹配套餐码"><Input value={rule.plan ?? ""} placeholder="按套餐码精确匹配，如 plus" onChange={e => patch({ plan: e.target.value })} /></Field>
            <Field label="允许自动检测"><Switch aria-label="允许自动检测" checked={rule.automatic} onCheckedChange={automatic => patch({ automatic })} /></Field>
            <Field label="仅检测正常账号">
              <Switch aria-label="仅检测正常账号" checked={!!rule.normal_only} onCheckedChange={normal_only => patch({ normal_only })} />
              <span className="text-xs text-muted-foreground">跳过异常或不可调度账号；不影响手动检测和已处置账号的恢复复检。</span>
            </Field>
            <p className="col-span-full text-xs text-muted-foreground">以下参数留空时继承全局配置。</p>
            <Field label="检测模型"><Input value={rule.model ?? ""} placeholder="继承来源的检测模型" onChange={e => patch({ model: e.target.value })} /></Field>
            {rule.channel === "codex" && <Field label="推理强度"><Select aria-label="推理强度" value={rule.reasoning_effort ?? ""} options={[inherit, ...choices({ none: "none", minimal: "minimal", low: "low", medium: "medium", high: "high", xhigh: "xhigh", max: "max", ultra: "ultra" })]} onValueChange={v => patch({ reasoning_effort: v || undefined })} /></Field>}
            <Field label="判断方式"><Select aria-label="判断方式" value={rule.judge_mode ?? ""} options={[inherit, ...choices({ answer: "仅答案", latency: "仅耗时", both: "答案和耗时均满足" })]} onValueChange={v => patch({ judge_mode: (v || undefined) as IntelligenceRule["judge_mode"] })} /></Field>
            {intelligenceRuleNumbers.map(([key, label, max]) => <Field key={key} label={label}>
              <Input type="number" min={1} max={max} value={rule[key] ?? ""} placeholder="继承全局" onChange={e => patch({ [key]: e.target.value === "" ? undefined : Number(e.target.value) })} />
            </Field>)}
            <Field label="处置策略"><Select aria-label="处置策略" value={rule.action ?? ""} options={[inherit, ...choices(intelligenceActionOptions)]} onValueChange={v => patch({
              action: (v || undefined) as IntelligenceRule["action"],
              target_group_id: intelligenceActionMovesGroups(v) ? rule.target_group_id : undefined,
            })} /></Field>
            {(intelligenceActionMovesGroups(rule.action) || !!rule.target_group_id) && <Field label="规则目标分组">
              <Select aria-label="规则目标分组" value={String(rule.target_group_id || "")} options={groupOptions(rule.target_group_id, "请选择同来源分组")} onValueChange={v => patch({ target_group_id: Number(v) || undefined })} />
              <span className="text-xs text-muted-foreground">恢复达标后还原原分组；管理员手动改组优先。</span>
            </Field>}
            <fieldset className="col-span-full min-w-0 space-y-2 rounded-md border px-3 py-2 text-sm">
              <legend className="px-1 font-medium">指定题目</legend>
              <p className="text-xs text-muted-foreground">未勾选时使用全部启用题目；指定后至少有一道已启用题目，按全局题库顺序轮换。</p>
              <div className="flex flex-wrap gap-x-4 gap-y-2">
                {settings.questions.map(q => <label key={q.id} className="flex min-w-0 items-center gap-2">
                  <input type="checkbox" className="size-4 shrink-0 accent-primary" checked={questionIDs.includes(q.id)} onChange={e => patch({ question_ids: e.target.checked ? [...questionIDs, q.id] : questionIDs.filter(id => id !== q.id) })} />
                  <span className="[overflow-wrap:anywhere]">{q.name}{!q.enabled && "（未启用）"}</span>
                </label>)}
                {missingIDs.length > 0 && <Button variant="outline" size="sm" onClick={() => patch({ question_ids: questionIDs.filter(id => !missingIDs.includes(id)) })}>移除已删除题目</Button>}
                {questionIDs.length > 0 && <Button variant="ghost" size="sm" onClick={() => patch({ question_ids: undefined })}>使用全部启用题目</Button>}
              </div>
            </fieldset>
          </fieldset>}
        </CardContent></Card></li>;
      })}
    </ol>
  </section>;
}
