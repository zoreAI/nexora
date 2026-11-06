"use client";

import { useEffect, useState } from "react";
import {
  ChevronDown,
  FileArchive,
  FileText,
  ListChecks,
  type LucideIcon,
} from "lucide-react";

import webConfig from "@/constants/common-env";
import { getStoredAuthSession } from "@/store/auth";

type ParamRow = [string, string, string];

type ApiDoc = {
  title: string;
  method: string;
  path: string;
  icon: LucideIcon;
  input: ParamRow[];
  output: ParamRow[];
  example: (baseUrl: string, serviceBaseUrl: string, key: string) => string;
};

const docs: ApiDoc[] = [
  {
    title: "模型列表",
    method: "GET",
    path: "/v1/models",
    icon: ListChecks,
    input: [["Authorization", "header", "Bearer <auth-key>。"]],
    output: [["data", "array", "模型列表，包含 id、object、created、owned_by。"]],
    example: (baseUrl, _serviceBaseUrl, key) => `curl ${baseUrl}/models \\
  -H "Authorization: Bearer ${key}"`,
  },
  {
    title: "聊天补全",
    method: "POST",
    path: "/v1/chat/completions",
    icon: FileText,
    input: [
      ["model", "string", "模型名，默认 auto。"],
      ["messages", "array", "OpenAI 兼容消息数组。"],
      ["stream", "boolean", "可选，是否流式返回。"],
    ],
    output: [
      ["id", "string", "响应 ID。"],
      ["choices", "array", "OpenAI 兼容 choices。"],
      ["usage", "object", "可选，token 使用信息。"],
    ],
    example: (baseUrl, _serviceBaseUrl, key) => `curl ${baseUrl}/chat/completions \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer ${key}" \\
  -d '{"model":"auto","messages":[{"role":"user","content":"你好"}]}'`,
  },
  {
    title: "Responses",
    method: "POST",
    path: "/v1/responses",
    icon: FileText,
    input: [
      ["model", "string", "模型名。"],
      ["input", "string | array | object", "用户输入；图片工具调用会走生图逻辑。"],
      ["tools", "array", "可选，Responses 工具定义。"],
      ["stream", "boolean", "可选，是否流式返回。"],
    ],
    output: [
      ["id", "string", "响应 ID。"],
      ["output", "array", "Responses 兼容输出。"],
      ["status", "string", "响应状态。"],
    ],
    example: (baseUrl, _serviceBaseUrl, key) => `curl ${baseUrl}/responses \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer ${key}" \\
  -d '{"model":"auto","input":"生成一张未来城市图片"}'`,
  },
  {
    title: "Messages",
    method: "POST",
    path: "/v1/messages",
    icon: FileText,
    input: [
      ["model", "string", "模型名，默认 auto。"],
      ["messages", "array", "Anthropic Messages 兼容消息数组。"],
      ["stream", "boolean", "可选，是否流式返回。"],
    ],
    output: [
      ["id", "string", "响应 ID。"],
      ["content", "array", "Messages 兼容输出内容。"],
      ["usage", "object", "可选，token 使用信息。"],
    ],
    example: (baseUrl, _serviceBaseUrl, key) => `curl ${baseUrl}/messages \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer ${key}" \\
  -d '{"model":"auto","messages":[{"role":"user","content":"你好"}]}'`,
  },
  {
    title: "图片生成",
    method: "POST",
    path: "/v1/images/generations",
    icon: FileArchive,
    input: [
      ["prompt", "string", "图片生成提示词。"],
      ["model", "string", "可选，默认 gpt-image-2，支持 gpt-image-2.6-sunburst / gpt-image-2.6-flare。"],
      ["n", "number", "可选，生成数量，当前限制 1-4。"],
      ["size", "string", "可选，支持 1024x1024、9:16、16:9 等。"],
      ["resolution", "string", "可选，1k / 2k / 4k；gpt-image-2 的 2k/4k 走 Codex；指定 Web 版本模型时作为尺寸提示，实际尺寸由上游决定。"],
      ["response_format", "string", "可选，url 或 b64_json，默认 b64_json。"],
      ["stream", "boolean", "可选，是否流式返回。"],
    ],
    output: [
      ["data", "array", "图片结果列表。"],
      ["data[].b64_json", "string", "base64 图片内容。"],
      ["data[].url", "string", "本地图片访问 URL。"],
      ["data[].revised_prompt", "string", "模型改写后的提示词。"],
    ],
    example: (baseUrl, _serviceBaseUrl, key) => `curl ${baseUrl}/images/generations \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer ${key}" \\
  -d '{"model":"gpt-image-2","prompt":"一个亚洲女孩在大街上","n":1,"size":"9:16","resolution":"4k","response_format":"url"}'`,
  },
  {
    title: "图片编辑",
    method: "POST",
    path: "/v1/images/edits",
    icon: FileArchive,
    input: [
      ["image / images / image_url", "file | JSON", "参考图；支持 multipart 文件，也支持 JSON 中的 base64、data URL 或公网图片链接，最多 16 张。"],
      ["markup", "PNG file | JSON", "可选；与首张参考图同尺寸的透明 PNG。JSON 支持 image_url、data URL 或 base64，不支持 file_id。Web 号池会将标注叠加到首张参考图供模型参考；不等同于上游原生 markup。不能与 mask 同时提交。"],
      ["mask", "file | JSON", "当前 Web 号池不支持；提交时返回错误。"],
      ["prompt", "string", "编辑提示词。"],
      ["model", "string", "可选，默认 gpt-image-2。"],
      ["n", "number", "可选，生成数量，当前限制 1-4。"],
      ["size", "string", "可选，图片尺寸或比例。"],
      ["resolution", "string", "可选，1k / 2k / 4k。"],
      ["response_format", "string", "可选，url 或 b64_json。"],
    ],
    output: [
      ["data", "array", "编辑后的图片结果列表。"],
      ["data[].b64_json", "string", "base64 图片内容。"],
      ["data[].url", "string", "本地图片访问 URL。"],
    ],
    example: (baseUrl, _serviceBaseUrl, key) => `curl ${baseUrl}/images/edits \\
  -H "Authorization: Bearer ${key}" \\
  -F "model=gpt-image-2" \\
  -F "prompt=改成赛博朋克夜景" \\
  -F "image=@./input.png" \\
  -F "markup=@./markup.png" \\
  -F "response_format=url"`,
  },
  {
    title: "创建图片任务",
    method: "POST",
    path: "/api/image-tasks/generations",
    icon: FileArchive,
    input: [
      ["client_task_id", "string", "可选；同一密钥重复提交该 ID 时返回已有任务。"],
      ["prompt", "string", "图片生成提示词。"],
      ["model", "string", "可选，默认 gpt-image-2。"],
      ["n", "number", "可选，生成数量，当前限制 1-4。"],
      ["size", "string", "可选，图片尺寸或比例。"],
      ["resolution", "string", "可选，1k / 2k / 4k。"],
    ],
    output: [
      ["id", "string", "任务 ID，用于后续查询或取消。"],
      ["status", "queued | running | success | error | canceled", "任务状态。"],
      ["data", "array", "成功后包含图片结果。"],
      ["error", "string", "失败时返回错误信息。"],
    ],
    example: (_baseUrl, serviceBaseUrl, key) => `curl ${serviceBaseUrl}/api/image-tasks/generations \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer ${key}" \\
  -d '{"client_task_id":"demo-001","model":"gpt-image-2","prompt":"一张极简产品海报","n":1}'`,
  },
  {
    title: "创建图片编辑任务",
    method: "POST",
    path: "/api/image-tasks/edits",
    icon: FileArchive,
    input: [
      ["client_task_id", "string", "可选；同一密钥重复提交该 ID 时返回已有任务。"],
      ["image", "file | file[]", "参考图，支持 multipart 的 image 或 image[]。"],
      ["prompt", "string", "图片编辑提示词。"],
      ["model", "string", "可选，默认 gpt-image-2。"],
      ["size", "string", "可选，图片尺寸或比例。"],
      ["resolution", "string", "可选，1k / 2k / 4k。"],
    ],
    output: [
      ["id", "string", "任务 ID，用于后续查询或取消。"],
      ["status", "queued | running | success | error | canceled", "任务状态。"],
      ["data", "array", "成功后包含图片结果。"],
      ["error", "string", "失败时返回错误信息。"],
    ],
    example: (_baseUrl, serviceBaseUrl, key) => `curl ${serviceBaseUrl}/api/image-tasks/edits \\
  -H "Authorization: Bearer ${key}" \\
  -F "client_task_id=edit-001" \\
  -F "model=gpt-image-2" \\
  -F "prompt=保持主体不变，改成电商白底图" \\
  -F "image=@./input.png"`,
  },
  {
    title: "图片任务查询",
    method: "GET",
    path: "/api/image-tasks",
    icon: ListChecks,
    input: [
      ["Authorization", "header", "Bearer <auth-key>。"],
      ["ids", "query", "可选，逗号分隔的任务 ID。"],
    ],
    output: [
      ["items", "array", "当前密钥可见的任务列表。"],
      ["items[].status", "string", "queued / running / success / error / canceled。"],
      ["items[].data", "array", "成功任务的图片结果。"],
      ["missing_ids", "array", "请求的 ID 中未找到的任务。"],
    ],
    example: (_baseUrl, serviceBaseUrl, key) => `curl ${serviceBaseUrl}/api/image-tasks \\
  -H "Authorization: Bearer ${key}"`,
  },
];

const usableModels = [
  "gpt-image-2",
  "gpt-image-2.5",
  "gpt-image-2.5-flare",
  "gpt-image-2.5-sunburst",
  "gpt-image-2.6-sunburst",
  "gpt-image-2.6-flare",
  "codex-gpt-image-2",
  "auto",
  "gpt-5-3",
  "gpt-5-3-mini",
  "gpt-5-5",
  "gpt-5-5-mini",
  "gpt-5-mini",
];

function ParamTable({ rows }: { rows: ParamRow[] }) {
  return (
    <div className="overflow-hidden rounded-xl border border-stone-200">
      <table className="w-full text-left text-xs">
        <thead className="bg-stone-50 text-stone-500">
          <tr>
            <th className="px-3 py-2 font-medium">参数</th>
            <th className="px-3 py-2 font-medium">类型</th>
            <th className="px-3 py-2 font-medium">说明</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-stone-100 bg-white">
          {rows.map(([name, type, desc]) => (
            <tr key={name}>
              <td className="px-3 py-2 font-mono text-stone-800">{name}</td>
              <td className="px-3 py-2 font-mono text-stone-500">{type}</td>
              <td className="px-3 py-2 text-stone-600">{desc}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function ApiDocsSection() {
  const [authKey, setAuthKey] = useState("");
  const serviceBaseUrl = webConfig.apiUrl.replace(/\/$/, "") || (typeof window !== "undefined" ? window.location.origin : "");
  const openAIBaseUrl = `${serviceBaseUrl}/v1`;
  const displayKey = authKey || "<当前密钥>";

  useEffect(() => {
    let active = true;
    void getStoredAuthSession().then((session) => {
      if (active) setAuthKey(session?.key || "");
    });
    return () => {
      active = false;
    };
  }, []);

  return (
    <div className="space-y-5">
      <div className="grid gap-3 md:grid-cols-2">
        <div className="space-y-1 rounded-xl border border-stone-200 bg-white px-3 py-2">
          <div className="text-xs text-stone-500">服务地址</div>
          <div className="break-all font-mono text-xs text-stone-800">{serviceBaseUrl}</div>
        </div>
        <div className="space-y-1 rounded-xl border border-stone-200 bg-white px-3 py-2">
          <div className="text-xs text-stone-500">Base URL（OpenAI）</div>
          <div className="break-all font-mono text-xs text-stone-800">{openAIBaseUrl}</div>
        </div>
        <div className="space-y-1 rounded-xl border border-stone-200 bg-white px-3 py-2">
          <div className="text-xs text-stone-500">API Key</div>
          <div className="break-all font-mono text-xs text-stone-800">{displayKey}</div>
        </div>
        <div className="space-y-1 rounded-xl border border-stone-200 bg-white px-3 py-2">
          <div className="text-xs text-stone-500">请求头</div>
          <div className="break-all font-mono text-xs text-stone-800">Authorization: Bearer {displayKey}</div>
        </div>
      </div>

      <div className="space-y-2">
        <div className="text-xs font-medium text-stone-600">常用模型，也可请求 /v1/models 获取</div>
        <div className="flex flex-wrap gap-2">
          {usableModels.map((model) => (
            <span key={model} className="rounded-md border border-stone-200 bg-white px-2 py-1 font-mono text-xs text-stone-700">
              {model}
            </span>
          ))}
        </div>
      </div>

      <div className="space-y-3">
        {docs.map((item) => {
          const Icon = item.icon;
          return (
            <details key={item.path} className="group rounded-xl border border-stone-200 bg-white px-4 py-3">
              <summary className="flex cursor-pointer list-none items-center justify-between gap-3">
                <span className="flex min-w-0 items-center gap-3">
                  <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-stone-100 text-stone-600">
                    <Icon className="size-4" />
                  </span>
                  <span className="min-w-0">
                    <span className="block text-sm font-semibold text-stone-900">{item.title}</span>
                    <span className="mt-1 block truncate font-mono text-xs text-stone-500">
                      {item.method} {item.path}
                    </span>
                  </span>
                </span>
                <ChevronDown className="size-4 shrink-0 text-stone-400 transition group-open:rotate-180" />
              </summary>

              <div className="mt-4 grid gap-4 lg:grid-cols-2">
                <div className="space-y-2">
                  <h3 className="text-xs font-semibold text-stone-700">输入参数</h3>
                  <ParamTable rows={item.input} />
                </div>
                <div className="space-y-2">
                  <h3 className="text-xs font-semibold text-stone-700">输出参数</h3>
                  <ParamTable rows={item.output} />
                </div>
                <div className="space-y-2 lg:col-span-2">
                  <h3 className="text-xs font-semibold text-stone-700">调用示例</h3>
                  <pre className="overflow-auto whitespace-pre-wrap break-all rounded-xl bg-stone-950 px-3 py-3 text-xs leading-5 text-stone-100">
                    {item.example(openAIBaseUrl, serviceBaseUrl, displayKey)}
                  </pre>
                </div>
              </div>
            </details>
          );
        })}
      </div>
    </div>
  );
}
