import { useResource } from '../api.js';
import { SessionsTable } from '../components.jsx';
import { filterQuery } from '../format.js';

export default function Sessions({ filters, options }) {
  const { data, error, reload } = useResource('/api/sessions?' + filterQuery(filters));
  if (!data) return error ? <div className="notice danger" role="alert">{error}</div> : <div className="loading-state" role="status">Loading sessions…</div>;
  return <section className="panel" id="sessions"><div className="panel-heading"><div><h2>Sessions</h2><p>One row per session; subagent usage is included in its parent. Select a session to see what it did.</p></div></div>
    <SessionsTable sessions={data} labels={options?.label} onLabelled={reload} />
    <div className="table-footer">{data.length} session{data.length === 1 ? '' : 's'} · API-equivalent token estimates, not billed charges. “+” means partial pricing.</div></section>;
}
