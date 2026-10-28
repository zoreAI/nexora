"use client";

import { useEffect, useMemo, useState } from "react";
import { AlertCircle, Ban, CheckCircle2, Clock3, Coins, LoaderCircle, Users } from "lucide-react";

import { fetchDashboard, type DashboardRange, type DashboardResponse } from "@/lib/api";
import { useAuthGuard } from "@/lib/use-auth-guard";

const ranges: DashboardRange[] = ["24h", "7d", "30d"];
const rangeLabels: Record<DashboardRange, string> = { "24h": "24小时", "7d": "7天", "30d": "30天" };
const modelColors = ["#5488f4", "#55cfa4", "#f5b91d", "#249d9b", "#a78bda"];

function RangeSwitch({ value, onChange }: { value: DashboardRange; onChange: (value: DashboardRange) => void }) {
  return <div role="group" aria-label="时间范围" className="flex shrink-0 rounded-md border border-border bg-secondary/50 p-1">
    {ranges.map((range) => <button key={range} type="button" onClick={() => onChange(range)} aria-pressed={value === range} className={`h-8 rounded px-3 text-xs font-medium transition-colors ${value === range ? "bg-card text-foreground shadow-sm" : "text-muted-foreground hover:text-foreground"}`}>{rangeLabels[range]}</button>)}
  </div>;
}

function useDashboardRange(range: DashboardRange, enabled: boolean) {
  const [data, setData] = useState<DashboardResponse | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    if (!enabled) return;
    let active = true;
    setLoading(true);
    setError("");
    setData(null);
    void fetchDashboard(range).then((result) => {
      if (active) setData(result);
    }).catch((cause: unknown) => {
      if (active) setError(cause instanceof Error ? cause.message : "概览加载失败");
    }).finally(() => {
      if (active) setLoading(false);
    });
    return () => { active = false; };
  }, [range, enabled]);
  return { data, error, loading };
}

function axisLabel(time: string, range: DashboardRange) {
  return range === "24h" ? `${time.slice(11)}:00` : time.slice(5);
}

function ChartPanel({ title, range, onRangeChange, loading, error, children }: {
  title: string;
  range: DashboardRange;
  onRangeChange: (value: DashboardRange) => void;
  loading: boolean;
  error: string;
  children: React.ReactNode;
}) {
  return <section className="rounded-md border border-border bg-card p-4 sm:p-5">
    <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border pb-4">
      <h2 className="text-sm font-semibold">{title}</h2>
      <RangeSwitch value={range} onChange={onRangeChange} />
    </div>
    {loading ? <div className="flex h-[300px] items-center justify-center"><LoaderCircle className="size-5 animate-spin text-muted-foreground" /></div>
      : error ? <div className="flex h-[300px] items-center justify-center text-sm text-destructive">{error}</div>
      : children}
  </section>;
}

function ModelChart({ data }: { data: DashboardResponse }) {
  const totals = useMemo(() => {
    const result: Record<string, number> = {};
    for (const bucket of data.buckets) for (const [model, count] of Object.entries(bucket.models)) result[model] = (result[model] || 0) + count;
    return Object.entries(result).sort((a, b) => b[1] - a[1]);
  }, [data]);
  const models = totals.slice(0, 4).map(([model]) => model);
  if (totals.length > 4) models.push("其他模型");
  const max = Math.max(1, ...data.buckets.map((bucket) => Object.values(bucket.models).reduce((sum, count) => sum + count, 0)));
  const allEmpty = totals.length === 0;
  return <div className="pt-4">
    <div className="flex min-h-8 flex-wrap justify-end gap-x-4 gap-y-1 text-xs text-muted-foreground">
      {models.map((model, index) => <span key={model} className="inline-flex items-center gap-1.5"><span className="size-2.5 rounded-sm" style={{ backgroundColor: modelColors[index] }} />{model}</span>)}
    </div>
    {allEmpty ? <div className="flex h-[252px] items-center justify-center text-sm text-muted-foreground">该时段暂无调用记录</div> :
      <div className="overflow-x-auto pb-1">
        <div className="flex h-[252px] min-w-[640px] gap-1.5 border-b border-border pt-4">
          {data.buckets.map((bucket, index) => {
            const other = Object.entries(bucket.models).filter(([model]) => !models.includes(model)).reduce((sum, [, count]) => sum + count, 0);
            const counts = models.map((model) => model === "其他模型" ? other : bucket.models[model] || 0);
            const total = counts.reduce((sum, count) => sum + count, 0);
            return <div key={bucket.time} className="flex min-w-0 flex-1 flex-col items-center justify-end" title={`${bucket.time} · ${total} 次`}>
              <div className="flex w-full max-w-10 flex-col-reverse overflow-hidden rounded-t-sm" style={{ height: `${Math.max(0, total / max * 200)}px` }}>
                {counts.map((count, colorIndex) => count > 0 && <div key={models[colorIndex]} style={{ height: `${count / total * 100}%`, backgroundColor: modelColors[colorIndex] }} />)}
              </div>
              <span className="mt-2 h-6 whitespace-nowrap text-[10px] text-muted-foreground">{index % (data.range === "24h" ? 3 : data.range === "30d" ? 5 : 1) === 0 ? axisLabel(bucket.time, data.range) : ""}</span>
            </div>;
          })}
        </div>
      </div>}
  </div>;
}

function CallsChart({ data }: { data: DashboardResponse }) {
  const success = data.buckets.reduce((sum, bucket) => sum + bucket.success, 0);
  const failed = data.buckets.reduce((sum, bucket) => sum + bucket.failed, 0);
  const max = Math.max(1, ...data.buckets.map((bucket) => Math.max(bucket.success, bucket.failed)));
  return <div className="pt-4">
    <div className="flex min-h-8 justify-end gap-4 text-xs text-muted-foreground">
      <span className="flex items-center gap-1.5"><span className="size-2.5 rounded-sm bg-teal-500" />成功 {success}</span>
      <span className="flex items-center gap-1.5"><span className="size-2.5 rounded-sm bg-rose-500" />失败 {failed}</span>
    </div>
    {success + failed === 0 ? <div className="flex h-[252px] items-center justify-center text-sm text-muted-foreground">该时段暂无调用记录</div> :
      <div className="overflow-x-auto pb-1">
        <div className="flex h-[252px] min-w-[640px] gap-1.5 border-b border-border pt-4">
          {data.buckets.map((bucket, index) => <div key={bucket.time} className="flex min-w-0 flex-1 flex-col items-center justify-end" title={`${bucket.time} · 成功 ${bucket.success} / 失败 ${bucket.failed}`}>
            <div className="flex h-[200px] w-full max-w-10 items-end justify-center gap-0.5">
              <div className="w-1/2 rounded-t-sm bg-teal-500" style={{ height: `${bucket.success / max * 100}%` }} />
              <div className="w-1/2 rounded-t-sm bg-rose-500" style={{ height: `${bucket.failed / max * 100}%` }} />
            </div>
            <span className="mt-2 h-6 whitespace-nowrap text-[10px] text-muted-foreground">{index % (data.range === "24h" ? 3 : data.range === "30d" ? 5 : 1) === 0 ? axisLabel(bucket.time, data.range) : ""}</span>
          </div>)}
        </div>
      </div>}
  </div>;
}

export default function HomePage() {
  const { session, isCheckingAuth } = useAuthGuard(["admin"]);
  const [modelRange, setModelRange] = useState<DashboardRange>("30d");
  const [callsRange, setCallsRange] = useState<DashboardRange>("24h");
  const model = useDashboardRange(modelRange, !!session && session.role === "admin");
  const calls = useDashboardRange(callsRange, !!session && session.role === "admin");
  const accounts = model.data?.accounts;

  if (isCheckingAuth || !session || session.role !== "admin") return <div className="flex min-h-[40vh] items-center justify-center"><LoaderCircle className="size-5 animate-spin text-muted-foreground" /></div>;

  const cards = [
    { label: "账号总数", value: accounts?.total, icon: Users, tone: "bg-sky-100 text-sky-700" },
    { label: "正常账号", value: accounts?.statuses["正常"], icon: CheckCircle2, tone: "bg-emerald-100 text-emerald-700" },
    { label: "限流账号", value: accounts?.statuses["限流"], icon: Clock3, tone: "bg-amber-100 text-amber-700" },
    { label: "异常账号", value: accounts?.statuses["异常"], icon: AlertCircle, tone: "bg-rose-100 text-rose-700" },
    { label: "封号账号", value: accounts?.statuses["封号"], icon: Ban, tone: "bg-red-100 text-red-800" },
    { label: "禁用账号", value: accounts?.statuses["禁用"], icon: Ban, tone: "bg-slate-100 text-slate-700" },
    { label: accounts?.unknown_quota ? "已知剩余额度" : "剩余额度", value: accounts?.remaining_quota, icon: Coins, tone: "bg-cyan-100 text-cyan-700" },
  ];

  return <div className="space-y-5 py-6 sm:py-8">
    <section aria-label="账号概览" className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
      {cards.map(({ label, value, icon: Icon, tone }) => <div key={label} className="relative min-h-[110px] rounded-md border border-border bg-card p-4">
        <p className="pr-9 text-xs text-muted-foreground">{label}</p>
        <span className={`absolute right-4 top-4 grid size-9 place-items-center rounded-full ${tone}`}><Icon className="size-4" /></span>
        <p className="mt-4 text-2xl font-semibold tabular-nums">{model.loading ? "--" : value ?? "--"}</p>
      </div>)}
    </section>
    <ChartPanel title="模型请求分布" range={modelRange} onRangeChange={setModelRange} loading={model.loading} error={model.error}>
      {model.data && <ModelChart data={model.data} />}
    </ChartPanel>
    <ChartPanel title="调用趋势" range={callsRange} onRangeChange={setCallsRange} loading={calls.loading} error={calls.error}>
      {calls.data && <CallsChart data={calls.data} />}
    </ChartPanel>
  </div>;
}
