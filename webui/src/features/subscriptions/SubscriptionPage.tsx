import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createColumnHelper } from "@tanstack/react-table";
import { AlertTriangle, ArrowRight, Eye, Filter, Info, Pencil, Plus, Power, RefreshCw, Route, Search, Sparkles, Trash2, Upload, X } from "lucide-react";
import { type ChangeEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { Link } from "react-router-dom";
import { z } from "zod";
import { Badge } from "../../components/ui/Badge";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { DataTable } from "../../components/ui/DataTable";
import { Input } from "../../components/ui/Input";
import { OffsetPagination } from "../../components/ui/OffsetPagination";
import { Select } from "../../components/ui/Select";
import { Switch } from "../../components/ui/Switch";
import { Textarea } from "../../components/ui/Textarea";
import { ToastContainer } from "../../components/ui/Toast";
import { useToast } from "../../hooks/useToast";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { formatDateTime, formatGoDuration, formatRelativeTime } from "../../lib/time";
import { listPlatforms } from "../platforms/api";
import type { Platform } from "../platforms/types";
import {
  cleanupSubscriptionCircuitOpenNodes,
  createSubscription,
  deleteSubscription,
  getSubscription,
  listSubscriptions,
  refreshSubscription,
  updateSubscription,
} from "./api";
import { mergeUniqueSubscriptionLines } from "./import-text";
import type { Subscription } from "./types";

type EnabledFilter = "all" | "enabled" | "disabled";
type SubscriptionSourceType = "remote" | "local";

const SUBSCRIPTION_SOURCE_TABS: Array<{ key: SubscriptionSourceType; label: string; hint: string }> = [
  { key: "remote", label: "远程", hint: "从 HTTP/HTTPS 订阅链接拉取内容" },
  { key: "local", label: "本地", hint: "直接填写订阅文本，不经过网络拉取" },
];

const subscriptionCreateSchema = z.object({
  name: z.string().trim().min(1, "订阅名称不能为空"),
  source_type: z.enum(["remote", "local"]),
  url: z.string(),
  content: z.string(),
  relay_platform_id: z.string(),
  update_interval: z.string().trim().min(1, "更新间隔不能为空"),
  ephemeral_node_evict_delay: z.string().trim().min(1, "临时节点驱逐延迟不能为空"),
  enabled: z.boolean(),
  ephemeral: z.boolean(),
  incremental_alive_nodes: z.boolean(),
}).superRefine((value, ctx) => {
  const url = value.url.trim();
  const content = value.content.trim();
  if (value.source_type === "remote") {
    if (!url) {
      ctx.addIssue({ code: z.ZodIssueCode.custom, path: ["url"], message: "URL 不能为空" });
      return;
    }
    if (!(url.startsWith("http://") || url.startsWith("https://"))) {
      ctx.addIssue({ code: z.ZodIssueCode.custom, path: ["url"], message: "URL 必须是 http/https 地址" });
    }
    return;
  }
  if (!content) {
    ctx.addIssue({ code: z.ZodIssueCode.custom, path: ["content"], message: "订阅内容不能为空" });
  }
});

const subscriptionEditSchema = subscriptionCreateSchema;

type SubscriptionCreateForm = z.infer<typeof subscriptionCreateSchema>;
type SubscriptionEditForm = z.infer<typeof subscriptionEditSchema>;
const EMPTY_SUBSCRIPTIONS: Subscription[] = [];
const EMPTY_PLATFORMS: Platform[] = [];
const PAGE_SIZE_OPTIONS = [10, 20, 50, 100] as const;
const LOCAL_SOURCE_UPDATE_INTERVAL = "12h";
const LARGE_SUBSCRIPTION_CONTENT_LENGTH = 256 * 1024;
const SUBSCRIPTION_DISABLE_HINT = "禁用订阅后，相关节点不会参与平台路由、健康统计或自动探测。";
const SUBSCRIPTION_EPHEMERAL_HINT = "临时订阅的非健康节点会在一段时间后被自动删除。订阅本身不会被删除。";
const SUBSCRIPTION_INCREMENTAL_HINT = "开启后刷新时保留当前仍存活的旧节点，仅清理失效旧节点，并合并新订阅内容；关闭后仅保留刷新后的订阅内容。";

function extractHostname(url: string): string {
  try {
    return new URL(url).hostname;
  } catch {
    return url;
  }
}

function subscriptionToEditForm(subscription: Subscription): SubscriptionEditForm {
  return {
    name: subscription.name,
    source_type: subscription.source_type,
    url: subscription.url,
    content: subscription.content ?? "",
    relay_platform_id: subscription.relay_platform_id ?? "",
    update_interval: subscription.update_interval,
    ephemeral_node_evict_delay: subscription.ephemeral_node_evict_delay,
    enabled: subscription.enabled,
    ephemeral: subscription.ephemeral,
    incremental_alive_nodes: subscription.incremental_alive_nodes,
  };
}

function sourceTypeLabel(sourceType: SubscriptionSourceType): string {
  return sourceType === "local" ? "本地" : "远程";
}

function parseEnabledFilter(value: EnabledFilter): boolean | undefined {
  if (value === "enabled") {
    return true;
  }
  if (value === "disabled") {
    return false;
  }
  return undefined;
}

function normalizeSubmitUpdateInterval(sourceType: SubscriptionSourceType, raw: string): string {
  if (sourceType === "local") {
    return LOCAL_SOURCE_UPDATE_INTERVAL;
  }
  return raw.trim();
}

/** Loads every Platform page so the relay selector never hides valid choices. */
async function listRelayPlatformOptions(): Promise<Platform[]> {
  const pageSize = 100;
  const first = await listPlatforms({ limit: pageSize, offset: 0 });
  if (first.items.length >= first.total) {
    return first.items;
  }
  const remainingPages = Math.ceil((first.total - first.items.length) / pageSize);
  const remaining = await Promise.all(
    Array.from({ length: remainingPages }, (_, index) =>
      listPlatforms({ limit: pageSize, offset: (index + 1) * pageSize })
    )
  );
  return first.items.concat(remaining.flatMap((page) => page.items));
}

type RelayPlatformPickerProps = {
  id: string;
  value: string;
  platforms: Platform[];
  loading: boolean;
  loadError: boolean;
  onChange: (platformID: string) => void;
};

/** Renders a compact route preview and the single-hop Platform selector. */
function RelayPlatformPicker({ id, value, platforms, loading, loadError, onChange }: RelayPlatformPickerProps) {
  const { t } = useI18n();
  const selected = platforms.find((item) => item.id === value);

  return (
    <div className={`field-group field-span-2 relay-platform-picker ${value ? "relay-platform-picker-active" : ""}`}>
      <div className="relay-platform-picker-head">
        <div>
          <label className="field-label" htmlFor={id}>{t("前置 Platform")}</label>
          <p>{t("为本订阅解析出的节点统一指定单跳前置出口。")}</p>
        </div>
        <span className="relay-mode-badge">{value ? t("单跳") : t("直连")}</span>
      </div>

      <Select
        id={id}
        value={value}
        disabled={loading}
        onChange={(event) => onChange(event.target.value)}
      >
        <option value="">{t("直连（不使用前置节点）")}</option>
        {value && !selected ? <option value={value}>{t("当前引用的 Platform（{{id}}）", { id: value })}</option> : null}
        {platforms.map((item) => (
          <option key={item.id} value={item.id}>
            {t("{{name}} · 路由视图 {{count}} 个节点", { name: item.name, count: item.routable_node_count })}
          </option>
        ))}
      </Select>

      <div className="relay-route-preview" aria-live="polite">
        <span>Resin</span>
        <ArrowRight size={14} />
        {value ? (
          <>
            <strong><Route size={14} />{selected?.name ?? t("前置 Platform")}</strong>
            <ArrowRight size={14} />
          </>
        ) : null}
        <span>{t("订阅节点")}</span>
      </div>

      {loadError ? (
        <p className="field-error">{t("Platform 列表加载失败，请刷新后重试。")}</p>
      ) : value ? (
        <div className="relay-platform-hint-stack">
          <p className="relay-platform-hint">
            {t("每次连接从该 Platform 的健康直连节点中轮询，最多尝试 3 个；候选耗尽时保持失败，不回退直连。")}
          </p>
          <p className="relay-platform-hint relay-platform-change-hint">
            {t("保存链路变化时会立即移除旧节点身份，并刷新生成新身份。")}
          </p>
        </div>
      ) : (
        <p className="relay-platform-hint">{t("节点将按原始配置直接连接。")}</p>
      )}
    </div>
  );
}

export function SubscriptionPage() {
  const { t } = useI18n();
  const [enabledFilter, setEnabledFilter] = useState<EnabledFilter>("all");
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(0);
  const [pageSize, setPageSize] = useState<number>(20);
  const [selectedSubscriptionId, setSelectedSubscriptionId] = useState("");
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [createModalOpen, setCreateModalOpen] = useState(false);
  const [createContentEditorExpanded, setCreateContentEditorExpanded] = useState(true);
  const [createContentLength, setCreateContentLength] = useState(0);
  const [editContentEditorExpanded, setEditContentEditorExpanded] = useState(true);
  const [editContentLength, setEditContentLength] = useState(0);
  const [editFormSubscriptionId, setEditFormSubscriptionId] = useState("");
  const [pendingRefreshIds, setPendingRefreshIds] = useState<Set<string>>(() => new Set());
  const { toasts, showToast, dismissToast } = useToast();
  const pendingRefreshIdsRef = useRef<Set<string>>(new Set());
  const createImportInputRef = useRef<HTMLInputElement>(null);
  const editImportInputRef = useRef<HTMLInputElement>(null);

  const queryClient = useQueryClient();
  const enabledValue = parseEnabledFilter(enabledFilter);
  const subscriptionContentPlaceholder = [
    t("支持格式："),
    t("sing-box / Clash|Mihomo / URI（vmess:// vless:// trojan:// ss:// ...）或他们的 base64 格式"),
    "",
    t("HTTP/HTTPS/SOCKS 示例："),
    t("1.2.3.4:8080:user:pass （HTTP 认证代理）"),
    t("http://user:pass@1.2.3.4:8080（HTTP 认证代理）"),
    t("https://user:pass@example.com:8443?sni=example.com（HTTPS + SNI）"),
    t("socks5://user:pass@1.2.3.4:1080"),
    t("socks5h://user:pass@example.com:1080"),
  ].join("\n");

  const subscriptionsQuery = useQuery({
    queryKey: ["subscriptions", enabledFilter, page, pageSize, search],
    queryFn: () =>
      listSubscriptions({
        enabled: enabledValue,
        limit: pageSize,
        offset: page * pageSize,
        keyword: search,
      }),
    refetchInterval: 30_000,
    placeholderData: (prev) => prev,
  });

  const subscriptions = subscriptionsQuery.data?.items ?? EMPTY_SUBSCRIPTIONS;
  const totalSubscriptions = subscriptionsQuery.data?.total ?? 0;

  const totalPages = Math.max(1, Math.ceil(totalSubscriptions / pageSize));
  const currentPage = Math.min(page, totalPages - 1);

  const relayPlatformsQuery = useQuery({
    queryKey: ["platforms", "subscription-relay-options"],
    queryFn: listRelayPlatformOptions,
    staleTime: 30_000,
  });
  const relayPlatforms = relayPlatformsQuery.data ?? EMPTY_PLATFORMS;
  const relayPlatformByID = useMemo(
    () => new Map(relayPlatforms.map((item) => [item.id, item])),
    [relayPlatforms]
  );

  const selectedSubscription = useMemo(() => {
    if (!selectedSubscriptionId) {
      return null;
    }
    return subscriptions.find((item) => item.id === selectedSubscriptionId) ?? null;
  }, [selectedSubscriptionId, subscriptions]);

  const drawerVisible = drawerOpen && Boolean(selectedSubscription);

  const subscriptionDetailQuery = useQuery({
    queryKey: ["subscription", selectedSubscriptionId],
    queryFn: () => getSubscription(selectedSubscriptionId),
    enabled: drawerVisible,
    staleTime: 30_000,
  });

  const createForm = useForm<SubscriptionCreateForm>({
    resolver: zodResolver(subscriptionCreateSchema),
    defaultValues: {
      name: "",
      source_type: "remote",
      url: "",
      content: "",
      relay_platform_id: "",
      update_interval: "12h",
      ephemeral_node_evict_delay: "72h",
      enabled: true,
      ephemeral: false,
      incremental_alive_nodes: false,
    },
  });

  const createEphemeral = createForm.watch("ephemeral");
  const createSourceType = createForm.watch("source_type");
  const createRelayPlatformID = createForm.watch("relay_platform_id");

  const editForm = useForm<SubscriptionEditForm>({
    resolver: zodResolver(subscriptionEditSchema),
    defaultValues: {
      name: "",
      source_type: "remote",
      url: "",
      content: "",
      relay_platform_id: "",
      update_interval: "12h",
      ephemeral_node_evict_delay: "72h",
      enabled: true,
      ephemeral: false,
      incremental_alive_nodes: false,
    },
  });

  const editEphemeral = editForm.watch("ephemeral");
  const editSourceType = editForm.watch("source_type");
  const editRelayPlatformID = editForm.watch("relay_platform_id");

  useEffect(() => {
    if (!drawerOpen || !subscriptionDetailQuery.data) {
      return;
    }
    const contentLength = subscriptionDetailQuery.data.content.length;
    editForm.reset(subscriptionToEditForm(subscriptionDetailQuery.data));
    setEditContentLength(contentLength);
    setEditContentEditorExpanded(contentLength < LARGE_SUBSCRIPTION_CONTENT_LENGTH);
    setEditFormSubscriptionId(subscriptionDetailQuery.data.id);
  }, [drawerOpen, subscriptionDetailQuery.data, editForm]);

  useEffect(() => {
    if (!drawerVisible) {
      return;
    }

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") {
        return;
      }
      setDrawerOpen(false);
    };

    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [drawerVisible]);

  const invalidateSubscriptions = async () => {
    await queryClient.invalidateQueries({ queryKey: ["subscriptions"] });
  };

  const invalidateSubscriptionsAndNodes = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["subscriptions"] }),
      queryClient.invalidateQueries({ queryKey: ["nodes"] }),
    ]);
  };

  const importLocalSubscriptionFiles = async (
    event: ChangeEvent<HTMLInputElement>,
    currentContent: string,
    setContent: (content: string) => void,
    setContentLength: (length: number) => void,
    setEditorExpanded: (expanded: boolean) => void,
  ) => {
    const input = event.currentTarget;
    const files = Array.from(input.files ?? []);
    if (files.length === 0) {
      return;
    }

    if (currentContent.length + files.reduce((total, file) => total + file.size, 0) >= LARGE_SUBSCRIPTION_CONTENT_LENGTH) {
      setEditorExpanded(false);
    }

    try {
      const result = mergeUniqueSubscriptionLines(currentContent, await Promise.all(files.map((file) => file.text())));
      setContent(result.content);
      setContentLength(result.content.length);
      setEditorExpanded(result.content.length < LARGE_SUBSCRIPTION_CONTENT_LENGTH);
      showToast("success", t("已导入 {{added}} 条，跳过 {{duplicates}} 条重复内容", result));
    } catch {
      showToast("error", t("读取订阅文本文件失败"));
    } finally {
      input.value = "";
    }
  };

  const createMutation = useMutation({
    mutationFn: createSubscription,
    onSuccess: async (created) => {
      await invalidateSubscriptions();
      setCreateModalOpen(false);
      createForm.reset({
        name: "",
        source_type: "remote",
        url: "",
        content: "",
        relay_platform_id: "",
        update_interval: LOCAL_SOURCE_UPDATE_INTERVAL,
        ephemeral_node_evict_delay: "72h",
        enabled: true,
        ephemeral: false,
        incremental_alive_nodes: false,
      });
      setCreateContentEditorExpanded(true);
      setCreateContentLength(0);
      showToast("success", t("订阅 {{name}} 创建成功", { name: created.name }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const updateMutation = useMutation({
    mutationFn: async (formData: SubscriptionEditForm) => {
      if (!selectedSubscription) {
        throw new Error("请选择要编辑的订阅");
      }

      const payload = {
        name: formData.name.trim(),
        update_interval: normalizeSubmitUpdateInterval(formData.source_type, formData.update_interval),
        ephemeral_node_evict_delay: formData.ephemeral_node_evict_delay.trim(),
        enabled: formData.enabled,
        ephemeral: formData.ephemeral,
        incremental_alive_nodes: formData.incremental_alive_nodes,
        relay_platform_id: formData.relay_platform_id,
        ...(formData.source_type === "remote"
          ? { url: formData.url.trim() }
          : editForm.getFieldState("content").isDirty
            ? { content: formData.content }
            : {}),
      };
      return updateSubscription(selectedSubscription.id, payload);
    },
    onSuccess: async (updated) => {
      queryClient.setQueryData(["subscription", updated.id], updated);
      await invalidateSubscriptions();
      setSelectedSubscriptionId(updated.id);
      showToast("success", t("订阅 {{name}} 已更新", { name: updated.name }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const toggleEnabledMutation = useMutation({
    mutationFn: async ({ subscription, enabled }: { subscription: Subscription; enabled: boolean }) =>
      updateSubscription(subscription.id, { enabled }),
    onSuccess: async (updated) => {
      queryClient.setQueryData(["subscription", updated.id], updated);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["subscriptions"] }),
        queryClient.invalidateQueries({ queryKey: ["nodes"] }),
        queryClient.invalidateQueries({ queryKey: ["platforms"] }),
      ]);
      showToast(
        "success",
        updated.enabled
          ? t("订阅 {{name}} 已启用", { name: updated.name })
          : t("订阅 {{name}} 已禁用", { name: updated.name }),
      );
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });
  const toggleEnabledMutateAsync = toggleEnabledMutation.mutateAsync;
  const isToggleEnabledPending = toggleEnabledMutation.isPending;

  const deleteMutation = useMutation({
    mutationFn: async (subscription: Subscription) => {
      await deleteSubscription(subscription.id);
      return subscription;
    },
    onSuccess: async (deleted) => {
      await invalidateSubscriptions();
      if (selectedSubscriptionId === deleted.id) {
        setSelectedSubscriptionId("");
        setDrawerOpen(false);
      }
      showToast("success", t("订阅 {{name}} 已删除", { name: deleted.name }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });
  const deleteSubscriptionMutateAsync = deleteMutation.mutateAsync;
  const isDeletePending = deleteMutation.isPending;

  const refreshMutation = useMutation({
    mutationFn: async (subscription: Subscription) => {
      await refreshSubscription(subscription.id);
      return subscription;
    },
    onSuccess: async (subscription) => {
      await invalidateSubscriptions();
      showToast("success", t("订阅 {{name}} 已手动刷新", { name: subscription.name }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });
  const refreshSubscriptionMutateAsync = refreshMutation.mutateAsync;

  const markRefreshPending = useCallback((subscriptionId: string): boolean => {
    if (pendingRefreshIdsRef.current.has(subscriptionId)) {
      return false;
    }
    const next = new Set(pendingRefreshIdsRef.current);
    next.add(subscriptionId);
    pendingRefreshIdsRef.current = next;
    setPendingRefreshIds(next);
    return true;
  }, []);

  const clearRefreshPending = useCallback((subscriptionId: string) => {
    if (!pendingRefreshIdsRef.current.has(subscriptionId)) {
      return;
    }
    const next = new Set(pendingRefreshIdsRef.current);
    next.delete(subscriptionId);
    pendingRefreshIdsRef.current = next;
    setPendingRefreshIds(next);
  }, []);

  const isRefreshPending = useCallback((subscriptionId: string): boolean => pendingRefreshIds.has(subscriptionId), [pendingRefreshIds]);

  const cleanupCircuitOpenNodesMutation = useMutation({
    mutationFn: async (subscription: Subscription) => {
      const cleanedCount = await cleanupSubscriptionCircuitOpenNodes(subscription.id);
      return { subscription, cleanedCount };
    },
    onSuccess: async ({ subscription, cleanedCount }) => {
      await invalidateSubscriptionsAndNodes();
      if (cleanedCount > 0) {
        showToast("success", t("订阅 {{name}} 已清理 {{count}} 个节点", { name: subscription.name, count: cleanedCount }));
        return;
      }
      showToast("success", t("订阅 {{name}} 没有可清理的熔断或异常节点", { name: subscription.name }));
    },
    onError: (error) => {
      showToast("error", formatApiErrorMessage(error, t));
    },
  });

  const onCreateSubmit = createForm.handleSubmit(async (values) => {
    const payload = {
      name: values.name.trim(),
      source_type: values.source_type,
      update_interval: normalizeSubmitUpdateInterval(values.source_type, values.update_interval),
      ephemeral_node_evict_delay: values.ephemeral_node_evict_delay.trim(),
      enabled: values.enabled,
      ephemeral: values.ephemeral,
      incremental_alive_nodes: values.incremental_alive_nodes,
      relay_platform_id: values.relay_platform_id,
      ...(values.source_type === "remote"
        ? { url: values.url.trim() }
        : { content: values.content }),
    };
    await createMutation.mutateAsync(payload);
  });

  const onEditSubmit = editForm.handleSubmit(async (values) => {
    await updateMutation.mutateAsync(values);
  });

  const handleDelete = useCallback(async (subscription: Subscription) => {
    const confirmed = window.confirm(t("确认删除订阅 {{name}}？关联节点会被清理。", { name: subscription.name }));
    if (!confirmed) {
      return;
    }
    await deleteSubscriptionMutateAsync(subscription);
  }, [deleteSubscriptionMutateAsync, t]);

  const handleToggleEnabled = useCallback(async (subscription: Subscription) => {
    const enabled = !subscription.enabled;
    const confirmed = window.confirm(
      enabled
        ? t("确认启用订阅 {{name}}？启用后相关节点将重新进入检查和路由流程。", { name: subscription.name })
        : t("确认禁用订阅 {{name}}？禁用后相关节点将立即退出平台路由。", { name: subscription.name }),
    );
    if (!confirmed) {
      return;
    }
    try {
      await toggleEnabledMutateAsync({ subscription, enabled });
    } catch {
      // Mutation callbacks already surface the failure to the user.
    }
  }, [t, toggleEnabledMutateAsync]);

  const handleCleanupCircuitOpenNodes = async (subscription: Subscription) => {
    const confirmed = window.confirm(t("确认立即清理订阅 {{name}} 中的熔断或异常节点？", { name: subscription.name }));
    if (!confirmed) {
      return;
    }
    await cleanupCircuitOpenNodesMutation.mutateAsync(subscription);
  };

  const openDrawer = useCallback((subscription: Subscription) => {
    setEditFormSubscriptionId("");
    setSelectedSubscriptionId(subscription.id);
    setDrawerOpen(true);
  }, []);

  const handleRefresh = useCallback(async (subscription: Subscription) => {
    if (!markRefreshPending(subscription.id)) {
      return;
    }
    try {
      await refreshSubscriptionMutateAsync(subscription);
    } catch {
      // Mutation callbacks already surface the failure to the user.
    } finally {
      clearRefreshPending(subscription.id);
    }
  }, [clearRefreshPending, markRefreshPending, refreshSubscriptionMutateAsync]);

  const changePageSize = (next: number) => {
    setPageSize(next);
    setPage(0);
  };

  const col = useMemo(() => createColumnHelper<Subscription>(), []);

  const subColumns = useMemo(
    () => [
      col.accessor("name", {
        header: t("名称"),
        cell: (info) => <p className="subscriptions-name-cell">{info.getValue()}</p>,
      }),
      col.accessor("url", {
        header: t("订阅源"),
        cell: (info) => {
          const s = info.row.original;
          if (s.source_type === "local") {
            return (
              <p className="subscriptions-url-cell" title={t("本地订阅")}>
                {t("本地订阅")}
              </p>
            );
          }
          return (
            <p className="subscriptions-url-cell" title={info.getValue()}>
              {extractHostname(info.getValue())}
            </p>
          );
        },
      }),
      col.accessor("update_interval", {
        header: t("更新间隔"),
        cell: (info) => formatGoDuration(info.getValue()),
      }),
      col.display({
        id: "relay_platform",
        header: t("链路"),
        cell: (info) => {
          const platformID = info.row.original.relay_platform_id;
          if (!platformID) {
            return <span className="subscription-route-chip subscription-route-chip-direct">{t("直连")}</span>;
          }
          const platformName = relayPlatformByID.get(platformID)?.name ?? t("未知 Platform");
          return (
            <span className="subscription-route-chip" title={platformID}>
              <Route size={12} />
              {platformName}
            </span>
          );
        },
      }),
      col.display({
        id: "node_count",
        header: t("节点数"),
        cell: (info) => {
          const s = info.row.original;
          return `${s.healthy_node_count} / ${s.node_count}`;
        },
      }),
      col.display({
        id: "status",
        header: t("状态"),
        cell: (info) => {
          const s = info.row.original;
          return (
            <div className="subscriptions-status-cell">
              {!s.enabled ? (
                <Badge variant="warning">{t("已禁用")}</Badge>
              ) : s.last_error ? (
                <Badge variant="danger">{t("错误")}</Badge>
              ) : (
                <Badge variant="success">{t("正常")}</Badge>
              )}
            </div>
          );
        },
      }),
      col.accessor("last_checked", {
        header: t("上次检查"),
        cell: (info) => formatRelativeTime(info.getValue() || ""),
      }),
      col.accessor("last_updated", {
        header: t("上次更新"),
        cell: (info) => formatRelativeTime(info.getValue() || ""),
      }),
      col.display({
        id: "actions",
        header: t("操作"),
        cell: (info) => {
          const s = info.row.original;
          return (
            <div className="subscriptions-row-actions" onClick={(event) => event.stopPropagation()}>
              <Link
                className="btn btn-ghost btn-sm"
                to={`/nodes?subscription_id=${encodeURIComponent(s.id)}`}
                title={t("预览节点池")}
                aria-label={t("预览订阅 {{name}} 的节点池", { name: s.name })}
              >
                <Eye size={14} />
              </Link>
              <Button size="sm" variant="ghost" onClick={() => openDrawer(s)} title={t("编辑")}>
                <Pencil size={14} />
              </Button>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => void handleRefresh(s)}
                disabled={isRefreshPending(s.id)}
                title={t("刷新")}
              >
                <RefreshCw size={14} />
              </Button>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => void handleToggleEnabled(s)}
                disabled={isToggleEnabledPending}
                title={s.enabled ? t("禁用订阅") : t("启用订阅")}
                aria-label={s.enabled ? t("禁用订阅 {{name}}", { name: s.name }) : t("启用订阅 {{name}}", { name: s.name })}
              >
                <Power size={14} />
              </Button>
              <Button
                size="sm"
                variant="ghost"
                onClick={() => void handleDelete(s)}
                disabled={isDeletePending}
                title={t("删除")}
                style={{ color: "var(--delete-btn-color, #c27070)" }}
              >
                <Trash2 size={14} />
              </Button>
            </div>
          );
        },
      }),
    ],
    [col, handleDelete, handleRefresh, handleToggleEnabled, isDeletePending, isRefreshPending, isToggleEnabledPending, openDrawer, relayPlatformByID, t]
  );

  return (
    <section className="platform-page">
      <header className="module-header">
        <div>
          <h2>{t("订阅管理")}</h2>
          <p className="module-description">{t("保障订阅按计划更新，异常时可一键刷新。")}</p>
        </div>
      </header>

      <ToastContainer toasts={toasts} onDismiss={dismissToast} />

      <Card className="platform-list-card platform-directory-card">
        <div className="list-card-header">
          <div>
            <h3>{t("订阅列表")}</h3>
            <p>{t("共 {{count}} 个订阅", { count: totalSubscriptions })}</p>
          </div>
          <div style={{ display: "flex", gap: "0.5rem", alignItems: "center" }}>
            <label className="subscription-inline-filter" htmlFor="sub-status-filter" style={{ flexDirection: "row", alignItems: "center", gap: 6 }}>
              <Filter size={16} />
              <Select
                id="sub-status-filter"
                value={enabledFilter}
                onChange={(event) => {
                  setEnabledFilter(event.target.value as EnabledFilter);
                  setPage(0);
                }}
              >
                <option value="all">{t("全部")}</option>
                <option value="enabled">{t("仅启用")}</option>
                <option value="disabled">{t("仅禁用")}</option>
              </Select>
            </label>
            <label className="search-box" htmlFor="subscription-search" style={{ maxWidth: 200, margin: 0, gap: 6 }}>
              <Search size={16} />
              <Input
                id="subscription-search"
                placeholder={t("搜索订阅")}
                value={search}
                onChange={(event) => {
                  setSearch(event.target.value);
                  setPage(0);
                }}
                style={{ padding: "6px 10px", borderRadius: 8 }}
              />
            </label>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setCreateModalOpen(true)}
            >
              <Plus size={16} />
              {t("新建")}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => subscriptionsQuery.refetch()}
              disabled={subscriptionsQuery.isFetching}
            >
              <RefreshCw size={16} className={subscriptionsQuery.isFetching ? "spin" : undefined} />
              {t("刷新")}
            </Button>
          </div>
        </div>
      </Card>

      <Card className="platform-cards-container subscriptions-table-card">
        {subscriptionsQuery.isLoading ? <p className="muted">{t("正在加载订阅数据...")}</p> : null}

        {subscriptionsQuery.isError ? (
          <div className="callout callout-error">
            <AlertTriangle size={14} />
            <span>{formatApiErrorMessage(subscriptionsQuery.error, t)}</span>
          </div>
        ) : null}

        {!subscriptionsQuery.isLoading && !subscriptions.length ? (
          <div className="empty-box">
            <Sparkles size={16} />
            <p>{t("没有匹配的订阅")}</p>
          </div>
        ) : null}

        {subscriptions.length ? (
          <DataTable
            data={subscriptions}
            columns={subColumns}
            onRowClick={openDrawer}
            getRowId={(s) => s.id}
            className="data-table-subs"
          />
        ) : null}

        <OffsetPagination
          page={currentPage}
          totalPages={totalPages}
          totalItems={totalSubscriptions}
          pageSize={pageSize}
          pageSizeOptions={PAGE_SIZE_OPTIONS}
          onPageChange={setPage}
          onPageSizeChange={changePageSize}
        />
      </Card>

      {drawerVisible && selectedSubscription ? (
        <div
          className="drawer-overlay"
          role="dialog"
          aria-modal="true"
          aria-label={t("编辑订阅 {{name}}", { name: selectedSubscription.name })}
          onClick={() => setDrawerOpen(false)}
        >
          <Card className="drawer-panel" onClick={(event) => event.stopPropagation()}>
            <div className="drawer-header">
              <div>
                <h3>{selectedSubscription.name}</h3>
                <p>{selectedSubscription.id}</p>
              </div>
              <div className="drawer-header-actions">
                <Badge variant={selectedSubscription.enabled ? "success" : "warning"}>
                  {selectedSubscription.enabled ? t("运行中") : t("已停用")}
                </Badge>
                <Button
                  variant="ghost"
                  size="sm"
                  aria-label={t("关闭编辑面板")}
                  onClick={() => setDrawerOpen(false)}
                >
                  <X size={16} />
                </Button>
              </div>
            </div>
            <div className="platform-drawer-layout">
              <section className="platform-drawer-section">
                <div className="platform-drawer-section-head">
                  <h4>{t("订阅配置")}</h4>
                  <p>
                    {editSourceType === "local"
                      ? t("更新本地订阅配置、刷新周期与状态开关后点击保存。")
                      : t("更新 URL、刷新周期与状态开关后点击保存。")}
                  </p>
                </div>

                <div className="stats-grid">
                  <div>
                    <span>{t("创建时间")}</span>
                    <p>{formatDateTime(selectedSubscription.created_at)}</p>
                  </div>
                  <div>
                    <span>{t("上次检查")}</span>
                    <p>{formatDateTime(selectedSubscription.last_checked || "")}</p>
                  </div>
                  <div>
                    <span>{t("上次更新")}</span>
                    <p>{formatDateTime(selectedSubscription.last_updated || "")}</p>
                  </div>
                </div>

                {selectedSubscription.last_error ? (
                  <div className="callout callout-error">{t("最近错误：{{message}}", { message: selectedSubscription.last_error })}</div>
                ) : (
                  <div className="callout callout-success">{t("最近一次刷新无错误")}</div>
                )}

                {subscriptionDetailQuery.isError ? (
                  <div className="callout callout-error">
                    <AlertTriangle size={14} />
                    <span>{formatApiErrorMessage(subscriptionDetailQuery.error, t)}</span>
                  </div>
                ) : subscriptionDetailQuery.isLoading
                  || !subscriptionDetailQuery.data
                  || subscriptionDetailQuery.data.id !== editFormSubscriptionId ? (
                  <p className="muted">{t("正在加载订阅详情...")}</p>
                ) : (
                <form className="form-grid" onSubmit={onEditSubmit}>
                  <input type="hidden" {...editForm.register("source_type")} />

                  <div className="field-group">
                    <label className="field-label" htmlFor="edit-sub-name">
                      {t("订阅名称")}
                    </label>
                    <Input
                      id="edit-sub-name"
                      invalid={Boolean(editForm.formState.errors.name)}
                      {...editForm.register("name")}
                    />
                    {editForm.formState.errors.name?.message ? (
                      <p className="field-error">{t(editForm.formState.errors.name.message)}</p>
                    ) : null}
                  </div>

                  <div className="field-group">
                    <label className="field-label" htmlFor="edit-sub-source-type">
                      {t("订阅类型")}
                    </label>
                    <Input
                      id="edit-sub-source-type"
                      value={t(sourceTypeLabel(editSourceType))}
                      readOnly
                      disabled
                    />
                  </div>

                  {editSourceType === "remote" ? (
                    <>
                      <div className="field-group field-span-2">
                        <label className="field-label" htmlFor="edit-sub-interval">
                          {t("更新间隔")}
                        </label>
                        <Input
                          id="edit-sub-interval"
                          placeholder={t("例如 12h")}
                          invalid={Boolean(editForm.formState.errors.update_interval)}
                          {...editForm.register("update_interval")}
                        />
                        {editForm.formState.errors.update_interval?.message ? (
                          <p className="field-error">{t(editForm.formState.errors.update_interval.message)}</p>
                        ) : null}
                      </div>

                      <div className="field-group field-span-2">
                        <label className="field-label" htmlFor="edit-sub-url">
                          {t("订阅链接")}
                        </label>
                        <Input id="edit-sub-url" invalid={Boolean(editForm.formState.errors.url)} {...editForm.register("url")} />
                        {editForm.formState.errors.url?.message ? (
                          <p className="field-error">{t(editForm.formState.errors.url.message)}</p>
                        ) : null}
                      </div>
                    </>
                  ) : (
                    <div className="field-group field-span-2">
                      <div className="subscription-content-heading">
                        <label className="field-label" htmlFor={editContentEditorExpanded ? "edit-sub-content" : undefined}>
                          {t("订阅内容")}
                        </label>
                        <Button
                          size="sm"
                          variant="secondary"
                          title={t("导入 TXT")}
                          onClick={() => editImportInputRef.current?.click()}
                        >
                          <Upload size={14} />
                          {t("导入 TXT")}
                        </Button>
                        <input
                          ref={editImportInputRef}
                          type="file"
                          accept=".txt,text/plain"
                          multiple
                          hidden
                          aria-label={t("导入 TXT")}
                          onChange={(event) => void importLocalSubscriptionFiles(
                            event,
                            editForm.getValues("content"),
                            (content) => {
                              editForm.setValue("content", content, { shouldDirty: true });
                              editForm.clearErrors("content");
                            },
                            setEditContentLength,
                            setEditContentEditorExpanded,
                          )}
                        />
                      </div>
                      {editContentEditorExpanded ? (
                        <Textarea
                          id="edit-sub-content"
                          rows={8}
                          placeholder={subscriptionContentPlaceholder}
                          invalid={Boolean(editForm.formState.errors.content)}
                          {...editForm.register("content")}
                        />
                      ) : (
                        <div className="subscription-content-collapsed">
                          <span>
                            {t("正文较大，已折叠以保持页面流畅")}
                            <small>{t("{{count}} 个字符", { count: editContentLength.toLocaleString() })}</small>
                          </span>
                          <Button size="sm" variant="secondary" onClick={() => setEditContentEditorExpanded(true)}>
                            <Eye size={14} />
                            {t("展开编辑")}
                          </Button>
                        </div>
                      )}
                      {editForm.formState.errors.content?.message ? (
                        <p className="field-error">{t(editForm.formState.errors.content.message)}</p>
                      ) : null}
                    </div>
                  )}

                  <RelayPlatformPicker
                    id="edit-sub-relay-platform"
                    value={editRelayPlatformID}
                    platforms={relayPlatforms}
                    loading={relayPlatformsQuery.isLoading}
                    loadError={relayPlatformsQuery.isError}
                    onChange={(platformID) => editForm.setValue("relay_platform_id", platformID, { shouldDirty: true, shouldValidate: true })}
                  />

                  <div className="field-group">
                    <label className="field-label" htmlFor="edit-sub-ephemeral" style={{ visibility: "hidden" }}>
                      {t("临时订阅")}
                    </label>
                    <div className="subscription-switch-item">
                      <label className="subscription-switch-label" htmlFor="edit-sub-ephemeral">
                        <span>{t("临时订阅")}</span>
                        <span
                          className="subscription-info-icon"
                          title={t(SUBSCRIPTION_EPHEMERAL_HINT)}
                          aria-label={t(SUBSCRIPTION_EPHEMERAL_HINT)}
                          tabIndex={0}
                        >
                          <Info size={13} />
                        </span>
                      </label>
                      <Switch id="edit-sub-ephemeral" {...editForm.register("ephemeral")} />
                    </div>
                  </div>

                  <div className="field-group">
                    <label className="field-label" htmlFor="edit-sub-incremental-alive-nodes" style={{ visibility: "hidden" }}>
                      {t("存活节点增量模式")}
                    </label>
                    <div className="subscription-switch-item">
                      <label className="subscription-switch-label" htmlFor="edit-sub-incremental-alive-nodes">
                        <span>{t("存活节点增量模式")}</span>
                        <span
                          className="subscription-info-icon"
                          title={t(SUBSCRIPTION_INCREMENTAL_HINT)}
                          aria-label={t(SUBSCRIPTION_INCREMENTAL_HINT)}
                          tabIndex={0}
                        >
                          <Info size={13} />
                        </span>
                      </label>
                      <Switch id="edit-sub-incremental-alive-nodes" {...editForm.register("incremental_alive_nodes")} />
                    </div>
                  </div>

                  <div className="field-group">
                    <label className="field-label" htmlFor="edit-sub-ephemeral-evict-delay">
                      {t("临时节点驱逐延迟")}
                    </label>
                    <Input
                      id="edit-sub-ephemeral-evict-delay"
                      placeholder={t("例如 72h")}
                      invalid={Boolean(editForm.formState.errors.ephemeral_node_evict_delay)}
                      disabled={!editEphemeral}
                      {...editForm.register("ephemeral_node_evict_delay")}
                    />
                    {editForm.formState.errors.ephemeral_node_evict_delay?.message ? (
                      <p className="field-error">{t(editForm.formState.errors.ephemeral_node_evict_delay.message)}</p>
                    ) : null}
                  </div>

                  <div className="field-group">
                    <label className="field-label" htmlFor="edit-sub-enabled" style={{ visibility: "hidden" }}>
                      {t("启用")}
                    </label>
                    <div className="subscription-switch-item">
                      <label className="subscription-switch-label" htmlFor="edit-sub-enabled">
                        <span>{t("启用")}</span>
                        <span
                          className="subscription-info-icon"
                          title={t(SUBSCRIPTION_DISABLE_HINT)}
                          aria-label={t(SUBSCRIPTION_DISABLE_HINT)}
                          tabIndex={0}
                        >
                          <Info size={13} />
                        </span>
                      </label>
                      <Switch id="edit-sub-enabled" {...editForm.register("enabled")} />
                    </div>
                  </div>

                  <div className="platform-config-actions">
                    <Button type="submit" disabled={updateMutation.isPending}>
                      {updateMutation.isPending ? t("保存中...") : t("保存配置")}
                    </Button>
                  </div>
                </form>
                )}
              </section>

              <section className="platform-drawer-section platform-ops-section">
                <div className="platform-drawer-section-head">
                  <h4>{t("运维操作")}</h4>
                </div>

                <div className="platform-ops-list">
                  <div className="platform-op-item">
                    <div className="platform-op-copy">
                      <h5>{t("手动刷新")}</h5>
                      <p className="platform-op-hint">{t("立即刷新订阅并同步节点。")}</p>
                    </div>
                    <Button
                      variant="secondary"
                      onClick={() => void handleRefresh(selectedSubscription)}
                      disabled={isRefreshPending(selectedSubscription.id)}
                    >
                      {isRefreshPending(selectedSubscription.id) ? t("刷新中...") : t("立即刷新")}
                    </Button>
                  </div>

                  <div className="platform-op-item">
                    <div className="platform-op-copy">
                      <h5>{t("清理失效节点")}</h5>
                      <p className="platform-op-hint">{t("立即清理当前熔断，或出错的节点。")}</p>
                    </div>
                    <Button
                      variant="secondary"
                      onClick={() => void handleCleanupCircuitOpenNodes(selectedSubscription)}
                      disabled={cleanupCircuitOpenNodesMutation.isPending}
                    >
                      {cleanupCircuitOpenNodesMutation.isPending ? t("清理中...") : t("立即清理")}
                    </Button>
                  </div>

                  <div className="platform-op-item">
                    <div className="platform-op-copy">
                      <h5>{t("删除订阅")}</h5>
                      <p className="platform-op-hint">{t("删除订阅并清理关联节点，操作不可撤销。")}</p>
                    </div>
                    <Button
                      variant="danger"
                      onClick={() => void handleDelete(selectedSubscription)}
                      disabled={deleteMutation.isPending}
                    >
                      {deleteMutation.isPending ? t("删除中...") : t("删除订阅")}
                    </Button>
                  </div>
                </div>
              </section>
            </div>
          </Card>
        </div>
      ) : null}

      {createModalOpen ? (
        <div className="modal-overlay" role="dialog" aria-modal="true">
          <Card className="modal-card">
            <div className="modal-header">
              <h3>{t("新建订阅")}</h3>
              <Button variant="ghost" size="sm" onClick={() => setCreateModalOpen(false)}>
                <X size={16} />
              </Button>
            </div>

            <form className="form-grid" onSubmit={onCreateSubmit}>
              <input type="hidden" {...createForm.register("source_type")} />

              <div className="field-group field-span-2">
                <label className="field-label" htmlFor="create-sub-name">
                  {t("订阅名称")}
                </label>
                <Input
                  id="create-sub-name"
                  invalid={Boolean(createForm.formState.errors.name)}
                  {...createForm.register("name")}
                />
                {createForm.formState.errors.name?.message ? (
                  <p className="field-error">{t(createForm.formState.errors.name.message)}</p>
                ) : null}
              </div>

              <div className="field-group field-span-2">
                <label className="field-label">{t("订阅来源")}</label>
                <div className="platform-detail-tabs" role="tablist" aria-label={t("订阅来源类型")}>
                  {SUBSCRIPTION_SOURCE_TABS.map((tab) => {
                    const selected = createSourceType === tab.key;
                    return (
                      <button
                        key={tab.key}
                        type="button"
                        role="tab"
                        aria-selected={selected}
                        className={`platform-detail-tab ${selected ? "platform-detail-tab-active" : ""}`}
                        title={t(tab.hint)}
                        onClick={() => createForm.setValue("source_type", tab.key, { shouldDirty: true, shouldValidate: true })}
                      >
                        <span>{t(tab.label)}</span>
                      </button>
                    );
                  })}
                </div>
              </div>

              {createSourceType === "remote" ? (
                <>
                  <div className="field-group field-span-2">
                    <label className="field-label" htmlFor="create-sub-interval">
                      {t("更新间隔")}
                    </label>
                    <Input
                      id="create-sub-interval"
                      placeholder={t("例如 12h")}
                      invalid={Boolean(createForm.formState.errors.update_interval)}
                      {...createForm.register("update_interval")}
                    />
                    {createForm.formState.errors.update_interval?.message ? (
                      <p className="field-error">{t(createForm.formState.errors.update_interval.message)}</p>
                    ) : null}
                  </div>

                  <div className="field-group field-span-2">
                    <label className="field-label" htmlFor="create-sub-url">
                      {t("订阅链接")}
                    </label>
                    <Input
                      id="create-sub-url"
                      invalid={Boolean(createForm.formState.errors.url)}
                      {...createForm.register("url")}
                    />
                    {createForm.formState.errors.url?.message ? (
                      <p className="field-error">{t(createForm.formState.errors.url.message)}</p>
                    ) : null}
                  </div>
                </>
              ) : (
                <div className="field-group field-span-2">
                  <div className="subscription-content-heading">
                    <label className="field-label" htmlFor={createContentEditorExpanded ? "create-sub-content" : undefined}>
                      {t("订阅内容")}
                    </label>
                    <Button
                      size="sm"
                      variant="secondary"
                      title={t("导入 TXT")}
                      onClick={() => createImportInputRef.current?.click()}
                    >
                      <Upload size={14} />
                      {t("导入 TXT")}
                    </Button>
                    <input
                      ref={createImportInputRef}
                      type="file"
                      accept=".txt,text/plain"
                      multiple
                      hidden
                      aria-label={t("导入 TXT")}
                      onChange={(event) => void importLocalSubscriptionFiles(
                        event,
                        createForm.getValues("content"),
                        (content) => {
                          createForm.setValue("content", content, { shouldDirty: true });
                          createForm.clearErrors("content");
                        },
                        setCreateContentLength,
                        setCreateContentEditorExpanded,
                      )}
                    />
                  </div>
                  {createContentEditorExpanded ? (
                    <Textarea
                      id="create-sub-content"
                      rows={8}
                      placeholder={subscriptionContentPlaceholder}
                      invalid={Boolean(createForm.formState.errors.content)}
                      {...createForm.register("content")}
                    />
                  ) : (
                    <div className="subscription-content-collapsed">
                      <span>
                        {t("正文较大，已折叠以保持页面流畅")}
                        <small>{t("{{count}} 个字符", { count: createContentLength.toLocaleString() })}</small>
                      </span>
                      <Button size="sm" variant="secondary" onClick={() => setCreateContentEditorExpanded(true)}>
                        <Eye size={14} />
                        {t("展开编辑")}
                      </Button>
                    </div>
                  )}
                  {createForm.formState.errors.content?.message ? (
                    <p className="field-error">{t(createForm.formState.errors.content.message)}</p>
                  ) : null}
                </div>
              )}

              <RelayPlatformPicker
                id="create-sub-relay-platform"
                value={createRelayPlatformID}
                platforms={relayPlatforms}
                loading={relayPlatformsQuery.isLoading}
                loadError={relayPlatformsQuery.isError}
                onChange={(platformID) => createForm.setValue("relay_platform_id", platformID, { shouldDirty: true, shouldValidate: true })}
              />

              <div className="field-group">
                <label className="field-label" htmlFor="create-sub-ephemeral" style={{ visibility: "hidden" }}>
                  {t("临时订阅")}
                </label>
                <div className="subscription-switch-item">
                  <label className="subscription-switch-label" htmlFor="create-sub-ephemeral">
                    <span>{t("临时订阅")}</span>
                    <span
                      className="subscription-info-icon"
                      title={t(SUBSCRIPTION_EPHEMERAL_HINT)}
                      aria-label={t(SUBSCRIPTION_EPHEMERAL_HINT)}
                      tabIndex={0}
                    >
                      <Info size={13} />
                    </span>
                  </label>
                  <Switch id="create-sub-ephemeral" {...createForm.register("ephemeral")} />
                </div>
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="create-sub-incremental-alive-nodes" style={{ visibility: "hidden" }}>
                  {t("存活节点增量模式")}
                </label>
                <div className="subscription-switch-item">
                  <label className="subscription-switch-label" htmlFor="create-sub-incremental-alive-nodes">
                    <span>{t("存活节点增量模式")}</span>
                    <span
                      className="subscription-info-icon"
                      title={t(SUBSCRIPTION_INCREMENTAL_HINT)}
                      aria-label={t(SUBSCRIPTION_INCREMENTAL_HINT)}
                      tabIndex={0}
                    >
                      <Info size={13} />
                    </span>
                  </label>
                  <Switch id="create-sub-incremental-alive-nodes" {...createForm.register("incremental_alive_nodes")} />
                </div>
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="create-sub-ephemeral-evict-delay">
                  {t("临时节点驱逐延迟")}
                </label>
                <Input
                  id="create-sub-ephemeral-evict-delay"
                  placeholder={t("例如 72h")}
                  invalid={Boolean(createForm.formState.errors.ephemeral_node_evict_delay)}
                  disabled={!createEphemeral}
                  {...createForm.register("ephemeral_node_evict_delay")}
                />
                {createForm.formState.errors.ephemeral_node_evict_delay?.message ? (
                  <p className="field-error">{t(createForm.formState.errors.ephemeral_node_evict_delay.message)}</p>
                ) : null}
              </div>

              <div className="field-group">
                <label className="field-label" htmlFor="create-sub-enabled" style={{ visibility: "hidden" }}>
                  {t("启用")}
                </label>
                <div className="subscription-switch-item">
                  <label className="subscription-switch-label" htmlFor="create-sub-enabled">
                    <span>{t("启用")}</span>
                    <span
                      className="subscription-info-icon"
                      title={t(SUBSCRIPTION_DISABLE_HINT)}
                      aria-label={t(SUBSCRIPTION_DISABLE_HINT)}
                      tabIndex={0}
                    >
                      <Info size={13} />
                    </span>
                  </label>
                  <Switch id="create-sub-enabled" {...createForm.register("enabled")} />
                </div>
              </div>

              <div className="detail-actions" style={{ justifyContent: "flex-end" }}>
                <Button type="submit" disabled={createMutation.isPending}>
                  {createMutation.isPending ? t("创建中...") : t("确认创建")}
                </Button>
                <Button variant="secondary" onClick={() => setCreateModalOpen(false)}>
                  {t("取消")}
                </Button>
              </div>
            </form>
          </Card>
        </div>
      ) : null}
    </section>
  );
}
