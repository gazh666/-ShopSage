import { useEffect, useState } from 'react';
import {
  EvalReportSummary,
  ExtOverview,
  FitlabTryOnResult,
  TelemetryEventView,
  getExtEvalReportDetail,
  getExtEvalReports,
  getExtOverview,
  sendTelemetryEvents,
  tryOnFitlab
} from '../../api/extensions';
import { PanelMessage, formatCount, formatDateTime, formatPercent, useAsyncValue } from './AdminCommon';

const CATEGORY_OPTIONS = [
  { value: 'auto', label: '自动（上装）' },
  { value: 'upper', label: '上装' },
  { value: 'lower', label: '下装' },
  { value: 'dress', label: '连衣裙' }
];

export function AdminExtensionsPage({ token }: { token: string }) {
  const overview = useAsyncValue<ExtOverview>(() => getExtOverview(token, 24), [token]);
  const reports = useAsyncValue(() => getExtEvalReports(token), [token]);

  const [personFile, setPersonFile] = useState<File>();
  const [garmentFile, setGarmentFile] = useState<File>();
  const [category, setCategory] = useState('auto');
  const [running, setRunning] = useState(false);
  const [tryonError, setTryonError] = useState('');
  const [degraded, setDegraded] = useState(false);
  const [result, setResult] = useState<FitlabTryOnResult>();
  const [detailId, setDetailId] = useState('');
  const [detail, setDetail] = useState<unknown>();
  const [detailError, setDetailError] = useState('');

  useEffect(() => {
    return () => {
      if (result?.url) {
        URL.revokeObjectURL(result.url);
      }
    };
  }, [result]);

  async function handleTryOn() {
    if (!personFile || !garmentFile) {
      setTryonError('请先选择人物图和服装图');
      return;
    }
    setRunning(true);
    setTryonError('');
    setDegraded(false);
    try {
      const next = await tryOnFitlab(token, personFile, garmentFile, category);
      setResult(next);
      void sendTelemetryEvents(
        [
          {
            event_type: 'fitlab_tryon',
            path: '/admin/extensions',
            status: 200,
            duration_ms: next.durationMs,
            metadata: { category: next.category, engine: next.engine }
          }
        ],
        token
      ).catch(() => undefined);
    } catch (error) {
      const typed = error as Error & { degraded?: boolean };
      setTryonError(typed.message || '试衣失败');
      setDegraded(Boolean(typed.degraded));
    } finally {
      setRunning(false);
    }
  }

  async function loadDetail(report: EvalReportSummary) {
    setDetailId(report.id);
    setDetailError('');
    try {
      const payload = await getExtEvalReportDetail(token, report.id);
      setDetail(payload.raw);
    } catch (error) {
      setDetail(undefined);
      setDetailError(error instanceof Error ? error.message : '加载报告详情失败');
    }
  }

  function refresh() {
    void overview.reload();
    void reports.reload();
  }

  const fitlab = overview.value?.fitlab;
  const telemetry = overview.value?.telemetry;
  const reportItems = reports.value?.reports ?? [];

  return (
    <section>
      <header className="page-header">
        <div>
          <h1>扩展能力</h1>
          <p>试衣服务、追踪埋点与扩展评测均以非侵入式方式接入，独立开关、可单独降级。</p>
        </div>
        <button className="button button--ghost" disabled={overview.loading || reports.loading} onClick={refresh}>
          {overview.loading || reports.loading ? '刷新中' : '刷新'}
        </button>
      </header>
      {overview.error ? <p className="form-error">{overview.error}（请确认后端 EXT_ENABLED=true）</p> : null}

      <div className="admin-grid">
        <section className="panel eval-panel">
          <div className="panel-title-row">
            <div>
              <h2>运行状态</h2>
              <p>扩展开关、试衣服务健康度与近 24 小时埋点概览。</p>
            </div>
            {fitlab ? (
              <span className="tag">{fitlab.available ? '试衣可用' : '试衣降级'}</span>
            ) : null}
          </div>
          <div className="trace-summary">
            <Metric label="扩展开关" value={overview.value?.enabled ? '已开启' : '未开启'} />
            <Metric label="试衣开关" value={overview.value?.config.fitlab_enabled ? '已开启' : '已关闭'} />
            <Metric label="试衣服务" value={fitlab?.available ? '可用' : fitlab?.error || '不可用'} />
            <Metric label="试衣引擎" value={fitlab?.engine || '-'} />
            <Metric label="探测时延" value={`${formatCount(fitlab?.latency_ms ?? 0)}ms`} />
          </div>
          <div className="trace-summary">
            <Metric label="埋点事件" value={formatCount(telemetry?.events)} />
            <Metric label="服务端请求" value={formatCount(telemetry?.http_requests)} />
            <Metric label="前端事件" value={formatCount(telemetry?.client_events)} />
            <Metric label="错误率" value={formatPercent(telemetry?.error_rate)} />
            <Metric label="P95 时延" value={`${formatCount(telemetry?.p95_ms)}ms`} />
            <Metric label="活跃账号" value={formatCount(telemetry?.unique_accounts)} />
          </div>
          {telemetry?.error ? <p className="empty-text">埋点状态：{telemetry.error}</p> : null}
          {overview.value?.config.fitlab_base_urls.length ? (
            <div className="eval-detail-path">
              <span>试衣服务地址</span>
              <code>{overview.value.config.fitlab_base_urls.join(' , ')}</code>
            </div>
          ) : null}
        </section>

        <section className="panel eval-panel">
          <div className="panel-title-row">
            <div>
              <h2>试衣试跑</h2>
              <p>独立 HTTP 工具：后端代理 FitLab，服务异常时自动降级为文字导购，不影响 Agent 主链路。</p>
            </div>
          </div>
          <div className="form-grid">
            <label>
              人物图
              <input type="file" accept="image/*" onChange={(event) => setPersonFile(event.target.files?.[0])} />
            </label>
            <label>
              服装图
              <input type="file" accept="image/*" onChange={(event) => setGarmentFile(event.target.files?.[0])} />
            </label>
            <label>
              服装品类
              <select className="table-input" value={category} onChange={(event) => setCategory(event.target.value)}>
                {CATEGORY_OPTIONS.map((option) => (
                  <option value={option.value} key={option.value}>
                    {option.label}
                  </option>
                ))}
              </select>
            </label>
          </div>
          <div className="panel-actions-inline">
            <button className="button" disabled={running} onClick={handleTryOn}>
              {running ? '生成中...' : '生成试衣效果'}
            </button>
            {result ? (
              <span className="tag">
                {result.engine || 'fitlab'} / {result.category} / {result.durationMs}ms
              </span>
            ) : null}
          </div>
          {tryonError ? (
            <p className="form-error">
              {tryonError}
              {degraded ? '（已降级，可继续使用原文字导购流程）' : ''}
            </p>
          ) : null}
          {result ? (
            <div className="eval-detail">
              <img src={result.url} alt="试衣结果" style={{ maxWidth: '100%', borderRadius: 12 }} />
            </div>
          ) : (
            <p className="empty-text">上传人物图与服装图后生成合成结果。</p>
          )}
        </section>

        <section className="panel">
          <div className="panel-title-row">
            <div>
              <h2>埋点明细</h2>
              <p>按路径聚合的服务端与前端事件，队列满时自动丢弃，不阻塞业务请求。</p>
            </div>
          </div>
          <div className="table-scroll">
            <table className="data-table">
              <thead>
                <tr>
                  <th>路径</th>
                  <th>事件数</th>
                  <th>错误数</th>
                  <th>错误率</th>
                  <th>平均耗时</th>
                  <th>P95</th>
                </tr>
              </thead>
              <tbody>
                {(telemetry?.by_path ?? []).map((item) => (
                  <tr key={item.path}>
                    <td>
                      <code>{item.path}</code>
                    </td>
                    <td>{formatCount(item.count)}</td>
                    <td>{formatCount(item.errors)}</td>
                    <td>{formatPercent(item.error_rate)}</td>
                    <td>{formatCount(item.average_ms)}ms</td>
                    <td>{formatCount(item.p95_ms)}ms</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <PanelMessage loading={overview.loading} empty={!telemetry?.by_path.length ? '暂无埋点数据' : ''} />
          <div className="table-scroll">
            <table className="data-table">
              <thead>
                <tr>
                  <th>时间</th>
                  <th>来源</th>
                  <th>事件</th>
                  <th>路径</th>
                  <th>状态</th>
                  <th>耗时</th>
                </tr>
              </thead>
              <tbody>
                {(telemetry?.recent ?? []).map((event) => (
                  <RecentEventRow event={event} key={event.event_id} />
                ))}
              </tbody>
            </table>
          </div>
        </section>

        <section className="panel eval-panel">
          <div className="panel-title-row">
            <div>
              <h2>扩展评测报告</h2>
              <p>复用 quality/evals 产出的 JSON 报告，提供只读汇总与原始报告查看。</p>
            </div>
            <span className="tag">{formatCount(reportItems.length)} 份</span>
          </div>
          <div className="table-scroll">
            <table className="data-table">
              <thead>
                <tr>
                  <th>报告</th>
                  <th>类型</th>
                  <th>通过率</th>
                  <th>样本</th>
                  <th>生成时间</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {reportItems.map((report) => (
                  <tr className={detailId === report.id ? 'selected-row' : ''} key={report.id}>
                    <td>
                      <strong>{report.id}</strong>
                      <small>{report.path}</small>
                    </td>
                    <td>{report.type || report.kind}</td>
                    <td>{formatPercent(report.pass_rate)}</td>
                    <td>
                      {report.hits} / {report.evaluated || report.total}
                    </td>
                    <td>{formatDateTime(report.generated_at)}</td>
                    <td>
                      <button className="button button--ghost" onClick={() => void loadDetail(report)}>
                        查看
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <PanelMessage
            loading={reports.loading}
            error={reports.error}
            empty={!reportItems.length ? (reports.value?.dir ? `报告目录 ${reports.value.dir} 暂无报告` : '暂无扩展评测报告') : ''}
          />
          {detailError ? <p className="form-error">{detailError}</p> : null}
          {detail ? <pre className="trace-meta">{JSON.stringify(detail, null, 2)}</pre> : null}
        </section>
      </div>
    </section>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="trace-summary-item">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function RecentEventRow({ event }: { event: TelemetryEventView }) {
  return (
    <tr>
      <td>{formatDateTime(event.created_at)}</td>
      <td>{event.source === 'client' ? '前端' : '服务端'}</td>
      <td>{event.event_type}</td>
      <td>
        <code>{event.path || '-'}</code>
      </td>
      <td>{event.status || '-'}</td>
      <td>{event.duration_ms ? `${event.duration_ms}ms` : '-'}</td>
    </tr>
  );
}
