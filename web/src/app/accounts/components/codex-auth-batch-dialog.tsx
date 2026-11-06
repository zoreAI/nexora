"use client";

import { useEffect, useState } from "react";
import { Bot, Download, LoaderCircle } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import { fetchCodexAuthBatchProgress, getCodexAuthBatchExportUrl, startCodexAuthBatch, type CodexAuthCredential, type CodexAuthBatchProgress } from "@/lib/api";
import { request } from "@/lib/request";

function parseCredentials(input: string): CodexAuthCredential[] {
  const accounts: CodexAuthCredential[] = [];
  for (const [index, rawLine] of input.split(/\r?\n/).entries()) {
    const line = rawLine.trim();
    if (!line) continue;
    const first = line.indexOf("----");
    const last = line.lastIndexOf("----");
    if (first < 0 || last === first) throw new Error(`第 ${index + 1} 行格式错误`);
    const email = line.slice(0, first).trim();
    const password = line.slice(first + 4, last).trim();
    const mfa_secret = line.slice(last + 4).trim();
    if (!email || !password || !mfa_secret) throw new Error(`第 ${index + 1} 行缺少账号、密码或 2FA 密钥`);
    accounts.push({ email, password, mfa_secret });
  }
  if (accounts.length === 0) throw new Error("请填写至少一个账号");
  return accounts;
}

export function CodexAuthBatchDialog({ disabled, mode = "codex" }: { disabled?: boolean; mode?: "codex" | "web" }) {
  const [open, setOpen] = useState(false);
  const [input, setInput] = useState("");
  const [progress, setProgress] = useState<CodexAuthBatchProgress | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!progress || progress.done) return;
    const timer = window.setInterval(() => {
      void fetchCodexAuthBatchProgress(progress.progress_id).then(setProgress).catch(() => undefined);
    }, 1500);
    return () => window.clearInterval(timer);
  }, [progress]);

  const start = async () => {
    let accounts: CodexAuthCredential[];
    try {
      accounts = parseCredentials(input);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "账号格式错误");
      return;
    }
    setBusy(true);
    try {
      const result = await startCodexAuthBatch(accounts, 3, mode);
      setProgress({ progress_id: result.progress_id, total: result.total, processed: 0, succeeded: 0, failed: 0, done: false, items: [] });
      toast.success(`已提交 ${result.total} 个 Codex 授权任务`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "提交失败");
    } finally {
      setBusy(false);
    }
  };

  const download = async () => {
    if (!progress) return;
    try {
      const response = await request.get(getCodexAuthBatchExportUrl(progress.progress_id), { responseType: "blob" });
      const url = URL.createObjectURL(response.data);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = "codex-sub2api.json";
      anchor.click();
      URL.revokeObjectURL(url);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "导出失败");
    }
  };

  return <>
    <Button variant="outline" className="h-9 rounded-md" disabled={disabled} onClick={() => setOpen(true)}><Bot className="size-4" />{mode === "codex" ? "批量 Codex 授权" : "批量普通网页登录"}</Button>
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>{mode === "codex" ? "批量获取 Codex OAuth" : "批量普通网页登录"}</DialogTitle>
          <DialogDescription>每行一个账号，格式：账号----密码----2FA 密钥。完成后可下载授权结果。</DialogDescription>
        </DialogHeader>
        {!progress || progress.done ? <Textarea value={input} onChange={(event) => setInput(event.target.value)} placeholder="user@example.com----登录密码----验证器密钥" className="min-h-64 rounded-xl font-mono text-xs" spellCheck={false} /> : null}
        {progress ? <div className="space-y-3 rounded-xl border border-border bg-secondary/30 p-4 text-sm">
          <div className="flex items-center justify-between"><span>进度 {progress.processed}/{progress.total}</span><span>成功 {progress.succeeded} · 失败 {progress.failed}</span></div>
          <div className="h-2 overflow-hidden rounded-full bg-secondary"><div className="h-full bg-foreground transition-all" style={{ width: `${progress.total ? (progress.processed / progress.total) * 100 : 0}%` }} /></div>
          <div className="max-h-40 space-y-1 overflow-auto text-xs">{progress.items.map((item) => <div key={item.index} className="flex justify-between gap-3"><span>{item.email}</span><span className={item.status === "success" ? "text-emerald-600" : item.status === "failed" ? "text-rose-600" : "text-muted-foreground"}>{item.status === "success" ? "成功" : item.status === "failed" ? item.error || "失败" : "等待中"}</span></div>)}</div>
        </div> : null}
        <DialogFooter>
          {progress?.done && progress.succeeded > 0 ? <Button onClick={() => void download()}><Download className="size-4" />下载 Sub2API JSON</Button> : null}
          {!progress || progress.done ? <Button onClick={() => void start()} disabled={busy}>{busy ? <LoaderCircle className="size-4 animate-spin" /> : null}开始授权</Button> : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </>;
}
