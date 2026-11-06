"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import {
  Ban,
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  CircleAlert,
  CircleOff,
  Copy,
  Download,
  LayoutGrid,
  KeyRound,
  LogIn,
  LoaderCircle,
  MoreHorizontal,
  Pencil,
  RefreshCw,
  Rows3,
  Search,
  Trash2,
  UserRound,
  UsersRound,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  deleteAccounts,
  fetchAccountGroups,
  fetchAccountRefreshProgress,
  fetchAccountRepairProgress,
  fetchAccounts,
  fetchAccountMFACode,
  fetchModels,
  fetchProxyGroups,
  loginAbnormalAccountWithMFA,
  setupAccountMFA,
  startAccountRefresh,
  startAccountRepair,
  updateAccount,
  updateAccountsGroup,
  updateAccountsStatus,
  type Account,
  type AccountGroup,
  type AccountRefreshProgress,
  type AccountStatus,
  type Model,
  type PendingRegisterAccount,
  type ProxyGroup,
} from "@/lib/api";
import { useAuthGuard } from "@/lib/use-auth-guard";
import { cn } from "@/lib/utils";

import { AccountImportDialog } from "./components/account-import-dialog";
import { AccountGroupsDialog } from "./components/account-groups-dialog";
import { CodexAuthBatchDialog } from "./components/codex-auth-batch-dialog";
import { PendingRegistration } from "./components/pending-registration";

type AccountRow = Account | PendingRegisterAccount;
function isPendingRow(row: AccountRow): row is PendingRegisterAccount {
  return !("access_token" in row);
}

const accountStatusOptions: { label: string; value: AccountStatus | "all" }[] = [
  { label: "全部状态", value: "all" },
  { label: "正常", value: "正常" },
  { label: "限流", value: "限流" },
  { label: "异常", value: "异常" },
  { label: "封号", value: "封号" },
  { label: "禁用", value: "禁用" },
];

const trialFilterOptions = [
  { label: "全部优惠", value: "all" },
  { label: "有优惠资格", value: "offer" },
  { label: "一个月免费", value: "eligible" },
  { label: "折扣券", value: "discount" },
  { label: "无优惠", value: "not-eligible" },
  { label: "已订阅", value: "subscribed" },
  { label: "未判定", value: "unknown" },
] as const;

const metricCards = [
  { key: "total", label: "账户总数", color: "text-stone-900", icon: UserRound },
  { key: "active", label: "正常账户", color: "text-emerald-600", icon: CheckCircle2 },
  { key: "limited", label: "限流账户", color: "text-orange-500", icon: CircleAlert },
  { key: "abnormal", label: "异常账户", color: "text-rose-500", icon: CircleOff },
  { key: "banned", label: "封号账户", color: "text-red-700", icon: Ban },
  { key: "disabled", label: "禁用账户", color: "text-stone-500", icon: Ban },
  { key: "quota", label: "剩余额度", color: "text-blue-500", icon: RefreshCw },
] as const;

function isUnlimitedImageQuotaAccount(account: Account) {
  return account.type === "pro" || account.type === "prolite";
}

function imageQuotaUnknown(account: Account) {
  return Boolean(account.image_quota_unknown);
}

function formatCompact(value: number) {
  if (value >= 1000) {
    return `${(value / 1000).toFixed(1)}k`;
  }
  return String(value);
}

function formatQuota(account: Account) {
  if (isUnlimitedImageQuotaAccount(account)) {
    return "∞";
  }
  if (imageQuotaUnknown(account)) {
    return "未知";
  }
  return String(Math.max(0, account.quota));
}

function formatRestoreAt(value?: string | null) {
  if (!value) {
    return { absolute: "—", relative: "" };
  }

  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return { absolute: value, relative: "" };
  }

  const diffMs = Math.max(0, date.getTime() - Date.now());
  const totalHours = Math.ceil(diffMs / (1000 * 60 * 60));
  const days = Math.floor(totalHours / 24);
  const hours = totalHours % 24;
  const relative = diffMs > 0 ? `剩余 ${days}d ${hours}h` : "已到恢复时间";

  const pad = (num: number) => String(num).padStart(2, "0");
  const absolute = `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(
    date.getHours(),
  )}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;

  return { absolute, relative };
}

function formatDateTime(value?: string | null) {
  if (!value) return "—";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const pad = (num: number) => String(num).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function formatQuotaSummary(accounts: Account[]) {
  const availableAccounts = accounts.filter((account) => account.status === "正常");
  if (availableAccounts.some(isUnlimitedImageQuotaAccount)) {
    return "∞";
  }
  if (availableAccounts.some(imageQuotaUnknown)) {
    return "未知";
  }
  return formatCompact(availableAccounts.reduce((sum, account) => sum + Math.max(0, account.quota), 0));
}

function maskToken(token?: string) {
  if (!token) return "—";
  if (token.length <= 18) return token;
  return `${token.slice(0, 16)}...${token.slice(-8)}`;
}

function downloadAccounts(accounts: Account[], format: "txt" | "json") {
  const content = format === "json"
    ? JSON.stringify(accounts, null, 2)
    : `${accounts.map((account) => account.access_token).join("\n")}\n`;
  const blob = new Blob([content], { type: format === "json" ? "application/json" : "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `accounts-${Date.now()}.${format}`;
  link.click();
  URL.revokeObjectURL(url);
}

function displayAccountType(account: Account) {
  return account.type || "Free";
}

function displayAccountSource(account: Account) {
  return String(account.source_type || "web").toLowerCase() === "codex" ? "Codex" : "Web";
}

function proxyAssignmentLabel(assignment: string, groups: ProxyGroup[]) {
  if (!assignment) return "默认出口";
  if (assignment === "direct") return "直连";
  if (assignment.startsWith("group:")) {
    const groupID = assignment.slice(6);
    return groups.find((group) => group.id === groupID)?.name ?? "代理组已移除";
  }
  return "自定义代理";
}

function accountProxyLabel(account: Account, groups: ProxyGroup[], accountGroups: AccountGroup[]) {
  const group = accountGroups.find((item) => item.id === account.group_id);
  const assignment = account.proxy?.trim() || (group?.enabled ? group.proxy : "");
  const label = proxyAssignmentLabel(assignment ?? "", groups);
  return group ? `${group.name} · ${label}` : account.group_id ? `账号组已移除 · ${label}` : label;
}

function TrialBadge({ account }: { account: Account }) {
  if (!account.trial_state) return null;
  const label = account.trial_promo_label || (account.trial_state === "unknown" ? "优惠未判定" : account.trial_state === "subscribed" ? "已订阅" : "无优惠");
  return (
    <Badge
      variant="outline"
      title={account.trial_checked_at ? `优惠检查：${formatDateTime(account.trial_checked_at)}` : undefined}
      className={cn(
        "rounded-md font-medium",
        account.trial_state === "eligible" && "border-emerald-200 bg-emerald-50 text-emerald-700",
        account.trial_state === "discount" && "border-amber-200 bg-amber-50 text-amber-700",
        account.trial_state === "unknown" && "text-muted-foreground",
      )}
    >
      {label}
    </Badge>
  );
}

const TYPE_BADGE_CLASS: Record<string, string> = {
  Free: "border-stone-200 bg-stone-50 text-stone-700",
  Plus: "border-blue-200 bg-blue-50 text-blue-700",
  pro: "border-violet-200 bg-violet-50 text-violet-700",
  prolite: "border-emerald-200 bg-emerald-50 text-emerald-700",
};

const SOURCE_BADGE_CLASS: Record<"Web" | "Codex", string> = {
  Web: "border-stone-200 bg-stone-50 text-stone-600",
  Codex: "border-cyan-200 bg-cyan-50 text-cyan-700",
};

const STATUS_BADGE_CLASS: Record<AccountStatus, string> = {
  正常: "border-emerald-200 bg-emerald-50 text-emerald-700",
  限流: "border-amber-200 bg-amber-50 text-amber-700",
  异常: "border-rose-200 bg-rose-50 text-rose-700",
  封号: "border-red-300 bg-red-100 text-red-800",
  禁用: "border-stone-200 bg-stone-100 text-stone-600",
};

function quotaReferenceBaseline(account: Account) {
  const baseline = account.image_quota_baseline;
  return typeof baseline === "number" && Number.isFinite(baseline) && baseline >= 0
    ? Math.max(baseline, account.quota)
    : Math.max(0, account.quota);
}

function accountWindowStats(account: Account, now: number) {
  if (!account.window_started_at) return null;
  const resetAt = account.window_reset_at ? Date.parse(account.window_reset_at) : NaN;
  if (Number.isFinite(resetAt) && now >= resetAt) {
    return { success: 0, fail: 0 };
  }
  return { success: Math.max(0, account.window_success ?? 0), fail: Math.max(0, account.window_fail ?? 0) };
}

function PillProgress({ percent, segments = 15 }: { percent: number; segments?: number }) {
  const clamped = Math.max(0, Math.min(100, percent));
  const filled = Math.round((clamped / 100) * segments);
  return (
    <div className="flex gap-[3px]" role="progressbar" aria-label="参考剩余额度" aria-valuemin={0} aria-valuemax={100} aria-valuenow={clamped}>
      {Array.from({ length: segments }).map((_, i) => (
        <span key={i} className={cn("h-2 flex-1 rounded-full transition-colors", i < filled ? "bg-emerald-500" : "bg-amber-400/75")} />
      ))}
    </div>
  );
}

function HealthPills({
  success,
  fail,
  segments = 14,
}: {
  success: number;
  fail: number;
  segments?: number;
}) {
  const total = success + fail;
  const successCount = total > 0 ? Math.round((success / total) * segments) : 0;
  const failCount = total > 0 ? segments - successCount : 0;
  const idleCount = total > 0 ? 0 : segments;
  return (
    <div className="flex gap-[3px]" aria-label={`成功 ${success} / 失败 ${fail}`}>
      {Array.from({ length: successCount }).map((_, i) => (
        <span key={`s${i}`} className="h-1.5 flex-1 rounded-full bg-emerald-500/85" />
      ))}
      {Array.from({ length: failCount }).map((_, i) => (
        <span key={`f${i}`} className="h-1.5 flex-1 rounded-full bg-rose-500/80" />
      ))}
      {Array.from({ length: idleCount }).map((_, i) => (
        <span key={`i${i}`} className="h-1.5 flex-1 rounded-full bg-secondary" />
      ))}
    </div>
  );
}

function AccountCard({
  account,
  statsNow,
  proxyLabel,
  selected,
  onSelectChange,
  onEdit,
  onRefresh,
  onMFALogin,
  onMFASetup,
  onToggleStatus,
  onDelete,
  onCopyToken,
  onCopyCredentials,
  onCopyMFACode,
  isRefreshing,
  isMFALoggingIn,
  isMFASettingUp,
  isDeleting,
  isUpdating,
}: {
  account: Account;
  statsNow: number;
  proxyLabel: string;
  selected: boolean;
  onSelectChange: (v: boolean) => void;
  onEdit: () => void;
  onRefresh: () => void;
  onMFALogin: () => void;
  onMFASetup: () => void;
  onToggleStatus: () => void;
  onDelete: () => void;
  onCopyToken: () => void;
  onCopyCredentials: () => void;
  onCopyMFACode: () => void;
  isRefreshing: boolean;
  isMFALoggingIn: boolean;
  isMFASettingUp: boolean;
  isDeleting: boolean;
  isUpdating: boolean;
}) {
  const typeName = displayAccountType(account);
  const sourceName = displayAccountSource(account);
  const typeAvatar = (typeName[0] || "?").toUpperCase();
  const isUnlimited = isUnlimitedImageQuotaAccount(account);
  const isUnknown = imageQuotaUnknown(account);
  const maxQuota = quotaReferenceBaseline(account);
  const remainingPct = isUnlimited ? 100 : isUnknown || maxQuota === 0 ? 0 : Math.max(0, Math.min(100, Math.round((account.quota / maxQuota) * 100)));
  const restore = formatRestoreAt(account.restore_at);
  const windowStats = accountWindowStats(account, statsNow);

  return (
    <div
      className={cn(
        "group flex flex-col rounded-lg border border-border bg-card p-4 transition hover:border-primary/40",
        selected && "border-primary/60 ring-1 ring-primary/20",
      )}
    >
      <div className="flex items-center gap-3">
        <Checkbox
          checked={selected}
          onCheckedChange={(c) => onSelectChange(Boolean(c))}
        />
        <div className="grid size-9 shrink-0 place-items-center rounded-full bg-gradient-to-br from-primary/25 to-primary/5 text-sm font-semibold text-primary">
          {typeAvatar}
        </div>
        <div className="flex flex-1 flex-wrap items-center gap-1.5">
          <Badge
            variant="outline"
            className={cn("rounded-md font-medium", TYPE_BADGE_CLASS[typeName] ?? TYPE_BADGE_CLASS.Free)}
          >
            {typeName}
          </Badge>
          <Badge
            variant="outline"
            className={cn("rounded-md font-medium", SOURCE_BADGE_CLASS[sourceName])}
          >
            {sourceName}
          </Badge>
          <Badge
            variant="outline"
            className={cn("rounded-md font-medium", STATUS_BADGE_CLASS[account.status])}
          >
            {account.status}
          </Badge>
          <TrialBadge account={account} />
          {account.is_signup_token ? <Badge variant="outline">注册令牌</Badge> : null}
          {account.mfa_status ? (
            <Badge variant="outline" title={account.mfa_status === "pending" ? "验证码确认尚未成功" : undefined}>
              2FA {account.mfa_status === "enabled" ? "已启用" : account.mfa_status === "pending" ? "待确认" : "失败"}
            </Badge>
          ) : null}
          <Badge variant="outline" className={account.refresh_token ? "border-emerald-200 text-emerald-700" : "text-muted-foreground"}>
            RT {account.refresh_token ? "已保存" : "缺失"}
          </Badge>
        </div>
      </div>

      <div className="mt-3 truncate text-[15px] font-semibold text-foreground" title={account.email ?? ""}>
        {account.email ?? "—"}
      </div>

      <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground">
        <span>出口：{proxyLabel}</span>
        <span className="inline-flex items-center gap-1">
          <span>Token</span>
          <span className="font-data text-foreground/80">{maskToken(account.access_token)}</span>
        </span>
        {account.password ? <span>密码已保存</span> : null}
        <button type="button" onClick={onCopyCredentials} title="复制账号----密码----2FA 密钥" className="inline-flex items-center gap-1 hover:text-foreground">
          <Copy className="size-3" /> 复制账号凭据
        </button>
        {account.mfa_status === "enabled" ? (
          <button type="button" onClick={onCopyMFACode} title="复制当前 2FA 验证码" className="inline-flex items-center gap-1 hover:text-foreground">
            <KeyRound className="size-3" /> 验证码
          </button>
        ) : null}
        {restore.absolute !== "—" ? (
          <span className="inline-flex items-center gap-1">
            <span>恢复</span>
            <span className="font-data text-foreground/80">{restore.absolute}</span>
          </span>
        ) : null}
      </div>

      <div className="mt-2.5 flex items-center gap-3 text-xs" title="当前额度窗口内的调用统计">
        <span className="inline-flex items-center gap-1">
          <span className="size-1.5 rounded-full bg-emerald-500" />
          <span className="text-muted-foreground">成功</span>
          <span className="font-data tabular-nums font-semibold text-emerald-600">{windowStats?.success ?? "—"}</span>
        </span>
        <span className="inline-flex items-center gap-1">
          <span className="size-1.5 rounded-full bg-rose-500" />
          <span className="text-muted-foreground">失败</span>
          <span className="font-data tabular-nums font-semibold text-rose-500">{windowStats?.fail ?? "—"}</span>
        </span>
      </div>

      <div className="mt-3 space-y-1.5">
        <div className="font-data text-[10px] font-semibold tracking-[0.18em] uppercase text-muted-foreground" title="按当前额度窗口内的成功和失败次数计算">
          健康状态
        </div>
        <HealthPills success={windowStats?.success ?? 0} fail={windowStats?.fail ?? 0} />
      </div>

      <div className="mt-3 space-y-1.5">
        <div className="flex items-baseline justify-between">
          <span className="font-data text-[10px] font-semibold tracking-[0.18em] uppercase text-muted-foreground">
            剩余额度
          </span>
          <span className="font-data tabular-nums text-xs">
            {isUnlimited ? (
              <span className="font-semibold text-emerald-600">∞</span>
            ) : isUnknown ? (
              <span className="text-muted-foreground">未知</span>
            ) : (
              <span title="参考百分比：以同一重置窗口内读取到的最高剩余次数为基准；上游未提供真实总额度。">
                <span className="font-semibold text-foreground">{remainingPct}%</span>
                <span className="mx-1 text-muted-foreground">·</span>
                <span className="text-muted-foreground">{account.quota}/{maxQuota}</span>
              </span>
            )}
          </span>
        </div>
        {!isUnknown || isUnlimited ? <PillProgress percent={remainingPct} /> : null}
      </div>

      <div className="mt-4 flex items-center gap-0.5 border-t border-border/60 pt-2.5 text-muted-foreground">
        <button
          type="button"
          className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground"
          onClick={onCopyToken}
          title="复制 Token"
        >
          <Copy className="size-4" />
        </button>
        <button
          type="button"
          className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground disabled:opacity-50"
          onClick={onEdit}
          disabled={isUpdating}
          title="编辑账号"
        >
          <Pencil className="size-4" />
        </button>
        <button
          type="button"
          className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground disabled:opacity-50"
          onClick={onRefresh}
          disabled={isRefreshing}
          title="刷新额度"
        >
          <RefreshCw className={cn("size-4", isRefreshing && "animate-spin")} />
        </button>
        {account.status === "异常" && account.mfa_secret ? (
          <button type="button" className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground disabled:opacity-50" onClick={onMFALogin} disabled={isMFALoggingIn || !account.email || !account.password} title={!account.email || !account.password ? "缺少邮箱或登录密码" : "使用密码和 2FA 尝试登录"} aria-label="使用密码和 2FA 尝试登录">
            {isMFALoggingIn ? <LoaderCircle className="size-4 animate-spin" /> : <LogIn className="size-4" />}
          </button>
        ) : null}
        {(account.mfa_status === "failed" || account.mfa_status === "pending") ? (
          <button type="button" className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground disabled:opacity-50" onClick={onMFASetup} disabled={isMFASettingUp} title="补设验证器 2FA" aria-label="补设验证器 2FA">
            {isMFASettingUp ? <LoaderCircle className="size-4 animate-spin" /> : <KeyRound className="size-4" />}
          </button>
        ) : null}
        <button
          type="button"
          className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground disabled:opacity-50"
          onClick={onToggleStatus}
          disabled={isUpdating}
          title={account.status === "禁用" ? "启用账号" : "禁用账号"}
          aria-label={account.status === "禁用" ? "启用账号" : "禁用账号"}
        >
          {account.status === "禁用" ? <CheckCircle2 className="size-4" /> : <Ban className="size-4" />}
        </button>
        <button
          type="button"
          className="ml-auto cursor-pointer rounded-md p-1.5 transition hover:bg-rose-50 hover:text-rose-500 disabled:opacity-50"
          onClick={onDelete}
          disabled={isDeleting}
          title="删除"
        >
          <Trash2 className="size-4" />
        </button>
      </div>
    </div>
  );
}

// 模块级缓存。号池页表格高度可观，重新挂载时若从 [] 起跳，
// 会出现塌缩→撑回的视觉跳动；命中缓存时直接给出已有数据并后台静默刷新。
let cachedAccounts: Account[] | null = null;

function AccountsPageContent() {
  const didLoadRef = useRef(false);
  const accountsLoadVersionRef = useRef(0);
  // 命中模块级缓存时直接拿来当初始 state，避免再次切到号池页时
  // accounts 从 [] 起跳、isLoading=true 让大表格塌缩成 spinner 再撑回。
  const [accounts, setAccountsState] = useState<Account[]>(() => cachedAccounts ?? []);
  const [statsNow, setStatsNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setStatsNow(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, []);
  const [availableModels, setAvailableModels] = useState<Model[]>([]);
  const [isLoadingModels, setIsLoadingModels] = useState(true);
  const [modelsError, setModelsError] = useState("");
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [pendingRegistrations, setPendingRegistrations] = useState<PendingRegisterAccount[]>([]);
  const [selectedPendingEmails, setSelectedPendingEmails] = useState<string[]>([]);
  const [query, setQuery] = useState("");
  const [typeFilter, setTypeFilter] = useState("all");
  const [statusFilter, setStatusFilter] = useState<AccountStatus | "all">("all");
  const [trialFilter, setTrialFilter] = useState<(typeof trialFilterOptions)[number]["value"]>("all");
  const [groupFilter, setGroupFilter] = useState("all");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState("10");
  const [editingAccount, setEditingAccount] = useState<Account | null>(null);
  const [editStatus, setEditStatus] = useState<AccountStatus>("正常");
  const [proxyGroups, setProxyGroups] = useState<ProxyGroup[]>([]);
  const [accountGroups, setAccountGroups] = useState<AccountGroup[]>([]);
  const [editAccountGroupID, setEditAccountGroupID] = useState("");
  const [editPassword, setEditPassword] = useState("");
  const [editMFASecret, setEditMFASecret] = useState("");
  const [editProxyMode, setEditProxyMode] = useState<"inherit" | "direct" | "group" | "custom">("inherit");
  const [editProxyGroupID, setEditProxyGroupID] = useState("");
  const [editProxyURL, setEditProxyURL] = useState("");
  const [isLoading, setIsLoading] = useState(() => cachedAccounts === null);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [mfaLoginToken, setMFALoginToken] = useState<string | null>(null);
  const [mfaSetupToken, setMFASetupToken] = useState<string | null>(null);
  const [refreshProgress, setRefreshProgress] = useState<AccountRefreshProgress | null>(null);
  const [activeRefreshId, setActiveRefreshId] = useState<string | null>(null);
  const [repairProgress, setRepairProgress] = useState<AccountRefreshProgress | null>(null);
  const [activeRepairId, setActiveRepairId] = useState<string | null>(null);
  const [isStartingRepair, setIsStartingRepair] = useState(false);
  const [isDeleting, setIsDeleting] = useState(false);
  const [isUpdating, setIsUpdating] = useState(false);
  const [isBatchUpdating, setIsBatchUpdating] = useState(false);
  const [groupMenuOpen, setGroupMenuOpen] = useState(false);
  const [viewMode, setViewMode] = useState<"list" | "grid">("list");
  // 进度条最小显示时长：即便接口几十 ms 就返回，进度条也至少撑过一次完整滑动，
  // 避免出现"按钮 disabled 闪一下、进度条一闪而过"的视觉抖动。
  const [showProgress, setShowProgress] = useState(false);

  // 写 accounts 同步刷新缓存。
  const setAccounts = (next: Account[]) => {
    cachedAccounts = next;
    setAccountsState(next);
  };

  const copyMFACode = async (account: Account) => {
    try {
      const result = await fetchAccountMFACode(account.access_token);
      await navigator.clipboard.writeText(result.code);
      toast.success(`2FA 验证码已复制，${result.expires_in} 秒后更新`);
    } catch {
      toast.error("获取 2FA 验证码失败");
    }
  };

  const copyCredentials = async (account: Account) => {
    try {
      await navigator.clipboard.writeText(`${account.email ?? ""}----${account.password ?? ""}----${account.mfa_secret ?? ""}`);
      toast.success("账号凭据已复制");
    } catch {
      toast.error("复制账号凭据失败，请检查浏览器剪贴板权限");
    }
  };

  // 持久化视图偏好
  useEffect(() => {
    const saved = window.localStorage.getItem("accounts.viewMode");
    if (saved === "list" || saved === "grid") {
      setViewMode(saved);
    } else if (window.matchMedia("(max-width: 639px)").matches) {
      setViewMode("grid");
    }
  }, []);

  useEffect(() => {
    const savedId = window.sessionStorage.getItem("accounts.refreshProgressId");
    if (savedId) {
      setActiveRefreshId(savedId);
      setIsRefreshing(true);
    }
  }, []);

  useEffect(() => {
    const savedId = window.sessionStorage.getItem("accounts.repairProgressId");
    if (savedId) setActiveRepairId(savedId);
  }, []);

  useEffect(() => {
    if (!activeRepairId) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      try {
        const progress = await fetchAccountRepairProgress(activeRepairId);
        if (stopped) return;
        setRepairProgress(progress);
        if (progress.done) {
          window.sessionStorage.removeItem("accounts.repairProgressId");
          setActiveRepairId(null);
          void loadAccounts(true, true);
          if (progress.failed > 0) toast.error(`修复成功 ${progress.refreshed} 个，失败 ${progress.failed} 个`);
          else toast.success(`修复成功 ${progress.refreshed} 个账号`);
          return;
        }
        timer = setTimeout(() => void poll(), 3000);
      } catch (error) {
        if (stopped) return;
        if (error instanceof Error && error.message === "progress not found") {
          window.sessionStorage.removeItem("accounts.repairProgressId");
          setActiveRepairId(null);
          setRepairProgress(null);
          toast.error("修复任务已失效，请重新发起");
          return;
        }
        timer = setTimeout(() => void poll(), 5000);
      }
    };
    void poll();
    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
    };
  }, [activeRepairId]);

  useEffect(() => {
    if (!activeRefreshId) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      try {
        const progress = await fetchAccountRefreshProgress(activeRefreshId);
        if (stopped) return;
        setRefreshProgress(progress);
        if (progress.done) {
          window.sessionStorage.removeItem("accounts.refreshProgressId");
          setActiveRefreshId(null);
          setIsRefreshing(false);
          void loadAccounts(true, true);
          if (progress.failed > 0) {
            toast.error(`刷新成功 ${progress.refreshed} 个，失败 ${progress.failed} 个`);
          } else {
            toast.success(`刷新成功 ${progress.refreshed} 个账户`);
          }
          return;
        }
        timer = setTimeout(() => void poll(), 3000);
      } catch (error) {
        if (stopped) return;
        const message = error instanceof Error ? error.message : "查询刷新进度失败";
        if (message === "progress not found") {
          window.sessionStorage.removeItem("accounts.refreshProgressId");
          setActiveRefreshId(null);
          setIsRefreshing(false);
          setRefreshProgress(null);
          void loadAccounts(true, true);
          toast.error("刷新任务已失效，请重新发起");
          return;
        }
        timer = setTimeout(() => void poll(), 5000);
      }
    };
    void poll();
    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
    };
  }, [activeRefreshId]);

  const loadAccounts = async (silent = false, reportError = false) => {
    const version = ++accountsLoadVersionRef.current;
    if (!silent) {
      setIsLoading(true);
    }
    try {
      const data = await fetchAccounts();
      if (version !== accountsLoadVersionRef.current) return;
      setAccounts(data.items);
      const pending = data.pending_registrations ?? [];
      setPendingRegistrations(pending);
      setSelectedPendingEmails((prev) => prev.filter((email) => pending.some((item) => item.email === email)));
      setSelectedIds((prev) => prev.filter((id) => data.items.some((item) => item.access_token === id)));
    } catch (error) {
      if (version !== accountsLoadVersionRef.current) return;
      const message = error instanceof Error ? error.message : "加载账户失败";
      if (!silent || reportError) toast.error(`加载账户列表失败：${message}`);
    } finally {
      if (version === accountsLoadVersionRef.current) {
        setIsLoading(false);
      }
    }
  };

  const loadModels = async () => {
    setIsLoadingModels(true);
    setModelsError("");
    try {
      const data = await fetchModels();
      setAvailableModels(Array.isArray(data.data) ? data.data.filter((model) => Boolean(model.id)) : []);
    } catch (error) {
      setModelsError(error instanceof Error ? error.message : "加载模型列表失败");
    } finally {
      setIsLoadingModels(false);
    }
  };

  const copyModel = async (model: string) => {
    try {
      await navigator.clipboard.writeText(model);
      toast.success("模型名已复制");
    } catch {
      toast.error("复制模型名失败");
    }
  };

  // 进度条状态机：任一操作中立刻显示，操作结束后再保留 1.2s 让滑动动画走完一程，
  // 避免缓存命中（几十毫秒返回）时进度条一闪而过。
  useEffect(() => {
    const isActive = isLoading || isRefreshing || isDeleting;
    if (isActive) {
      setShowProgress(true);
      return;
    }
    const timer = setTimeout(() => setShowProgress(false), 1200);
    return () => clearTimeout(timer);
  }, [isLoading, isRefreshing, isDeleting]);

  useEffect(() => {
    if (didLoadRef.current) {
      return;
    }
    didLoadRef.current = true;
    // 已有缓存：后台静默刷新，不让大表格塌缩。
    void loadAccounts(cachedAccounts !== null);
    void loadModels();
  }, []);

  useEffect(() => {
    void fetchProxyGroups().then((data) => setProxyGroups(data.groups)).catch(() => toast.error("加载代理组失败"));
    void fetchAccountGroups().then((data) => setAccountGroups(data.groups)).catch(() => toast.error("加载账号组失败"));
  }, []);

  const filteredPoolAccounts = useMemo(() => {
    const normalizedQuery = query.trim().toLowerCase();
    return accounts.filter((account) => {
      const searchMatched = normalizedQuery.length === 0 || [
        account.email,
        account.user_id,
        account.access_token,
        displayAccountType(account),
        displayAccountSource(account),
      ].some((value) => value?.toLowerCase().includes(normalizedQuery));
      const typeMatched = typeFilter === "all" || displayAccountType(account) === typeFilter;
      const statusMatched = statusFilter === "all" || account.status === statusFilter;
      const trialState = account.trial_state ?? "unknown";
      const trialMatched = trialFilter === "all" ||
        (trialFilter === "offer" ? trialState === "eligible" || trialState === "discount" : trialState === trialFilter);
      const groupMatched = groupFilter === "all" || (groupFilter === "none" ? !account.group_id : account.group_id === groupFilter);
      return searchMatched && typeMatched && statusMatched && trialMatched && groupMatched;
    });
  }, [accounts, query, statusFilter, typeFilter, trialFilter, groupFilter]);

  const filteredAccounts = useMemo<AccountRow[]>(() => {
    const normalizedQuery = query.trim().toLowerCase();
    const pending = pendingRegistrations.filter((item) =>
      (statusFilter === "all" || statusFilter === "异常") &&
      (typeFilter === "all" || typeFilter === "注册未完成") &&
      (trialFilter === "all" || trialFilter === "unknown") &&
      (groupFilter === "all" || groupFilter === "none") &&
      (!normalizedQuery || [item.email, item.state, "注册未完成"].some((value) => value.toLowerCase().includes(normalizedQuery)))
    );
    return [...pending, ...filteredPoolAccounts];
  }, [filteredPoolAccounts, pendingRegistrations, query, statusFilter, typeFilter, trialFilter, groupFilter]);

  const pageCount = Math.max(1, Math.ceil(filteredAccounts.length / Number(pageSize)));
  const safePage = Math.min(page, pageCount);
  const startIndex = (safePage - 1) * Number(pageSize);
  const currentRows = filteredAccounts.slice(startIndex, startIndex + Number(pageSize));
  const allCurrentSelected =
    currentRows.length > 0 && currentRows.every((row) => isPendingRow(row) ? selectedPendingEmails.includes(row.email) : selectedIds.includes(row.access_token));

  const summary = useMemo(() => {
    const total = accounts.length + pendingRegistrations.length;
    const active = accounts.filter((item) => item.status === "正常").length;
    const limited = accounts.filter((item) => item.status === "限流").length;
    const abnormal = accounts.filter((item) => item.status === "异常").length + pendingRegistrations.length;
    const banned = accounts.filter((item) => item.status === "封号").length;
    const disabled = accounts.filter((item) => item.status === "禁用").length;
    const quota = formatQuotaSummary(accounts);

    return { total, active, limited, abnormal, banned, disabled, quota };
  }, [accounts, pendingRegistrations]);

  const accountTypeOptions = useMemo(
    () => [
      { label: "全部类型", value: "all" },
      ...(pendingRegistrations.length ? [{ label: "注册未完成", value: "注册未完成" }] : []),
      ...Array.from(new Set(accounts.map(displayAccountType))).map((type) => ({ label: type, value: type })),
    ],
    [accounts, pendingRegistrations],
  );

  const selectedTokens = useMemo(() => {
    const selectedSet = new Set(selectedIds);
    return accounts.filter((item) => selectedSet.has(item.access_token)).map((item) => item.access_token);
  }, [accounts, selectedIds]);

  const selectedAccounts = useMemo(() => {
    const selectedSet = new Set(selectedIds);
    return accounts.filter((item) => selectedSet.has(item.access_token));
  }, [accounts, selectedIds]);

  const selectedAbnormalTokens = useMemo(() => selectedAccounts.filter((item) => item.status === "异常").map((item) => item.access_token), [selectedAccounts]);

  const refreshAllTokens = useMemo(() => accounts.filter((item) => item.status !== "异常" && item.status !== "封号").map((item) => item.access_token), [accounts]);

  const abnormalTokens = useMemo(() => {
    return accounts.filter((item) => item.status === "异常").map((item) => item.access_token);
  }, [accounts]);

  const paginationItems = useMemo(() => {
    const items: (number | "...")[] = [];
    const start = Math.max(1, safePage - 1);
    const end = Math.min(pageCount, safePage + 1);

    if (start > 1) items.push(1);
    if (start > 2) items.push("...");
    for (let current = start; current <= end; current += 1) items.push(current);
    if (end < pageCount - 1) items.push("...");
    if (end < pageCount) items.push(pageCount);

    return items;
  }, [pageCount, safePage]);

  const handleDeleteTokens = async (tokens: string[]) => {
    if (tokens.length === 0) {
      toast.error("请先选择要删除的账户");
      return;
    }

    setIsDeleting(true);
    try {
      const data = await deleteAccounts(tokens);
      setAccounts(data.items);
      setSelectedIds((prev) => prev.filter((id) => data.items.some((item) => item.access_token === id)));
      toast.success(`删除 ${data.removed ?? 0} 个账户`);
    } catch (error) {
      const message = error instanceof Error ? error.message : "删除账户失败";
      toast.error(message);
    } finally {
      setIsDeleting(false);
    }
  };

  const handleRefreshAccounts = async (accessTokens: string[]) => {
    if (accessTokens.length === 0) {
      toast.error("没有需要刷新的账户");
      return;
    }

    setIsRefreshing(true);
    setRefreshProgress(null);
    try {
      const started = await startAccountRefresh(accessTokens);
      window.sessionStorage.setItem("accounts.refreshProgressId", started.progress_id);
      setActiveRefreshId(started.progress_id);
    } catch (error) {
      const message = error instanceof Error ? error.message : "刷新账户失败";
      toast.error(message);
      setIsRefreshing(false);
    }
  };

  const handleMFALogin = async (account: Account) => {
    setMFALoginToken(account.access_token);
    try {
      await loginAbnormalAccountWithMFA(account.access_token);
      toast.success(`${account.email} 登录成功`);
      await loadAccounts(true, true);
    } catch (error) {
      toast.error(`${account.email} 登录失败：${error instanceof Error ? error.message : "未知错误"}`);
    } finally {
      setMFALoginToken(null);
    }
  };

  const handleMFASetup = async (account: Account) => {
    setMFASetupToken(account.access_token);
    try {
      await setupAccountMFA(account.access_token);
      toast.success(`${account.email} 2FA 已启用`);
      await loadAccounts(true, true);
    } catch (error) {
      toast.error(`${account.email} 2FA 设置失败：${error instanceof Error ? error.message : "未知错误"}`);
    } finally {
      setMFASetupToken(null);
    }
  };

  const handleRepairSelected = async (tokens = selectedAbnormalTokens, recoveryEmails = selectedPendingEmails, retryBlocked = false) => {
    if (tokens.length + recoveryEmails.length === 0 || activeRepairId || isStartingRepair) return;
    const requestedCount = tokens.length + recoveryEmails.length;
    setIsStartingRepair(true);
    setRepairProgress(null);
    try {
      const started = await startAccountRepair(tokens, recoveryEmails, retryBlocked);
      window.sessionStorage.setItem("accounts.repairProgressId", started.progress_id);
      setActiveRepairId(started.progress_id);
      const count = typeof started.total === "number" && Number.isFinite(started.total) ? started.total : requestedCount;
      toast.success(`已启动 ${count} 个异常账号的修复${started.skipped ? `，跳过 ${started.skipped} 个需人工处理的记录` : ""}`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "启动修复失败");
    } finally {
      setIsStartingRepair(false);
    }
  };

  const openEditDialog = (account: Account) => {
    setEditingAccount(account);
    setEditStatus(account.status);
    setEditAccountGroupID(account.group_id ?? "");
    setEditPassword("");
    setEditMFASecret("");
    const assignment = account.proxy?.trim() ?? "";
    setEditProxyMode(!assignment ? "inherit" : assignment === "direct" ? "direct" : assignment.startsWith("group:") ? "group" : "custom");
    setEditProxyGroupID(assignment.startsWith("group:") ? assignment.slice(6) : "");
    setEditProxyURL(assignment && assignment !== "direct" && !assignment.startsWith("group:") ? assignment : "");
  };

  const handleUpdateAccount = async () => {
    if (!editingAccount) {
      return;
    }

    if (editProxyMode === "group" && !editProxyGroupID) {
      toast.error("请选择代理组");
      return;
    }
    if (editProxyMode === "custom" && !editProxyURL.trim()) {
      toast.error("请填写代理地址");
      return;
    }
    const proxy = editProxyMode === "inherit" ? "" : editProxyMode === "direct" ? "direct" : editProxyMode === "group" ? `group:${editProxyGroupID}` : editProxyURL.trim();
    setIsUpdating(true);
    try {
      const data = await updateAccount(editingAccount.access_token, {
        status: editStatus,
        group_id: editAccountGroupID,
        proxy,
        ...(editPassword !== "" ? { password: editPassword } : {}),
        ...(editMFASecret.trim() !== "" ? { mfa_secret: editMFASecret.trim() } : {}),
      });
      setAccounts(data.items);
      setSelectedIds((prev) => prev.filter((id) => data.items.some((item) => item.access_token === id)));
      setEditingAccount(null);
      toast.success("账号信息已更新");
    } catch (error) {
      const message = error instanceof Error ? error.message : "更新账号失败";
      toast.error(message);
    } finally {
      setIsUpdating(false);
    }
  };

  const handleUpdateStatuses = async (tokens: string[], status: "正常" | "禁用") => {
    if (tokens.length === 0) return;
    setIsBatchUpdating(true);
    try {
      const data = await updateAccountsStatus(tokens, status);
      setAccounts(data.items);
      setSelectedIds((prev) => prev.filter((id) => data.items.some((item) => item.access_token === id)));
      toast.success(`${status === "正常" ? "启用" : "禁用"} ${data.updated} 个账号`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "更新账号状态失败");
    } finally {
      setIsBatchUpdating(false);
    }
  };

  const handleAssignGroup = async (groupID: string) => {
    if (selectedTokens.length === 0) return;
    setIsBatchUpdating(true);
    try {
      const data = await updateAccountsGroup(selectedTokens, groupID);
      setAccounts(data.items);
      setGroupMenuOpen(false);
      toast.success(`已更新 ${data.updated} 个账号的分组`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "批量分组失败");
    } finally {
      setIsBatchUpdating(false);
    }
  };

  const selectRows = (rows: AccountRow[], checked: boolean) => {
    const tokens = rows.flatMap((row) => isPendingRow(row) ? [] : [row.access_token]);
    const emails = rows.flatMap((row) => isPendingRow(row) ? [row.email] : []);
    setSelectedIds((prev) => checked ? Array.from(new Set([...prev, ...tokens])) : prev.filter((id) => !tokens.includes(id)));
    setSelectedPendingEmails((prev) => checked ? Array.from(new Set([...prev, ...emails])) : prev.filter((email) => !emails.includes(email)));
  };
  const toggleSelectAll = (checked: boolean) => selectRows(currentRows, checked);
  const pendingRow = (item: PendingRegisterAccount, layout: "list" | "grid") => <PendingRegistration
    key={`pending:${item.email.toLowerCase()}`} item={item} layout={layout}
    selected={selectedPendingEmails.includes(item.email)}
    onSelect={(checked) => selectRows([item], checked)}
    busy={activeRepairId !== null || isStartingRepair}
    onRepair={() => void handleRepairSelected([], [item.email], true)}
  />;

  return (
    <>
      {/* 顶部进度条：任一刷新/删除操作期间显示，并保证至少滑过一程，
          避免缓存命中时一闪而过。1.5px 高度 + 主色渐变胶囊滑动。 */}
      {showProgress ? (
        <div
          aria-hidden
          className="pointer-events-none fixed inset-x-0 top-0 z-50 h-[3px] overflow-hidden bg-stone-100/40"
        >
          <div
            className="animate-top-progress absolute top-0 left-0 h-full w-1/3 rounded-full"
            style={{
              background:
                "linear-gradient(90deg, transparent 0%, oklch(0.55 0.18 258 / 0.95) 50%, transparent 100%)",
            }}
          />
        </div>
      ) : null}

      <section className="mt-4 border-b border-border pb-4 sm:mt-5">
        <h1 className="text-xl font-semibold">号池管理</h1>
      </section>

      {refreshProgress ? (
        <section className="space-y-2 py-2" aria-live="polite">
          <div className="flex flex-wrap items-center justify-between gap-2 text-sm">
            <span className="font-medium text-foreground">
              {refreshProgress.done ? "账号刷新完成" : "正在刷新账号"}
            </span>
            <span className="font-data tabular-nums text-muted-foreground">
              {refreshProgress.processed}/{refreshProgress.total} · 成功 {refreshProgress.refreshed} · 失败 {refreshProgress.failed}
            </span>
          </div>
          <div className="h-1.5 overflow-hidden rounded-full bg-stone-200" role="progressbar" aria-valuenow={refreshProgress.processed} aria-valuemin={0} aria-valuemax={refreshProgress.total}>
            <div className="h-full bg-emerald-600 transition-all" style={{ width: `${refreshProgress.total ? (refreshProgress.processed / refreshProgress.total) * 100 : 0}%` }} />
          </div>
          {refreshProgress.errors.length > 0 ? (
            <details className="text-sm text-rose-700">
              <summary className="cursor-pointer">失败账号（{refreshProgress.failed}）</summary>
              <ul className="mt-2 max-h-48 space-y-1 overflow-y-auto">
                {refreshProgress.errors.map((item, index) => (
                  <li key={`${item.account}-${index}`} className="break-words">
                    {item.account}: {item.error}
                  </li>
                ))}
              </ul>
            </details>
          ) : null}
        </section>
      ) : null}

      {repairProgress ? (
        <section className="space-y-2 py-2" aria-live="polite">
          <div className="flex flex-wrap items-center justify-between gap-2 text-sm">
            <span className="font-medium">{repairProgress.done ? "异常账号修复完成" : "正在修复异常账号"}</span>
            <span className="font-data tabular-nums text-muted-foreground">{repairProgress.processed}/{repairProgress.total} · 成功 {repairProgress.refreshed} · 失败 {repairProgress.failed}</span>
          </div>
          <div className="h-1.5 overflow-hidden rounded-full bg-stone-200" role="progressbar" aria-valuenow={repairProgress.processed} aria-valuemin={0} aria-valuemax={repairProgress.total}>
            <div className="h-full bg-emerald-600 transition-all" style={{ width: `${repairProgress.total ? (repairProgress.processed / repairProgress.total) * 100 : 0}%` }} />
          </div>
          {repairProgress.errors.length > 0 ? (
            <details className="text-xs text-rose-700">
              <summary className="cursor-pointer">失败账号（{repairProgress.failed}）</summary>
              <ul className="mt-2 space-y-1">{repairProgress.errors.map((item, index) => <li key={index}>{item.account}: {item.error}</li>)}</ul>
            </details>
          ) : null}
        </section>
      ) : null}

      <Dialog open={Boolean(editingAccount)} onOpenChange={(open) => (!open ? setEditingAccount(null) : null)}>
        <DialogContent showCloseButton={false} className="max-h-[85vh] overflow-y-auto rounded-2xl p-6">
          <DialogHeader className="gap-2">
            <DialogTitle>编辑账户</DialogTitle>
            <DialogDescription className="text-sm leading-6">
              修改账号状态、上游出口和登录凭据。
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4">
            <div className="space-y-2">
              <label htmlFor="account-login-password" className="text-sm font-medium text-stone-700">登录密码</label>
              <Input id="account-login-password" type="password" value={editPassword} onChange={(event) => setEditPassword(event.target.value)} placeholder={editingAccount?.password ? "已保存，留空不修改" : "输入登录密码"} autoComplete="new-password" />
            </div>
            <div className="space-y-2">
              <label htmlFor="account-mfa-secret" className="text-sm font-medium text-stone-700">2FA 密钥</label>
              <Input id="account-mfa-secret" type="password" value={editMFASecret} onChange={(event) => setEditMFASecret(event.target.value)} placeholder={editingAccount?.mfa_secret ? "已保存，留空不修改" : "输入验证器密钥"} autoComplete="off" spellCheck={false} />
              <p className="text-xs text-muted-foreground">填写已在账号上启用的验证器密钥；保存密钥不会替你开启 2FA。</p>
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium text-stone-700">账号组</label>
              <Select value={editAccountGroupID || "none"} onValueChange={(value) => setEditAccountGroupID(value === "none" ? "" : value)}>
                <SelectTrigger className="h-11 rounded-md border-stone-200 bg-white"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">未分组</SelectItem>
                  {accountGroups.map((group) => <SelectItem key={group.id} value={group.id}>{group.name}{!group.enabled ? "（已停用）" : ""}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium text-stone-700">状态</label>
              <Select value={editStatus} onValueChange={(value) => setEditStatus(value as AccountStatus)}>
                <SelectTrigger className="h-11 rounded-xl border-stone-200 bg-white">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {accountStatusOptions
                    .filter((option) => option.value !== "all")
                    .map((option) => (
                      <SelectItem key={option.value} value={option.value}>
                        {option.label}
                      </SelectItem>
                    ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium text-stone-700">上游出口</label>
              <Select value={editProxyMode} onValueChange={(value) => setEditProxyMode(value as typeof editProxyMode)}>
                <SelectTrigger className="h-11 rounded-md border-stone-200 bg-white"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="inherit">继承账号组或默认出口</SelectItem>
                  <SelectItem value="direct">直连</SelectItem>
                  <SelectItem value="group">代理组</SelectItem>
                  <SelectItem value="custom">自定义代理</SelectItem>
                </SelectContent>
              </Select>
              {editProxyMode === "group" ? (
                <Select value={editProxyGroupID} onValueChange={setEditProxyGroupID}>
                  <SelectTrigger className="h-11 rounded-md border-stone-200 bg-white"><SelectValue placeholder="选择代理组" /></SelectTrigger>
                  <SelectContent>
                    {proxyGroups.map((group) => <SelectItem key={group.id} value={group.id} disabled={!group.enabled || !group.nodes.some((node) => node.enabled)}>{group.name}</SelectItem>)}
                  </SelectContent>
                </Select>
              ) : null}
              {editProxyMode === "custom" ? <Input value={editProxyURL} onChange={(event) => setEditProxyURL(event.target.value)} placeholder="http://user:pass@host:port" autoComplete="off" /> : null}
            </div>
          </div>
          <DialogFooter className="pt-2">
            <Button
              variant="secondary"
              className="h-10 rounded-xl bg-stone-100 px-5 text-stone-700 hover:bg-stone-200"
              onClick={() => setEditingAccount(null)}
              disabled={isUpdating}
            >
              取消
            </Button>
            <Button
              className="h-10 rounded-xl bg-stone-950 px-5 text-white hover:bg-stone-800"
              onClick={() => void handleUpdateAccount()}
              disabled={isUpdating}
            >
              {isUpdating ? <LoaderCircle className="size-4 animate-spin" /> : null}
              保存修改
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <section className="border-b border-border py-3" aria-label="账号概览">
        <div className="grid grid-cols-2 gap-x-4 gap-y-3 sm:grid-cols-3 xl:grid-cols-6">
          {metricCards.map((item) => {
            const Icon = item.icon;
            const value = summary[item.key];
            return (
              <div key={item.key} className="min-w-0 border-l border-border pl-3 sm:pl-4">
                <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                  <Icon className={cn("size-3.5 shrink-0", item.color)} />
                  <span>{item.label}</span>
                </div>
                <div className={cn("mt-1 font-data text-xl font-semibold tabular-nums", item.color)}>
                  {typeof value === "number" ? formatCompact(value) : value}
                </div>
              </div>
            );
          })}
        </div>
      </section>

      <section className="border-b border-border py-4" aria-label="系统可用模型">
        <div className="mb-3 flex items-center gap-2">
          <h2 className="text-sm font-medium">系统可用模型 <span className="text-muted-foreground">({availableModels.length})</span></h2>
          <Button variant="ghost" size="icon" className="size-7 rounded-md" onClick={() => void loadModels()} disabled={isLoadingModels} title="刷新模型列表" aria-label="刷新模型列表">
            <RefreshCw className={cn("size-3.5", isLoadingModels && "animate-spin")} />
          </Button>
        </div>
        {availableModels.length > 0 ? (
          <div className="flex flex-wrap gap-2">
            {availableModels.map((model) => (
              <button key={model.id} type="button" onClick={() => void copyModel(model.id)} title={`点击复制 ${model.id}`} className="inline-flex max-w-full items-center gap-1.5 rounded-md border border-border bg-background px-2.5 py-1 text-xs text-foreground transition hover:bg-secondary">
                <Copy className="size-3 shrink-0 text-muted-foreground" />
                <span className="break-all text-left">{model.id}</span>
              </button>
            ))}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">{isLoadingModels ? "正在加载模型列表..." : modelsError || "当前暂无可用模型"}</p>
        )}
      </section>

      <section className="pt-4">
        <div className="flex flex-wrap items-center gap-2">
            <div className="relative min-w-[220px] flex-1 sm:max-w-sm">
              <Search className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-stone-400" />
              <Input
                value={query}
                onChange={(event) => {
                  setQuery(event.target.value);
                  setPage(1);
                }}
                placeholder="搜索邮箱、ID、Token 或套餐"
                aria-label="搜索账号"
                className="h-9 rounded-md border-border bg-background pl-10"
              />
            </div>
            <Select
              value={typeFilter}
              onValueChange={(value) => {
                setTypeFilter(value);
                setPage(1);
              }}
            >
              <SelectTrigger aria-label="筛选账号套餐" className="h-9 w-[130px] rounded-md border-border bg-background">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {accountTypeOptions.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select
              value={statusFilter}
              onValueChange={(value) => {
                setStatusFilter(value as AccountStatus | "all");
                setPage(1);
              }}
            >
              <SelectTrigger aria-label="筛选账号状态" className="h-9 w-[130px] rounded-md border-border bg-background">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {accountStatusOptions.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select
              value={trialFilter}
              onValueChange={(value) => {
                setTrialFilter(value as (typeof trialFilterOptions)[number]["value"]);
                setPage(1);
              }}
            >
              <SelectTrigger aria-label="筛选优惠资格" className="h-9 w-[145px] rounded-md border-border bg-background">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {trialFilterOptions.map((option) => (
                  <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select value={groupFilter} onValueChange={(value) => { setGroupFilter(value); setPage(1); }}>
              <SelectTrigger aria-label="筛选账号组" className="h-9 w-[140px] rounded-md border-border bg-background"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="all">全部账号组</SelectItem>
                <SelectItem value="none">未分组</SelectItem>
                {accountGroups.map((group) => <SelectItem key={group.id} value={group.id}>{group.name}</SelectItem>)}
              </SelectContent>
            </Select>
            <div className="inline-flex h-9 shrink-0 items-center rounded-md border border-border bg-secondary/40 p-1 sm:ml-auto">
              <button
                type="button"
                aria-pressed={viewMode === "grid"}
                aria-label="卡片视图"
                className={cn(
                  "inline-flex size-7 cursor-pointer items-center justify-center rounded-sm transition",
                  viewMode === "grid"
                    ? "bg-background text-foreground shadow-sm"
                    : "text-muted-foreground hover:text-foreground",
                )}
                onClick={() => {
                  setViewMode("grid");
                  window.localStorage.setItem("accounts.viewMode", "grid");
                }}
                title="卡片视图"
              >
                <LayoutGrid className="size-4" />
              </button>
              <button
                type="button"
                aria-pressed={viewMode === "list"}
                aria-label="列表视图"
                className={cn(
                  "inline-flex size-7 cursor-pointer items-center justify-center rounded-sm transition",
                  viewMode === "list"
                    ? "bg-background text-foreground shadow-sm"
                    : "text-muted-foreground hover:text-foreground",
                )}
                onClick={() => {
                  setViewMode("list");
                  window.localStorage.setItem("accounts.viewMode", "list");
                }}
                title="列表视图"
              >
                <Rows3 className="size-4" />
              </button>
            </div>
        </div>

        <div className="mt-3 flex flex-wrap items-center gap-2 border-t border-border py-3">
          <AccountImportDialog
            disabled={showProgress}
            onImported={(items) => {
              setAccounts(items);
              setSelectedIds([]);
              setPage(1);
              void loadAccounts(true);
            }}
          />
          <CodexAuthBatchDialog disabled={showProgress} />
          <CodexAuthBatchDialog disabled={showProgress} mode="web" />
          <AccountGroupsDialog groups={accountGroups} proxyGroups={proxyGroups} accounts={accounts} onChanged={async () => { const data = await fetchAccountGroups(); setAccountGroups(data.groups); }} />
          <Popover open={groupMenuOpen} onOpenChange={setGroupMenuOpen}>
            <PopoverTrigger asChild>
              <Button variant="outline" className="h-9 rounded-md" disabled={selectedTokens.length === 0 || isBatchUpdating}><UsersRound className="size-4" />分组选中</Button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-48 rounded-md border-border p-1">
              <button type="button" onClick={() => void handleAssignGroup("")} className="w-full rounded-sm px-3 py-2 text-left text-sm hover:bg-secondary">移出账号组</button>
              {accountGroups.map((group) => <button key={group.id} type="button" onClick={() => void handleAssignGroup(group.id)} className="w-full rounded-sm px-3 py-2 text-left text-sm hover:bg-secondary">{group.name}{!group.enabled ? "（已停用）" : ""}</button>)}
            </PopoverContent>
          </Popover>
          <Popover>
            <PopoverTrigger asChild>
              <Button variant="outline" className="h-9 rounded-md" disabled={accounts.length === 0}>
                <Download className="size-4" />
                导出
              </Button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-64 rounded-md border-border p-1">
              {([
                ["选中账号 · Token TXT", selectedAccounts, "txt"],
                ["选中账号 · 完整 JSON", selectedAccounts, "json"],
                ["全部账号 · Token TXT", accounts, "txt"],
                ["全部账号 · 完整 JSON", accounts, "json"],
              ] as const).map(([label, items, format]) => (
                <button key={label} type="button" disabled={items.length === 0} onClick={() => downloadAccounts(items, format)} className="w-full rounded-sm px-3 py-2 text-left text-sm hover:bg-secondary disabled:opacity-40">
                  {label}
                </button>
              ))}
            </PopoverContent>
          </Popover>
          <Button variant="outline" className="h-9 rounded-md" onClick={() => void handleRefreshAccounts(selectedTokens)} disabled={selectedTokens.length === 0 || isRefreshing}>
            {isRefreshing ? <LoaderCircle className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
            刷新选中
          </Button>
          <Button variant="outline" className="h-9 rounded-md" title="刷新全部账号，跳过异常和封号账号" onClick={() => void handleRefreshAccounts(refreshAllTokens)} disabled={showProgress || refreshAllTokens.length === 0}>
            <RefreshCw className="size-4" />
            刷新全部
          </Button>
          <Popover>
            <PopoverTrigger asChild>
              <Button variant="outline" className="h-9 rounded-md" disabled={accounts.length + pendingRegistrations.length === 0}>
                <MoreHorizontal className="size-4" />
                批量处理
              </Button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-56 rounded-md border-border p-1">
              <button type="button" onClick={() => { setSelectedIds(accounts.map((item) => item.access_token)); setSelectedPendingEmails(pendingRegistrations.map((item) => item.email)); }} className="w-full rounded-sm px-3 py-2 text-left text-sm hover:bg-secondary">全选账号</button>
              <button type="button" onClick={() => selectRows(filteredAccounts, true)} className="w-full rounded-sm px-3 py-2 text-left text-sm hover:bg-secondary">全选筛选结果</button>
              <button type="button" disabled={selectedIds.length + selectedPendingEmails.length === 0} onClick={() => { setSelectedIds([]); setSelectedPendingEmails([]); }} className="w-full rounded-sm px-3 py-2 text-left text-sm hover:bg-secondary disabled:opacity-40">取消选择</button>
              <div className="my-1 border-t border-border" />
              <button type="button" disabled={selectedTokens.length === 0 || isBatchUpdating} onClick={() => void handleUpdateStatuses(selectedTokens, "正常")} className="w-full rounded-sm px-3 py-2 text-left text-sm hover:bg-secondary disabled:opacity-40">批量启用</button>
              <button type="button" disabled={selectedTokens.length === 0 || isBatchUpdating} onClick={() => void handleUpdateStatuses(selectedTokens, "禁用")} className="w-full rounded-sm px-3 py-2 text-left text-sm hover:bg-secondary disabled:opacity-40">批量禁用</button>
              <button type="button" disabled={selectedAbnormalTokens.length + selectedPendingEmails.length === 0 || activeRepairId !== null || isStartingRepair} onClick={() => void handleRepairSelected()} className="w-full rounded-sm px-3 py-2 text-left text-sm hover:bg-secondary disabled:opacity-40">修复选中异常账号（{selectedAbnormalTokens.length + selectedPendingEmails.length}）</button>
              <button type="button" disabled={abnormalTokens.length === 0 || isDeleting} onClick={() => void handleDeleteTokens(abnormalTokens)} className="w-full rounded-sm px-3 py-2 text-left text-sm text-rose-600 hover:bg-rose-50 disabled:opacity-40">移除异常账号</button>
              <button type="button" disabled={selectedTokens.length === 0 || isDeleting} onClick={() => void handleDeleteTokens(selectedTokens)} className="w-full rounded-sm px-3 py-2 text-left text-sm text-rose-600 hover:bg-rose-50 disabled:opacity-40">删除选中</button>
            </PopoverContent>
          </Popover>
          {selectedIds.length + selectedPendingEmails.length > 0 ? <span className="text-xs font-medium text-muted-foreground">已选 {selectedIds.length + selectedPendingEmails.length}</span> : null}
          {viewMode === "grid" ? (
            <label className="inline-flex cursor-pointer items-center gap-2 text-xs text-muted-foreground sm:ml-auto">
              <Checkbox checked={allCurrentSelected} onCheckedChange={(checked) => toggleSelectAll(Boolean(checked))} />
              全选本页
            </label>
          ) : null}
          <Button variant="ghost" size="icon" className="ml-auto size-9 rounded-md" onClick={() => void loadAccounts()} disabled={showProgress} title="刷新列表" aria-label="刷新列表">
            <RefreshCw className="size-4" />
          </Button>
        </div>

        {isLoading && accounts.length === 0 ? (
          <div className="flex items-center justify-center gap-2 border-t border-border py-14 text-sm text-muted-foreground">
            <LoaderCircle className="size-4 animate-spin" />
            正在加载账户
          </div>
        ) : null}

        <div
          className={cn(
            "border-t border-border",
            isLoading && accounts.length === 0 ? "hidden" : "",
          )}
        >
          <div>
            {viewMode === "list" ? (
              <div className="overflow-x-auto">
              <table className="w-full min-w-[920px] text-left">
                <thead className="border-b border-border bg-secondary/40 text-[12px] font-medium text-muted-foreground">
                  <tr>
                    <th className="w-12 px-4 py-2.5">
                      <Checkbox
                        checked={allCurrentSelected}
                        onCheckedChange={(checked) => toggleSelectAll(Boolean(checked))}
                      />
                    </th>
                    <th className="w-56 px-4 py-2.5 font-medium">Token</th>
                    <th className="w-36 px-4 py-2.5 font-medium">类型</th>
                    <th className="w-24 px-4 py-2.5 font-medium">状态</th>
                    <th className="w-56 px-4 py-2.5 font-medium">账号信息</th>
                    <th className="w-24 px-4 py-2.5 font-medium">额度</th>
                    <th className="w-40 px-4 py-2.5 font-medium">恢复时间</th>
                    <th className="w-36 px-4 py-2.5 font-medium">注册时间</th>
                    <th className="w-18 px-4 py-2.5 font-medium" title="当前额度窗口内的成功次数">成功</th>
                    <th className="w-18 px-4 py-2.5 font-medium" title="当前额度窗口内的失败次数">失败</th>
                    <th className="w-24 px-4 py-2.5 font-medium">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {currentRows.map((row) => {
                    if (isPendingRow(row)) return pendingRow(row, "list");
                    const account = row;
                    const windowStats = accountWindowStats(account, statsNow);
                    return (
                      <tr
                        key={account.access_token}
                        className="border-b border-border/60 text-sm text-foreground/80 transition-colors even:bg-secondary/30 hover:bg-secondary/60"
                      >
                        <td className="px-4 py-3">
                          <Checkbox
                            checked={selectedIds.includes(account.access_token)}
                            onCheckedChange={(checked) => {
                              setSelectedIds((prev) =>
                                checked
                                  ? Array.from(new Set([...prev, account.access_token]))
                                  : prev.filter((item) => item !== account.access_token),
                              );
                            }}
                          />
                        </td>
                        <td className="px-4 py-3">
                          <div className="flex items-center gap-2">
                            <span className="font-data text-[12.5px] text-foreground">
                              {maskToken(account.access_token)}
                            </span>
                            <button
                              type="button"
                              title="复制 Token"
                              aria-label="复制 Token"
                              className="cursor-pointer rounded-md p-1 text-muted-foreground transition hover:bg-secondary hover:text-foreground"
                              onClick={() => {
                                void navigator.clipboard.writeText(account.access_token);
                                toast.success("token 已复制");
                              }}
                            >
                              <Copy className="size-4" />
                            </button>
                          </div>
                          <div className="mt-1 flex items-center gap-2 text-[11px] text-muted-foreground">
                            <span>AT 已保存</span>
                            <span>·</span>
                            <span className={account.refresh_token ? "text-emerald-700" : ""}>RT {account.refresh_token ? "已保存" : "缺失"}</span>
                            {account.refresh_token ? (
                              <button type="button" title="复制 Refresh Token" aria-label="复制 Refresh Token" onClick={() => {
                                void navigator.clipboard.writeText(account.refresh_token ?? "");
                                toast.success("Refresh Token 已复制");
                              }} className="rounded-sm p-0.5 hover:text-foreground">
                                <Copy className="size-3" />
                              </button>
                            ) : null}
                          </div>
                          <div className="mt-1 text-[11px] text-muted-foreground">出口：{accountProxyLabel(account, proxyGroups, accountGroups)}</div>
                        </td>
                        <td className="px-4 py-3">
                          <div className="flex flex-wrap gap-1.5">
                            <Badge variant="secondary" className="rounded-md bg-secondary font-data text-[11px] font-medium text-foreground/80">
                              {displayAccountType(account)}
                            </Badge>
                            <Badge
                              variant="outline"
                              className={cn("rounded-md font-data text-[11px] font-medium", SOURCE_BADGE_CLASS[displayAccountSource(account)])}
                            >
                              {displayAccountSource(account)}
                            </Badge>
                            {account.is_signup_token ? <Badge variant="outline">注册令牌</Badge> : null}
                            <TrialBadge account={account} />
                          </div>
                        </td>
                        <td className="px-4 py-3">
                          <span className="inline-flex items-center gap-2">
                            <span
                              className={cn(
                                "size-2 rounded-full ring-[3px]",
                                account.status === "正常" && "bg-emerald-500 ring-emerald-500/15",
                                account.status === "限流" && "bg-amber-500 ring-amber-500/15",
                                account.status === "异常" && "bg-rose-500 ring-rose-500/15",
                                account.status === "封号" && "bg-red-700 ring-red-700/15",
                                account.status === "禁用" && "bg-stone-400 ring-stone-400/15",
                              )}
                            />
                            <span className="text-sm font-medium text-foreground">{account.status}</span>
                          </span>
                        </td>
                        <td className="px-4 py-3">
                          <div className="font-data text-[12px] leading-5 text-muted-foreground">{account.email ?? "—"}</div>
                          {account.password ? <div className="mt-1 text-xs text-muted-foreground">密码已保存</div> : null}
                          <button
                            type="button"
                            title="复制账号----密码----2FA 密钥"
                            aria-label="复制账号凭据"
                            onClick={() => void copyCredentials(account)}
                            className="mt-1 inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
                          >
                            <Copy className="size-3.5" /> 复制账号凭据
                          </button>
                          {account.mfa_status ? (
                            <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
                              <span>2FA {account.mfa_status === "enabled" ? "已启用" : account.mfa_status === "pending" ? "待确认" : "失败"}</span>
                              {account.mfa_status === "enabled" ? (
                                <button
                                  type="button"
                                  title="复制当前 2FA 验证码"
                                  aria-label="复制当前 2FA 验证码"
                                  onClick={() => void copyMFACode(account)}
                                  className="cursor-pointer hover:text-foreground"
                                >
                                  <KeyRound className="size-3.5" />
                                </button>
                              ) : null}
                            </div>
                          ) : null}
                        </td>
                        <td className="px-4 py-3">
                          <span className="font-data tabular-nums text-sm font-semibold text-foreground">
                            {formatQuota(account)}
                          </span>
                        </td>
                        <td className="px-4 py-3 text-xs leading-5 text-muted-foreground">
                          {(() => {
                            const restore = formatRestoreAt(account.restore_at);
                            return (
                              <div className="space-y-0.5">
                                {restore.relative ? <div className="font-medium text-foreground">{restore.relative}</div> : null}
                                <div className="font-data">{restore.absolute}</div>
                              </div>
                            );
                          })()}
                        </td>
                        <td className="px-4 py-3 font-data text-xs leading-5 text-muted-foreground">
                          {formatDateTime(account.created_at)}
                        </td>
                        <td className="px-4 py-3 font-data tabular-nums text-emerald-600">{windowStats?.success ?? "—"}</td>
                        <td className="px-4 py-3 font-data tabular-nums text-rose-500">{windowStats?.fail ?? "—"}</td>
                        <td className="px-4 py-3">
                          <div className="flex items-center gap-0.5 text-muted-foreground">
                            <button
                              type="button"
                              title="编辑账号"
                              aria-label="编辑账号"
                              className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground"
                              onClick={() => openEditDialog(account)}
                              disabled={isUpdating}
                            >
                              <Pencil className="size-4" />
                            </button>
                            <button
                              type="button"
                              title="刷新账号信息和额度"
                              aria-label="刷新账号信息和额度"
                              className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground"
                              onClick={() => void handleRefreshAccounts([account.access_token])}
                              disabled={isRefreshing}
                            >
                              <RefreshCw className={cn("size-4", isRefreshing ? "animate-spin" : "")} />
                            </button>
                            {account.status === "异常" && account.mfa_secret ? (
                              <button type="button" title={!account.email || !account.password ? "缺少邮箱或登录密码" : "使用密码和 2FA 尝试登录"} aria-label="使用密码和 2FA 尝试登录" className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground disabled:opacity-50" onClick={() => void handleMFALogin(account)} disabled={mfaLoginToken !== null || !account.email || !account.password}>
                                {mfaLoginToken === account.access_token ? <LoaderCircle className="size-4 animate-spin" /> : <LogIn className="size-4" />}
                              </button>
                            ) : null}
                            {(account.mfa_status === "failed" || account.mfa_status === "pending") ? (
                              <button type="button" title="补设验证器 2FA" aria-label="补设验证器 2FA" className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground disabled:opacity-50" onClick={() => void handleMFASetup(account)} disabled={mfaSetupToken !== null}>
                                {mfaSetupToken === account.access_token ? <LoaderCircle className="size-4 animate-spin" /> : <KeyRound className="size-4" />}
                              </button>
                            ) : null}
                            <button
                              type="button"
                              title={account.status === "禁用" ? "启用账号" : "禁用账号"}
                              aria-label={account.status === "禁用" ? "启用账号" : "禁用账号"}
                              className="cursor-pointer rounded-md p-1.5 transition hover:bg-secondary hover:text-foreground"
                              onClick={() => void handleUpdateStatuses([account.access_token], account.status === "禁用" ? "正常" : "禁用")}
                              disabled={isBatchUpdating}
                            >
                              {account.status === "禁用" ? <CheckCircle2 className="size-4" /> : <Ban className="size-4" />}
                            </button>
                            <button
                              type="button"
                              title="删除账号"
                              aria-label="删除账号"
                              className="cursor-pointer rounded-md p-1.5 transition hover:bg-rose-50 hover:text-rose-500"
                              onClick={() => void handleDeleteTokens([account.access_token])}
                              disabled={isDeleting}
                            >
                              <Trash2 className="size-4" />
                            </button>
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
            ) : (
              <div className="grid gap-3 p-4 sm:grid-cols-2 xl:grid-cols-3">
                {currentRows.map((row) => {
                  if (isPendingRow(row)) return pendingRow(row, "grid");
                  const account = row;
                  return <AccountCard
                    key={account.access_token}
                    account={account}
                    statsNow={statsNow}
                    proxyLabel={accountProxyLabel(account, proxyGroups, accountGroups)}
                    selected={selectedIds.includes(account.access_token)}
                    onSelectChange={(checked) => {
                      setSelectedIds((prev) =>
                        checked
                          ? Array.from(new Set([...prev, account.access_token]))
                          : prev.filter((item) => item !== account.access_token),
                      );
                    }}
                    onEdit={() => openEditDialog(account)}
                    onRefresh={() => void handleRefreshAccounts([account.access_token])}
                    onMFALogin={() => void handleMFALogin(account)}
                    onMFASetup={() => void handleMFASetup(account)}
                    onToggleStatus={() => void handleUpdateStatuses([account.access_token], account.status === "禁用" ? "正常" : "禁用")}
                    onDelete={() => void handleDeleteTokens([account.access_token])}
                    onCopyToken={() => {
                      void navigator.clipboard.writeText(account.access_token);
                      toast.success("token 已复制");
                    }}
                    onCopyCredentials={() => void copyCredentials(account)}
                    onCopyMFACode={() => void copyMFACode(account)}
                    isRefreshing={isRefreshing}
                    isMFALoggingIn={mfaLoginToken !== null}
                    isMFASettingUp={mfaSetupToken !== null}
                    isDeleting={isDeleting}
                    isUpdating={isUpdating}
                  />;
                })}
              </div>
            )}

            {!isLoading && currentRows.length === 0 ? (
              <div className="flex flex-col items-center justify-center gap-3 px-6 py-14 text-center">
                <div className="rounded-xl bg-stone-100 p-3 text-stone-500">
                  <Search className="size-5" />
                </div>
                <div className="space-y-1">
                  <p className="text-sm font-medium text-stone-700">没有匹配的账户</p>
                  <p className="text-sm text-stone-500">调整筛选条件或搜索关键字后重试。</p>
                </div>
              </div>
            ) : null}

            <div className="border-t border-stone-100 px-4 py-4">
              <div className="flex items-center justify-center gap-3 overflow-x-auto whitespace-nowrap">
                <div className="shrink-0 text-sm text-stone-500">
                显示第 {filteredAccounts.length === 0 ? 0 : startIndex + 1} -{" "}
                {Math.min(startIndex + Number(pageSize), filteredAccounts.length)} 条，共{" "}
                {filteredAccounts.length} 条
                </div>

                <span className="shrink-0 text-sm leading-none text-stone-500">
                  {safePage} / {pageCount} 页
                </span>
                <Select
                  value={pageSize}
                  onValueChange={(value) => {
                    setPageSize(value);
                    setPage(1);
                  }}
                >
                  <SelectTrigger className="h-10 w-[108px] shrink-0 rounded-lg border-stone-200 bg-white text-sm leading-none">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="10">10 / 页</SelectItem>
                    <SelectItem value="20">20 / 页</SelectItem>
                    <SelectItem value="50">50 / 页</SelectItem>
                    <SelectItem value="100">100 / 页</SelectItem>
                  </SelectContent>
                </Select>
                <Button
                  variant="outline"
                  size="icon"
                  className="size-10 shrink-0 rounded-lg border-stone-200 bg-white"
                  disabled={safePage <= 1}
                  onClick={() => setPage((prev) => Math.max(1, prev - 1))}
                >
                  <ChevronLeft className="size-4" />
                </Button>
                {paginationItems.map((item, index) =>
                  item === "..." ? (
                    <span key={`ellipsis-${index}`} className="px-1 text-sm text-stone-400">
                      ...
                    </span>
                  ) : (
                    <Button
                      key={item}
                      variant={item === safePage ? "default" : "outline"}
                      className={cn(
                        "h-10 min-w-10 shrink-0 rounded-lg px-3",
                        item === safePage
                          ? "bg-stone-950 text-white hover:bg-stone-800"
                          : "border-stone-200 bg-white text-stone-700",
                      )}
                      onClick={() => setPage(item)}
                    >
                      {item}
                    </Button>
                  ),
                )}
                <Button
                  variant="outline"
                  size="icon"
                  className="size-10 shrink-0 rounded-lg border-stone-200 bg-white"
                  disabled={safePage >= pageCount}
                  onClick={() => setPage((prev) => Math.min(pageCount, prev + 1))}
                >
                  <ChevronRight className="size-4" />
                </Button>
              </div>
            </div>
          </div>
        </div>
      </section>
    </>
  );
}

export default function AccountsPage() {
  const { isCheckingAuth, session } = useAuthGuard(["admin"]);

  if (isCheckingAuth || !session || session.role !== "admin") {
    return (
      <div className="flex min-h-[40vh] items-center justify-center">
        <LoaderCircle className="size-5 animate-spin text-stone-400" />
      </div>
    );
  }

  return <AccountsPageContent />;
}
