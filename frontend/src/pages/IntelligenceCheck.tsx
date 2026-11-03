import { useEffect, useRef, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router-dom";
import {
  ArrowDown,
  ArrowUp,
  Plus,
  Save,
  Trash2,
} from "lucide-react";
import { api } from "../api";
import PageHeader from "../components/PageHeader";
import AccountIntelligenceHistoryDialog from "../components/AccountIntelligenceHistoryDialog";
import IntelligenceRunDialog from "../components/IntelligenceRunDialog";
import { Button } from "../components/ui/button";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "../components/ui/card";
import { Input } from "../components/ui/input";
import { Switch } from "../components/ui/switch";
import { Select } from "../components/ui/select";
import { useToast } from "../hooks/useToast";
import { getErrorMessage } from "../utils/error";
import { refreshIntelligenceBars } from "../hooks/useIntelligenceBars";
import {
  intelligenceTargetConflict,
  type IntelligenceTargetConflict,
  type IntelligenceQuestion,
  type IntelligenceSettings,
} from "../lib/intelligenceCheck";

const fieldClass =
  "w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring";
const field = (label: string, child: ReactNode, hint?: string) => (
  <label className="flex flex-col gap-2 text-sm">
    <span className="font-medium">{label}</span>
    {child}
    {hint && <span className="text-xs text-muted-foreground">{hint}</span>}
  </label>
);
const options = (values: Record<string, string>) =>
  Object.entries(values).map(([value, label]) => ({ value, label }));

export default function IntelligenceCheck() {
  const [params, setParams] = useSearchParams();
  const accountID = Number(params.get("account_id")) || undefined;
  const runID = Number(params.get("run")) || null;
  const [settings, setSettings] = useState<IntelligenceSettings | null>(null);
  const [saved, setSaved] = useState<IntelligenceSettings | null>(null);
  const [groups, setGroups] = useState<
    { id: number; name: string; channel?: string }[]
  >([]);
  const [targetConflict, setTargetConflict] = useState<IntelligenceTargetConflict | null>(null);
  const conflictPanel = useRef<HTMLDivElement>(null);
  const [configError, setConfigError] = useState("");
  const [busy, setBusy] = useState(false);
  const [editor, setEditor] = useState<string | null>(null);
  const { showToast } = useToast();
  const dirty =
    !!settings && JSON.stringify(settings) !== JSON.stringify(saved);

  useEffect(() => {
    if (targetConflict) conflictPanel.current?.scrollIntoView({ block: "center" });
  }, [targetConflict]);

  useEffect(() => {
    const controller = new AbortController();
    void Promise.all([
      api.getIntelligenceSettings(controller.signal),
      api.listAccountGroups(),
    ])
      .then(([config, result]) => {
        if (controller.signal.aborted) return;
        setSettings(config);
        setSaved(config);
        setGroups(result.groups ?? []);
        setConfigError("");
      })
      .catch((e) => {
        if (!controller.signal.aborted) setConfigError(getErrorMessage(e));
      });
    return () => controller.abort();
  }, []);
  useEffect(() => {
    if (!dirty) return;
    const handler = (event: BeforeUnloadEvent) => {
      event.preventDefault();
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, [dirty]);

  const change = <K extends keyof IntelligenceSettings>(
    key: K,
    value: IntelligenceSettings[K],
  ) => {
    if (key === "action" || key === "target_group_id") setTargetConflict(null);
    setSettings((s) => (s ? { ...s, [key]: value } : s));
  };
  const mutate = async (fn: () => Promise<unknown>, message: string) => {
    setBusy(true);
    try {
      await fn();
      showToast(message);
    } catch (e) {
      const conflict = intelligenceTargetConflict(e);
      if (conflict) setTargetConflict(conflict);
      showToast(
        conflict
          ? `有 ${conflict.conflict_count} 个账号的检测通道不兼容，请处理下方列表后重新保存`
          : getErrorMessage(e),
        "error",
      );
    } finally {
      setBusy(false);
    }
  };
  const save = () =>
    mutate(async () => {
      if (!settings) return;
      const next = await api.saveIntelligenceSettings(settings);
      setSettings(next);
      setSaved(next);
      setTargetConflict(null);
    }, "检测配置和题库已保存");
  const disableConflicts = () =>
    mutate(async () => {
      if (!targetConflict) return;
      await api.setIntelligenceAccounts(
        targetConflict.conflicts.map(a => a.account_id), false,
      );
      refreshIntelligenceBars();
      setTargetConflict(null);
    }, "已关闭所列账号的自动检测，请重新保存配置");
  const question = settings?.questions.find((q) => q.id === editor);
  const editQuestion = (patch: Partial<IntelligenceQuestion>) => {
    if (settings)
      change(
        "questions",
        settings.questions.map((q) =>
          q.id === editor ? { ...q, ...patch } : q,
        ),
      );
  };
  const reorder = (index: number, direction: number) => {
    if (!settings) return;
    const questions = [...settings.questions];
    [questions[index], questions[index + direction]] = [
      questions[index + direction],
      questions[index],
    ];
    change("questions", questions);
  };
  const numeric = (
    key:
      | "pause_initial_seconds"
      | "pause_max_seconds"
      | "recovery_initial_seconds"
      | "recovery_min_seconds"
      | "interval_seconds"
      | "retry_seconds"
      | "timeout_seconds"
      | "concurrency"
      | "retention"
      | "degrade_threshold"
      | "recover_threshold",
    label: string,
    min: number,
    max: number,
  ) =>
    field(
      label,
      <Input
        type="number"
        min={min}
        max={max}
        value={settings?.[key] ?? ""}
        onChange={(e) => change(key, Number(e.target.value))}
      />,
    );
  return (
    <>
      <PageHeader
        title="降智检测"
        description="固定账号逐题检测，记录答案与耗时，按已保存策略处理连续结果。"
        actions={
          <div className="flex flex-wrap items-center gap-2">
            {dirty && (
              <span className="text-xs text-muted-foreground">
                有未保存更改
              </span>
            )}
            <Button
              onClick={() => void save()}
              disabled={!settings || busy || !dirty}
            >
              <Save className="size-4" />
              保存配置
            </Button>
          </div>
        }
      />
      {configError && (
        <p role="alert" className="mb-4 text-sm text-destructive">
          配置加载失败：{configError}。请刷新页面重试。
        </p>
      )}
      {settings && <div className="grid items-start gap-6 xl:grid-cols-2">
        <section aria-labelledby="intelligence-settings-title" className="min-w-0 space-y-4">
          <h2 id="intelligence-settings-title" className="text-lg font-semibold">检测配置</h2>
          <p className="text-xs text-muted-foreground">
            检测使用账号绑定的代理；未绑定则直连。客户端身份、指纹和时区沿用账号或全局配置。
          </p>
          <Card>
            <CardHeader className="pb-0">
              <CardTitle>模型与判定</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-5 sm:grid-cols-2">
              {field(
                "自动检测总开关",
                <Switch
                  aria-label="自动检测总开关"
                  checked={settings.enabled}
                  onCheckedChange={(v) => change("enabled", v)}
                />,
                "保存后生效；关闭时仍可手动检测。",
              )}
              {field(
                "GPT / Codex 模型",
                <Input
                  value={settings.gpt_model}
                  onChange={(e) => change("gpt_model", e.target.value)}
                />,
              )}
              {field(
                "Claude 模型",
                <Input
                  value={settings.claude_model}
                  onChange={(e) => change("claude_model", e.target.value)}
                />,
              )}
              {field(
                "GPT 推理强度",
                <Select
                  value={settings.reasoning_effort}
                  onValueChange={(v) => change("reasoning_effort", v)}
                  options={options({
                    "": "模型默认",
                    none: "none",
                    minimal: "minimal",
                    low: "low",
                    medium: "medium",
                    high: "high",
                    xhigh: "xhigh",
                    max: "max",
                    ultra: "ultra",
                  })}
                />,
              )}
              {field(
                "判断方式",
                <Select
                  value={settings.judge_mode}
                  onValueChange={(v) =>
                    change(
                      "judge_mode",
                      v as IntelligenceSettings["judge_mode"],
                    )
                  }
                  options={options({
                    answer: "仅答案",
                    latency: "仅耗时",
                    both: "答案和耗时均满足",
                  })}
                />,
                "仅完整成功响应参与判定，耗时必须严格小于题目阈值。",
              )}
              {field(
                "新增账号默认检测",
                <Switch
                  checked={settings.new_accounts_enabled}
                  onCheckedChange={(v) => change("new_accounts_enabled", v)}
                />,
                "仅支持 Codex 和 Claude；切换分组时，仅同通道新账号默认参与。已有账号不受此选项影响。",
              )}
            </CardContent>
          </Card>
          <Card>
            <CardHeader className="pb-0">
              <CardTitle>频率与资源</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-5 sm:grid-cols-2">
              {numeric("interval_seconds", "正常间隔（秒）", 1, 86400)}
              {numeric("retry_seconds", "异常重试间隔（秒）", 1, 86400)}
              {numeric("timeout_seconds", "请求超时（秒）", 1, 3600)}
              {numeric("concurrency", "全局检测并发", 1, 100)}
              {numeric("retention", "每账号保留记录", 1, 10000)}
              <div className="sm:col-span-2">
                {field("自适应检测", <Switch aria-label="自适应检测" checked={settings.adaptive_enabled ?? false} onCheckedChange={v => change("adaptive_enabled", v)} />,
                  "暂停后未通过则间隔翻倍；检测正常后加速确认。恢复调度后，正常检测逐步减轻历史退避。关闭时沿用固定间隔。")}
              </div>
              {settings.adaptive_enabled && <>
                {numeric("pause_initial_seconds", "暂停初始间隔（秒）", 1, 2592000)}
                {numeric("pause_max_seconds", "暂停最大间隔（秒）", 1, 2592000)}
                {numeric("recovery_initial_seconds", "恢复确认初始间隔（秒）", 1, 2592000)}
                {numeric("recovery_min_seconds", "恢复确认最短间隔（秒）", 1, 2592000)}
                <p className="text-xs text-muted-foreground sm:col-span-2">恢复确认间隔逐次减半（最短间隔至少一分钟时按整分钟向下取整），直到达到下方连续通过次数。实际等待有最多 10% 的提前抖动。最大间隔越长，发现恢复可能越晚。</p>
              </>}

            </CardContent>
          </Card>
          <Card>
            <CardHeader className="pb-0">
              <CardTitle>自动处置</CardTitle>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="grid gap-5 sm:grid-cols-2">
                {field(
                  "达到未通过阈值时",
                  <Select
                    value={settings.action}
                    onValueChange={(v) =>
                      change("action", v as IntelligenceSettings["action"])
                    }
                    options={options({
                      none: "不自动处置",
                      pause: "暂停业务调度",
                      switch_group: "切换分组",
                    })}
                  />,
                )}
                {numeric("degrade_threshold", "连续未通过次数", 1, 1000)}
                {numeric("recover_threshold", "连续恢复次数", 1, 1000)}
                {settings.action === "switch_group" &&
                  field(
                    "目标分组",
                    <Select
                      value={String(settings.target_group_id || "")}
                      onValueChange={(v) =>
                        change("target_group_id", Number(v))
                      }
                      placeholder="选择分组"
                      options={groups
                        .filter(g => !g.channel || g.channel === "codex" || g.channel === "claude")
                        .map(g => ({
                          value: String(g.id),
                          label: `${g.name}（${g.channel === "claude" ? "Claude" : "Codex"}）`,
                        }))}
                    />,
                  )}
              </div>
              {targetConflict && (
                <div
                  ref={conflictPanel}
                  role="alert"
                  className="space-y-3 rounded-md border border-destructive/40 p-3 text-sm"
                >
                  <p className="font-medium">
                    有 {targetConflict.conflict_count}{" "}
                    个账号的检测通道与目标分组不兼容，配置尚未保存。
                  </p>
                  <p className="text-muted-foreground">
                    目标分组：
                    {groups.find(
                      (g) => g.id === targetConflict.target_group_id,
                    )?.name || `#${targetConflict.target_group_id}`}
                    （{targetConflict.target_group_channel}
                    ）。可关闭下列账号的自动检测后重新保存，或调整处置策略。
                  </p>
                  <ul className="max-h-60 space-y-2 overflow-y-auto">
                    {targetConflict.conflicts.map((account) => (
                      <li
                        key={account.account_id}
                        className="flex flex-wrap items-center justify-between gap-2"
                      >
                        <span className="min-w-0 break-all">
                          #{account.account_id} ·{" "}
                          {account.account_name || "未命名账号"} ·{" "}
                          {account.channel}
                        </span>
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={busy}
                          onClick={() => {
                            const next = new URLSearchParams(params);
                            next.delete("run");
                            next.set(
                              "account_id",
                              String(account.account_id),
                            );
                            setParams(next);
                          }}
                        >
                          查看账号检测
                        </Button>
                      </li>
                    ))}
                  </ul>
                  {targetConflict.conflict_count >
                    targetConflict.conflicts.length && (
                    <p className="text-muted-foreground">
                      当前列出前 {targetConflict.conflicts.length}{" "}
                      个；处理后重新保存可查看剩余冲突。
                    </p>
                  )}
                  <Button
                    variant="outline"
                    disabled={busy}
                    onClick={() => void disableConflicts()}
                  >
                    关闭以上 {targetConflict.conflicts.length}{" "}
                    个账号的自动检测
                  </Button>
                  <p className="text-xs text-muted-foreground">
                    仅关闭自动检测，账号的业务调度和已有处置恢复状态保持不变。
                  </p>
                </div>
              )}
              <p className="text-xs leading-relaxed text-muted-foreground">
                上游错误不计为降智；开启自适应时会中断恢复确认。人工修改调度或分组优先，并重置连续次数。关闭暂停策略不会自动释放已有暂停；关闭切组策略后，已切组账号仍可按恢复阈值还原原分组。检测暂停期间继续检测。
              </p>
            </CardContent>
          </Card>
        </section>
        <section aria-labelledby="intelligence-questions-title" className="min-w-0 space-y-4">
          <h2 id="intelligence-questions-title" className="text-lg font-semibold">检测题库</h2>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="text-sm text-muted-foreground">
              每个账号按启用题目顺序轮换，每次一道题；编辑后统一保存。
            </p>
            <Button
              variant="outline"
              onClick={() => {
                const id = crypto.randomUUID();
                change("questions", [
                  ...settings.questions,
                  {
                    id,
                    name: "新题目",
                    prompt: "",
                    expected_answer: "",
                    match_mode: "exact",
                    max_duration_ms: 20000,
                    enabled: false,
                  },
                ]);
                setEditor(id);
              }}
            >
              <Plus className="size-4" />
              新增题目
            </Button>
          </div>
          {settings.questions.map((q, index) => (
            <Card key={q.id}>
              <CardContent className="flex flex-wrap items-center gap-3 py-4">
                <Switch
                  aria-label={`启用 ${q.name}`}
                  checked={q.enabled}
                  onCheckedChange={(enabled) =>
                    change(
                      "questions",
                      settings.questions.map((item) =>
                        item.id === q.id ? { ...item, enabled } : item,
                      ),
                    )
                  }
                />
                <div className="min-w-0 flex-1">
                  <div className="font-medium">{q.name}</div>
                  <div className="mt-1 text-xs text-muted-foreground">
                    {q.match_mode} · {q.max_duration_ms} ms · 预期{" "}
                    {q.expected_answer.slice(0, 60)}
                  </div>
                </div>
                <div className="flex gap-1">
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={`上移 ${q.name}`}
                    disabled={index === 0}
                    onClick={() => reorder(index, -1)}
                  >
                    <ArrowUp className="size-4" />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={`下移 ${q.name}`}
                    disabled={index === settings.questions.length - 1}
                    onClick={() => reorder(index, 1)}
                  >
                    <ArrowDown className="size-4" />
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setEditor(editor === q.id ? null : q.id)}
                  >
                    编辑
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={`删除 ${q.name}`}
                    onClick={() => {
                      change(
                        "questions",
                        settings.questions.filter((item) => item.id !== q.id),
                      );
                      if (editor === q.id) setEditor(null);
                    }}
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </div>
                {question?.id === q.id && (
                  <div className="grid w-full gap-4 border-t pt-4 sm:grid-cols-2">
                    {field(
                      "题目名称",
                      <Input
                        value={question.name}
                        onChange={(e) => editQuestion({ name: e.target.value })}
                      />,
                    )}
                    {field(
                      "匹配方式",
                      <Select
                        value={question.match_mode}
                        onValueChange={(v) =>
                          editQuestion({
                            match_mode: v as IntelligenceQuestion["match_mode"],
                          })
                        }
                        options={options({
                          exact: "标准答案（完整匹配）",
                          contains: "包含匹配",
                          regex: "正则匹配（RE2）",
                        })}
                      />,
                    )}
                    <div className="sm:col-span-2">
                      {field(
                        "题目正文",
                        <textarea
                          rows={7}
                          className={fieldClass}
                          value={question.prompt}
                          onChange={(e) =>
                            editQuestion({ prompt: e.target.value })
                          }
                        />,
                      )}
                    </div>
                    {field(
                      "预期答案 / 正则",
                      <Input
                        value={question.expected_answer}
                        onChange={(e) =>
                          editQuestion({ expected_answer: e.target.value })
                        }
                      />,
                    )}
                    {field(
                      "耗时阈值（毫秒）",
                      <Input
                        type="number"
                        min={1}
                        value={question.max_duration_ms}
                        onChange={(e) =>
                          editQuestion({
                            max_duration_ms: Number(e.target.value),
                          })
                        }
                      />,
                    )}
                  </div>
                )}
              </CardContent>
            </Card>
          ))}
          {!settings.questions.length && (
            <p className="py-12 text-center text-muted-foreground">
              暂无题目，开启自动检测前请添加并启用至少一道题。
            </p>
          )}
        </section>
      </div>}
      <AccountIntelligenceHistoryDialog accountID={runID ? null : accountID ?? null} onClose={() => { const next = new URLSearchParams(params); next.delete("account_id"); setParams(next); }} />
      <IntelligenceRunDialog
        id={runID}
        onClose={() => {
          const next = new URLSearchParams(params);
          next.delete("run");
          setParams(next);
        }}
      />
    </>
  );
}