import { useState } from 'react';
import { Link, useParams } from 'react-router';
import { useResource } from '../api.js';
import { ChartPanel, EmptyRow, LabelButton, Metric, ModelsTable, NotFound, SessionLabelDialog, Share, TotalsMetrics } from '../components.jsx';
import { BILLING_MODES, BILLING_SOURCES, compact, cost, dateTime, duration, filterQuery, number, percent, periodLabel, periodOnly, shortId, tokens } from '../format.js';

const sessionLink = id => '/sessions/' + encodeURIComponent(id);
const EFFORT_SOURCES = { flag: 'set with --effort when Muse started', settings: 'from Muse settings when the prompt was sent', default: 'Muse’s default; no effort was set' };
const completionCost = row => row.api_equivalent_usd == null ? 'Unknown' : '$' + Number(row.api_equivalent_usd).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 6 });

const images = n => `${number(n)} image${n === 1 ? '' : 's'}`;

/** How long a turn ran, from its prompt to its last completion, to the second. */
function turnDuration(start, end) {
  const seconds = Math.max(0, Math.round((new Date(end) - new Date(start)) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${String(seconds % 60).padStart(2, '0')}s`;
  return `${Math.floor(minutes / 60)}h ${String(minutes % 60).padStart(2, '0')}m`;
}

/** Time since the turn started, as +m:ss. */
function elapsed(start, at) {
  const seconds = Math.max(0, Math.round((new Date(at) - new Date(start)) / 1000));
  return `+${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}

/** One prompt and everything it cost; expands to its completions, the subagents' among them. */
function TurnRow({ turn, index, events, session }) {
  const [open, setOpen] = useState(false);
  const subagents = new Set(turn.subagent_sessions);
  const completions = events.filter(row => row.session === session ? (row.turn || '') === turn.id : subagents.has(row.session));
  const prompt = turn.prompt != null ? turn.prompt + (turn.prompt_truncated ? '…' : '')
    : turn.recorded_by ? `Recorded by ${turn.recorded_by}; the prompt isn’t recorded`
    : turn.id ? 'Prompt no longer in the Muse log' : 'Usage recorded before turns were tracked';
  return <>
    <tr className={'turn-row' + (open ? ' open' : '')}>
      <td><button type="button" className="turn-toggle" aria-expanded={open} aria-label={`${open ? 'Hide' : 'Show'} completions of turn ${index + 1}`} onClick={() => setOpen(!open)}>{open ? '▾' : '▸'} {index + 1}</button></td>
      <td className="nowrap">{dateTime(turn.started)}</td>
      <td className="numeric" title={turn.ended ? `Prompt sent ${new Date(turn.started).toLocaleTimeString()}; last completion ${new Date(turn.ended).toLocaleTimeString()}` : undefined}>{turn.ended ? turnDuration(turn.started, turn.ended) : '—'}</td>
      <td className={'turn-prompt' + (turn.prompt == null ? ' unavailable' : '')} title={prompt + (turn.prompts > 1 ? `\n\n${turn.prompts} prompts went into this turn; the first is shown.` : '')}><span className="text">{prompt}</span>{turn.prompts > 1 && <span className="tree-tag">+{turn.prompts - 1}</span>}</td>
      <td>{turn.models.join(', ') || '—'}{turn.images > 0 && <span className="turn-subagents">{images(turn.images)}</span>}</td>
      <td title={turn.effort ? EFFORT_SOURCES[turn.effort_source] : 'Not recorded. Turns record their effort once the prompt hook is installed with meta-cost hook install.'}>{turn.effort || '—'}</td>
      <td className="numeric">{number(turn.completions)}{turn.subagents > 0 && <span className="turn-subagents">+ {turn.subagents} subagent{turn.subagents === 1 ? '' : 's'}</span>}</td>
      <td className="numeric">{compact(turn.input)} / {compact(turn.output)}</td>
      <td className="numeric">{cost(turn)}</td>
    </tr>
    {open && <tr className="turn-detail"><td colSpan={9}>
      <table><thead><tr><th>Time</th><th>Agent</th><th>Model</th><th>Input (cached)</th><th>Output</th><th>Cost</th></tr></thead><tbody>
        {!completions.length && <tr className="turn-note"><td colSpan={6}>No completions in this period.</td></tr>}
        {completions.map(row => <tr key={row.id} className={row.session === session ? '' : 'subagent'}>
          <td title={new Date(row.recorded_at).toLocaleString()}>{elapsed(turn.started, row.recorded_at)}</td>
          <td>{row.session === session ? 'Main' : <Link className="row-link" to={sessionLink(row.session)} title={row.session}>Subagent {shortId(row.session)}</Link>}</td>
          <td>{row.model}{row.images != null && ` · ${images(row.images)}`}</td><td>{number(row.input)} ({row.cached == null ? '?' : number(row.cached)})</td><td>{number(row.output)}</td><td>{completionCost(row)}</td></tr>)}
      </tbody></table></td></tr>}
  </>;
}

/** How the billing mode was found: the Muse hook's detections, or a hint when there are none. */
function BillingSource({ detections }) {
  if (!detections?.length) return <span className="billing-source" id="billing-source">Not detected. Sessions record their billing mode once the Muse hook is installed with meta-cost hook install.</span>;
  return <span className="billing-source" id="billing-source">{detections.map(d => `${BILLING_MODES[d.billing_mode] || d.billing_mode}: ${BILLING_SOURCES[d.source] || d.source}`
    + (d.key_label ? `, Keyguard key ${d.key_label}` : '') + (d.hook_source === 'resume' ? ' (resumed)' : '') + ` at ${dateTime(d.detected_at)}`).join('; ')}</span>;
}

function TreeNode({ node, total, current }) {
  return <li>
    <div className="tree-row">
      {node.id === current ? <strong title={node.id}>{shortId(node.id)} <span className="tree-tag">this session</span></strong> : <Link className="row-link" to={sessionLink(node.id)} title={node.id}>{shortId(node.id)}</Link>}
      <span className="tree-stats">{number(tokens(node))} tokens · {cost(node)}</span>
      <span className="tree-share"><Share value={percent(tokens(node), total)} /></span>
    </div>
    {node.children.length > 0 && <ul>{node.children.map(child => <TreeNode key={child.id} node={child} total={total} current={current} />)}</ul>}
  </li>;
}

export function NoUsageInPeriod({ filters, setFilters, subject }) {
  return <div className="notice" role="status">No usage for this {subject} in {periodLabel(filters).toLowerCase()}.
    {filters.period !== 'all' && <> <button type="button" className="inline-button" onClick={() => setFilters(previous => ({ ...previous, period: 'all', from: '', to: '' }))}>Show all time</button></>}</div>;
}

export default function SessionDetail({ filters, setFilters, options }) {
  const { id } = useParams();
  const [labelling, setLabelling] = useState(false);
  const { data, error, status, reload } = useResource(`/api/sessions/${encodeURIComponent(id)}?` + filterQuery(periodOnly(filters)));
  const [copied, setCopied] = useState(false);
  if (status === 404) return <NotFound kind="Session" message={`No usage has been recorded for session ${id}.`} />;
  if (!data) return error ? <div className="notice danger" role="alert">{error}</div> : <div className="loading-state" role="status">Loading session…</div>;
  const t = data.totals, family = tokens(t);
  function copy() { navigator.clipboard?.writeText(data.id).then(() => { setCopied(true); setTimeout(() => setCopied(false), 1500); }); }
  return <>
    <nav className="breadcrumb" aria-label="Breadcrumb"><Link to="/sessions">Sessions</Link>
      {data.root !== data.id && <><span aria-hidden="true">›</span><Link to={sessionLink(data.root)} title={data.root}>{shortId(data.root)}</Link></>}
      <span aria-hidden="true">›</span><span aria-current="page">{shortId(data.id)}</span></nav>
    <section className="panel detail-header" id="session-header">
      <div className="detail-title"><h2>{data.name ? <span id="session-name">{data.name}</span> : data.project ? <Link to={'/projects/' + encodeURIComponent(data.project)}>{data.project}</Link> : 'Session'}</h2>
        <code title="Session ID">{data.id}</code><button type="button" className="small-button" onClick={copy}>{copied ? 'Copied' : 'Copy ID'}</button>
        {data.root === data.id ? <LabelButton session={data} onClick={() => setLabelling(true)} />
          : data.label && <span className="session-label" title="Subagents take the label of their top-level session">{data.label}</span>}</div>
      {labelling && <SessionLabelDialog session={data} labels={options?.label} onClose={() => setLabelling(false)} onSaved={() => { setLabelling(false); reload(); }} />}
      <dl className="facts">
        {data.name && data.project && <div><dt>Project</dt><dd><Link className="row-link" to={'/projects/' + encodeURIComponent(data.project)}>{data.project}</Link></dd></div>}
        {data.parent && <div><dt>Subagent of</dt><dd><Link className="row-link" id="parent-link" to={sessionLink(data.parent)} title={data.parent}>{shortId(data.parent)}</Link></dd></div>}
        <div><dt>Started</dt><dd>{dateTime(data.started)}</dd></div>
        <div><dt>Last activity</dt><dd>{dateTime(data.last_activity)}</dd></div>
        <div><dt>Duration</dt><dd>{duration(data.started, data.last_activity)}</dd></div>
        <div><dt>Billing</dt><dd>{data.billing_modes.map(mode => BILLING_MODES[mode] || mode).join(', ') || '—'}<BillingSource detections={data.billing_detections} /></dd></div>
      </dl>
    </section>
    {!t.events && <NoUsageInPeriod filters={filters} setFilters={setFilters} subject="session" />}
    <TotalsMetrics totals={t} extra={<Metric label="This session alone" icon="◷" id="own-share" accent value={percent(tokens(data.own), family).toFixed(1) + '%'}
      description={`${number(tokens(data.own))} tokens · ${cost(data.own)} · subagents make up the rest`} />} />
    <section className="panel" id="turns"><div className="panel-heading"><div><h2>Turns</h2><p>Each prompt and what it cost, including the subagents it started. Prompts are read from the Muse log and never stored.</p></div></div>
      <div className="table-wrap"><table><thead><tr><th>#</th><th>Started</th><th>Duration</th><th>Prompt</th><th>Model</th><th>Effort</th><th>Completions</th><th>Input / output</th><th>Cost</th></tr></thead><tbody id="turn-rows">
        {!data.turns.length && <EmptyRow columns={9}>No turns in this period.</EmptyRow>}
        {data.turns.map((turn, i) => <TurnRow key={turn.id || 'untracked'} turn={turn} index={i} events={data.events} session={data.id} />)}
      </tbody></table></div></section>
    <section className="chart-layout"><ChartPanel timeline={data.timeline} unit={data.timeline_unit} />
      <article className="panel" id="subagents"><div className="panel-heading"><div><h2>Subagents</h2><p>Each session’s own usage and share of the total</p></div></div>
        {data.tree && data.tree.children.length ? <ul className="tree" id="subagent-tree"><TreeNode node={data.tree} total={family} current={data.id} /></ul>
          : <p className="panel-empty">This session started no subagents{t.events ? '' : ' in this period'}.</p>}
      </article></section>
    <section className="panel"><div className="panel-heading"><h2>Models</h2></div><ModelsTable models={data.models} total={family} /></section>
  </>;
}
