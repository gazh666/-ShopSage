import { requestJSON } from './http';

export type FitlabHealth = {
  enabled: boolean;
  available: boolean;
  base_url?: string;
  latency_ms: number;
  engine?: string;
  error?: string;
};

export type TelemetryPathStat = {
  path: string;
  count: number;
  errors: number;
  error_rate: number;
  average_ms: number;
  p95_ms: number;
};

export type TelemetryEventView = {
  event_id: string;
  event_type: string;
  path: string;
  method: string;
  status: number;
  duration_ms: number;
  account_id?: string;
  role?: string;
  source: string;
  created_at: string;
};

export type TelemetrySummary = {
  window_hours: number;
  enabled: boolean;
  events: number;
  http_requests: number;
  client_events: number;
  errors: number;
  error_rate: number;
  average_ms: number;
  p95_ms: number;
  unique_accounts: number;
  by_path: TelemetryPathStat[];
  recent: TelemetryEventView[];
  error?: string;
};

export type EvalReportSummary = {
  id: string;
  run_id: string;
  kind: string;
  path: string;
  type: string;
  dataset: string;
  total: number;
  evaluated: number;
  hits: number;
  pass_rate: number;
  generated_at: string;
};

export type ExtEvalReports = {
  dir: string;
  reports: EvalReportSummary[];
  error?: string;
};

export type ExtOverview = {
  enabled: boolean;
  config: {
    fitlab_enabled: boolean;
    telemetry_enabled: boolean;
    fitlab_base_urls: string[];
    eval_dir: string;
  };
  fitlab: FitlabHealth;
  telemetry: TelemetrySummary;
  eval: {
    dir: string;
    reports: number;
    latest: EvalReportSummary[];
    error?: string;
  };
};

export type FitlabTryOnResult = {
  url: string;
  engine: string;
  category: string;
  durationMs: number;
};

export async function getExtOverview(token: string, hours = 24): Promise<ExtOverview> {
  return requestJSON<ExtOverview>(`/admin/ext/overview?hours=${hours}`, {
    headers: { Authorization: `Bearer ${token}` }
  });
}

export async function getExtEvalReports(token: string, kind?: string): Promise<ExtEvalReports> {
  const query = kind ? `?kind=${encodeURIComponent(kind)}` : '';
  return requestJSON<ExtEvalReports>(`/admin/ext/eval/reports${query}`, {
    headers: { Authorization: `Bearer ${token}` }
  });
}

export async function getExtEvalReportDetail(token: string, id: string): Promise<{ report: EvalReportSummary; raw: unknown }> {
  return requestJSON<{ report: EvalReportSummary; raw: unknown }>(
    `/admin/ext/eval/reports/detail?id=${encodeURIComponent(id)}`,
    { headers: { Authorization: `Bearer ${token}` } }
  );
}

export async function sendTelemetryEvents(
  events: Array<{
    event_type: string;
    path?: string;
    method?: string;
    status?: number;
    duration_ms?: number;
    metadata?: Record<string, unknown>;
  }>,
  token?: string
): Promise<{ accepted: number }> {
  const headers: Record<string, string> = {};
  if (token) headers.Authorization = `Bearer ${token}`;
  return requestJSON<{ accepted: number }>('/ext/telemetry/events', {
    method: 'POST',
    headers,
    body: JSON.stringify({ events })
  });
}

export async function tryOnFitlab(
  token: string,
  personImage: File,
  garmentImage: File,
  category: string
): Promise<FitlabTryOnResult> {
  const payload = new FormData();
  payload.append('person_image', personImage);
  payload.append('garment_image', garmentImage);
  payload.append('category', category);
  const response = await fetch('/api/v1/fitlab/tryon', {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}` },
    body: payload
  });
  if (!response.ok) {
    let message = `试衣服务暂不可用（${response.status}）`;
    let code = 'fitlab_unavailable';
    let degraded = true;
    try {
      const body = await response.json();
      if (body && typeof body === 'object') {
        message = String(body.message || message);
        code = String(body.code || code);
        degraded = Boolean(body.degraded);
      }
    } catch {
      // 保底使用 HTTP 状态信息。
    }
    const error = new Error(message) as Error & { code?: string; degraded?: boolean };
    error.code = code;
    error.degraded = degraded;
    throw error;
  }
  const blob = await response.blob();
  return {
    url: URL.createObjectURL(blob),
    engine: response.headers.get('X-Fitlab-Engine') ?? '',
    category: response.headers.get('X-Fitlab-Category') ?? category,
    durationMs: Number(response.headers.get('X-Fitlab-Duration-Ms') ?? 0)
  };
}
