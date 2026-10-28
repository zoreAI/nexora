"use client";

import { ChevronDown, LoaderCircle, PlugZap, Plus, RefreshCw, Save, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { deleteProxyGroup, fetchProxyGroups, fetchProxyView, saveProxyDefaults, saveProxyGroup, syncImageStorage, testImageStorage, testProxy, testProxyGroup, type ProxyGroup, type ProxyTestResult } from "@/lib/api";

import { useSettingsStore } from "../store";
import { ProxyNodeImportDialog } from "./proxy-node-import-dialog";

/**
 * 6 个 section 组件集中放这里：
 *   - 都只负责呈现自己 section 的字段，store 逻辑共用
 *   - 不再带保存按钮——保存统一走 FloatingSaveBar
 *   - 视觉只用 grid + 输入框，不再每个 section 套 Card（避免 Card 套 Card 套 Card）
 *
 * 共享样式常量挑出来，避免几十处 className 重复。
 */
const INPUT_CLASS = "h-10 rounded-xl border-stone-200 bg-white";
const LABEL_CLASS = "text-sm text-stone-700";
const HELP_CLASS = "text-xs text-stone-500";
const TILE_CLASS = "rounded-xl border border-stone-200 bg-white px-4 py-3";
const PROXY_STRATEGY_LABELS: Record<ProxyGroup["strategy"], string> = { round_robin: "轮询", request_random: "随机", time_window: "时间窗口" };
const PROXY_HEALTH_LABELS: Record<ProxyGroup["health"]["state"], string> = { healthy: "可用", unhealthy: "不可用", unknown: "待检测" };

/* ───────────────────────── 账号 ───────────────────────── */

export function AccountSection() {
  const config = useSettingsStore((s) => s.config);
  const setRefreshAccountIntervalMinute = useSettingsStore((s) => s.setRefreshAccountIntervalMinute);
  const setAutoRemoveInvalidAccounts = useSettingsStore((s) => s.setAutoRemoveInvalidAccounts);
  const setAutoRemoveRateLimitedAccounts = useSettingsStore((s) => s.setAutoRemoveRateLimitedAccounts);

  return (
    <div className="grid gap-4 md:grid-cols-2">
      <div className="space-y-2 md:col-span-2">
        <label className={LABEL_CLASS}>账号刷新间隔（分钟）</label>
        <Input
          value={String(config?.refresh_account_interval_minute || "")}
          onChange={(e) => setRefreshAccountIntervalMinute(e.target.value)}
          placeholder="5"
          className={INPUT_CLASS + " md:max-w-xs"}
        />
        <p className={HELP_CLASS}>控制账号自动刷新频率。</p>
      </div>
      <label className={`flex items-center gap-3 ${TILE_CLASS} text-sm text-stone-700`}>
        <Checkbox
          checked={Boolean(config?.auto_remove_invalid_accounts)}
          onCheckedChange={(c) => setAutoRemoveInvalidAccounts(Boolean(c))}
        />
        自动移除异常账号
      </label>
      <label className={`flex items-center gap-3 ${TILE_CLASS} text-sm text-stone-700`}>
        <Checkbox
          checked={Boolean(config?.auto_remove_rate_limited_accounts)}
          onCheckedChange={(c) => setAutoRemoveRateLimitedAccounts(Boolean(c))}
        />
        自动移除限流账号
      </label>
    </div>
  );
}

/* ───────────────────────── 网络 ───────────────────────── */

export function NetworkSection() {
  const [isTestingProxy, setIsTestingProxy] = useState(false);
  const [proxyTestResult, setProxyTestResult] = useState<ProxyTestResult | null>(null);
  const config = useSettingsStore((s) => s.config);
  const setProxy = useSettingsStore((s) => s.setProxy);
  const [groups, setGroups] = useState<ProxyGroup[]>([]);
  const [selectedGroupID, setSelectedGroupID] = useState("");
  const [groupName, setGroupName] = useState("");
  const [groupStrategy, setGroupStrategy] = useState<ProxyGroup["strategy"]>("round_robin");
  const [nodeURLs, setNodeURLs] = useState("");
  const [nodeLimits, setNodeLimits] = useState<Record<string, string>>({});
  const [groupsLoading, setGroupsLoading] = useState(false);
  const [groupsBusy, setGroupsBusy] = useState(false);
  const [groupNotice, setGroupNotice] = useState<{ id: string; message: string } | null>(null);
  const [expandedGroupID, setExpandedGroupID] = useState("");
  const [testingGroupID, setTestingGroupID] = useState("");
  const [groupTestResults, setGroupTestResults] = useState<Record<string, Record<string, ProxyTestResult>>>({});
  const [defaultMode, setDefaultMode] = useState<"direct" | "custom" | "group">("custom");
  const [defaultGroupID, setDefaultGroupID] = useState("");
  const [fallbackGroupID, setFallbackGroupID] = useState("");
  const [defaultsBusy, setDefaultsBusy] = useState(false);

  const loadProxyGroups = async () => {
    setGroupsLoading(true);
    try {
      const data = await fetchProxyGroups();
      setGroups(data.groups);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "加载代理组失败");
    } finally {
      setGroupsLoading(false);
    }
  };

  const loadProxyView = async () => {
    try {
      const data = await fetchProxyView();
      setGroups(data.groups);
      setDefaultMode(data.default_reference.mode);
      setDefaultGroupID(data.default_reference.group_id || "");
      setFallbackGroupID(data.fallback_reference?.mode === "group" ? data.fallback_reference.group_id : "");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "加载代理默认配置失败");
    }
  };

  useEffect(() => { void loadProxyView(); }, []);

  const editProxyGroup = (group: ProxyGroup) => {
    setSelectedGroupID(group.id);
    setGroupName(group.name);
    setGroupStrategy(group.strategy);
    setNodeURLs(group.nodes.map((node) => node.url).join("\n"));
    setNodeLimits(Object.fromEntries(group.nodes.map((node) => [node.url, String(node.image_concurrency_limit)])));
  };

  const resetProxyGroup = () => {
    setSelectedGroupID("");
    setGroupName("");
    setGroupStrategy("round_robin");
    setNodeURLs("");
    setNodeLimits({});
  };

  const parsedNodeURLs = nodeURLs.split(/\r?\n/).map((url) => url.trim()).filter(Boolean);

  const applyImportedNodes = (nodes: Array<{ url: string; image_concurrency_limit: number }>) => {
    setNodeURLs((current) => [current.trim(), ...nodes.map((node) => node.url)].filter(Boolean).join("\n"));
    setNodeLimits((current) => ({
      ...current,
      ...Object.fromEntries(nodes.map((node) => [node.url, String(node.image_concurrency_limit)])),
    }));
  };

  const handleSaveProxyGroup = async () => {
    if (!groupName.trim() || parsedNodeURLs.length === 0) { toast.error("请填写代理组名称和至少一个节点"); return; }
    if (parsedNodeURLs.some((url) => {
      const value = Number(nodeLimits[url] ?? "0");
      return !Number.isInteger(value) || value < 0 || value > 10000;
    })) { toast.error("节点生图并发需为 0 到 10000 的整数"); return; }
    const existingGroup = groups.find((group) => group.id === selectedGroupID);
    const existingNodes = existingGroup?.nodes ?? [];
    setGroupsBusy(true);
    try {
      const response = await saveProxyGroup({
        id: selectedGroupID || undefined,
        name: groupName.trim(),
        strategy: groupStrategy,
        rotation_interval_minutes: existingGroup?.rotation_interval_minutes ?? 0,
        enabled: existingGroup?.enabled ?? true,
        notes: existingGroup?.notes ?? "",
        nodes: parsedNodeURLs.map((url) => {
          const existing = existingNodes.find((node) => node.url === url);
          return { id: existing?.id, name: existing?.name, url, enabled: existing?.enabled, image_concurrency_limit: Number(nodeLimits[url] ?? existing?.image_concurrency_limit ?? 0), notes: existing?.notes };
        }),
      });
      await loadProxyGroups();
      editProxyGroup(response.group);
      setGroupTestResults((current) => {
        const next = { ...current };
        delete next[response.group.id];
        return next;
      });
      toast.success("代理组已保存");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "保存代理组失败");
    } finally { setGroupsBusy(false); }
  };

  const handleTestProxyGroup = async (group: ProxyGroup) => {
    setGroupsBusy(true);
    setTestingGroupID(group.id);
    setExpandedGroupID(group.id);
    setGroupNotice(null);
    try {
      const result = await testProxyGroup({ id: group.id });
      setGroupTestResults((current) => ({
        ...current,
        [group.id]: Object.fromEntries(result.results.filter((item) => item.node_id).map((item) => [item.node_id, item.result])),
      }));
      setGroupNotice({ id: group.id, message: result.summary.message });
      await loadProxyGroups();
    } catch (error) { toast.error(error instanceof Error ? error.message : "测试代理组失败"); }
    finally { setGroupsBusy(false); setTestingGroupID(""); }
  };

  const handleSaveProxyDefaults = async () => {
    if (defaultMode === "group" && !defaultGroupID) { toast.error("请选择默认代理组"); return; }
    if (fallbackGroupID && fallbackGroupID === defaultGroupID && defaultMode === "group") { toast.error("备用代理组不能与默认代理组相同"); return; }
    setDefaultsBusy(true);
    try {
      await saveProxyDefaults({
        default_reference: { mode: defaultMode, group_id: defaultMode === "group" ? defaultGroupID : "", url: defaultMode === "custom" ? String(config?.proxy || "").trim() : "" },
        fallback_reference: fallbackGroupID ? { mode: "group", group_id: fallbackGroupID, url: "" } : null,
      });
      toast.success("默认代理路由已保存");
    } catch (error) { toast.error(error instanceof Error ? error.message : "保存默认代理失败"); }
    finally { setDefaultsBusy(false); }
  };

  const handleTestProxy = async () => {
    const candidate = String(config?.proxy || "").trim();
    if (!candidate) {
      toast.error("请先填写代理地址");
      return;
    }
    setIsTestingProxy(true);
    setProxyTestResult(null);
    try {
      const data = await testProxy(candidate);
      setProxyTestResult(data.result);
      if (data.result.ok) {
        toast.success(`代理可用（${data.result.latency_ms} ms，HTTP ${data.result.status}）`);
      } else {
        toast.error(`代理不可用：${data.result.error ?? "未知错误"}`);
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "测试代理失败");
    } finally {
      setIsTestingProxy(false);
    }
  };

  return (
    <div className="space-y-3">
      <label className={LABEL_CLASS}>全局代理</label>
      <Input
        value={String(config?.proxy || "")}
        onChange={(e) => {
          setProxy(e.target.value);
          setProxyTestResult(null);
        }}
        placeholder="http://127.0.0.1:7890"
        className={INPUT_CLASS}
      />
      <p className={HELP_CLASS}>留空表示不使用代理。代理同时影响生图请求和上游 OpenAI 转发。</p>
      {proxyTestResult ? (
        <div
          className={`rounded-xl border px-3 py-2 text-xs leading-6 ${
            proxyTestResult.ok
              ? "border-emerald-200 bg-emerald-50 text-emerald-800"
              : "border-rose-200 bg-rose-50 text-rose-800"
          }`}
        >
          {proxyTestResult.ok
            ? `代理可用：HTTP ${proxyTestResult.status}，用时 ${proxyTestResult.latency_ms} ms`
            : `代理不可用：${proxyTestResult.error ?? "未知错误"}（用时 ${proxyTestResult.latency_ms} ms）`}
        </div>
      ) : null}
      <div className="flex justify-end">
        <Button
          type="button"
          variant="outline"
          className="h-9 rounded-xl border-stone-200 bg-white px-4 text-stone-700"
          onClick={() => void handleTestProxy()}
          disabled={isTestingProxy}
        >
          {isTestingProxy ? <LoaderCircle className="size-4 animate-spin" /> : <PlugZap className="size-4" />}
          测试代理
        </Button>
      </div>
      <div className="space-y-3 border-t border-stone-200 pt-5">
        <div>
          <h3 className="text-sm font-semibold text-stone-800">请求路由</h3>
          <p className={HELP_CLASS}>默认组按策略选择节点；默认组没有可用节点时使用备用组。保存后新请求立即使用。</p>
        </div>
        <div className="grid gap-3 md:grid-cols-2">
          <select value={defaultMode} onChange={(event) => setDefaultMode(event.target.value as typeof defaultMode)} className="h-10 rounded-xl border border-stone-200 bg-white px-3 text-sm">
            <option value="custom">使用全局代理地址</option><option value="group">使用代理组</option><option value="direct">直连</option>
          </select>
          <select value={defaultGroupID} onChange={(event) => setDefaultGroupID(event.target.value)} disabled={defaultMode !== "group"} className="h-10 rounded-xl border border-stone-200 bg-white px-3 text-sm disabled:opacity-50">
            <option value="">选择默认代理组</option>{groups.map((group) => <option key={group.id} value={group.id}>{group.name}</option>)}
          </select>
          <select value={fallbackGroupID} onChange={(event) => setFallbackGroupID(event.target.value)} className="h-10 rounded-xl border border-stone-200 bg-white px-3 text-sm md:col-span-2">
            <option value="">不使用备用代理组</option>{groups.map((group) => <option key={group.id} value={group.id}>{group.name}（备用）</option>)}
          </select>
        </div>
        <div className="flex justify-end"><Button type="button" variant="outline" className="h-9 rounded-xl" onClick={() => void handleSaveProxyDefaults()} disabled={defaultsBusy}><Save className="size-4" />保存请求路由</Button></div>
      </div>
      <div className="space-y-4 border-t border-stone-200 pt-5">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h3 className="text-sm font-semibold text-stone-800">代理节点组</h3>
            <p className={HELP_CLASS}>节点配置保存在 SQL 数据库，可批量粘贴 URL 并探测连通性。默认请求路由在下方选择。</p>
          </div>
          <Button type="button" variant="outline" className="h-8 shrink-0 rounded-lg" onClick={() => void loadProxyGroups()} disabled={groupsLoading}>
            <RefreshCw className={`size-3.5 ${groupsLoading ? "animate-spin" : ""}`} />刷新
          </Button>
        </div>
        {groups.map((group) => (
          <div key={group.id} className="rounded-xl border border-stone-200 bg-white p-3">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <button type="button" className="min-w-0 flex-1 text-left" onClick={() => editProxyGroup(group)}>
                <span className="block truncate text-sm font-medium text-stone-800">{group.name}</span>
                <span className="text-xs text-stone-500">{group.nodes.length} 个节点 · {PROXY_STRATEGY_LABELS[group.strategy]} · {PROXY_HEALTH_LABELS[group.health.state]}</span>
              </button>
              <div className="flex gap-2">
                <Button type="button" variant="outline" className="h-8 rounded-lg" disabled={groupsBusy} onClick={() => void handleTestProxyGroup(group)}>
                  {testingGroupID === group.id ? <LoaderCircle className="size-3.5 animate-spin" /> : <PlugZap className="size-3.5" />}测试
                </Button>
                <Button type="button" variant="outline" className="size-8 rounded-lg p-0" aria-label={expandedGroupID === group.id ? "收起节点状态" : "展开节点状态"} aria-expanded={expandedGroupID === group.id} title={expandedGroupID === group.id ? "收起节点状态" : "展开节点状态"} onClick={() => setExpandedGroupID((current) => current === group.id ? "" : group.id)}>
                  <ChevronDown className={`size-3.5 transition-transform ${expandedGroupID === group.id ? "rotate-180" : ""}`} />
                </Button>
                <Button type="button" variant="outline" className="size-8 rounded-lg p-0" aria-label={`删除代理组 ${group.name}`} title="删除代理组" disabled={groupsBusy} onClick={async () => {
                  if (!window.confirm(`删除代理组“${group.name}”？`)) return;
                  setGroupsBusy(true);
                  try {
                    await deleteProxyGroup(group.id);
                    if (selectedGroupID === group.id) resetProxyGroup();
                    if (expandedGroupID === group.id) setExpandedGroupID("");
                    if (groupNotice?.id === group.id) setGroupNotice(null);
                    setGroupTestResults((current) => { const next = { ...current }; delete next[group.id]; return next; });
                    await loadProxyGroups();
                    toast.success("代理组已删除");
                  } catch (error) { toast.error(error instanceof Error ? error.message : "删除代理组失败"); }
                  finally { setGroupsBusy(false); }
                }}><Trash2 className="size-3.5" /></Button>
              </div>
            </div>
            {expandedGroupID === group.id ? (
              <div className="mt-3 border-t border-stone-200 pt-2">
                {groupNotice?.id === group.id ? <p className="mb-2 text-xs text-stone-600" role="status">{groupNotice.message}</p> : null}
                {group.nodes.map((node) => {
                  const result = groupTestResults[group.id]?.[node.id];
                  const testing = testingGroupID === group.id && node.enabled;
                  const detail = !node.enabled ? "已停用"
                    : testing ? "检测中"
                    : result?.ok ? `HTTP ${result.status} · ${result.latency_ms} ms`
                    : result ? result.error || "检测失败"
                    : node.health.state === "unhealthy" ? node.health.error || "检测失败"
                    : node.health.state === "healthy" ? `${node.health.latency_ms ?? 0} ms`
                    : "待检测";
                  const tone = testing ? "text-sky-700"
                    : !node.enabled ? "text-stone-500"
                    : result ? result.ok ? "text-emerald-700" : "text-rose-700"
                    : node.health.state === "healthy" ? "text-emerald-700"
                    : node.health.state === "unhealthy" ? "text-rose-700" : "text-stone-500";
                  return (
                    <div key={node.id} className="grid gap-1 border-t border-stone-100 py-2 text-xs first:border-t-0 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)] sm:gap-3">
                      <div className="min-w-0"><span className="block truncate font-medium text-stone-700" title={node.name}>{node.name}</span><span className="font-mono text-stone-400">{node.id}</span></div>
                      <span className={`min-w-0 break-all sm:text-right ${tone}`}>{detail}</span>
                    </div>
                  );
                })}
              </div>
            ) : null}
          </div>
        ))}
        <div className="grid gap-3 md:grid-cols-2">
          <Input value={groupName} onChange={(event) => setGroupName(event.target.value)} placeholder="代理组名称" className={INPUT_CLASS} />
          <select value={groupStrategy} onChange={(event) => setGroupStrategy(event.target.value as ProxyGroup["strategy"])} className="h-10 rounded-xl border border-stone-200 bg-white px-3 text-sm">
            <option value="round_robin">轮询</option><option value="request_random">每请求随机</option><option value="time_window">时间窗口</option>
          </select>
          <Textarea value={nodeURLs} onChange={(event) => setNodeURLs(event.target.value)} placeholder={'每行一个代理 URL\nhttp://127.0.0.1:7890\nsocks5h://127.0.0.1:1080'} className="min-h-24 rounded-xl border-stone-200 bg-white font-mono text-xs md:col-span-2" />
          <p className="text-xs text-muted-foreground md:col-span-2">SOCKS 节点支持 socks://host:port:user:pass、socks5h://host:port:user:pass，也支持标准的 socks5://user:pass@host:port。</p>
        </div>
        <div className="flex justify-end"><ProxyNodeImportDialog existingURLs={parsedNodeURLs} onApply={applyImportedNodes} /></div>
        {parsedNodeURLs.length > 0 ? (
          <div className="space-y-2">
            <div className="grid grid-cols-[minmax(0,1fr)_7rem] gap-3 text-xs text-stone-500">
              <span>节点</span><span>生图并发上限</span>
            </div>
            {parsedNodeURLs.map((url, index) => (
              <div key={`${url}-${index}`} className="grid grid-cols-[minmax(0,1fr)_7rem] items-center gap-3">
                <span className="truncate font-mono text-xs text-stone-700" title={url}>{url}</span>
                <Input
                  type="number" min={0} max={10000} step={1}
                  aria-label={`${url} 生图并发上限`}
                  value={nodeLimits[url] ?? "0"}
                  onChange={(event) => setNodeLimits((current) => ({ ...current, [url]: event.target.value }))}
                  className={INPUT_CLASS}
                />
              </div>
            ))}
            <p className={HELP_CLASS}>0 表示不限制；满额时优先使用组内其他节点。</p>
          </div>
        ) : null}
        <div className="flex justify-between">
          <Button type="button" variant="ghost" className="h-9 rounded-xl" onClick={resetProxyGroup}><Plus className="size-4" />新建代理组</Button>
          <Button type="button" className="h-9 rounded-xl bg-stone-950 text-white" onClick={() => void handleSaveProxyGroup()} disabled={groupsBusy}><Save className="size-4" />{groupsBusy ? "保存中…" : selectedGroupID ? "保存代理组" : "创建代理组"}</Button>
        </div>
      </div>
    </div>
  );
}

/* ───────────────────────── 图片 ───────────────────────── */

export function ImageSection() {
  const [isTestingStorage, setIsTestingStorage] = useState(false);
  const [isSyncingStorage, setIsSyncingStorage] = useState(false);
  const config = useSettingsStore((s) => s.config);
  const saveConfig = useSettingsStore((s) => s.saveConfig);
  const setBaseUrl = useSettingsStore((s) => s.setBaseUrl);
  const setImageRetentionDays = useSettingsStore((s) => s.setImageRetentionDays);
  const setCleanupProtectGallery = useSettingsStore((s) => s.setCleanupProtectGallery);
  const setCleanupProtectUserImages = useSettingsStore((s) => s.setCleanupProtectUserImages);
  const setImagePollTimeoutSecs = useSettingsStore((s) => s.setImagePollTimeoutSecs);
  const setImagePollIntervalSecs = useSettingsStore((s) => s.setImagePollIntervalSecs);
  const setImagePollInitialWaitSecs = useSettingsStore((s) => s.setImagePollInitialWaitSecs);
  const setImageAccountConcurrency = useSettingsStore((s) => s.setImageAccountConcurrency);
  const setImageStorageField = useSettingsStore((s) => s.setImageStorageField);

  const handleTestStorage = async () => {
    if (!config?.image_storage) return;
    setIsTestingStorage(true);
    try {
      const { result } = await testImageStorage(config.image_storage);
      if (!result.ok) throw new Error(result.error || "连接失败");
      toast.success("存储连接正常");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "连接失败");
    } finally {
      setIsTestingStorage(false);
    }
  };

  const r2Selected = config?.image_storage?.mode === "r2" || config?.image_storage?.mode === "r2_both";

  const handleSyncStorage = async () => {
    setIsSyncingStorage(true);
    try {
      if (!await saveConfig()) return;
      const { result } = await syncImageStorage();
      toast.success(`同步完成：原图上传 ${result.uploaded}，缩略图上传 ${result.thumbnails_uploaded || 0}，失败 ${result.failed + (result.thumbnails_failed || 0)}`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "同步失败");
    } finally {
      setIsSyncingStorage(false);
    }
  };

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <label className={LABEL_CLASS}>图片访问地址</label>
        <Input
          value={String(config?.base_url || "")}
          onChange={(e) => setBaseUrl(e.target.value)}
          placeholder="https://example.com"
          className={INPUT_CLASS}
        />
        <p className={HELP_CLASS}>用作生成结果 URL 的前缀。留空则按请求 host 自动推断。</p>
      </div>

      <div className="space-y-4 border-t border-stone-200 pt-5">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <label className="flex items-center gap-3 text-sm font-medium text-stone-800">
            <Checkbox checked={Boolean(config?.image_storage?.enabled)} onCheckedChange={(checked) => setImageStorageField("enabled", Boolean(checked))} />
            生图对象存储
          </label>
          <div className="flex gap-2">
            <Button variant="outline" size="sm" disabled={!(r2Selected ? config?.image_storage?.r2_endpoint : config?.image_storage?.webdav_url) || isTestingStorage} onClick={() => void handleTestStorage()}>
              {isTestingStorage ? <LoaderCircle className="size-4 animate-spin" /> : <PlugZap className="size-4" />}
              测试连接
            </Button>
            <Button variant="outline" size="sm" disabled={!config?.image_storage?.enabled || config.image_storage.mode === "local" || isSyncingStorage} onClick={() => void handleSyncStorage()}>
              {isSyncingStorage ? <LoaderCircle className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
              同步历史图片
            </Button>
          </div>
        </div>
        <div className="grid gap-4 md:grid-cols-2">
          <div className="space-y-2">
            <label className={LABEL_CLASS}>保存模式</label>
            <Select value={config?.image_storage?.mode || "local"} onValueChange={(value) => setImageStorageField("mode", value)} disabled={!config?.image_storage?.enabled}>
              <SelectTrigger className={INPUT_CLASS}><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="local">仅本机</SelectItem>
                <SelectItem value="webdav">仅 WebDAV</SelectItem>
                <SelectItem value="both">本机 + WebDAV</SelectItem>
                <SelectItem value="r2">仅 Cloudflare R2</SelectItem>
                <SelectItem value="r2_both">本机 + Cloudflare R2</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {!r2Selected && <><div className="space-y-2">
            <label className={LABEL_CLASS}>WebDAV URL</label>
            <Input value={config?.image_storage?.webdav_url || ""} onChange={(event) => setImageStorageField("webdav_url", event.target.value)} placeholder="https://example.com/dav" className={INPUT_CLASS} />
          </div>
          <div className="space-y-2">
            <label className={LABEL_CLASS}>用户名</label>
            <Input value={config?.image_storage?.webdav_username || ""} onChange={(event) => setImageStorageField("webdav_username", event.target.value)} className={INPUT_CLASS} />
          </div>
          <div className="space-y-2">
            <label className={LABEL_CLASS}>密码</label>
            <Input type="password" value={config?.image_storage?.webdav_password || ""} onChange={(event) => setImageStorageField("webdav_password", event.target.value)} className={INPUT_CLASS} />
          </div>
          <div className="space-y-2">
            <label className={LABEL_CLASS}>远端目录</label>
            <Input value={config?.image_storage?.webdav_root_path || ""} onChange={(event) => setImageStorageField("webdav_root_path", event.target.value)} placeholder="chatgpt2api/images" className={INPUT_CLASS} />
          </div>
          <div className="space-y-2">
            <label className={LABEL_CLASS}>公开访问前缀</label>
            <Input value={config?.image_storage?.public_base_url || ""} onChange={(event) => setImageStorageField("public_base_url", event.target.value)} placeholder="https://cdn.example.com/images" className={INPUT_CLASS} />
            <p className={HELP_CLASS}>留空时使用本应用的 /images/ 地址。</p>
          </div></>}
          {r2Selected && <>
            <div className="space-y-2"><label className={LABEL_CLASS}>R2 S3 Endpoint</label><Input value={config?.image_storage?.r2_endpoint || ""} onChange={(event) => setImageStorageField("r2_endpoint", event.target.value)} placeholder="https://ACCOUNT_ID.r2.cloudflarestorage.com" className={INPUT_CLASS} /></div>
            <div className="space-y-2"><label className={LABEL_CLASS}>Bucket</label><Input value={config?.image_storage?.r2_bucket || ""} onChange={(event) => setImageStorageField("r2_bucket", event.target.value)} className={INPUT_CLASS} /></div>
            <div className="space-y-2"><label className={LABEL_CLASS}>Access Key ID</label><Input value={config?.image_storage?.r2_access_key_id || ""} onChange={(event) => setImageStorageField("r2_access_key_id", event.target.value)} className={INPUT_CLASS} /></div>
            <div className="space-y-2"><label className={LABEL_CLASS}>Secret Access Key</label><Input type="password" value={config?.image_storage?.r2_secret_access_key || ""} onChange={(event) => setImageStorageField("r2_secret_access_key", event.target.value)} className={INPUT_CLASS} /></div>
            <div className="space-y-2"><label className={LABEL_CLASS}>对象前缀</label><Input value={config?.image_storage?.r2_prefix || ""} onChange={(event) => setImageStorageField("r2_prefix", event.target.value)} placeholder="images" className={INPUT_CLASS} /></div>
            <div className="space-y-2"><label className={LABEL_CLASS}>R2 公开访问前缀</label><Input value={config?.image_storage?.r2_public_url || ""} onChange={(event) => setImageStorageField("r2_public_url", event.target.value)} placeholder="https://cdn.example.com" className={INPUT_CLASS} /><p className={HELP_CLASS}>留空时由本应用读取私有 Bucket；公开地址需要已绑定的域名。</p></div>
          </>}
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <div className="space-y-2">
          <label className={LABEL_CLASS}>图片总等待超时（秒）</label>
          <Input
            value={String(config?.image_poll_timeout_secs || "")}
            onChange={(e) => setImagePollTimeoutSecs(e.target.value)}
            placeholder="120"
            className={INPUT_CLASS}
          />
          <p className={HELP_CLASS}>控制图片 SSE 等待和最终图片记录轮询的最长时间；例如 600 表示最多等待 10 分钟。</p>
        </div>
        <div className="space-y-2">
          <label className={LABEL_CLASS}>图片轮询间隔（秒）</label>
          <Input
            value={String(config?.image_poll_interval_secs || "")}
            onChange={(e) => setImagePollIntervalSecs(e.target.value)}
            placeholder="4"
            className={INPUT_CLASS}
          />
          <p className={HELP_CLASS}>等待图片任务完成时查询上游记录的间隔，调大可减少无效请求。</p>
        </div>
        <div className="space-y-2">
          <label className={LABEL_CLASS}>首次轮询等待（秒）</label>
          <Input
            value={String(config?.image_poll_initial_wait_secs || "")}
            onChange={(e) => setImagePollInitialWaitSecs(e.target.value)}
            placeholder="0"
            className={INPUT_CLASS}
          />
          <p className={HELP_CLASS}>SSE 结束后首次查询前的等待时间；网络慢或容易 429 时可设为 5-10。</p>
        </div>
        <div className="space-y-2">
          <label className={LABEL_CLASS}>单账号图片并发</label>
          <Input
            value={String(config?.image_account_concurrency || "")}
            onChange={(e) => setImageAccountConcurrency(e.target.value)}
            placeholder="3"
            className={INPUT_CLASS}
          />
          <p className={HELP_CLASS}>限制每个账号同时处理的图片请求数量，多图请求会按此上限并行。</p>
        </div>
      </div>

      <div className="space-y-3 rounded-xl border border-stone-200 bg-stone-50/60 p-4">
        <div className="space-y-2">
          <label className={LABEL_CLASS}>本地保留天数</label>
          <Input
            value={String(config?.image_retention_days || "")}
            onChange={(e) => setImageRetentionDays(e.target.value)}
            placeholder="30"
            className={INPUT_CLASS + " md:max-w-xs"}
          />
          <p className={HELP_CLASS}>
            自动删除多少天前的本地图片。下面两项保护开关默认开启，避免清理把"还在用的图"也删掉造成画廊裂图或用户作品凭空消失。
          </p>
        </div>
        <label className={`flex items-start gap-3 ${TILE_CLASS} text-sm text-stone-700`}>
          <Checkbox
            checked={Boolean(config?.cleanup_protect_gallery ?? true)}
            onCheckedChange={(c) => setCleanupProtectGallery(Boolean(c))}
          />
          <div className="space-y-1">
            <div className="font-medium">保护画廊已发布的图片</div>
            <div className="text-xs leading-5 text-stone-500">
              发布到画廊视为用户主动表示"这张图有保留价值"。关闭后画廊瓦片可能因 PNG 被删变成裂图。
            </div>
          </div>
        </label>
        <label className={`flex items-start gap-3 ${TILE_CLASS} text-sm text-stone-700`}>
          <Checkbox
            checked={Boolean(config?.cleanup_protect_user_images ?? true)}
            onCheckedChange={(c) => setCleanupProtectUserImages(Boolean(c))}
          />
          <div className="space-y-1">
            <div className="font-medium">保护用户「我的作品」</div>
            <div className="text-xs leading-5 text-stone-500">
              保留所有有归属密钥的图。匿名 / admin 自己生成的无归属图仍按 mtime 清理。关闭后所有过期图按一刀切删除。
            </div>
          </div>
        </label>
      </div>
    </div>
  );
}

/* ───────────────────────── 内容安全 ───────────────────────── */

export function SecuritySection() {
  const config = useSettingsStore((s) => s.config);
  const setGlobalSystemPrompt = useSettingsStore((s) => s.setGlobalSystemPrompt);
  const setSensitiveWordsText = useSettingsStore((s) => s.setSensitiveWordsText);

  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <label className={LABEL_CLASS}>全局附加指令</label>
        <Textarea
          value={String(config?.global_system_prompt || "")}
          onChange={(e) => setGlobalSystemPrompt(e.target.value)}
          placeholder="例如：先判断用户提示词是否合规；遇到违法、色情、暴力、仇恨等请求时拒绝回答。"
          className="min-h-28 rounded-xl border-stone-200 bg-white font-mono text-xs shadow-none"
        />
        <p className={HELP_CLASS}>
          每次请求都会作为 system 消息注入。可用于审核用户提示词、统一约束模型行为或固定角色设定。
        </p>
      </div>
      <div className="space-y-2">
        <label className={LABEL_CLASS}>敏感词</label>
        <Textarea
          value={(config?.sensitive_words || []).join("\n")}
          onChange={(e) => setSensitiveWordsText(e.target.value)}
          placeholder="一行一个，命中即拒绝"
          className="min-h-28 rounded-xl border-stone-200 bg-white font-mono text-xs shadow-none"
        />
        <p className={HELP_CLASS}>用户请求包含任意敏感词时直接返回拒绝，不再下发到生图账号。</p>
      </div>
    </div>
  );
}

/* ───────────────────────── AI 审核 ───────────────────────── */

export function AIReviewSection() {
  const config = useSettingsStore((s) => s.config);
  const setAIReviewField = useSettingsStore((s) => s.setAIReviewField);

  return (
    <div className="space-y-4">
      <label className="flex items-center gap-3 text-sm text-stone-700">
        <Checkbox
          checked={Boolean(config?.ai_review?.enabled)}
          onCheckedChange={(c) => setAIReviewField("enabled", Boolean(c))}
        />
        启用 AI 审核
      </label>
      <p className="text-xs leading-6 text-stone-500">
        开启后会在请求进入生图账号前先调用审核模型，审核不通过会直接拒绝，减少违规提示词触达账号造成风控或封号的风险。
      </p>
      <div className="grid gap-4 md:grid-cols-3">
        <div className="space-y-2">
          <label className={LABEL_CLASS}>Base URL</label>
          <Input
            value={String(config?.ai_review?.base_url || "")}
            onChange={(e) => setAIReviewField("base_url", e.target.value)}
            placeholder="https://api.openai.com"
            className={INPUT_CLASS}
          />
        </div>
        <div className="space-y-2">
          <label className={LABEL_CLASS}>API Key</label>
          <Input
            value={String(config?.ai_review?.api_key || "")}
            onChange={(e) => setAIReviewField("api_key", e.target.value)}
            placeholder="sk-..."
            className={INPUT_CLASS}
          />
        </div>
        <div className="space-y-2">
          <label className={LABEL_CLASS}>Model</label>
          <Input
            value={String(config?.ai_review?.model || "")}
            onChange={(e) => setAIReviewField("model", e.target.value)}
            placeholder="gpt-4o-mini"
            className={INPUT_CLASS}
          />
        </div>
      </div>
      <div className="space-y-2">
        <label className={LABEL_CLASS}>审核提示词</label>
        <Textarea
          value={String(config?.ai_review?.prompt || "")}
          onChange={(e) => setAIReviewField("prompt", e.target.value)}
          placeholder="判断用户请求是否允许。只回答 ALLOW 或 REJECT。"
          className="min-h-24 rounded-xl border-stone-200 bg-white text-xs shadow-none"
        />
      </div>
    </div>
  );
}

/* ───────────────────────── 日志 ───────────────────────── */

export function LogSection() {
  const config = useSettingsStore((s) => s.config);
  const setLogLevel = useSettingsStore((s) => s.setLogLevel);
  const logLevelOptions = ["debug", "info", "warning", "error"];

  return (
    <div className="space-y-3">
      <label className={LABEL_CLASS}>控制台日志级别</label>
      <p className={HELP_CLASS}>不选择时使用默认 info / warning / error。</p>
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {logLevelOptions.map((level) => (
          <label
            key={level}
            className={`flex items-center gap-2 ${TILE_CLASS} text-sm capitalize text-stone-700`}
          >
            <Checkbox
              checked={Boolean(config?.log_levels?.includes(level))}
              onCheckedChange={(c) => setLogLevel(level, Boolean(c))}
            />
            {level}
          </label>
        ))}
      </div>
    </div>
  );
}
