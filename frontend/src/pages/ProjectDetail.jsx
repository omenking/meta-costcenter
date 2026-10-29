import { Link, useParams } from 'react-router';
import { useResource } from '../api.js';
import { ChartPanel, Metric, ModelsTable, NotFound, SessionsTable, TotalsMetrics } from '../components.jsx';
import { filterQuery, number, periodOnly, tokens, usd } from '../format.js';
import { AlertMeter, WarnLevels, alertClass, windowText } from './Alerts.jsx';
import { NoUsageInPeriod } from './SessionDetail.jsx';

const STATUS = { ok: 'On track', warning: 'Warning', exceeded: 'Limit exceeded', paused: 'Paused' };

/** The alerts that cover this project, at the top of the page so that spending can be watched
 * against them: this project's own alerts first, then those for every project. */
function ProjectAlerts({ project, alerts }) {
  const covering = alerts.filter(alert => !alert.project || alert.project === project)
    .sort((a, b) => Boolean(b.project) - Boolean(a.project));
  return <section className="panel" id="project-alerts"><div className="panel-heading"><div><h2>Alerts</h2><p>Spending against the limits that cover this project</p></div>
    <Link className="panel-link" to="/alerts" state={{ project }}>Add alert for this project →</Link></div>
    {!covering.length ? <p className="panel-empty">No alerts cover this project yet.</p>
      : <ul className="project-alert-list">{covering.map(alert => <li key={alert.id} className={alertClass(alert)}>
        <div className="project-alert-head"><Link to="/alerts" className="project-alert-name">{alert.label}</Link>
          <span className="alert-status">{STATUS[alert.level] || alert.level}</span></div>
        <div className="project-alert-amount"><strong>{usd(alert.spent_usd)}</strong> of {usd(alert.limit_usd)} {windowText(alert)}<span>{alert.percent}%</span></div>
        <AlertMeter alert={alert} label={`${alert.label}: ${alert.percent}% of limit`} />
        <div className="project-alert-note">{alert.project ? 'This project' : 'All projects'}{alert.billing_mode ? ` · ${alert.billing_mode === 'payg' ? 'pay as you go' : alert.billing_mode} only` : ''}
          {alert.warn_levels?.length ? <> · warns at <WarnLevels levels={alert.warn_levels} /></> : ''}{alert.revoke_keys ? ` · revokes keys matching ${alert.key_match}` : ''}</div>
      </li>)}</ul>}
  </section>;
}

export default function ProjectDetail({ filters, setFilters, alerts = [], resolution, setResolution }) {
  const { name } = useParams();
  const { data, error, status } = useResource(`/api/projects/${encodeURIComponent(name)}?` + filterQuery({ ...periodOnly(filters), resolution }));
  if (status === 404) return <NotFound kind="Project" message={`No usage has been recorded for project ${name}.`} />;
  if (!data) return error ? <div className="notice danger" role="alert">{error}</div> : <div className="loading-state" role="status">Loading project…</div>;
  const t = data.totals;
  return <>
    <nav className="breadcrumb" aria-label="Breadcrumb"><Link to="/projects">Projects</Link><span aria-hidden="true">›</span><span aria-current="page">{data.project}</span></nav>
    <ProjectAlerts project={data.project} alerts={alerts} />
    {!t.events && <NoUsageInPeriod filters={filters} setFilters={setFilters} subject="project" />}
    <TotalsMetrics totals={t} extra={<Metric label="Sessions" icon="◷" id="session-count" accent value={number(data.sessions.length)} description="Subagents included in their parent" />} />
    <ChartPanel chart={data.chart} setResolution={setResolution} />
    <section className="panel" id="project-sessions"><div className="panel-heading"><div><h2>Sessions</h2><p>Select a session to see what it did</p></div></div>
      <SessionsTable sessions={data.sessions} showProject={false} empty="No sessions in this period." /></section>
    <section className="panel"><div className="panel-heading"><h2>Models</h2></div><ModelsTable models={data.models} total={tokens(t)} /></section>
  </>;
}
