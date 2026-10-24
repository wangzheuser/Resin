import { useQuery } from "@tanstack/react-query";
import { Badge } from "../../components/ui/Badge";
import { Select } from "../../components/ui/Select";
import { useI18n } from "../../i18n";
import { apiRequest } from "../../lib/api-client";
import { formatDateTime } from "../../lib/time";
import type { Platform } from "../platforms/types";
import type { DownloadMode, Subscription } from "./types";

const modes: Record<DownloadMode, string> = {
  auto: "自动选择下载出口", direct: "仅直连下载", platform: "通过 Resin 平台下载", external_proxy: "通过外部代理下载",
};

export function SubscriptionDownloadFields({ id, mode, platformID, platforms, onChange }: {
  id: string; mode: DownloadMode; platformID: string; platforms: Platform[];
  onChange: (mode: DownloadMode, platformID: string) => void;
}) {
  const { t } = useI18n();
  const env = useQuery({ queryKey: ["subscription-download-env"], queryFn: () => apiRequest<{
    subscription_proxy_configured: boolean; subscription_proxy_address: string;
  }>("/api/v1/system/config/env"), staleTime: 60_000 });
  return <div className="field-group field-span-2">
    <label className="field-label" htmlFor={id}>{t("订阅下载出口")}</label>
    <Select id={id} value={mode} onChange={(event) => onChange(event.target.value as DownloadMode, "")}>
      {Object.entries(modes).map(([value, label]) => <option key={value} value={value}>{t(label)}</option>)}
    </Select>
    {mode === "platform" ? <Select aria-label={t("下载平台")} value={platformID} onChange={(event) => onChange(mode, event.target.value)}>
      <option value="">{t("请选择下载平台")}</option>
      {platforms.map((platform) => <option key={platform.id} value={platform.id}>{platform.name}</option>)}
    </Select> : null}
    <small className="muted">{t("仅用于获取订阅内容，不改变节点的前置连接方式。")}</small>
    {mode === "auto" || mode === "external_proxy" ? <small className="muted">
      {env.isError ? t("外部代理配置状态读取失败") : env.isLoading ? t("加载中...") : env.data?.subscription_proxy_configured
        ? `${t("外部代理已配置")} · ${env.data.subscription_proxy_address}` : t("外部代理尚未配置")}
    </small> : null}
  </div>;
}

const statuses: Record<string, string> = {
  never: "尚未检查", queued: "排队中", updating: "更新中", ok: "更新成功", retry_wait: "等待重试", error: "更新失败", disabled: "已禁用",
};
const stages: Record<string, string> = {
  complete: "下载完成", timeout: "下载超时", dns: "DNS 解析失败", connection: "连接失败", tls: "TLS 握手失败",
  http: "上游响应异常", no_route: "无可用下载出口", configuration: "下载配置错误", cancelled: "已取消",
};

export function SubscriptionUpdateStatus({ subscription: sub, detail = false }: { subscription: Subscription; detail?: boolean }) {
  const { t } = useI18n();
  const status = sub.update_status ?? (!sub.enabled ? "disabled" : sub.last_error ? "error" : sub.last_updated ? "ok" : "never");
  return <div className="subscriptions-status-cell">
    <Badge variant={status === "ok" ? "success" : status === "error" ? "danger" : "warning"}>
      {t(status === "ok" && sub.source_type === "local" ? "解析正常" : statuses[status] ?? "尚未检查")}
    </Badge>
    {detail ? <>
      {sub.next_refresh ? <p className="muted">{t("下次执行：{{time}}", { time: formatDateTime(sub.next_refresh) })}</p> : null}
      {sub.last_error ? <p className="muted">{t("更新失败期间保留缓存节点；节点健康状态独立显示。")}</p> : null}
      {sub.download_attempts?.length ? <ol>
        {sub.download_attempts.map((attempt, index) => <li key={index}>
          {t(modes[attempt.route as DownloadMode] ?? attempt.route)} · {t(stages[attempt.stage] ?? attempt.stage)}
          {attempt.http_status ? ` · HTTP ${attempt.http_status}` : ""} · {attempt.duration_ms} ms
          {attempt.node_hash ? <small> · {attempt.node_hash.slice(0, 12)}</small> : null}
        </li>)}
      </ol> : null}
    </> : null}
  </div>;
}
