export type DownloadMode = "auto" | "direct" | "platform" | "external_proxy";
export type DownloadAttempt = { route: string; node_hash?: string; stage: string; duration_ms: number; http_status?: number; error?: string };
export type Subscription = {
  download_mode?: DownloadMode;
  download_platform_id?: string;
  update_status?: "never" | "queued" | "updating" | "ok" | "retry_wait" | "error" | "disabled";
  consecutive_failures?: number;
  next_refresh?: string;
  last_error_kind?: string;
  download_attempts?: DownloadAttempt[];
  id: string;
  name: string;
  source_type: "remote" | "local";
  url: string;
  content: string;
  relay_platform_id: string;
  update_interval: string;
  node_count: number;
  healthy_node_count: number;
  pending_node_count: number;
  failed_node_count: number;
  build_failed_node_count: number;
  disabled_node_count: number;
  stale_node_count: number;
  last_probe_at?: string;
  ephemeral: boolean;
  incremental_alive_nodes: boolean;
  ephemeral_node_evict_delay: string;
  enabled: boolean;
  created_at: string;
  last_checked?: string;
  last_updated?: string;
  last_error?: string;
};

export type SubscriptionProbeJob = {
  id: string;
  subscription_id: string;
  subscription_name: string;
  status: "running" | "cancelling" | "cancelled" | "completed";
  total: number;
  completed: number;
  succeeded: number;
  failed: number;
  skipped: number;
  started_at: string;
  finished_at?: string;
};

export type PageResponse<T> = {
  items: T[];
  total: number;
  limit: number;
  offset: number;
};

export type SubscriptionCreateInput = {
  download_mode?: DownloadMode;
  download_platform_id?: string;
  name: string;
  source_type?: "remote" | "local";
  url?: string;
  content?: string;
  relay_platform_id?: string;
  update_interval?: string;
  enabled?: boolean;
  ephemeral?: boolean;
  incremental_alive_nodes?: boolean;
  ephemeral_node_evict_delay?: string;
};

export type SubscriptionUpdateInput = {
  download_mode?: DownloadMode;
  download_platform_id?: string;
  name?: string;
  url?: string;
  content?: string;
  relay_platform_id?: string;
  update_interval?: string;
  enabled?: boolean;
  ephemeral?: boolean;
  incremental_alive_nodes?: boolean;
  ephemeral_node_evict_delay?: string;
};
