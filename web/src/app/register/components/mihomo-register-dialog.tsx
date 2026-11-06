"use client";

import { useState } from "react";
import { LoaderCircle, Network } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { applyRegisterMihomo, readRegisterMihomoGroups, type ProxyGroup } from "@/lib/api";

export function MihomoRegisterDialog({ disabled, onApplied }: {
  disabled: boolean;
  onApplied: (group: ProxyGroup, proxy: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [controller, setController] = useState("");
  const [secret, setSecret] = useState("");
  const [configPath, setConfigPath] = useState("");
  const [portStart, setPortStart] = useState("18001");
  const [groups, setGroups] = useState<Array<{ name: string; nodes: number; members: number }>>([]);
  const [group, setGroup] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const options = { controller: controller.trim(), secret, config_path: configPath.trim(), group, port_start: Number(portStart) };
  const read = async () => {
    setBusy(true); setError(""); setGroups([]); setGroup("");
    try {
      const result = await readRegisterMihomoGroups(options);
      setGroups(result.groups);
      setController(result.controller);
      setConfigPath(result.config_path);
      setGroup(result.groups.find((item) => item.nodes > 0)?.name || "");
    } catch (error) { setError(error instanceof Error ? error.message : "读取分组失败"); }
    finally { setBusy(false); }
  };
  const apply = async () => {
    setBusy(true); setError("");
    try {
      const result = await applyRegisterMihomo(options);
      onApplied(result.group, result.proxy);
      toast.success(`注册将轮询 ${result.group.name} 的 ${result.group.nodes.length} 个节点`);
      setOpen(false);
    } catch (error) { setError(error instanceof Error ? error.message : "保存分组失败"); }
    finally { setBusy(false); }
  };
  const invalidate = () => { setGroups([]); setGroup(""); setError(""); };

  return <Dialog open={open} onOpenChange={(value) => { if (!busy) { setOpen(value); if (!value) setSecret(""); } }}>
    <DialogTrigger asChild><Button type="button" variant="outline" size="sm" disabled={disabled}><Network className="size-4" />从 Mihomo 选择分组</Button></DialogTrigger>
    <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-lg">
      <DialogHeader><DialogTitle>注册专用 Mihomo 分组</DialogTitle></DialogHeader>
      <p className="text-xs text-muted-foreground">复用与 Nexora 同机运行的 Mihomo。只为注册添加独立出口，保留系统代理、当前节点选择及其他功能的默认代理。</p>
      <label className="space-y-1 text-sm"><span>控制 API 地址</span><Input value={controller} onChange={(event) => { setController(event.target.value); invalidate(); }} placeholder="留空自动识别本机 Clash Verge" disabled={busy} /></label>
      <label className="space-y-1 text-sm"><span>控制 API 密钥</span><Input type="password" autoComplete="new-password" value={secret} onChange={(event) => { setSecret(event.target.value); invalidate(); }} placeholder="留空从本机运行配置读取" disabled={busy} /></label>
      <details className="text-sm"><summary className="cursor-pointer">高级配置</summary><div className="mt-3 space-y-3">
        <label className="block space-y-1"><span>运行配置文件路径</span><Input value={configPath} onChange={(event) => { setConfigPath(event.target.value); invalidate(); }} placeholder="留空自动识别本机 Clash Verge" disabled={busy} /></label>
        <label className="block space-y-1"><span>注册出口起始端口</span><Input type="number" min={1024} max={65535} value={portStart} onChange={(event) => setPortStart(event.target.value)} disabled={busy} /></label>
      </div></details>
      <Button type="button" variant="outline" disabled={busy} onClick={() => void read()}>{busy ? <LoaderCircle className="size-4 animate-spin" /> : null}读取分组</Button>
      {groups.length > 0 ? <label className="space-y-1 text-sm"><span>用于注册的分组</span><select aria-label="Mihomo 注册分组" value={group} onChange={(event) => setGroup(event.target.value)} disabled={busy} className="h-10 w-full rounded-md border border-input bg-background px-3">
        {groups.map((item) => <option key={item.name} value={item.name} disabled={item.nodes === 0}>{item.name}（{item.nodes} 个节点）</option>)}
      </select><p className="text-xs text-muted-foreground">具体节点参与轮询，子分组入口、直连及拒绝节点跳过。每个注册任务固定一个出口。</p></label> : null}
      {error ? <p role="alert" className="text-sm text-destructive">{error}</p> : null}
      <p className="text-xs text-muted-foreground">保存时会向现有 Mihomo 加载注册专用监听端口，不改写其磁盘配置。再次启动注册时自动恢复这些端口，无需另启动代理进程。</p>
      <Button type="button" disabled={busy || !group || !Number.isInteger(Number(portStart)) || Number(portStart) < 1024 || Number(portStart) > 65535} onClick={() => void apply()}>保存并用于注册</Button>
    </DialogContent>
  </Dialog>;
}
