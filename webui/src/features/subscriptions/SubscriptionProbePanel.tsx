import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { Button } from "../../components/ui/Button";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { cancelSubscriptionProbe, getSubscriptionProbeJob, subscriptionProbeKey } from "./api";

export function SubscriptionProbePanel() {
  const { t } = useI18n();
  const client = useQueryClient();
  const query = useQuery({
    queryKey: subscriptionProbeKey,
    queryFn: getSubscriptionProbeJob,
    refetchInterval: (q) => q.state.data?.status === "running" || q.state.data?.status === "cancelling" ? 1_000 : 10_000,
  });
  const job = query.data;
  const cancel = useMutation({
    mutationFn: cancelSubscriptionProbe,
    onSuccess: (result) => client.setQueryData(subscriptionProbeKey, result),
  });
  useEffect(() => {
    if (job?.status === "completed" || job?.status === "cancelled") {
      void client.invalidateQueries({ queryKey: ["subscriptions"] });
      void client.invalidateQueries({ queryKey: ["nodes"] });
    }
  }, [client, job?.id, job?.status]);

  if (query.isError) return <p className="field-error">{formatApiErrorMessage(query.error, t)}</p>;
  if (!job) return null;
  const active = job.status === "running" || job.status === "cancelling";
  const labels = { running: "复测中", cancelling: "正在取消", cancelled: "已取消", completed: "复测完成" };
  return (
    <div className="subscription-probe-panel" aria-live="polite">
      <div>
        <strong>{job.subscription_name} · {t(labels[job.status])}</strong>
        <p>{t("已处理 {{done}} / {{total}} · 成功 {{success}} · 失败 {{failed}} · 跳过 {{skipped}}", {
          done: job.completed, total: job.total, success: job.succeeded, failed: job.failed, skipped: job.skipped,
        })}</p>
        <progress aria-label={t("订阅复测进度")} max={Math.max(job.total, 1)} value={job.completed} />
        <p className="subscription-health-hint">{t("跳过已移除或尚未创建出站的节点；任务记录保留至下次复测或服务重启。")}</p>
        {cancel.isError ? <p className="field-error">{formatApiErrorMessage(cancel.error, t)}</p> : null}
      </div>
      {active ? <Button size="sm" variant="secondary" disabled={cancel.isPending || job.status === "cancelling"}
        onClick={() => cancel.mutate(job.id)}>{t("取消复测")}</Button> : null}
    </div>
  );
}
