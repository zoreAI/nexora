"use client";

import { useEffect, useState } from "react";
import { Copy, Eye, EyeOff, RefreshCw } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { fetchRegisterConfig, type PendingRegisterAccount } from "@/lib/api";

export function PendingRegistration({ item, selected, onSelect, onRepair, busy, layout }: {
  item: PendingRegisterAccount;
  selected: boolean;
  onSelect: (selected: boolean) => void;
  onRepair: () => void;
  busy: boolean;
  layout: "list" | "grid";
}) {
  const [password, setPassword] = useState<string | null>(null);
  const [revealed, setRevealed] = useState(false);
  const [loadingPassword, setLoadingPassword] = useState(false);
  const needsManual = item.recovery_status === "identity_verification" || item.recovery_status === "registration_disallowed";
  const recoveryLabel = item.recovery_status === "identity_verification" ? "需身份验证" : item.recovery_status === "registration_disallowed" ? "上游拒绝注册" : "注册未完成";
  const recoveryHint = item.recovery_hint || "尚未取得登录令牌，完成注册、密码和 2FA 后才可用。";
  const recovery = <Badge variant="outline">{recoveryLabel}</Badge>;

  useEffect(() => {
    setPassword(null);
    setRevealed(false);
  }, [item.email, item.updated_at]);

  const readPassword = async () => {
    const result = await fetchRegisterConfig();
    const saved = result.register.pending_passwords?.find(
      (entry) => entry.email.trim().toLowerCase() === item.email.trim().toLowerCase(),
    )?.password;
    if (!saved) throw new Error("这条记录没有保存注册密码");
    setPassword(saved);
    return saved;
  };

  const usePassword = async (copy: boolean) => {
    if (!copy && revealed) {
      setRevealed(false);
      return;
    }
    setLoadingPassword(true);
    try {
      const saved = await readPassword();
      if (copy) {
        await navigator.clipboard.writeText(saved);
        toast.success("注册时保存的密码已复制");
      } else {
        setRevealed(true);
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "读取注册密码失败");
    } finally {
      setLoadingPassword(false);
    }
  };

  const selection = <Checkbox aria-label={`选择 ${item.email}`} checked={selected} onCheckedChange={(value) => onSelect(Boolean(value))} />;
  const status = <Badge variant="outline" className="border-rose-200 bg-rose-50 text-rose-700">异常</Badge>;
  const detail = <div className="min-w-0 space-y-1">
    <p className="break-all text-sm font-medium">{item.email}</p>
    <Badge variant="outline">{item.password_saved ? "密码已保存" : "密码未保存"}</Badge>
    {item.password_saved ? <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
        <button type="button" disabled={loadingPassword} onClick={() => void usePassword(false)} aria-label={`${revealed ? "隐藏" : "查看"} ${item.email} 的注册密码`} className="inline-flex cursor-pointer items-center gap-1 hover:text-foreground disabled:cursor-wait disabled:opacity-50">
          {revealed ? <EyeOff className="size-3" /> : <Eye className="size-3" />}{revealed ? "隐藏密码" : "查看密码"}
        </button>
        <button type="button" disabled={loadingPassword} onClick={() => void usePassword(true)} aria-label={`复制 ${item.email} 的注册密码`} className="inline-flex cursor-pointer items-center gap-1 hover:text-foreground disabled:cursor-wait disabled:opacity-50">
          <Copy className="size-3" />复制注册密码
        </button>
      </div>
      {revealed && password ? <code className="block select-all break-all rounded bg-secondary px-2 py-1 text-xs">{password}</code> : null}
    </div> : null}
    <p className="max-w-lg break-words text-xs text-muted-foreground">{recoveryHint}</p>
    {needsManual ? <p className="text-xs text-amber-700">整批修复会跳过此记录，邮箱和已保存密码继续保留。</p> : null}
    {item.state ? <details className="max-w-lg text-xs text-muted-foreground"><summary className="cursor-pointer">最近处理详情</summary><p className="mt-1 break-words">{item.state}</p></details> : null}
  </div>;
  const actionLabel = needsManual ? "重新检查" : "修复";
  const repair = <Button variant="outline" size="sm" disabled={busy} onClick={onRepair} aria-label={`${actionLabel} ${item.email}`}><RefreshCw className="size-3.5" />{actionLabel}</Button>;
  if (layout === "grid") {
    return <div className="space-y-3 rounded-lg border border-border bg-card p-4">
      <div className="flex items-center gap-2">{selection}{status}{recovery}</div>
      {detail}
      {repair}
    </div>;
  }
  return <tr className="border-b border-border/60 text-sm text-foreground/80 even:bg-secondary/30 hover:bg-secondary/60">
    <td className="px-4 py-3">{selection}</td>
    <td className="px-4 py-3 text-xs text-muted-foreground">尚未取得登录令牌</td>
    <td className="px-4 py-3">{recovery}</td>
    <td className="px-4 py-3">{status}</td>
    <td className="px-4 py-3">{detail}</td>
    <td className="px-4 py-3">—</td>
    <td className="px-4 py-3">—</td>
    <td className="px-4 py-3 text-xs text-muted-foreground">—</td>
    <td className="px-4 py-3">—</td>
    <td className="px-4 py-3">—</td>
    <td className="px-4 py-3">{repair}</td>
  </tr>;
}
